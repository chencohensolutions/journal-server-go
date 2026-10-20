// Command import copies the photos and videos found in folders on the server's disk into a user's data
// folder, once each: a file whose sha256 the data folder already has (from a device, an earlier run, or
// another copy found in this run) is not copied again. Every source file it is done with, copied or a
// duplicate, is then moved to the --imported folder under its original path, so that folder can be
// deleted as a whole, and whatever is left in the searched folders is what wasn't imported (see the report).
//
// Files are only copied, byte for byte (the copy's sha256 is checked): nothing is written into them. What
// is read from them (date, size, GPS) goes in the sidecar next to the copy, like an upload's facts.
//
// The date comes from the file name (phones name files by the local time they were taken), checked
// against the photo's EXIF (read with libvips' vipsheader, or from the DNG itself) or the video's own
// metadata (ffprobe), which add the time zone when they agree. Without a date in the name, the metadata
// is used. A time without an offset is taken as this machine's time zone. Files with no date are left
// where they are.
//
// Photos that look alike but aren't the same bytes (a resized copy, a burst) are found by their dHash and
// listed in near-duplicates-<time>.tsv; they are imported all the same. The dHash of every photo in the
// data folder is kept in its .cache/dhash.tsv, and the sha256 of every file looked at in
// .cache/source-hashes.tsv (by path, size and modified time), so the import after a dry run doesn't read
// everything again.
//
// The server may keep running: it and every import take turns at writing to the data folder (media/turns.go),
// and it sees imported files at once. Run from the server folder:
//
//	go run ./scripts/import --user hen --imported D:\Imported --not-imported "D:\Not imported" [--dry-run] D:\Backups "E:\Old phone"
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
)

// What is imported, by extension.
var mediaTypes = map[string]struct {
	kind media.Kind
	mime string
}{
	".jpg":  {media.KindPhoto, "image/jpeg"},
	".jpeg": {media.KindPhoto, "image/jpeg"},
	".heic": {media.KindPhoto, "image/heic"},
	".heif": {media.KindPhoto, "image/heif"},
	".png":  {media.KindPhoto, "image/png"},
	".webp": {media.KindPhoto, "image/webp"},
	".gif":  {media.KindPhoto, "image/gif"},
	".dng":  {media.KindPhoto, "image/x-adobe-dng"},
	".mp4":  {media.KindVideo, "video/mp4"},
	".mov":  {media.KindVideo, "video/quicktime"},
	".m4v":  {media.KindVideo, "video/x-m4v"},
	".3gp":  {media.KindVideo, "video/3gpp"},
	".mkv":  {media.KindVideo, "video/x-matroska"},
	".webm": {media.KindVideo, "video/webm"},
	".avi":  {media.KindVideo, "video/x-msvideo"},
}

// Folders never searched, besides hidden ones (".thumbnails", ".cache") and the data and imported folders.
var skippedFolders = map[string]bool{"$recycle.bin": true, "system volume information": true}

type importer struct {
	store          *media.Store
	vips           string
	vipsheader     string
	ffprobe        string
	workDir        string // the small previews dHash is made from
	user           string
	dataFolder     string
	importedFolder string
	runFolder      string // inside importedFolder, named by when this run started: what it moves, and its reports
	notImported    string
	notImportedRun string   // inside notImported, named like runFolder: the files this run didn't import, by why
	moveOther      bool     // also move files that aren't photos or videos, and hidden ones
	roots          []string // the folders searched
	dryRun         bool
	nearDistance   int
	dhashes        *dhashTable
	sources        *sourceHashes
	cloud          *cloudImport // set when importing from Google Drive or OneDrive (cloud.go)

	mu           sync.Mutex      // everything below
	seen         map[string]bool // dry run: sha256s that would have been imported
	waiting      map[string]int  // sha256s of near-duplicates waiting for review: their index in toReview
	photos       []photo         // compared for near-duplicates
	counts       counts
	notMedia     map[string]int // by extension
	leftBehind   []string       // report lines: files that stay where they are
	rejected     []string       // report lines: files moved to the not-imported folder
	others       []sourceFile   // files that aren't photos or videos, or are hidden: moved with --move-other
	dateNotes    []string
	near         []nearPair
	toReview     []reviewItem
	lastProgress time.Time
	started      time.Time
	totalFiles   int
	totalBytes   int64
	doneFiles    int
	doneBytes    int64
}

// photo is one photo in the data folder (or on its way there, or in a dry run one that would be), with
// its dHash.
type photo struct {
	hash    dhash
	sha256  string
	name    string
	takenAt *time.Time
	path    string // where it was found, for one from this run (the data folder's is looked up by sha256)
}

// nearPair is a photo found to look like one before it.
type nearPair struct {
	distance int
	burst    bool
	path     string // the new photo, where it was found
	like     photo
}

type counts struct {
	imported, duplicates, noDate, failed, hidden int
	importedBytes, duplicateBytes                int64
}

func main() {
	user := flag.String("user", "", "user whose data folder the files go to (from config.json)")
	imported := flag.String("imported", "", "folder the source files are moved to once imported (or found to be duplicates)")
	dryRun := flag.Bool("dry-run", false, "only count what would be imported; nothing is copied or moved")
	jobs := flag.Int("jobs", 4, "files worked on at once")
	near := flag.Int("near", 4, "most dHash bits two photos may differ in to be listed as near-duplicates (0–64)")
	review := flag.Bool("review", false, "show the near-duplicates waiting in the imported folder side by side, to import or skip each")
	noBrowser := flag.Bool("no-browser", false, "with --review, only print the page's address")
	notImported := flag.String("not-imported", "", "folder the files that weren't imported are moved to, by why (required to import)")
	moveOther := flag.Bool("move-other", false, "also move files that aren't photos or videos, and hidden ones (.trashed-…), to the not-imported folder")
	backfill := flag.Bool("backfill-folders", false, "only add the folders of files looked at by earlier runs (from .cache/source-hashes.tsv) to their sidecars")
	cloud := flag.String("cloud", "", "import from the cloud instead of folders: gdrive, onedrive, or gdrive,onedrive (through rclone; see cloud.go)")
	temp := flag.String("temp", "", "with --cloud, the folder files are downloaded to before they are imported (emptied as they are)")
	batchGB := flag.Float64("batch-gb", 20, "with --cloud, about how much is downloaded at a time")
	rclonePath := flag.String("rclone", "", "with --cloud, the rclone program (found on the PATH or where winget links it when empty)")
	configPath := flag.String("config", "config.json", "the server's config file")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/import --user <name> --imported <folder> --not-imported <folder> [--dry-run] [--move-other] <folder>...")
		fmt.Fprintln(os.Stderr, "       go run ./scripts/import --user <name> --imported <folder> --not-imported <folder> --cloud gdrive,onedrive --temp <folder> [--dry-run]")
		fmt.Fprintln(os.Stderr, "       go run ./scripts/import --user <name> --imported <folder> --review")
		fmt.Fprintln(os.Stderr, "       go run ./scripts/import --user <name> --imported <folder> --backfill-folders <folder>...")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *user == "" || *imported == "" || (flag.NArg() == 0) != (*review || *cloud != "") || *jobs < 1 ||
		(*cloud != "") != (*temp != "") || *batchGB <= 0 {
		flag.Usage()
		os.Exit(2)
	}

	config, err := loadConfig(*configPath)
	check(err)
	var dataFolder string
	for _, u := range config.Users {
		if u.Username == *user {
			dataFolder = u.DataFolder
		}
	}
	if dataFolder == "" {
		log.Fatalf("%s: user %q has no data_folder", *configPath, *user)
	}
	vips, vipsheader, err := media.FindVips(config.Vips)
	if err != nil {
		log.Fatalf("%v (brew install vips; on Windows go run ./scripts/bundle-vips; or set \"vips\" in config.json)", err)
	}
	_, ffprobe, err := media.FindFFmpeg(config.FFmpeg)
	if err != nil {
		log.Fatalf("%v (brew install ffmpeg; on Windows winget install Gyan.FFmpeg; or set \"ffmpeg\" in config.json)", err)
	}

	if !*review && !*backfill && *notImported == "" {
		log.Fatal("--not-imported: the folder for the files that aren't imported (for example \"D:\\Not imported\")")
	}
	started := claimRunName(absolute(*imported), time.Now().Format("2006-01-02-15-04-05"))
	im := &importer{
		vips:           vips,
		vipsheader:     vipsheader,
		ffprobe:        ffprobe,
		user:           *user,
		dataFolder:     absolute(dataFolder),
		importedFolder: absolute(*imported),
		runFolder:      filepath.Join(absolute(*imported), started),
		moveOther:      *moveOther,
		dryRun:         *dryRun,
		nearDistance:   *near,
		seen:           map[string]bool{},
		waiting:        map[string]int{},
		notMedia:       map[string]int{},
		lastProgress:   time.Now(),
	}
	if *notImported != "" {
		im.notImported = absolute(*notImported)
		im.notImportedRun = filepath.Join(im.notImported, started)
	}
	roots := make([]string, flag.NArg())
	for i, root := range flag.Args() {
		roots[i] = absolute(root)
		if im.excluded(roots[i]) {
			log.Fatalf("%s is inside the data folder, the imported folder or the not-imported folder", root)
		}
	}
	im.roots = roots
	im.store, err = media.OpenStore(im.dataFolder, time.Now)
	check(err)
	im.dhashes, err = openDHashTable(im.dataFolder)
	check(err)
	defer im.dhashes.close()
	im.sources, err = openSourceHashes(im.dataFolder)
	check(err)
	defer im.sources.close()
	im.workDir, err = os.MkdirTemp("", "journal-import-")
	check(err)
	if !im.dryRun {
		check(im.store.ClearImports())
	}
	// Files other imports, the server or a scan place meanwhile are compared with as well.
	im.store.OnCatchUp(im.learn)
	defer os.RemoveAll(im.workDir)
	defer func() {
		// A run that moved nothing and wrote no report leaves no folders behind.
		os.Remove(im.runFolder)
		if im.notImportedRun != "" {
			os.Remove(im.notImportedRun)
		}
	}()

	// Ctrl+C finishes the files in hand, then stops; running again continues where it stopped.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *review {
		im.review(ctx, !*noBrowser)
		return
	}
	if *backfill {
		im.backfillFolders()
		return
	}

	usage := im.store.Usage()
	log.Printf("Data folder %s: %d photos, %d videos", im.dataFolder, usage.Photos, usage.Videos)
	im.hashDataFolder(ctx, *jobs)
	if im.dryRun {
		log.Print("Dry run: nothing is copied or moved")
	} else if *cloud == "" {
		log.Printf("Moving what is done to %s", im.runFolder)
	}

	if *cloud != "" {
		rclone, err := findRclone(*rclonePath)
		check(err)
		tempFolder := absolute(*temp)
		if im.excluded(tempFolder) {
			log.Fatalf("--temp %s is inside the data folder, the imported folder or the not-imported folder", *temp)
		}
		im.importCloud(ctx, strings.Split(*cloud, ","), rclone, tempFolder, int64(*batchGB*(1<<30)), *jobs)
		im.summarize()
		im.addToReview()
		return
	}

	// List everything first, so progress can be shown against the total.
	var files []sourceFile
	for _, root := range roots {
		found, err := im.walk(ctx, root)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				log.Fatal("Stopped")
			}
			log.Printf("%s: %v", root, err)
		}
		files = append(files, found...)
	}
	for _, f := range files {
		im.totalBytes += f.size
	}
	im.totalFiles = len(files)
	log.Printf("%d photos and videos to look at, %s", im.totalFiles, gb(im.totalBytes))
	im.importFiles(ctx, files, *jobs)
	if ctx.Err() != nil {
		log.Print("Stopped; running again continues where it stopped")
	}
	if im.moveOther && ctx.Err() == nil {
		im.moveOthers()
	}
	im.summarize()
	im.addToReview()
}

// importFiles reads every file several at a time, then decides on the new ones one at a time, largest
// first: the original of a photo then goes in before its smaller copies (which wait as near-duplicates),
// and what happens doesn't depend on which file was read first.
func (im *importer) importFiles(ctx context.Context, files []sourceFile, jobs int) {
	im.started = time.Now()
	found := make([]*examined, len(files))
	queue := make(chan int)
	var workers sync.WaitGroup
	for range jobs {
		workers.Go(func() {
			for i := range queue {
				found[i] = im.examine(ctx, files[i].path)
				im.progress(files[i].size)
			}
		})
	}
	for i := range files {
		if ctx.Err() != nil {
			break
		}
		queue <- i
	}
	close(queue)
	workers.Wait()

	var news []*examined
	var newBytes int64
	for _, e := range found {
		if e != nil {
			news = append(news, e)
			newBytes += e.size
		}
	}
	sort.SliceStable(news, func(i, j int) bool { return news[i].size > news[j].size })
	if ctx.Err() != nil || len(news) == 0 {
		return
	}
	log.Printf("%d new files (%s): importing, largest first", len(news), gb(newBytes))
	start, last := time.Now(), time.Now()
	var doneBytes int64
	for i, e := range news {
		if ctx.Err() != nil {
			break
		}
		im.store.CatchUp() // decide knowing what the others placed meanwhile
		im.decide(e)
		doneBytes += e.size
		if time.Since(last) >= 10*time.Second || i == len(news)-1 {
			log.Printf("Importing: %5.1f%%  %s of %s, %d of %d files, about %s left",
				float64(doneBytes)*100/float64(max(newBytes, 1)), gb(doneBytes), gb(newBytes), i+1, len(news),
				timeLeft(time.Since(start), doneBytes, newBytes))
			last = time.Now()
		}
	}
}

// hashDataFolder makes the dHash of the photos in the data folder that don't have one in the table yet
// (the first import, or photos uploaded since), so new files are compared with them.
func (im *importer) hashDataFolder(ctx context.Context, jobs int) {
	var all, missing []media.Item
	for cursor, more := "", true; more; {
		var page []media.Item
		var err error
		page, cursor, more, err = im.store.Changes(cursor, 5000)
		check(err)
		for _, item := range page {
			if item.Sidecar.Facts.Kind == media.KindPhoto {
				all = append(all, item)
				if _, ok := im.dhashes.get(item.Sha256()); !ok {
					missing = append(missing, item)
				}
			}
		}
	}
	if len(missing) > 0 {
		log.Printf("Making the dHash of %d photos in the data folder (kept for later imports)", len(missing))
		items := make(chan media.Item)
		var workers sync.WaitGroup
		for range jobs {
			workers.Go(func() {
				for item := range items {
					im.dhashOf(ctx, item.Sha256(), filepath.Join(im.dataFolder, filepath.FromSlash(item.Path)))
				}
			})
		}
		for _, item := range missing {
			if ctx.Err() != nil {
				break
			}
			items <- item
		}
		close(items)
		workers.Wait()
		if ctx.Err() != nil {
			log.Fatal("Stopped")
		}
	}
	for _, item := range all {
		if h, _ := im.dhashes.get(item.Sha256()); h != nil {
			facts := item.Sidecar.Facts
			im.photos = append(im.photos, photo{hash: *h, sha256: item.Sha256(), name: facts.FileName, takenAt: facts.TakenAt})
		}
	}
}

// dhashOf returns the photo's dHash from the table, or makes it and adds it; nil when libvips can't read it.
func (im *importer) dhashOf(ctx context.Context, sha, path string) *dhash {
	if h, ok := im.dhashes.get(sha); ok {
		return h
	}
	h, err := photoDHash(ctx, im.vips, im.workDir, path)
	if ctx.Err() != nil {
		return nil // stopped: not remembered as unreadable
	}
	if err != nil {
		log.Printf("%s: no dHash: %v", path, err)
		im.dhashes.put(sha, nil)
		return nil
	}
	im.dhashes.put(sha, &h)
	return &h
}

// sourceFile is a photo or video found in a searched folder.
type sourceFile struct {
	path string
	size int64
}

// walk lists the photos and videos under root, and counts the files it doesn't look at.
func (im *importer) walk(ctx context.Context, root string) ([]sourceFile, error) {
	var files []sourceFile
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			log.Printf("%v; skipped", err)
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || skippedFolders[strings.ToLower(name)] || im.excluded(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		var size int64
		if info, err := entry.Info(); err == nil {
			size = info.Size()
		}
		im.mu.Lock()
		defer im.mu.Unlock()
		_, isMedia := mediaTypes[ext]
		switch {
		case !isMedia:
			im.notMedia[ext]++
			im.others = append(im.others, sourceFile{path, size})
		case strings.HasPrefix(name, "."):
			// Android's .trashed-… and .pending-… files: deleted on the phone, or never finished.
			im.counts.hidden++
			im.others = append(im.others, sourceFile{path, size})
		default:
			files = append(files, sourceFile{path, size})
		}
		return nil
	})
	return files, err
}

// Folders inside a run's not-imported folder, by why the files in them weren't imported.
const (
	notImportedNoDate = "no date"
	notImportedFailed = "failed"
	notImportedNear   = "near-duplicates" // waiting for --review
	notImportedOther  = "not photos or videos"
	notImportedHidden = "hidden"
)

// examined is a file read and found new to the data folder, waiting for decide.
type examined struct {
	path   string
	size   int64
	sha256 string
	folder string
	facts  facts
	hash   *dhash
}

// examine reads one file (several at a time): its sha256, then, unless the data folder already has it
// (a duplicate, moved to the imported folder at once), its date and dHash. A file that can't be
// imported is moved to the not-imported folder, by reason. New files come back for decide.
func (im *importer) examine(ctx context.Context, path string) *examined {
	info, err := os.Stat(path)
	if err != nil {
		im.leave(path, err.Error())
		return nil
	}
	if info.Size() == 0 {
		im.reject(path, notImportedFailed, "empty file")
		return nil
	}
	sha, err := im.sources.sha256(path, info)
	if err != nil {
		im.reject(path, notImportedFailed, err.Error())
		return nil
	}
	folder := folderOf(im.rootOf(path), path)
	if _, ok := im.store.Get(sha); ok {
		if im.addFolder(path, sha, folder) {
			im.count(true, info.Size())
			im.done(path, sha, resultDuplicate)
		}
		return nil
	}

	facts := im.readFacts(ctx, path)
	if ctx.Err() != nil {
		return nil // stopped while vipsheader or ffprobe ran: the facts may be missing
	}
	if facts.note != "" {
		im.mu.Lock()
		im.dateNotes = append(im.dateNotes, path+"\t"+facts.note)
		im.mu.Unlock()
	}
	if facts.takenAt == nil {
		im.reject(path, notImportedNoDate, "no date in its file name or metadata")
		im.mu.Lock()
		im.counts.noDate++
		im.mu.Unlock()
		return nil
	}
	e := &examined{path: path, size: info.Size(), sha256: sha, folder: folder, facts: facts}
	if facts.kind == media.KindPhoto {
		e.hash = im.dhashOf(ctx, sha, path)
	}
	if ctx.Err() != nil {
		return nil
	}
	return e
}

// decide imports one examined file, or keeps it for review as a near-duplicate; one file at a time, the
// largest first, so the original of a photo goes in before its smaller copies.
func (im *importer) decide(e *examined) {
	folders := []string{}
	if e.folder != "" {
		folders = append(folders, e.folder)
	}
	// Another copy found in this run: imported, or waiting for review.
	if _, ok := im.store.Get(e.sha256); ok {
		if im.addFolder(e.path, e.sha256, e.folder) {
			im.count(true, e.size)
			im.done(e.path, e.sha256, resultDuplicate)
		}
		return
	}
	if i, ok := im.waiting[e.sha256]; ok {
		if !im.dryRun {
			im.toReview[i].AlsoFoundIn = append(im.toReview[i].AlsoFoundIn, folders...)
		}
		im.count(true, e.size)
		im.done(e.path, e.sha256, resultDuplicateWaiting)
		return
	}
	if im.dryRun && im.seen[e.sha256] {
		im.count(true, e.size)
		return
	}

	if e.hash != nil {
		if wait, item := im.nearCheck(photo{hash: *e.hash, sha256: e.sha256, name: filepath.Base(e.path), takenAt: e.facts.takenAt, path: e.path}); wait {
			im.waiting[e.sha256] = len(im.toReview)
			im.waitForReview(item)
			return
		}
	}
	if im.dryRun {
		im.seen[e.sha256] = true
		im.count(false, e.size)
		return
	}
	_, existed, err := im.importInto(e.path, e.sha256, e.size, e.facts, folders)
	if err != nil {
		im.reject(e.path, notImportedFailed, err.Error())
		im.mu.Lock()
		im.counts.failed++
		im.mu.Unlock()
		return
	}
	if existed && !im.addFolder(e.path, e.sha256, e.folder) {
		return
	}
	im.count(existed, e.size)
	result := resultImported
	if existed {
		result = resultDuplicate
	}
	im.done(e.path, e.sha256, result)
}

// addFolder records the folder a copy of a file already in the data folder was found in, in that file's
// sidecar (dry run: nothing). False when it couldn't, and the copy stays where it is.
func (im *importer) addFolder(path, sha, folder string) bool {
	if im.dryRun || folder == "" {
		return true
	}
	if _, err := im.store.AddFolders(sha, []string{folder}); err != nil {
		im.leave(path, "its folder couldn't be added to the copy in the data folder: "+err.Error())
		return false
	}
	return true
}

// folderOf is the folder path was found in, from the searched folder down, with forward slashes
// (D:\Archive\phone DCIM, D:\Archive\phone DCIM\Flowers\x.jpg → "phone DCIM/Flowers").
func folderOf(root, path string) string {
	rel, err := filepath.Rel(filepath.Dir(root), filepath.Dir(path))
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// importInto copies the file into the data folder with the facts read from it.
func (im *importer) importInto(path, sha string, size int64, facts facts, folders []string) (media.Item, bool, error) {
	source := sourceOfPath(path)
	from := path
	if im.cloud != nil {
		from = im.cloud.origin(path)
	}
	return im.store.Import(path, media.UploadRequest{
		Sha256:       sha,
		Size:         size,
		FileName:     filepath.Base(path),
		MimeType:     facts.mime,
		Kind:         facts.kind,
		TakenAt:      facts.takenAt,
		Width:        facts.width,
		Height:       facts.height,
		DurationMs:   facts.durationMs,
		Location:     facts.location,
		Source:       &source,
		UploadedBy:   "import",
		ImportedFrom: &from,
		Folders:      folders,
	})
}

func (im *importer) count(duplicate bool, size int64) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if duplicate {
		im.counts.duplicates++
		im.counts.duplicateBytes += size
	} else {
		im.counts.imported++
		im.counts.importedBytes += size
	}
}

// nearCheck compares a new photo with the ones in the data folder and those going in this run. When the
// closest is within nearDistance bits and isn't from the same burst, the photo waits for the owner's
// decision (wait: don't import it now). Otherwise it is added at once to the photos later ones are
// compared with, so two copies worked on at the same time still meet.
func (im *importer) nearCheck(p photo) (wait bool, item reviewItem) {
	im.mu.Lock()
	defer im.mu.Unlock()
	best, like := 65, photo{}
	for _, q := range im.photos {
		if d := p.hash.distance(q.hash); d < best {
			best, like = d, q
		}
	}
	if best <= im.nearDistance {
		burst := sameBurst(p, like)
		im.near = append(im.near, nearPair{distance: best, burst: burst, path: p.path, like: like})
		if !burst {
			return true, reviewItem{
				Path: p.path, FoundAt: p.path, Sha256: p.sha256, Root: im.rootOf(p.path),
				LikeSha256: like.sha256, LikePath: like.path, Distance: best,
			}
		}
	}
	im.photos = append(im.photos, p)
	return false, reviewItem{}
}

// waitForReview moves a near-duplicate to the not-imported folder and lists it for --review.
func (im *importer) waitForReview(item reviewItem) {
	if im.dryRun {
		return
	}
	dest, err := moveFile(item.Path, filepath.Join(im.notImportedRun, notImportedNear, pathInside(im.roots, item.Path)))
	if err != nil {
		im.leave(item.Path, "a near-duplicate, but not moved to the not-imported folder: "+err.Error())
	} else {
		item.Path = dest
		if im.cloud != nil {
			im.cloud.record(item.FoundAt, item.Sha256, resultWaiting, "", dest)
		}
	}
	im.mu.Lock()
	im.toReview = append(im.toReview, item)
	im.mu.Unlock()
}

// Burst shots are numbered within their second: 20260120_081253_035.jpg, PIC_20251202_192632357.jpg.
var burstNumber = regexp.MustCompile(`((?:19|20)\d{6})[-_]?(\d{6})[-_]?(\d{3})(?:\D|$)`)

// sameBurst reports whether two photos that look alike are shots in a row rather than copies of one
// shot: taken 1 to 60 seconds apart, or in the same second with different burst numbers.
func sameBurst(a, b photo) bool {
	if a.takenAt == nil || b.takenAt == nil {
		return false
	}
	gap := a.takenAt.Sub(*b.takenAt).Abs()
	if gap >= time.Second && gap <= time.Minute {
		return true
	}
	if gap >= time.Second {
		return false
	}
	ma, mb := burstNumber.FindStringSubmatch(a.name), burstNumber.FindStringSubmatch(b.name)
	return ma != nil && mb != nil && ma[1] == mb[1] && ma[2] == mb[2] && ma[3] != mb[3]
}

// claimRunName makes this run's folder in the imported folder, named by when it started, with "-2", "-3", …
// when another import started in the same second; the not-imported folder uses the same name.
func claimRunName(importedFolder, started string) string {
	check(os.MkdirAll(importedFolder, 0o755))
	for n := 1; ; n++ {
		name := started
		if n > 1 {
			name = fmt.Sprintf("%s-%d", started, n)
		}
		err := os.Mkdir(filepath.Join(importedFolder, name), 0o755)
		if err == nil {
			return name
		}
		if !errors.Is(err, os.ErrExist) {
			log.Fatal(err)
		}
	}
}

// excluded reports whether path is in a folder the script writes to, which is never searched.
func (im *importer) excluded(path string) bool {
	return within(path, im.dataFolder) || within(path, im.importedFolder) ||
		(im.notImported != "" && within(path, im.notImported))
}

// moveOthers moves the files that aren't photos or videos, and hidden ones, to the not-imported folder.
func (im *importer) moveOthers() {
	for _, f := range im.others {
		why, reason := notImportedOther, "not a photo or video"
		if name := filepath.Base(f.path); strings.HasPrefix(name, ".") {
			why, reason = notImportedHidden, "hidden (Android's .trashed-… is deleted on the phone, .pending-… never finished)"
		}
		im.reject(f.path, why, reason)
	}
}

// learn adds a photo another process placed (another import, a phone's upload) to the photos new ones
// are compared with, so near-duplicates are found across them too.
func (im *importer) learn(item media.Item) {
	if item.Sidecar.Facts.Kind != media.KindPhoto {
		return
	}
	if h := im.dhashOf(context.Background(), item.Sha256(), filepath.Join(im.dataFolder, filepath.FromSlash(item.Path))); h != nil {
		facts := item.Sidecar.Facts
		im.mu.Lock()
		im.photos = append(im.photos, photo{hash: *h, sha256: item.Sha256(), name: facts.FileName, takenAt: facts.TakenAt})
		im.mu.Unlock()
	}
}

// rootOf is the searched folder path was found in.
func (im *importer) rootOf(path string) string {
	for _, root := range im.roots {
		if within(path, root) {
			return root
		}
	}
	return filepath.Dir(path)
}

// done moves a source file whose content is in the data folder to the imported folder.
// (A file downloaded from a cloud is deleted from the temp folder instead, and logged.)
func (im *importer) done(path, sha, result string) {
	if im.dryRun {
		return
	}
	if im.cloud != nil {
		im.cloud.finished(path, sha, result, "")
		return
	}
	if _, err := moveFile(path, filepath.Join(im.runFolder, pathInside(im.roots, path))); err != nil {
		// It is in the data folder; only the move failed, so a later run finds it as a duplicate.
		im.leave(path, "imported, but not moved: "+err.Error())
	}
}

// What happened to a file, as the cloud log says it.
const (
	resultImported         = "imported"
	resultDuplicate        = "duplicate"
	resultDuplicateWaiting = "duplicate of a near-duplicate waiting for review"
	resultWaiting          = "near-duplicate, waiting for review"
	resultNotImported      = "not imported"
)

// reject moves a file that wasn't imported to this run's not-imported folder, in the folder for why
// (dry run: only listed).
func (im *importer) reject(path, why, reason string) {
	movedTo := "(dry run)"
	if !im.dryRun {
		dest, err := moveFile(path, filepath.Join(im.notImportedRun, why, pathInside(im.roots, path)))
		if err != nil {
			im.leave(path, reason+"; not moved to the not-imported folder: "+err.Error())
			return
		}
		movedTo = dest
		if im.cloud != nil {
			im.cloud.record(path, "", resultNotImported, why+": "+reason, dest)
		}
	}
	im.mu.Lock()
	im.rejected = append(im.rejected, strings.Join([]string{why, reason, path, movedTo}, "\t"))
	im.mu.Unlock()
	log.Printf("%s: not imported (%s): %s", path, why, reason)
}

// leave notes a file that stays where it is.
func (im *importer) leave(path, reason string) {
	im.mu.Lock()
	im.leftBehind = append(im.leftBehind, path+"\t"+reason)
	im.mu.Unlock()
	log.Printf("%s: %s", path, reason)
}

// progress counts a file as done and, every 10 s and after the last file, shows how far along the run is.
func (im *importer) progress(size int64) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.doneFiles++
	im.doneBytes += size
	if time.Since(im.lastProgress) < 10*time.Second && im.doneFiles < im.totalFiles {
		return
	}
	percent := 100.0
	if im.totalBytes > 0 {
		percent = float64(im.doneBytes) * 100 / float64(im.totalBytes)
	}
	left := timeLeft(time.Since(im.started), im.doneBytes, im.totalBytes)
	c := im.counts
	waiting, bursts := im.nearCounts()
	log.Printf("%5.1f%%  %s of %s, %d of %d files, about %s left | %d new, %d duplicates, %d bursts, %d near-duplicates for you, %d left behind",
		percent, gb(im.doneBytes), gb(im.totalBytes), im.doneFiles, im.totalFiles, left,
		c.imported, c.duplicates, bursts, waiting, len(im.leftBehind))
	im.lastProgress = time.Now()
}

func (im *importer) nearCounts() (waiting, bursts int) {
	for _, pair := range im.near {
		if pair.burst {
			bursts++
		} else {
			waiting++
		}
	}
	return waiting, bursts
}

func (im *importer) summarize() {
	c := im.counts
	verb := "Imported"
	if im.dryRun {
		verb = "Would import"
	}
	fmt.Printf("\n%s %d files (%s)\n", verb, c.imported, gb(c.importedBytes))
	fmt.Printf("Duplicates (same sha256 as a file in the data folder or found before): %d (%s)\n", c.duplicates, gb(c.duplicateBytes))
	waiting, bursts := im.nearCounts()
	fmt.Printf("Look-alike shots of a burst (dHash within %d bits, taken in a row), imported: %d\n", im.nearDistance, bursts)
	moved := "moved to " + im.notImportedRun
	if im.dryRun {
		moved = "would be moved to the not-imported folder"
	}
	fmt.Printf("Near-duplicates for you to decide (%s, %s): %d\n", moved, notImportedNear, waiting)
	fmt.Printf("Not imported, %s: %d without a date, %d failed\n", moved, c.noDate, c.failed)
	if len(im.leftBehind) > 0 {
		fmt.Printf("Left where they are (they couldn't be moved): %d\n", len(im.leftBehind))
	}
	if len(im.dateNotes) > 0 {
		fmt.Printf("File name and metadata dates more than a day apart (the name was used): %d\n", len(im.dateNotes))
	}
	others := "not looked at (--move-other moves them to the not-imported folder)"
	if im.moveOther {
		others = moved
	}
	if c.hidden > 0 {
		fmt.Printf("Hidden files (.trashed-…, .pending-…), %s: %d\n", others, c.hidden)
	}
	if len(im.notMedia) > 0 {
		exts := make([]string, 0, len(im.notMedia))
		for ext := range im.notMedia {
			exts = append(exts, ext)
		}
		sort.Slice(exts, func(i, j int) bool { return im.notMedia[exts[i]] > im.notMedia[exts[j]] })
		parts := make([]string, 0, len(exts))
		for _, ext := range exts {
			if ext == "" {
				parts = append(parts, fmt.Sprintf("(none) %d", im.notMedia[ext]))
			} else {
				parts = append(parts, fmt.Sprintf("%s %d", ext, im.notMedia[ext]))
			}
		}
		fmt.Printf("Not photos or videos, %s: %s\n", others, strings.Join(parts, ", "))
	}

	sort.Strings(im.leftBehind)
	sort.Strings(im.rejected)
	sort.Strings(im.dateNotes)
	var near []string
	for _, pair := range im.near {
		kind := "waits for you"
		if pair.burst {
			kind = "burst, imported"
		}
		like := pair.like.path
		if item, ok := im.store.Get(pair.like.sha256); ok {
			like = filepath.Join(im.dataFolder, filepath.FromSlash(item.Path))
		}
		near = append(near, fmt.Sprintf("%d\t%s\t%s\t%s", pair.distance, kind, pair.path, like))
	}
	sort.Strings(near)
	for _, report := range []struct {
		folder, name, title, header string
		lines                       []string
	}{
		{im.notImportedRun, "not-imported", "The files not imported, and why", "folder\treason\tfound at\tnow at", im.rejected},
		{im.runFolder, "left-where-they-are", "The files left where they are, and why", "file\treason", im.leftBehind},
		{im.runFolder, "dates", "Files whose name and metadata disagree", "file\tdates", im.dateNotes},
		{im.runFolder, "near-duplicates", "Near-duplicates", "bits apart\tdecision\tphoto\tlooks like", near},
	} {
		if len(report.lines) == 0 {
			continue
		}
		path := filepath.Join(report.folder, report.name+".tsv")
		err := os.MkdirAll(report.folder, 0o755)
		if err == nil {
			err = os.WriteFile(path, []byte(report.header+"\n"+strings.Join(report.lines, "\n")+"\n"), 0o644)
		}
		if err != nil {
			log.Printf("couldn't write %s: %v", path, err)
			continue
		}
		fmt.Printf("%s: %s\n", report.title, path)
	}
}

// facts are what a device would have sent with the upload.
type facts struct {
	kind       media.Kind
	mime       string
	takenAt    *time.Time
	width      *int
	height     *int
	durationMs *int64
	location   *media.GeoPoint
	note       string            // the name's and the metadata's dates disagree
	exif       map[string]string // a photo's fields as vipsheader names them, shown by --review
}

func (im *importer) readFacts(ctx context.Context, path string) facts {
	t := mediaTypes[strings.ToLower(filepath.Ext(path))]
	f := facts{kind: t.kind, mime: t.mime}
	if t.kind == media.KindVideo {
		im.videoFacts(ctx, path, &f)
	} else {
		im.photoFacts(ctx, path, &f)
	}
	return f
}

// photoFacts reads the photo's size, GPS position and EXIF date with vipsheader (a DNG's EXIF from the
// file itself, as vipsheader doesn't show it).
func (im *importer) photoFacts(ctx context.Context, path string, f *facts) {
	fields := map[string]string{}
	if out, err := exec.CommandContext(ctx, im.vipsheader, "-a", path).Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			name, value, ok := strings.Cut(strings.TrimRight(line, "\r"), ": ")
			if !ok {
				continue
			}
			// EXIF fields read "2019:05:12 14:30:22 (2019:05:12 14:30:22, ASCII, 20 components, 20 bytes)".
			raw, _, _ := strings.Cut(value, " (")
			fields[name] = strings.TrimSpace(raw)
		}
	}
	if strings.EqualFold(filepath.Ext(path), ".dng") {
		if exif, err := tiffExif(path); err == nil {
			for name, value := range exif {
				fields[name] = value
			}
		}
	}

	f.exif = fields
	exif := exifTime(fields["exif-ifd2-DateTimeOriginal"], fields["exif-ifd2-OffsetTimeOriginal"])
	if exif == nil {
		exif = exifTime(fields["exif-ifd2-DateTimeDigitized"], fields["exif-ifd2-OffsetTimeDigitized"])
	}
	if exif == nil {
		// Editing programs also change this one.
		exif = exifTime(fields["exif-ifd0-DateTime"], fields["exif-ifd2-OffsetTime"])
	}
	f.takenAt, f.note = nameFirst(timeInName(filepath.Base(path)), exif, 2*time.Minute)

	width, errW := strconv.Atoi(fields["width"])
	height, errH := strconv.Atoi(fields["height"])
	if errW == nil && errH == nil && width > 0 && height > 0 {
		// Upright, as devices send it: a photo turned a quarter (orientation 5–8) is shown the other way round.
		if o, _ := strconv.Atoi(fields["orientation"]); o >= 5 && o <= 8 {
			width, height = height, width
		}
		f.width, f.height = &width, &height
	}

	lat, okLat := gpsDegrees(fields["exif-ifd3-GPSLatitude"], fields["exif-ifd3-GPSLatitudeRef"], "S")
	lon, okLon := gpsDegrees(fields["exif-ifd3-GPSLongitude"], fields["exif-ifd3-GPSLongitudeRef"], "W")
	if okLat && okLon && (lat != 0 || lon != 0) && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 {
		f.location = &media.GeoPoint{Lat: lat, Lon: lon}
	}
}

// nameFirst picks the date: the file name's, or the metadata's when they show the same local time
// (within slack), since the metadata also has the offset. Without a date in the name, the metadata's.
// note says when the two are more than a day apart.
func nameFirst(named, metadata *time.Time, slack time.Duration) (*time.Time, string) {
	switch {
	case named == nil:
		return metadata, ""
	case metadata == nil:
		return named, ""
	}
	gap := wallClock(*metadata).Sub(wallClock(*named)).Abs()
	if gap <= slack {
		return metadata, ""
	}
	if gap > 24*time.Hour {
		return named, fmt.Sprintf("name %s, metadata %s", named.Format("2006-01-02 15:04:05"), metadata.Format(time.RFC3339))
	}
	return named, ""
}

// wallClock is t's local date and time, as if it were UTC, to compare times written in different zones.
func wallClock(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

// videoFacts reads the video's size, length and date with ffprobe. Phones write the date in UTC
// (creation_time) and the local time in the file name, so the two together give the offset; Apple's
// creationdate already has one.
func (im *importer) videoFacts(ctx context.Context, path string, f *facts) {
	var probe struct {
		Streams []struct {
			CodecType    string            `json:"codec_type"`
			Width        int               `json:"width"`
			Height       int               `json:"height"`
			Tags         map[string]string `json:"tags"`
			SideDataList []struct {
				Rotation float64 `json:"rotation"`
			} `json:"side_data_list"`
		} `json:"streams"`
		Format struct {
			Duration string            `json:"duration"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
	}
	out, err := exec.CommandContext(ctx, im.ffprobe, "-v", "error", "-show_entries",
		"format=duration:format_tags:stream=codec_type,width,height:stream_tags=rotate:stream_side_data=rotation",
		"-of", "json", path).Output()
	if err == nil {
		json.Unmarshal(out, &probe)
	}

	for _, s := range probe.Streams {
		if s.CodecType != "video" || s.Width <= 0 || s.Height <= 0 {
			continue
		}
		width, height := s.Width, s.Height
		rotation, _ := strconv.ParseFloat(s.Tags["rotate"], 64)
		for _, side := range s.SideDataList {
			if side.Rotation != 0 {
				rotation = side.Rotation
			}
		}
		if r := int(rotation) % 180; r == 90 || r == -90 {
			width, height = height, width
		}
		f.width, f.height = &width, &height
		break
	}
	var length time.Duration
	if seconds, err := strconv.ParseFloat(probe.Format.Duration, 64); err == nil && seconds > 0 {
		ms := int64(seconds * 1000)
		f.durationMs = &ms
		length = time.Duration(ms) * time.Millisecond
	}
	if iso6709 := probe.Format.Tags["location"]; iso6709 != "" {
		f.location = parseISO6709(iso6709)
	}

	named := timeInName(filepath.Base(path))
	var apple, created *time.Time
	if value := probe.Format.Tags["com.apple.quicktime.creationdate"]; value != "" {
		for _, layout := range []string{"2006-01-02T15:04:05-0700", time.RFC3339} {
			if t, err := time.Parse(layout, value); err == nil && plausible(t) {
				apple = &t
				break
			}
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, probe.Format.Tags["creation_time"]); err == nil && plausible(t) {
		created = &t
	}
	switch {
	case apple != nil:
		f.takenAt, f.note = nameFirst(named, apple, length+2*time.Minute)
	case named != nil && created != nil:
		f.takenAt = withOffsetFromName(*created, *named, f.durationMs)
		if gap := created.Sub(*named).Abs(); gap > 38*time.Hour { // further than any two time zones
			f.note = fmt.Sprintf("name %s, metadata %s", named.Format("2006-01-02 15:04:05"), created.Format(time.RFC3339))
		}
	case named != nil:
		f.takenAt = named
	case created != nil:
		local := created.In(time.Local)
		f.takenAt = &local
	}
}

// withOffsetFromName returns the video's UTC creation time in the time zone it was filmed in: this
// machine's when the local time in its name matches (the name is when filming started, the creation
// time may be when it stopped), else the offset between the two, to the quarter hour. When they are
// too far apart for any time zone, the camera's clock was off and the name is used.
func withOffsetFromName(created, named time.Time, durationMs *int64) *time.Time {
	var length time.Duration
	if durationMs != nil {
		length = time.Duration(*durationMs) * time.Millisecond
	}
	if gap := created.Sub(named); gap >= -2*time.Minute && gap <= length+2*time.Minute {
		local := created.In(time.Local)
		return &local
	}
	wall := time.Date(named.Year(), named.Month(), named.Day(), named.Hour(), named.Minute(), named.Second(), 0, time.UTC)
	offset := wall.Sub(created).Round(15 * time.Minute)
	if offset < -12*time.Hour || offset > 14*time.Hour {
		return &named
	}
	t := created.In(time.FixedZone("", int(offset.Seconds())))
	return &t
}

// exifTime parses an EXIF date ("2019:05:12 14:30:22") with its offset ("+03:00"), or in this machine's
// time zone without one.
func exifTime(value, offset string) *time.Time {
	if value == "" || strings.HasPrefix(value, "0000") {
		return nil
	}
	location := time.Local
	if offset != "" {
		if o, err := time.Parse("-07:00", offset); err == nil {
			location = o.Location()
		}
	}
	t, err := time.ParseInLocation("2006:01:02 15:04:05", value, location)
	if err != nil || !plausible(t) {
		return nil
	}
	return &t
}

// Dates in file names: 20190512_143022, IMG_20190512_143022123, VID-20190512-WA0001 (no time),
// 2019-05-12 14.30.22.
var (
	dateTimeInName = regexp.MustCompile(`(?:^|\D)((?:19|20)\d\d)[-_.]?(\d\d)[-_.]?(\d\d)[ _\-T.]?(\d\d)[-_.:]?(\d\d)[-_.:]?(\d\d)`)
	dateInName     = regexp.MustCompile(`(?:^|\D)((?:19|20)\d\d)-?(\d\d)-?(\d\d)(?:\D|$)`)
)

// timeInName reads a date in the file name, in this machine's time zone. A date without a time is taken
// as noon, so it stays on its day.
func timeInName(name string) *time.Time {
	if m := dateTimeInName.FindStringSubmatch(name); m != nil {
		t, err := time.ParseInLocation("2006 01 02 15 04 05", strings.Join(m[1:], " "), time.Local)
		if err == nil && plausible(t) {
			return &t
		}
	}
	if m := dateInName.FindStringSubmatch(name); m != nil {
		t, err := time.ParseInLocation("2006 01 02 15", strings.Join(m[1:], " ")+" 12", time.Local)
		if err == nil && plausible(t) {
			return &t
		}
	}
	return nil
}

// plausible rules out empty dates (QuickTime's 1904, Unix's 1970) and dates in the future.
func plausible(t time.Time) bool {
	return t.Year() >= 1990 && t.Before(time.Now().Add(48*time.Hour))
}

// gpsDegrees parses EXIF's degrees, minutes and seconds ("32/1 50/1 49947000/1000000"), negative when
// ref is negativeRef ("S" or "W").
func gpsDegrees(value, ref, negativeRef string) (float64, bool) {
	parts := strings.Fields(value)
	if len(parts) != 3 {
		return 0, false
	}
	var degrees float64
	for i, part := range parts {
		num, den, ok := strings.Cut(part, "/")
		n, errN := strconv.ParseFloat(num, 64)
		d, errD := strconv.ParseFloat(den, 64)
		if !ok || errN != nil || errD != nil || d == 0 {
			return 0, false
		}
		degrees += n / d / []float64{1, 60, 3600}[i]
	}
	if strings.EqualFold(ref, negativeRef) {
		degrees = -degrees
	}
	return degrees, true
}

// iso6709Point matches a video's location tag, "+32.0853+034.7818/" or "+32.0853+034.7818+012.000/".
var iso6709Point = regexp.MustCompile(`^([+-]\d+(?:\.\d+)?)([+-]\d+(?:\.\d+)?)`)

func parseISO6709(value string) *media.GeoPoint {
	m := iso6709Point.FindStringSubmatch(value)
	if m == nil {
		return nil
	}
	lat, errLat := strconv.ParseFloat(m[1], 64)
	lon, errLon := strconv.ParseFloat(m[2], 64)
	if errLat != nil || errLon != nil || (lat == 0 && lon == 0) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return nil
	}
	return &media.GeoPoint{Lat: lat, Lon: lon}
}

// sourceOfPath is where the file came from, by its folder, as the Android app names it
// (sourceOfFolder in journal-client-kmp).
func sourceOfPath(path string) string {
	p := strings.ToLower(filepath.ToSlash(path))
	switch {
	case strings.Contains(p, "screenshot") || strings.Contains(p, "screenrecord") || strings.Contains(p, "screen record"):
		return "screenshots"
	case strings.Contains(p, "whatsapp"):
		return "whatsapp"
	case strings.Contains(p, "dcim"):
		return "camera"
	default:
		return "other"
	}
}

// pathInside is where a file goes inside the imported folder: its path from the searched folder it was
// found in, under that folder's name (D:\Archive\phone DCIM\Camera\x.jpg → phone DCIM\Camera\x.jpg; a
// whole drive searched, E:\, gives E\…).
func pathInside(roots []string, path string) string {
	for _, root := range roots {
		if !within(path, root) {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			break
		}
		name := filepath.Base(root)
		if name == "." || strings.ContainsAny(name, `\/:`) {
			name = strings.Trim(filepath.VolumeName(root), `:\/`)
		}
		return filepath.Join(name, rel)
	}
	return filepath.Base(path)
}

// moveFile moves from to to, or to "name (2).ext", … when to is taken. When it can't be renamed there
// (another disk), it is copied, then deleted.
func moveFile(from, to string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return "", err
	}
	stem, ext := strings.TrimSuffix(to, filepath.Ext(to)), filepath.Ext(to)
	for n := 2; ; n++ {
		if _, err := os.Lstat(to); errors.Is(err, os.ErrNotExist) {
			break
		}
		to = fmt.Sprintf("%s (%d)%s", stem, n, ext)
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		// On Windows a virus scanner or the search indexer briefly holding the file makes a rename fail.
		if err = os.Rename(from, to); err == nil {
			return to, nil
		}
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
	if copyErr := copyFile(from, to); copyErr != nil {
		os.Remove(to)
		return "", fmt.Errorf("%v; copying: %v", err, copyErr)
	}
	return to, os.Remove(from)
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, bufio.NewReaderSize(f, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// within reports whether path is dir or inside it, ignoring case (the folders must also work on Windows and macOS).
func within(path, dir string) bool {
	rel, err := filepath.Rel(strings.ToLower(dir), strings.ToLower(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func absolute(path string) string {
	abs, err := filepath.Abs(path)
	check(err)
	return abs
}

// timeLeft estimates the time left from the speed so far: "?" for the first 10 seconds.
func timeLeft(elapsed time.Duration, done, total int64) string {
	if done <= 0 || elapsed < 10*time.Second {
		return "?"
	}
	rest := time.Duration(float64(elapsed) * float64(total-done) / float64(done))
	return rest.Round(time.Minute).String()
}

func gb(bytes int64) string { return fmt.Sprintf("%.1f GB", float64(bytes)/(1<<30)) }

func loadConfig(path string) (auth.AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return auth.AppConfig{}, fmt.Errorf("%w (run from the server folder, or pass --config)", err)
	}
	var config auth.AppConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return auth.AppConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	return config.WithDefaults(), nil
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// backfillFolders adds to the sidecars the folders of files looked at before folders were kept (the
// duplicates moved away by earlier runs), from the sha256s in .cache/source-hashes.tsv of the files
// found under roots. Only sidecars change.
func (im *importer) backfillFolders() {
	var found, added, notInDataFolder int
	im.sources.each(func(path, sha string) {
		root := ""
		for _, r := range im.roots {
			if within(path, r) {
				root = r
			}
		}
		if root == "" {
			return
		}
		found++
		folder := folderOf(root, path)
		if _, ok := im.store.Get(sha); !ok || folder == "" {
			notInDataFolder++
			return
		}
		changed, err := im.store.AddFolders(sha, []string{folder})
		if err != nil {
			log.Printf("%s: %v", path, err)
			return
		}
		if changed {
			added++
		}
	})
	fmt.Printf("%d files looked at before under these folders; %d folders added to sidecars, %d files not in the data folder (not imported yet, or waiting for review)\n",
		found, added, notInDataFolder)
}
