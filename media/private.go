package media

import (
	"fmt"
	"sort"
)

// SetPrivate marks files private, or no longer private, in their sidecars, together with every file that goes with
// them: edits of a photo and the photo they edit, a Live Photo's video, a RAW and its JPEG (and so on, through
// versionOf and pairedWith). A private photo's edit would show the same picture, so they are marked as one. Only
// sidecars are rewritten; originals are never touched. Each changed file comes again in the change feed (SetAt is
// now, taken during the turn). Files already as asked are left as they are. It returns the files it changed.
func (s *Store) SetPrivate(hashes []string, private bool, by string) (changed []Item, err error) {
	err = s.Turn(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, sha := range hashes {
			if _, ok := s.items[sha]; !ok {
				return fmt.Errorf("%s isn't on the server", sha)
			}
		}
		now := s.now().UTC()
		for _, item := range s.groupOf(hashes) {
			if item.Sidecar.IsPrivate() == private {
				continue
			}
			sidecar := item.Sidecar
			sidecar.Privacy = &Privacy{Private: private, SetAt: now, SetBy: by}
			if err := s.rewriteSidecar(item, sidecar); err != nil {
				return err
			}
			changed = append(changed, *item)
		}
		return nil
	})
	return changed, err
}

// groupOf is the files in hashes and every file linked to them through versionOf or pairedWith, either way, sorted
// by sha256. Callers hold s.mu.
func (s *Store) groupOf(hashes []string) []*Item {
	links := map[string][]string{}
	for sha, item := range s.items {
		for _, to := range []*string{item.Sidecar.Facts.VersionOf, item.Sidecar.Facts.PairedWith} {
			if to != nil {
				links[sha] = append(links[sha], *to)
				links[*to] = append(links[*to], sha)
			}
		}
	}
	seen := map[string]bool{}
	queue := append([]string(nil), hashes...)
	var group []*Item
	for len(queue) > 0 {
		sha := queue[0]
		queue = queue[1:]
		if seen[sha] {
			continue
		}
		seen[sha] = true
		if item, ok := s.items[sha]; ok {
			group = append(group, item)
		}
		queue = append(queue, links[sha]...)
	}
	sort.Slice(group, func(i, j int) bool { return group[i].Sidecar.Sha256 < group[j].Sidecar.Sha256 })
	return group
}

// inheritedPrivacy is the mark a new file takes from the file it is an edit of or paired with: a later edit of a
// private photo is private too. Callers hold s.mu.
func (s *Store) inheritedPrivacy(req UploadRequest) *Privacy {
	for _, to := range []*string{req.VersionOf, req.PairedWith} {
		if to == nil {
			continue
		}
		if item, ok := s.items[*to]; ok && item.Sidecar.IsPrivate() {
			return &Privacy{Private: true, SetAt: s.now().UTC(), SetBy: req.UploadedBy}
		}
	}
	return nil
}
