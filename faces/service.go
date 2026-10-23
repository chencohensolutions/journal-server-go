// Package faces finds photos by the people in them, with two small open models run by ONNX Runtime on the server's
// CPU: YuNet finds the faces in a photo, SFace turns each into a vector, and faces of the same person point the same
// way. The server reads each file once (package reading: the photo at 640 and 1280 px, then each face from the
// original; a video's poster frame) and keeps its faces in the data folder's .cache. Faces alike are put in groups; the user names a group once ("Mom"), and from then on her faces
// are found as they come. The names, and the faces the user moved, are kept in people.json at the data folder's
// root. See docs/faces.md.
package faces

import (
	"context"
	"fmt"
	"slices"

	"github.com/journal/server-go/media"
	"github.com/journal/server-go/reading"
)

// Faces narrower than this many pixels in the 1280 px picture are not kept: nothing can be told from them.
const minPixels = 16

// The pictures the detector reads, each made from the original: the whole photo at YuNet's 640 for the big faces,
// and at 1280 for the tiles that find the small ones (media.FacePreview).
var (
	WholePicture = media.PreviewSpec{Width: detectorSide, Height: detectorSide, Fit: "contain"}
	TilesPicture = media.PreviewSpec{Width: media.FacePreview.Width, Height: media.FacePreview.Height, Fit: "contain"}
)

// Service is face search over the users' photos and videos.
type Service struct {
	model   *Model
	library *media.Library
	users   map[string]*people // by username
}

// NewService opens the model's faces and the people named in the data folder of each of library's users.
func NewService(model *Model, library *media.Library) (*Service, error) {
	s := &Service{model: model, library: library, users: map[string]*people{}}
	for _, user := range library.Users() {
		store := library.Store(user)
		index, err := OpenIndex(store.Root(), model.Name)
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
		if s.users[user], err = newPeople(store, index); err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
	}
	return s, nil
}

// NewReadOnlyService answers queries about the faces another process finds (journal-server index), named after
// its models: each user's faces opened to read only, and the people they named. Refresh reads what that process
// added; nothing is read here.
func NewReadOnlyService(model string, library *media.Library) (*Service, error) {
	s := &Service{model: &Model{Name: model}, library: library, users: map[string]*people{}}
	for _, user := range library.Users() {
		store := library.Store(user)
		index, err := OpenIndexReadOnly(store.Root(), model)
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
		if s.users[user], err = newPeople(store, index); err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
	}
	return s, nil
}

// Refresh reads the faces another process found since the last look (NewReadOnlyService); the next query puts
// them with the others.
func (s *Service) Refresh() error {
	for user, p := range s.users {
		if _, err := p.index.Refresh(); err != nil {
			return fmt.Errorf("user %s: %w", user, err)
		}
	}
	return nil
}

// Model is the name of the models the faces were found with.
func (s *Service) Model() string { return s.model.Name }

func (s *Service) user(user string) (*people, error) {
	p := s.users[user]
	if p == nil {
		return nil, fmt.Errorf("no data_folder is configured for %s", user)
	}
	return p, nil
}

// People is user's named people and (withGroups) groups of alike faces, as a device sees them: only faces in files
// keep accepts count, and those with none are left out.
func (s *Service) People(user string, keep func(sha string) bool, withGroups bool) ([]Person, error) {
	p, err := s.user(user)
	if err != nil {
		return nil, err
	}
	return p.list(keep, withGroups), nil
}

// FacesIn is the faces found in user's file sha, biggest first, and whom each is.
func (s *Service) FacesIn(user, sha string) []FaceInfo {
	if p := s.users[user]; p != nil {
		return p.facesIn(sha)
	}
	return nil
}

// Files is user's files that have a face of every one of people (IDs of people or groups).
func (s *Service) Files(user string, people []string) (map[string]bool, error) {
	p, err := s.user(user)
	if err != nil {
		return nil, err
	}
	return p.files(people)
}

// Similar is the limit files of user, of those keep accepts, with a face most like face, best first.
func (s *Service) Similar(user string, face FaceID, limit int, keep func(sha string) bool) ([]FaceHit, error) {
	p, err := s.user(user)
	if err != nil {
		return nil, err
	}
	return p.similar(face, limit, keep)
}

// Name names a group, or renames a person, and returns the person's ID; see people.name.
func (s *Service) Name(user, id, name string) (string, error) {
	p, err := s.user(user)
	if err != nil {
		return "", err
	}
	return p.name(id, name)
}

// Merge puts the faces of from (people or groups) in the person into.
func (s *Service) Merge(user, into string, from []string) error {
	p, err := s.user(user)
	if err != nil {
		return err
	}
	return p.merge(into, from)
}

// Move gives face to the person to ("": no one), and keeps it from the person it was.
func (s *Service) Move(user string, face FaceID, to string) error {
	p, err := s.user(user)
	if err != nil {
		return err
	}
	return p.move(face, to)
}

// Forget removes a person; their faces are put in groups again.
func (s *Service) Forget(user, id string) error {
	p, err := s.user(user)
	if err != nil {
		return err
	}
	return p.forget(id)
}

// Counts is how many of user's files have been read for faces, of all of them; files hide reports true for are left
// out of both.
func (s *Service) Counts(user string, hide func(media.Item) bool) (read, total int) {
	p, store := s.users[user], s.library.Store(user)
	if p == nil {
		return 0, 0
	}
	store.Range(func(item media.Item) {
		if hide(item) {
			return
		}
		total++
		if p.index.Has(item.Sha256()) {
			read++
		}
	})
	return read, total
}

// Index is the service as an index of package reading, which reads each file once for every index and calls Read.
func (s *Service) Index() reading.Index { return index{s} }

type index struct{ *Service }

func (x index) Name() string { return "faces" }

// Order: after the nudity scan (which hides), before text search.
func (x index) Order() int { return 10 }

func (x index) Wants() reading.Wants {
	return reading.Wants{Kinds: []media.Kind{media.KindPhoto, media.KindVideo},
		Pictures: []media.PreviewSpec{WholePicture, TilesPicture}, Regions: true}
}

// Needs reports whether item hasn't been read for faces.
func (x index) Needs(user string, item media.Item) bool {
	p := x.users[user]
	return p != nil && !p.index.Has(item.Sha256())
}

// Read finds the file's faces and keeps them (none is kept too: the file has been read).
func (x index) Read(ctx context.Context, user string, in reading.Input) error {
	large := in.Pictures[TilesPicture]
	var region Region
	if in.Original != nil {
		region = in.Original.Region
	}
	faces, err := x.model.FacesFrom(in.Pictures[WholePicture], large, region)
	if err != nil {
		return err
	}
	return x.users[user].index.Add(in.Item.Sha256(), keepable(faces, large.Bounds().Dx()))
}

// Finish puts the faces of the files just read with the others.
func (x index) Finish(user string) {
	if p := x.users[user]; p != nil {
		p.mu.Lock()
		p.update()
		p.mu.Unlock()
	}
}

func (x index) FailureLog() (folder, name string) { return "faces", x.model.Name + ".failed.log" }

// keepable is faces without those too small to tell apart, biggest first.
func keepable(faces []Face, width int) []Face {
	faces = slices.DeleteFunc(faces, func(f Face) bool { return f.Box[2]*float32(width) < minPixels })
	slices.SortStableFunc(faces, func(a, b Face) int {
		areaA, areaB := a.Box[2]*a.Box[3], b.Box[2]*b.Box[3]
		switch {
		case areaA > areaB:
			return -1
		case areaA < areaB:
			return 1
		}
		return 0
	})
	return faces
}

// Close closes the users' face files.
func (s *Service) Close() {
	for _, p := range s.users {
		p.index.Close()
	}
}
