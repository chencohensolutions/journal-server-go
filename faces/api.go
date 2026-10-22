package faces

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/journal/server-go/media"
)

// Person is a named person or a group of alike faces, as a device sees it: only the faces in files it may see.
type Person struct {
	ID   string
	Name string // "" for a group not named
	// The clearest of the faces: the one apps show.
	Cover FaceID
	// How many faces, and how many files they are in.
	Faces, Files int
}

// FaceInfo is a face in a file and whom it is.
type FaceInfo struct {
	ID FaceID
	Face
	// The person or group it is in, "" when none; Name is "" for a group not named.
	Person, Name string
}

// FaceHit is a face found like another, the best of its file.
type FaceHit struct {
	ID    FaceID
	Score float32
}

// MaxName is the longest a person's name may be, in bytes.
const MaxName = 200

// list is the people and groups with a face in a file keep accepts: named people first (by name), then the groups of
// at least MinGroup faces, those in the most files first; groups are left out unless withGroups.
func (p *people) list(keep func(sha string) bool, withGroups bool) []Person {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.update()
	var list []Person
	for _, g := range p.groups {
		if !g.Named() && (!withGroups || len(g.Faces) < MinGroup) {
			continue
		}
		person := Person{ID: g.ID, Name: g.Name}
		files := map[string]bool{}
		var best float32 = -1
		for _, id := range g.Faces {
			if !keep(id.Sha256) {
				continue
			}
			person.Faces++
			files[id.Sha256] = true
			f := p.index.Faces(id.Sha256)[id.N]
			if clarity := f.Score * f.Box[2]; clarity > best {
				best, person.Cover = clarity, id
			}
		}
		person.Files = len(files)
		if person.Faces == 0 || (!g.Named() && person.Faces < MinGroup) {
			continue
		}
		list = append(list, person)
	}
	slices.SortFunc(list, func(a, b Person) int {
		if (a.Name == "") != (b.Name == "") {
			if a.Name != "" {
				return -1
			}
			return 1
		}
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(b.Files, a.Files),
			strings.Compare(a.ID, b.ID))
	})
	return list
}

// facesIn is the faces found in the file sha, biggest first; nil when it hasn't been read or has none.
func (p *people) facesIn(sha string) []FaceInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.update()
	var infos []FaceInfo
	for n, f := range p.index.Faces(sha) {
		info := FaceInfo{ID: FaceID{sha, n}, Face: f}
		if g := p.groups[p.of[info.ID]]; g != nil && (g.Named() || len(g.Faces) >= MinGroup) {
			info.Person, info.Name = g.ID, g.Name
		}
		infos = append(infos, info)
	}
	return infos
}

// files is the files that have a face of every one of ids (people or groups).
func (p *people) files(ids []string) (map[string]bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.update()
	var in map[string]bool
	for _, id := range ids {
		g := p.groups[id]
		if g == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		these := map[string]bool{}
		for _, f := range g.Faces {
			if in == nil || in[f.Sha256] {
				these[f.Sha256] = true
			}
		}
		in = these
	}
	return in, nil
}

// similar is the limit files, of those keep accepts, with the faces most like id, best first: one face a file, and
// not id's own file.
func (p *people) similar(id FaceID, limit int, keep func(sha string) bool) ([]FaceHit, error) {
	faces := p.index.Faces(id.Sha256)
	if id.N >= len(faces) {
		return nil, fmt.Errorf("%w: face %s", ErrNotFound, id)
	}
	query := faces[id.N].Vector
	var hits []FaceHit
	p.index.Range(func(sha string, faces []Face) {
		if sha == id.Sha256 {
			return
		}
		best := FaceHit{Score: -2}
		for n, f := range faces {
			if s := dot(query, f.Vector); s > best.Score {
				best = FaceHit{FaceID{sha, n}, s}
			}
		}
		if best.Score > -2 {
			hits = append(hits, best)
		}
	})
	slices.SortFunc(hits, func(a, b FaceHit) int { return cmp.Compare(b.Score, a.Score) })
	kept := hits[:0]
	for _, h := range hits {
		if len(kept) == limit {
			break
		}
		if keep(h.ID.Sha256) {
			kept = append(kept, h)
		}
	}
	return kept, nil
}

// name names the group id, or renames the person id; a group given the name of a person (ignoring case) joins
// them. It returns the person's ID.
func (p *people) name(id, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > MaxName {
		return "", fmt.Errorf("a name is 1–%d bytes", MaxName)
	}
	var personID string
	err := p.change(func(named []media.Person) ([]media.Person, error) {
		g := p.groups[id]
		if g == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		now := p.store.Now()
		if g.Named() {
			i := slices.IndexFunc(named, func(n media.Person) bool { return n.ID == id })
			named[i].Name, named[i].SetAt = name, now
			personID = id
			return named, nil
		}
		refs := p.refs(g.Faces)
		if i := slices.IndexFunc(named, func(n media.Person) bool { return strings.EqualFold(n.Name, name) }); i >= 0 {
			named[i] = p.withFaces(named[i], refs, now)
			personID = named[i].ID
			return named, nil
		}
		personID = newPersonID()
		return append(named, media.Person{ID: personID, Name: name, Faces: refs, SetAt: now}), nil
	})
	return personID, err
}

// merge puts the faces of the people and groups from into the person into; the people of from are no more.
func (p *people) merge(into string, from []string) error {
	return p.change(func(named []media.Person) ([]media.Person, error) {
		target := p.groups[into]
		if target == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, into)
		}
		if !target.Named() {
			return nil, fmt.Errorf("%s is a group no one has named: name it first", into)
		}
		var refs []media.FaceRef
		for _, id := range from {
			g := p.groups[id]
			if g == nil {
				return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
			}
			if id == into {
				continue
			}
			refs = append(refs, p.refs(g.Faces)...)
			named = slices.DeleteFunc(named, func(n media.Person) bool { return n.ID == id })
		}
		i := slices.IndexFunc(named, func(n media.Person) bool { return n.ID == into })
		named[i] = p.withFaces(named[i], refs, p.store.Now())
		return named, nil
	})
}

// move takes the face id from the person it is now (who is then never given it again) and gives it to the person
// to; to "" leaves it with no one.
func (p *people) move(id FaceID, to string) error {
	return p.change(func(named []media.Person) ([]media.Person, error) {
		if id.N >= len(p.index.Faces(id.Sha256)) {
			return nil, fmt.Errorf("%w: face %s", ErrNotFound, id)
		}
		if to != "" {
			if g := p.groups[to]; g == nil || !g.Named() {
				return nil, fmt.Errorf("%w: person %s", ErrNotFound, to)
			}
		}
		ref, now := p.ref(id), p.store.Now()
		from := p.of[id]
		for i := range named {
			n := &named[i]
			switch n.ID {
			case to:
				*n = p.withFaces(*n, []media.FaceRef{ref}, now)
			case from:
				n.Faces = slices.DeleteFunc(slices.Clone(n.Faces), func(r media.FaceRef) bool { return p.is(r, id) })
				n.NotFaces = append(slices.Clone(n.NotFaces), ref)
				n.SetAt = now
			}
		}
		return named, nil
	})
}

// forget removes the person id; their faces are put in groups again.
func (p *people) forget(id string) error {
	return p.change(func(named []media.Person) ([]media.Person, error) {
		i := slices.IndexFunc(named, func(n media.Person) bool { return n.ID == id })
		if i < 0 {
			return nil, fmt.Errorf("%w: person %s", ErrNotFound, id)
		}
		return slices.Delete(named, i, i+1), nil
	})
}

// withFaces is person with refs added to their faces (and taken from the faces they are not).
func (p *people) withFaces(person media.Person, refs []media.FaceRef, now time.Time) media.Person {
	ids := map[FaceID]bool{}
	for _, r := range refs {
		if id, ok := p.find(r); ok {
			ids[id] = true
		}
	}
	isAdded := func(r media.FaceRef) bool {
		id, ok := p.find(r)
		return ok && ids[id]
	}
	person.Faces = append(slices.DeleteFunc(slices.Clone(person.Faces), isAdded), refs...)
	person.NotFaces = slices.DeleteFunc(slices.Clone(person.NotFaces), isAdded)
	person.SetAt = now
	return person
}

// refs is how people.json names faces ids.
func (p *people) refs(ids []FaceID) []media.FaceRef {
	refs := make([]media.FaceRef, len(ids))
	for i, id := range ids {
		refs[i] = p.ref(id)
	}
	return refs
}

// is reports whether ref names the face id.
func (p *people) is(ref media.FaceRef, id FaceID) bool {
	found, ok := p.find(ref)
	return ok && found == id
}
