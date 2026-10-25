package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/journal/server-go/media"
)

// What becomes of each file (moved to the imported or not-imported folder, or left), progress and the reports.

// done moves a source file whose content is in the data folder to the imported folder.
// (A file downloaded from a cloud is deleted from the temp folder instead, and logged.) With --delete-duplicates, a
// duplicate whose bytes the data folder has is deleted instead (deleteDuplicate).
func (im *importer) done(path, sha, result string) {
	if im.cloud != nil {
		if !im.dryRun {
			im.cloud.finished(path, sha, result, "")
		}
		return
	}
	if im.deleteDups && result == resultDuplicate && im.deleteDuplicate(path, sha) {
		return
	}
	if im.dryRun {
		return
	}
	if _, err := im.move(path, filepath.Join(im.runFolder, pathInside(im.roots, path))); err != nil {
		// It is in the data folder; only the move failed, so a later run finds it as a duplicate.
		im.leave(path, "imported, but not moved: "+err.Error())
	}
}

// deleteDuplicate deletes the source file at path when the data folder's original of sha has exactly its bytes:
// both are hashed again first (the data folder's once per run), so a file changed since it was looked at, or an
// original damaged in the data folder, is never the reason a copy is lost. A WAV found to be kept as FLAC isn't the
// same bytes and is moved as before. Nothing in the data folder is touched. False when it wasn't deleted.
func (im *importer) deleteDuplicate(path, sha string) bool {
	item, ok := im.store.Get(sha)
	if !ok || within(path, im.dataFolder) {
		return false
	}
	original := filepath.Join(im.dataFolder, filepath.FromSlash(item.Path))
	im.mu.Lock()
	same, checked := im.verified[sha]
	im.mu.Unlock()
	if !checked {
		sum, err := media.HashFile(original)
		same = err == nil && sum == sha
		im.mu.Lock()
		im.verified[sha] = same
		im.mu.Unlock()
		if !same {
			log.Printf("%s: THE ORIGINAL IN THE DATA FOLDER DOESN'T HAVE ITS SHA256 (%v); %s is kept", original, err, path)
		}
	}
	if !same {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if sum, err := media.HashFile(path); err != nil || sum != sha {
		im.leave(path, "changed since it was looked at, so not deleted")
		return true
	}
	how := "would be deleted (dry run)"
	if !im.dryRun {
		if err := os.Remove(path); err != nil {
			im.leave(path, "a duplicate, but not deleted: "+err.Error())
			return true
		}
		how = "deleted"
	}
	im.mu.Lock()
	im.counts.deleted++
	im.counts.deletedBytes += info.Size()
	im.deleted = append(im.deleted, strings.Join([]string{path, mb(info.Size()), original, sha, how}, "\t"))
	im.mu.Unlock()
	log.Printf("%s: %s, a duplicate of %s in the data folder (%s)", path, how, original, mb(info.Size()))
	return true
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
	im.rejectAs(path, pathInside(im.roots, path), why, reason)
}

// rejectAs is reject with the file's path inside the folder for why; false when it couldn't be moved.
func (im *importer) rejectAs(path, inside, why, reason string) bool {
	movedTo := "(dry run)"
	if !im.dryRun {
		dest, err := im.move(path, filepath.Join(im.notImportedRun, why, inside))
		if err != nil {
			im.leave(path, reason+"; not moved to the not-imported folder: "+err.Error())
			return false
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
	return true
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
	others, same, waiting := im.nearCounts()
	log.Printf("%5.1f%%  %s of %s, %d of %d files, about %s left | %d new, %d duplicates, %d other shots, %d same pictures, %d near-duplicates for you, %d left behind",
		percent, gb(im.doneBytes), gb(im.totalBytes), im.doneFiles, im.totalFiles, left,
		c.imported, c.duplicates, others, same, waiting, len(im.leftBehind))
	im.lastProgress = time.Now()
}

// nearCounts counts the look-alikes by what became of them: other shots (imported), the same picture
// (not imported), and those waiting for the owner.
func (im *importer) nearCounts() (others, same, waiting int) {
	for _, pair := range im.near {
		switch {
		case pair.waits():
			waiting++
		case pair.verdict == verdictSame:
			same++
		default:
			others++
		}
	}
	return others, same, waiting
}

func (im *importer) summarize() {
	c := im.counts
	verb := "Imported"
	if im.dryRun {
		verb = "Would import"
	}
	fmt.Printf("\n%s %d files (%s)\n", verb, c.imported, gb(c.importedBytes))
	if a := c.audio; len(a) > 0 {
		fmt.Printf("Of them sound recordings, hidden on devices until shown: %d recordings, %d calls (%d with no one named), %d voice messages\n",
			a[media.AudioRecordings], a[media.AudioCalls], a[callsWithoutName], a[media.AudioVoiceMessages])
	}
	if c.encoded > 0 {
		encoded := "WAVs kept as FLAC"
		if im.dryRun {
			encoded = "WAVs that would be kept as FLAC"
		}
		fmt.Printf("%s: %d\n", encoded, c.encoded)
	}
	fmt.Printf("Duplicates (same sha256 as a file in the data folder or found before): %d (%s)\n", c.duplicates, gb(c.duplicateBytes))
	if im.deleteDups {
		verb := "deleted"
		if im.dryRun {
			verb = "would be deleted"
		}
		fmt.Printf("Of them %s from where they were found (the data folder has the same bytes): %d (%s)\n", verb, c.deleted, gb(c.deletedBytes))
	}
	shots, same, waiting := im.nearCounts()
	fmt.Printf("Look-alikes (dHash within %d bits) found to be other shots, imported: %d\n", im.nearDistance, shots)
	moved := "moved to " + im.notImportedRun
	if im.dryRun {
		moved = "would be moved to the not-imported folder"
	}
	fmt.Printf("The same picture as one in the journal, not imported (%s, %s): %d\n", moved, notImportedSame, same)
	fmt.Printf("Near-duplicates for you to decide, larger copies or not sure (%s, %s): %d\n", moved, notImportedNear, waiting)
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
	sort.Strings(im.deleted)
	sort.Strings(im.rejected)
	sort.Strings(im.dateNotes)
	var near []string
	for _, pair := range im.near {
		// By the pixels' score, so the pairs around the thresholds sit together.
		score := "-"
		if pair.score > 0 {
			score = fmt.Sprintf("%04.1f", pair.score)
		}
		near = append(near, fmt.Sprintf("%s\t%d\t%s\t%s\t%s\t%s", score, pair.distance, pair.decision(), pair.why, pair.path, im.pathOf(pair.like)))
	}
	sort.Strings(near)
	for _, report := range []struct {
		folder, name, title, header string
		lines                       []string
	}{
		{im.notImportedRun, "not-imported", "The files not imported, and why", "folder\treason\tfound at\tnow at", im.rejected},
		{im.runFolder, "left-where-they-are", "The files left where they are, and why", "file\treason", im.leftBehind},
		{im.runFolder, "deleted-duplicates", "Duplicates deleted (the data folder has them)", "file\tsize\tthe same file in the data folder\tsha256\twhat happened", im.deleted},
		{im.runFolder, "dates", "Files whose name and metadata disagree", "file\tdates", im.dateNotes},
		{im.runFolder, "near-duplicates", "Near-duplicates", "pixels' score (dB)\tdHash bits apart\tdecision\twhy\tphoto\tlooks like", near},
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
