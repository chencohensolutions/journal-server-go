package main

import (
	"context"
	"log"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/journal/server-go/media"
)

// Near-duplicates: photos that look alike by their dHash, found across the data folder and this run.

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
