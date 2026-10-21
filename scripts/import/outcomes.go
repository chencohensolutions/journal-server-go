package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// What becomes of each file (moved to the imported or not-imported folder, or left), progress and the reports.

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
	if _, err := moveFile(path, im.movedTo(im.roots, path)); err != nil {
		// It is in the data folder; only the move failed, so a later run finds it as a duplicate.
		im.leave(path, "imported, but not moved: "+err.Error())
	}
}

// movedTo is where a file found at foundAt, under one of roots, goes in this run's imported folder.
func (im *importer) movedTo(roots []string, foundAt string) string {
	return filepath.Join(im.runFolder, pathInside(roots, foundAt))
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
