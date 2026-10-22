package media

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Person is someone the user named in face search (package faces): the faces they said are this person, and those
// they said are not. The faces themselves (where in a photo, what they look like) come from a model and are kept in
// .cache, which can be rebuilt; the names and the user's choices can't, so they are kept in people.json at the data
// folder's root, which moves with the data. See docs/faces.md.
type Person struct {
	// "p" and random hex; never reused.
	ID   string `json:"id"`
	Name string `json:"name"`
	// The faces that are this person, as the user named them (with the group the server had found).
	Faces []FaceRef `json:"faces"`
	// Faces the server took for this person that the user said are someone else.
	NotFaces []FaceRef `json:"notFaces,omitempty"`
	// When the user last changed the person.
	SetAt time.Time `json:"setAt"`
}

// FaceRef is one face in one file: the file's sha256 and the box around the face, as fractions of the upright
// photo's width and height (x, y, w, h; the same as a preview's crop). A face is named by its box rather than by
// its place in a model's list, so the user's choices outlive a change of model: the face found closest to the box
// is taken.
type FaceRef struct {
	Sha256 string     `json:"sha256"`
	Box    [4]float64 `json:"box"`
}

// PeopleFormat is the version of people.json.
const PeopleFormat = 1

type peopleFile struct {
	Format int      `json:"format"`
	People []Person `json:"people"`
}

func (s *Store) peoplePath() string { return filepath.Join(s.root, "people.json") }

// People reads people.json; a data folder without one has no people.
func (s *Store) People() ([]Person, error) {
	data, err := os.ReadFile(s.peoplePath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file peopleFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", s.peoplePath(), err)
	}
	if file.Format > PeopleFormat {
		return nil, fmt.Errorf("%s has format %d, newer than this server's %d", s.peoplePath(), file.Format, PeopleFormat)
	}
	return file.People, nil
}

// SetPeople replaces people.json (inside a turn, written whole). Only the server writes it.
func (s *Store) SetPeople(people []Person) error {
	if people == nil {
		people = []Person{}
	}
	return s.Turn(func() error {
		data, err := json.MarshalIndent(peopleFile{Format: PeopleFormat, People: people}, "", "  ")
		if err != nil {
			return err
		}
		return writeFileAtomic(s.peoplePath(), append(data, '\n'))
	})
}

// Now is the store's clock, in UTC: what it writes in sidecars and people.json.
func (s *Store) Now() time.Time { return s.now().UTC() }
