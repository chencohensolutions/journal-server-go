package media

import "time"

// SetTucked tucks files away, or brings them back, in their sidecars, together with every file that goes with them
// (edits, a Live Photo's video, a RAW and its JPEG), as SetPrivate does. Tucking is a lighter hiding than private:
// the apps leave a tucked file out of their feeds until the user reveals it, and search never finds it, but the
// server sends, previews and copies it like any other file. Each changed file comes again in the change feed. Files
// already as asked are left as they are. It returns the files it changed.
func (s *Store) SetTucked(hashes []string, tucked bool, by string) ([]Item, error) {
	return s.setMark(hashes, func(sidecar *Sidecar, now time.Time) bool {
		if sidecar.IsTucked() == tucked {
			return false
		}
		sidecar.Tucked = &Tuck{Tucked: tucked, SetAt: now, SetBy: by}
		return true
	})
}

// inheritedTuck is the mark a new file takes from the file it is an edit of or paired with: a later edit of a
// tucked photo is tucked too. Callers hold s.mu.
func (s *Store) inheritedTuck(req UploadRequest) *Tuck {
	for _, to := range []*string{req.VersionOf, req.PairedWith} {
		if to == nil {
			continue
		}
		if item, ok := s.items[*to]; ok && item.Sidecar.IsTucked() {
			return &Tuck{Tucked: true, SetAt: s.now().UTC(), SetBy: req.UploadedBy}
		}
	}
	return nil
}
