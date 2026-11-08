package importcmd

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"sort"
	"sync"

	"github.com/journal/server-go/media"
)

// Near-duplicates: photos that look alike by their dHash, found across the data folder and this run, then
// told apart by the same-picture check (same.go).

// hashDataFolder makes the dHash of the photos in the data folder that don't have one in the table yet
// (the first import, or photos uploaded since), so new files are compared with them.
func (im *importer) hashDataFolder(ctx context.Context, jobs int) {
	var all, missing []media.Item
	for cursor, more := "", true; more; {
		var page []media.Item
		var remaining int
		var err error
		page, cursor, remaining, err = im.store.Changes(cursor, 5000, media.View{})
		more = remaining > 0
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
			im.photos = append(im.photos, photo{hash: *h, sha256: item.Sha256()})
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

// alike returns the photos within nearDistance bits of h, closest first, at most compared of them.
// Called with im.mu held.
func (im *importer) alike(h dhash) []photo {
	var found []photo
	for _, q := range im.photos {
		if h.distance(q.hash) <= im.nearDistance {
			found = append(found, q)
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return h.distance(found[i].hash) < h.distance(found[j].hash) })
	return found[:min(len(found), compared)]
}

// prepare makes the pictures of a new photo's look-alikes while files are still read several at a time,
// so deciding, one file at a time, mostly finds them made.
func (im *importer) prepare(ctx context.Context, h dhash) {
	im.mu.Lock()
	alike := im.alike(h)
	im.mu.Unlock()
	for _, q := range alike {
		im.pictureOf(ctx, q.sha256, im.pathOf(q), nil)
	}
}

// pathOf is where a photo compared with is now: in the data folder, or where it was found.
func (im *importer) pathOf(q photo) string {
	if item, ok := im.store.Get(q.sha256); ok {
		return filepath.Join(im.dataFolder, filepath.FromSlash(item.Path))
	}
	return q.path
}

// nearCheck compares a new photo with its look-alikes in the data folder and among those going in this
// run, closest first, and reports the one closest to being the same picture (found: false when nothing
// looks alike). A photo that will be imported (nothing alike, or only other shots) is added at once to
// the photos later ones are compared with, so two copies worked on at the same time still meet.
func (im *importer) nearCheck(ctx context.Context, p photo, pic *picture) (pair nearPair, found bool) {
	im.mu.Lock()
	alike := im.alike(p.hash)
	im.mu.Unlock()
	for i, q := range alike {
		v, score, why := comparePictures(pic, im.pictureOf(ctx, q.sha256, im.pathOf(q), nil))
		if i == 0 || v > pair.verdict {
			pair = nearPair{distance: p.hash.distance(q.hash), verdict: v, score: score, why: why, path: p.path, like: q}
		}
		if v == verdictSame {
			break
		}
	}
	if ctx.Err() != nil {
		return nearPair{}, false // stopped while comparing: decided by the next run
	}
	if pair.verdict == verdictSame {
		like := im.pictureOf(ctx, pair.like.sha256, im.pathOf(pair.like), nil)
		pair.better = like.pixels() > 0 && pic.pixels() > like.pixels()
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	if len(alike) > 0 {
		im.near = append(im.near, pair)
	}
	if len(alike) == 0 || pair.verdict == verdictDifferent {
		im.photos = append(im.photos, p)
	}
	return pair, len(alike) > 0
}

// waits reports whether the pair waits for the owner: a larger copy of the same picture, or not sure.
func (pair nearPair) waits() bool {
	return pair.verdict == verdictUnsure || (pair.verdict == verdictSame && pair.better)
}

// decision is what became of the pair's new photo, as the report and review page say it.
func (pair nearPair) decision() string {
	switch {
	case pair.verdict == verdictDifferent:
		return "another shot, imported"
	case pair.verdict == verdictSame && pair.better:
		return "a larger copy of the same picture, waits for you"
	case pair.verdict == verdictSame:
		return "the same picture, not imported"
	}
	return "not sure, waits for you"
}

// reviewItemOf is a near-duplicate that waits for the owner, found at path.
func (im *importer) reviewItemOf(path, sha string, pair nearPair) reviewItem {
	return reviewItem{
		Path: path, FoundAt: path, Sha256: sha, Root: im.rootOf(path),
		LikeSha256: pair.like.sha256, LikePath: pair.like.path, Distance: pair.distance,
		Why: pair.reason(), Score: pair.score,
	}
}

// reason is why the pair waits, or what was found, in a few words with the pixels' score.
func (pair nearPair) reason() string {
	if pair.score > 0 {
		return fmt.Sprintf("%s (%.1f dB)", pair.why, pair.score)
	}
	return pair.why
}

// waitForReview moves a near-duplicate to the not-imported folder and lists it for --review.
func (im *importer) waitForReview(item reviewItem) {
	if im.dryRun {
		return
	}
	dest, err := im.move(item.Path, filepath.Join(im.notImportedRun, notImportedNear, pathInside(im.roots, item.Path)))
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

// skipSame moves a photo found to be the same picture as one already in the journal (or going in), and
// no larger, to the not-imported folder (at inside, its path there), and adds the folders it was found
// in to that one's sidecar: they are a clue about the photo. False when it couldn't be moved.
func (im *importer) skipSame(path, inside string, folders []string, pair nearPair) bool {
	if !im.dryRun && len(folders) > 0 {
		if _, ok := im.store.Get(pair.like.sha256); ok {
			if _, err := im.store.AddFolders(pair.like.sha256, folders); err != nil {
				log.Printf("%s: its folders couldn't be added to the same picture's: %v", path, err)
			}
		}
	}
	if !im.rejectAs(path, inside, notImportedSame, "the same picture as "+im.pathOf(pair.like)+": "+pair.reason()) {
		return false
	}
	im.mu.Lock()
	im.counts.same++
	im.mu.Unlock()
	return true
}

// Burst shots are numbered within their second: 20260120_081253_035.jpg, PIC_20251202_192632357.jpg.
var burstNumber = regexp.MustCompile(`((?:19|20)\d{6})[-_]?(\d{6})[-_]?(\d{3})(?:\D|$)`)

// learn adds a photo another process placed (another import, a phone's upload) to the photos new ones
// are compared with, so near-duplicates are found across them too.
func (im *importer) learn(item media.Item) {
	if item.Sidecar.Facts.Kind != media.KindPhoto {
		return
	}
	if h := im.dhashOf(context.Background(), item.Sha256(), filepath.Join(im.dataFolder, filepath.FromSlash(item.Path))); h != nil {
		im.mu.Lock()
		im.photos = append(im.photos, photo{hash: *h, sha256: item.Sha256()})
		im.mu.Unlock()
	}
}
