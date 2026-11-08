package importcmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// --recheck: deciding again on the near-duplicates waiting for review, with the same-picture check. Those
// held back before the check was there (when looking alike was enough) are mostly other shots.

// rechecked is a photo waiting for review, read again.
type rechecked struct {
	item    reviewItem
	size    int64
	gone    bool // the file isn't there any more
	facts   facts
	hash    *dhash
	picture *picture
}

// recheck goes through review.json largest first, as an import does: a photo that is another shot (or
// looks like nothing any more) is imported and moved to the imported folder, one that is the same picture
// as one in the journal, and no larger, is moved to the not-imported folder's "same picture", and the
// rest keep waiting, with what the check found. Ctrl+C stops it; running again continues.
func (im *importer) recheck(ctx context.Context, jobs int) {
	items, err := im.loadReview()
	check(err)
	if len(items) == 0 {
		fmt.Println("No near-duplicates wait for a decision.")
		return
	}
	im.hashDataFolder(ctx, jobs)
	if im.dryRun {
		log.Print("Dry run: nothing is copied or moved")
	}

	found := make([]*rechecked, len(items))
	for i, item := range items {
		found[i] = &rechecked{item: item}
		if info, err := os.Stat(item.Path); err == nil {
			found[i].size = info.Size()
			im.totalBytes += info.Size()
		} else {
			found[i].gone = errors.Is(err, os.ErrNotExist)
		}
	}
	im.totalFiles = len(items)
	im.started = time.Now()
	log.Printf("%d near-duplicates wait for review (%s): reading them", len(items), gb(im.totalBytes))
	queue := make(chan *rechecked)
	var workers sync.WaitGroup
	for range jobs {
		workers.Go(func() {
			for r := range queue {
				if !r.gone {
					im.readAgain(ctx, r)
				}
				im.progress(r.size)
			}
		})
	}
	for _, r := range found {
		if ctx.Err() != nil {
			break
		}
		queue <- r
	}
	close(queue)
	workers.Wait()
	if ctx.Err() != nil {
		log.Fatal("Stopped; running again continues where it stopped")
	}

	sort.SliceStable(found, func(i, j int) bool { return found[i].size > found[j].size })
	decided := map[string]bool{}       // by sha256: off the list
	updated := map[string]reviewItem{} // by sha256: still waiting, with what the check found
	for i, r := range found {
		if ctx.Err() != nil {
			log.Print("Stopped; running again continues where it stopped")
			break
		}
		im.store.CatchUp()
		switch keep, item := im.decideAgain(ctx, r); {
		case ctx.Err() != nil:
		case keep:
			updated[item.Sha256] = item
		default:
			decided[item.Sha256] = true
		}
		if (i+1)%1000 == 0 {
			log.Printf("Deciding: %d of %d", i+1, len(found))
		}
	}

	if !im.dryRun && (len(decided) > 0 || len(updated) > 0) {
		// Taken off the list as it is on disk now: imports and the review page may have changed it.
		err := im.store.Turn(func() error {
			items, err := im.loadReview()
			if err != nil {
				return err
			}
			kept := items[:0]
			for _, item := range items {
				if decided[item.Sha256] {
					continue
				}
				if u, ok := updated[item.Sha256]; ok {
					item = u
				}
				kept = append(kept, item)
			}
			return im.saveReview(kept)
		})
		if err != nil {
			log.Printf("%s: %v", im.reviewFile(), err)
		}
	}
	im.summarize()
	waiting := len(items) - len(decided)
	if im.dryRun {
		fmt.Printf("%d near-duplicates would still wait for you\n", waiting)
	} else if waiting > 0 {
		fmt.Printf("%d near-duplicates still wait for you. To see them side by side and decide:\n  journal-server import --user %s --imported \"%s\" --review\n", waiting, im.user, im.importedFolder)
	}
}

// readAgain reads a waiting photo's facts, dHash and picture (several at a time).
func (im *importer) readAgain(ctx context.Context, r *rechecked) {
	r.facts = im.readFacts(ctx, r.item.Path)
	r.hash = im.dhashOf(ctx, r.item.Sha256, r.item.Path)
	if r.hash != nil {
		r.picture = im.pictureOf(ctx, r.item.Sha256, r.item.Path, &r.facts)
		im.prepare(ctx, *r.hash)
	}
}

// decideAgain decides on one waiting photo; keep: it still waits, as item says.
func (im *importer) decideAgain(ctx context.Context, r *rechecked) (keep bool, item reviewItem) {
	item = r.item
	inside := pathInside([]string{item.Root}, item.foundAt())
	if r.gone {
		log.Printf("%s: isn't there any more; off the list", item.Path)
		return false, item
	}
	// Imported meanwhile (from the review page, or another copy): a duplicate now.
	if _, ok := im.store.Get(item.Sha256); ok {
		if im.dryRun {
			im.count(true, r.size)
			return false, item
		}
		if folders := item.folders(); len(folders) > 0 {
			im.store.AddFolders(item.Sha256, folders)
		}
		if _, err := im.move(item.Path, filepath.Join(im.runFolder, inside)); err != nil {
			im.leave(item.Path, "already in the journal, but not moved to the imported folder: "+err.Error())
			return true, item
		}
		im.count(true, r.size)
		return false, item
	}
	if r.hash == nil || r.facts.takenAt == nil {
		return true, item // can't be compared, or has no date: stays for the owner
	}
	pair, found := im.nearCheck(ctx, photo{hash: *r.hash, sha256: item.Sha256, path: item.Path}, r.picture)
	if ctx.Err() != nil {
		return true, item
	}
	switch {
	case found && pair.waits():
		item.LikeSha256, item.LikePath, item.Distance = pair.like.sha256, pair.like.path, pair.distance
		item.Why, item.Score = pair.reason(), pair.score
		return true, item
	case found && pair.verdict == verdictSame:
		return !im.skipSame(item.Path, inside, item.folders(), pair), item
	}
	if im.dryRun {
		im.count(false, r.size)
		return false, item
	}
	if _, _, err := im.importInto(ctx, item.Path, item.foundAt(), item.Sha256, r.size, r.facts, item.folders()); err != nil {
		im.leave(item.Path, "not imported: "+err.Error())
		im.mu.Lock()
		im.counts.failed++
		im.mu.Unlock()
		return true, item
	}
	im.count(false, r.size)
	if _, err := im.move(item.Path, filepath.Join(im.runFolder, inside)); err != nil {
		// It is in the journal; the next recheck finds it there and moves it.
		im.leave(item.Path, "imported, but not moved to the imported folder: "+err.Error())
	}
	return false, item
}
