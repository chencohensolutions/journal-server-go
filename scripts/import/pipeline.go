package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/journal/server-go/media"
)

// The run itself: finding the files, reading each one, and deciding what to do with the new ones.

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
		im.decide(ctx, e)
		doneBytes += e.size
		if time.Since(last) >= 10*time.Second || i == len(news)-1 {
			log.Printf("Importing: %5.1f%%  %s of %s, %d of %d files, about %s left",
				float64(doneBytes)*100/float64(max(newBytes, 1)), gb(doneBytes), gb(newBytes), i+1, len(news),
				timeLeft(time.Since(start), doneBytes, newBytes))
			last = time.Now()
		}
	}
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

// rootOf is the searched folder path was found in.
func (im *importer) rootOf(path string) string {
	for _, root := range im.roots {
		if within(path, root) {
			return root
		}
	}
	return filepath.Dir(path)
}

// Folders inside a run's not-imported folder, by why the files in them weren't imported.
const (
	notImportedNoDate = "no date"
	notImportedFailed = "failed"
	notImportedNear   = "near-duplicates" // waiting for --review
	notImportedSame   = "same picture"
	notImportedOther  = "not photos or videos"
	notImportedHidden = "hidden"
)

// examined is a file read and found new to the data folder, waiting for decide.
type examined struct {
	path   string
	size   int64
	sha256 string
	folder string
	facts   facts
	hash    *dhash
	picture *picture // for the same-picture check
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
	if isWAV(path) {
		if item, ok := im.store.EncodedFrom(sha); ok { // kept as FLAC by an earlier run
			if im.addFolder(path, item.Sha256(), folder) {
				im.count(true, info.Size())
				im.done(path, sha, resultDuplicate)
			}
			return nil
		}
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
	if facts.failed != "" {
		im.reject(path, notImportedFailed, facts.failed)
		im.mu.Lock()
		im.counts.failed++
		im.mu.Unlock()
		return nil
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
		if e.hash != nil {
			e.picture = im.pictureOf(ctx, sha, path, &facts)
			im.prepare(ctx, *e.hash)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return e
}

// decide imports one examined file, skips it as the same picture as one in the journal, or keeps it for
// review; one file at a time, the largest first, so the original of a photo goes in before its smaller
// copies.
func (im *importer) decide(ctx context.Context, e *examined) {
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
		pair, found := im.nearCheck(ctx, photo{hash: *e.hash, sha256: e.sha256, path: e.path}, e.picture)
		if ctx.Err() != nil {
			return
		}
		if found && pair.waits() {
			im.waiting[e.sha256] = len(im.toReview)
			im.waitForReview(im.reviewItemOf(e.path, e.sha256, pair))
			return
		}
		if found && pair.verdict == verdictSame {
			im.skipSame(e.path, pathInside(im.roots, e.path), folders, pair)
			return
		}
	}
	if im.dryRun {
		im.seen[e.sha256] = true
		im.count(false, e.size)
		im.countAudio(e)
		return
	}
	_, existed, err := im.importInto(ctx, e.path, e.path, e.sha256, e.size, e.facts, folders)
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
	if !existed {
		im.countAudio(e)
	}
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

// importInto copies the file at path into the data folder with the facts read from it; foundAt is where
// it was found (a near-duplicate has moved since), which its source is told from. A WAV is encoded as FLAC
// first (in encodingDir, on the not-imported folder's disk), and the FLAC is copied.
func (im *importer) importInto(ctx context.Context, path, foundAt, sha string, size int64, facts facts, folders []string) (media.Item, bool, error) {
	source := media.SourceOfPath(foundAt)
	from := foundAt
	if im.cloud != nil {
		from = im.cloud.origin(foundAt)
	}
	name := filepath.Base(path)
	var encodedFrom *media.EncodedFrom
	if isWAV(path) {
		if err := os.MkdirAll(im.encodingDir(), 0o755); err != nil {
			return media.Item{}, false, err
		}
		work, err := os.MkdirTemp(im.encodingDir(), "flac-")
		if err != nil {
			return media.Item{}, false, err
		}
		defer os.RemoveAll(work)
		flac, err := encodeFLAC(ctx, im.ffmpeg, work, path)
		if err != nil {
			return media.Item{}, false, err
		}
		info, err := os.Stat(flac)
		if err != nil {
			return media.Item{}, false, err
		}
		flacSha, err := media.HashFile(flac)
		if err != nil {
			return media.Item{}, false, err
		}
		encodedFrom = &media.EncodedFrom{Sha256: sha, MimeType: facts.mime, Size: size}
		path, sha, size, name, facts.mime = flac, flacSha, info.Size(), filepath.Base(flac), "audio/flac"
		im.mu.Lock()
		im.counts.encoded++
		im.mu.Unlock()
	}
	return im.store.Import(path, media.UploadRequest{
		Sha256:       sha,
		Size:         size,
		FileName:     name,
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
		Call:         facts.call,
		EncodedFrom:  encodedFrom,
	})
}

// encodingDir is where WAVs are encoded as FLAC, inside the run's not-imported folder; removed at the end.
func (im *importer) encodingDir() string { return filepath.Join(im.notImportedRun, ".encoding") }

// countAudio counts a sound recording imported (or that would be), by its collection.
func (im *importer) countAudio(e *examined) {
	source := media.SourceOfPath(e.path)
	c := media.AudioCollectionOf(media.Facts{Kind: e.facts.kind, Call: e.facts.call, Source: &source})
	if c == "" {
		return
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	im.counts.audio[c]++
	if c == media.AudioCalls && e.facts.call.Contact == nil && e.facts.call.Number == nil {
		im.counts.audio[callsWithoutName]++
	}
	if isWAV(e.path) && im.dryRun {
		im.counts.encoded++
	}
}

// callsWithoutName counts, in counts.audio, the calls whose name says no one (Memento's).
const callsWithoutName = "calls without a name"

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
