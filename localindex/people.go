package localindex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/faces"
)

// Members is the face index's people and groups with their faces (faces.Service).
type Members interface {
	Members(user string, keep func(sha string) bool, withGroups bool) ([]faces.Member, error)
}

// PeopleFormat is the version of the people document (ServePeople).
const PeopleFormat = 1

type peopleDoc struct {
	V      int          `json:"v"`
	Model  string       `json:"model"`
	People []peopleItem `json:"people"`
}

type peopleItem struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Faces are FaceIDs ("<sha256>:<n>", n counting the file's faces in the faces index's record, biggest first).
	Faces []string `json:"faces"`
}

// ServePeople answers GET /index/people: the user's named people (by name) and groups of alike faces (most files
// first), each with every face of theirs in files the device may see, as JSON. Groups are made in the server's memory
// and renumbered whenever the user names, merges or moves (their IDs aren't kept), so the document comes whole; its
// sha256 is the ETag, and a phone that has it (If-None-Match) gets 304.
func (s *Server) ServePeople(w http.ResponseWriter, r *http.Request) {
	user := auth.FromContext(r.Context())
	if s.people == nil || user == nil {
		http.NotFound(w, r)
		return
	}
	store := s.library.Store(user.Username)
	if store == nil {
		http.NotFound(w, r)
		return
	}
	view := store.ViewFor(auth.WithholdPrivateFromContext(r.Context()))
	keep := func(sha string) bool {
		item, ok := store.Get(sha)
		return ok && !store.Hides(view, item)
	}
	members, err := s.people.Members(user.Username, keep, true)
	if err != nil {
		log.Printf("localindex: people for %s: %v", user.Username, err)
		http.Error(w, "the people can't be read", http.StatusInternalServerError)
		return
	}
	doc := peopleDoc{V: PeopleFormat, People: make([]peopleItem, 0, len(members))}
	if src := s.sources["faces"]; src != nil {
		doc.Model = src.Cache.Model()
	}
	for _, m := range members {
		item := peopleItem{ID: m.ID, Name: m.Name, Faces: make([]string, len(m.Faces))}
		for i, f := range m.Faces {
			item.Faces[i] = f.String()
		}
		doc.People = append(doc.People, item)
	}
	data, _ := json.Marshal(doc)
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(data)
}
