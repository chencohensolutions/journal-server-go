package importcmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Importing from Google Drive and OneDrive goes through rclone (a separate program, like vips and ffprobe;
// winget install Rclone.Rclone, brew install rclone): it signs in in the browser the first time, with
// read-only access, and keeps the sign-in. The script lists every photo and video in the cloud, skips
// the ones handled by earlier runs (.cache/cloud-seen.tsv) and, on Google Drive, the ones whose sha256
// the data folder already has, without downloading them. The rest are downloaded to --temp in batches,
// largest first, and imported like files on disk; each is then deleted from --temp (or, when not
// imported, moved to --not-imported). Every file's outcome is appended to <imported>/cloud-log.tsv.
// Nothing in the cloud is ever changed.

// cloudService is a cloud the script imports from, through an rclone remote of its own.
type cloudService struct {
	label  string   // the folder its files are downloaded to inside --temp, and their folder clue
	remote string   // the rclone remote's name
	setup  []string // rclone config create arguments after the remote's name
}

var cloudServices = map[string]cloudService{
	"gdrive": {label: "Google Drive", remote: "journal-gdrive", setup: []string{"drive", "scope=drive.readonly"}},
	"onedrive": {label: "OneDrive", remote: "journal-onedrive",
		setup: []string{"onedrive", "access_scopes=Files.Read Files.Read.All Sites.Read.All offline_access"}},
}

// remoteFile is one file as rclone lsjson lists it.
type remoteFile struct {
	Path    string            `json:"Path"`
	Size    int64             `json:"Size"`
	ModTime string            `json:"ModTime"` // only to tell whether it changed since; never a photo's date
	IsDir   bool              `json:"IsDir"`
	Hashes  map[string]string `json:"Hashes"`
}

type cloudImport struct {
	im      *importer
	rclone  string
	temp    string
	service cloudService
	root    string // temp/<service label>: where this service's batch is downloaded

	mu       sync.Mutex
	seen     map[string]string // "<label>:<path>" → "<size>\t<modified>" of files handled before
	seenFile *os.File
	logFile  *os.File
	listed   map[string]remoteFile // this batch's files, by "<label>:<path>"

	skippedSeen, skippedKnown int
	skippedKnownBytes         int64
	downloadFailed            int
}

// findRclone returns the rclone program: the given path, the PATH, or where winget links it.
func findRclone(configured string) (string, error) {
	candidates := []string{configured, "rclone", "/opt/homebrew/bin/rclone", "/usr/local/bin/rclone"}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		candidates = append(candidates, filepath.Join(local, "Microsoft", "WinGet", "Links", "rclone.exe"))
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if found, err := exec.LookPath(candidate); err == nil {
			return found, nil
		}
	}
	return "", fmt.Errorf("rclone is not installed (winget install Rclone.Rclone; brew install rclone; or pass --rclone)")
}

func (im *importer) importCloud(ctx context.Context, names []string, rclone, temp string, batchBytes int64, jobs int) {
	c := &cloudImport{im: im, rclone: rclone, temp: temp, seen: map[string]string{}}
	im.cloud = c
	check(os.MkdirAll(temp, 0o755))
	c.openSeen()
	defer c.seenFile.Close()
	logPath := filepath.Join(im.importedFolder, "cloud-log.tsv")
	if !im.dryRun {
		check(os.MkdirAll(im.importedFolder, 0o755))
		_, statErr := os.Stat(logPath)
		var err error
		c.logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		check(err)
		defer c.logFile.Close()
		if os.IsNotExist(statErr) {
			fmt.Fprintln(c.logFile, "time\tresult\tcloud file\tsha256\tin the data folder\tnote")
		}
	}

	for _, name := range names {
		service, ok := cloudServices[strings.TrimSpace(name)]
		if !ok {
			log.Fatalf("--cloud: unknown %q (gdrive, onedrive)", name)
		}
		if ctx.Err() != nil {
			break
		}
		c.service = service
		c.root = filepath.Join(temp, service.label)
		im.roots = []string{c.root}
		c.ensureRemote(ctx)
		files := c.list(ctx)
		c.download(ctx, files, batchBytes, jobs)
	}
	fmt.Printf("\nCloud: %d files handled by earlier runs, %d (%s) already in the data folder by their sha256 (not downloaded), %d downloads failed (tried again next run)\n",
		c.skippedSeen, c.skippedKnown, gb(c.skippedKnownBytes), c.downloadFailed)
	if !im.dryRun {
		fmt.Printf("Every file's outcome: %s\n", logPath)
	}
}

// ensureRemote signs in to the service the first time: rclone opens the browser.
func (c *cloudImport) ensureRemote(ctx context.Context) {
	out, err := exec.CommandContext(ctx, c.rclone, "listremotes").Output()
	check(err)
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.TrimSpace(line) == c.service.remote+":" {
			return
		}
	}
	log.Printf("Connecting %s: rclone opens your browser to sign in (read-only access). Answer its questions with Enter for the defaults.", c.service.label)
	args := append([]string{"config", "create", c.service.remote}, c.service.setup...)
	cmd := exec.CommandContext(ctx, c.rclone, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("signing in to %s: %v", c.service.label, err)
	}
}

// list returns the photos and videos in the cloud that still need downloading, largest first. Files
// handled before are skipped, and on Google Drive so are files whose sha256 the data folder has (their
// folder is added to the copy there).
func (c *cloudImport) list(ctx context.Context) []remoteFile {
	log.Printf("Listing %s (this can take a few minutes)", c.service.label)
	cmd := exec.CommandContext(ctx, c.rclone, "lsjson", "--recursive", "--files-only", "--hash", "--fast-list", c.service.remote+":")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		log.Fatalf("listing %s: %v", c.service.label, err)
	}
	var all []remoteFile
	check(json.Unmarshal(out, &all))

	var files []remoteFile
	var bytes int64
	media := 0
	for _, f := range all {
		name := path.Base(f.Path)
		if f.IsDir || f.Size <= 0 || strings.HasPrefix(name, ".") {
			continue
		}
		if _, ok := mediaTypes[strings.ToLower(path.Ext(name))]; !ok {
			continue
		}
		media++
		key := c.key(f.Path)
		if c.seen[key] == c.stamp(f) {
			c.skippedSeen++
			continue
		}
		if sha := strings.ToLower(f.Hashes["sha256"]); sha != "" {
			if _, ok := c.im.store.Get(sha); ok {
				c.skippedKnown++
				c.skippedKnownBytes += f.Size
				folder := folderOf(c.root, filepath.Join(c.root, filepath.FromSlash(f.Path)))
				if !c.im.dryRun && c.im.addFolder(key, sha, folder) {
					c.log(resultDuplicate, key, sha, "not downloaded: its sha256 is already in the data folder")
					c.remember(key, c.stamp(f))
				}
				continue
			}
		}
		files = append(files, f)
		bytes += f.Size
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].Size > files[j].Size })
	log.Printf("%s: %d photos and videos, %d to download (%s)", c.service.label, media, len(files), gb(bytes))
	return files
}

// download fetches the files in batches of about batchBytes, largest first, and imports each batch.
func (c *cloudImport) download(ctx context.Context, files []remoteFile, batchBytes int64, jobs int) {
	if c.im.dryRun || len(files) == 0 {
		return
	}
	for start := 0; start < len(files) && ctx.Err() == nil; {
		end, size := start, int64(0)
		for end < len(files) && (end == start || size+files[end].Size <= batchBytes) {
			size += files[end].Size
			end++
		}
		batch := files[start:end]
		start = end
		log.Printf("%s: downloading %d files (%s) to %s", c.service.label, len(batch), gb(size), c.root)

		listPath := filepath.Join(c.im.workDir, "batch.txt")
		var lines []string
		c.listed = map[string]remoteFile{}
		for _, f := range batch {
			lines = append(lines, f.Path)
			c.listed[c.key(f.Path)] = f
		}
		check(os.WriteFile(listPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
		cmd := exec.CommandContext(ctx, c.rclone, "copy", c.service.remote+":", c.root, "--files-from-raw", listPath,
			"--transfers", strconv.Itoa(jobs), "--stats", "10s", "--stats-one-line", "--stats-log-level", "NOTICE")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			log.Printf("%s: rclone copy: %v (the files it got are imported; the rest are tried again next run)", c.service.label, err)
		}
		if ctx.Err() != nil {
			break // what was downloaded stays in the temp folder; the next run picks it up
		}

		var local []sourceFile
		for _, f := range batch {
			p := filepath.Join(c.root, filepath.FromSlash(f.Path))
			if info, err := os.Stat(p); err == nil && info.Size() == f.Size {
				local = append(local, sourceFile{p, f.Size})
			} else {
				c.downloadFailed++
				c.log("download failed", c.key(f.Path), "", fmt.Sprint(err))
			}
		}
		c.im.mu.Lock()
		c.im.totalFiles, c.im.totalBytes, c.im.doneFiles, c.im.doneBytes = len(local), size, 0, 0
		c.im.mu.Unlock()
		c.im.importFiles(ctx, local, jobs)
		removeEmptyFolders(c.root)
	}
}

// finished deletes a downloaded file that is now in the data folder (imported, or there already).
func (c *cloudImport) finished(path, sha, result, note string) {
	if err := os.Remove(path); err != nil {
		note = strings.TrimSpace(note + " (not deleted from the temp folder: " + err.Error() + ")")
	}
	c.record(path, sha, result, note, "")
}

// record logs what happened to a downloaded file and, unless it failed, remembers it so later runs
// don't download it again.
func (c *cloudImport) record(path, sha, result, note, nowAt string) {
	key := c.origin(path)
	if nowAt != "" {
		note = strings.TrimSpace(note + " now at " + nowAt)
	}
	c.log(result, key, sha, note)
	if !(result == resultNotImported && strings.HasPrefix(note, notImportedFailed)) {
		c.mu.Lock()
		f, ok := c.listed[key]
		c.mu.Unlock()
		if ok {
			c.remember(key, c.stamp(f))
		}
	}
}

func (c *cloudImport) log(result, key, sha, note string) {
	if c.logFile == nil {
		return
	}
	inData := ""
	if item, ok := c.im.store.Get(sha); ok && sha != "" {
		inData = item.Path
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintln(c.logFile, strings.Join([]string{time.Now().Format(time.RFC3339), result, key, sha, inData, note}, "\t"))
}

// origin is a downloaded file's place in the cloud ("Google Drive:Photos/2019/x.jpg"), from its place in
// the temp folder.
func (c *cloudImport) origin(local string) string {
	rel, err := filepath.Rel(c.root, local)
	if err != nil || strings.HasPrefix(rel, "..") {
		return local
	}
	return c.key(filepath.ToSlash(rel))
}

func (c *cloudImport) key(remotePath string) string { return c.service.label + ":" + remotePath }

func (c *cloudImport) stamp(f remoteFile) string { return fmt.Sprintf("%d\t%s", f.Size, f.ModTime) }

// The files handled before, in the data folder's .cache/cloud-seen.tsv ("<cloud file>\t<size>\t<modified>").
func (c *cloudImport) openSeen() {
	p := filepath.Join(c.im.dataFolder, ".cache", "cloud-seen.tsv")
	if f, err := os.Open(p); err == nil {
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			if key, stamp, ok := strings.Cut(scanner.Text(), "\t"); ok {
				c.seen[key] = stamp
			}
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			log.Fatalf("reading %s: %v", p, err)
		}
	}
	var err error
	c.seenFile, err = os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	check(err)
}

func (c *cloudImport) remember(key, stamp string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen[key] = stamp
	fmt.Fprintf(c.seenFile, "%s\t%s\n", key, stamp)
}

// removeEmptyFolders deletes the folders left empty under root once their files are imported.
func removeEmptyFolders(root string) {
	var dirs []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return nil
	})
	for _, dir := range slices.Backward(dirs) {
		os.Remove(dir) // fails, as it should, while a folder still has files
	}
}
