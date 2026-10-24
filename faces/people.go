package faces

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/journal/server-go/media"
)

// How faces are put together, with SFace's vectors. OpenCV takes a cosine of 0.363 between two faces as the same
// person; a face is compared here with the mean of many (a person's, a group's), which is closer to each of them
// than they are to each other, so the bar is set a little higher.
const (
	// A face is a named person's when it is at least this like the mean of the faces the user named…
	PersonMatch = 0.45
	// …or, when it is at least nearMatch like that mean, at least exemplarMatch like one of those faces (a child
	// grows; a face is seen from the side).
	nearMatch     = 0.3
	exemplarMatch = 0.5
	exemplars     = 64
	// A face at least this like a face the user said a person is not, and more like it than like any of the person's
	// own faces (face to face: a mean is closer to every face than faces are to each other), isn't theirs either: the
	// user's "Not Mom" holds for that face's lookalikes too (a sister, the same stranger again).
	NotMatch = 0.5
	// A face no one is named for joins the first group whose mean it is at least this like.
	GroupJoin = 0.45
	// Groups of fewer faces are not shown: most are strangers in the background.
	MinGroup = 3
	// Only faces the detector is this sure of, and this many pixels wide in the 1280 px preview, are put in groups
	// or taken for a named person; smaller ones look too much alike. A face the user names is theirs whatever its
	// size.
	goodScore  = 0.8
	goodPixels = 40
	// A face that has not joined a group of two or more is only compared with the faces found in the last window
	// files before it (in the order taken), which keeps grouping a library fast. People who are seen again come
	// back within that many files, or join a big group when it is merged.
	recentWindow = 2000
)

// FaceID names a face: the file's sha256 and the face's place in it (biggest first), "<sha256>:<n>".
type FaceID struct {
	Sha256 string
	N      int
}

func (id FaceID) String() string { return id.Sha256 + ":" + strconv.Itoa(id.N) }

// ParseFaceID reads a FaceID.
func ParseFaceID(s string) (FaceID, error) {
	sha, n, ok := strings.Cut(s, ":")
	i, err := strconv.Atoi(n)
	if !ok || err != nil || i < 0 || len(sha) != 64 {
		return FaceID{}, fmt.Errorf("%q is not a face", s)
	}
	return FaceID{strings.ToLower(sha), i}, nil
}

// Group is a named person, or a group of faces the server found alike that no one has named yet. A person's ID
// starts with "p" and never changes; a group's is "g" and its first face, and it may change whenever the groups are
// found again (when the user names, moves or merges faces).
type Group struct {
	ID    string
	Name  string // "" for a group not named
	Faces []FaceID
}

// Named reports whether g is a person the user named.
func (g *Group) Named() bool { return strings.HasPrefix(g.ID, "p") }

// ErrNotFound is a person, group or face that is not there (any more): the app's list is older than the server's.
var ErrNotFound = errors.New("not found")

// people is one user's faces put together: the people they named (people.json) and the groups found among the
// other faces. It is found again, whole, when the user changes a person, and grown as files are read.
type people struct {
	store *media.Store
	index *Index

	mu       sync.Mutex
	named    []media.Person // as in people.json
	groups   map[string]*Group
	of       map[FaceID]string // face → its group's ID
	built    bool
	seen     map[string]bool // the files grouped so far
	means    map[string][]float32
	examples map[string][][]float32 // a named person's exemplars
	nots     map[string][][]float32 // the vectors of the faces the user said a named person is not
	recent   []*cluster             // groups of one face, the last recentWindow
	clusters []*cluster             // groups of two or more
}

// cluster is a group being found: its faces and the sum of their vectors.
type cluster struct {
	group *Group
	sum   []float32
	mean  []float32 // sum, of length 1
}

func newPeople(store *media.Store, index *Index) (*people, error) {
	named, err := store.People()
	if err != nil {
		return nil, err
	}
	return &people{store: store, index: index, named: named}, nil
}

// update puts the faces of files read since the last call together (all of them, the first time). Callers hold
// p.mu.
func (p *people) update() {
	if !p.built {
		p.rebuild()
		return
	}
	if files, _ := p.index.Len(); files == len(p.seen) {
		return
	}
	var todo []string
	p.index.Range(func(sha string, _ []Face) {
		if !p.seen[sha] {
			todo = append(todo, sha)
		}
	})
	p.add(todo)
}

// rebuild puts every face together again: after the user changed a person. Callers hold p.mu.
func (p *people) rebuild() {
	p.groups, p.of, p.seen = map[string]*Group{}, map[FaceID]string{}, map[string]bool{}
	p.means, p.examples, p.nots = map[string][]float32{}, map[string][][]float32{}, map[string][][]float32{}
	p.recent, p.clusters = nil, nil
	for _, person := range p.named {
		g := &Group{ID: person.ID, Name: person.Name}
		p.groups[g.ID] = g
		var vectors [][]float32
		for _, ref := range person.Faces {
			id, ok := p.find(ref)
			if !ok {
				continue
			}
			if _, taken := p.of[id]; taken {
				continue
			}
			p.of[id] = g.ID
			g.Faces = append(g.Faces, id)
			vectors = append(vectors, p.index.Faces(id.Sha256)[id.N].Vector)
		}
		for _, ref := range person.NotFaces {
			if id, ok := p.find(ref); ok {
				p.nots[g.ID] = append(p.nots[g.ID], p.index.Faces(id.Sha256)[id.N].Vector)
			}
		}
		if len(vectors) > 0 {
			p.means[g.ID] = mean(vectors)
			step := max(1, len(vectors)/exemplars)
			for i := 0; i < len(vectors); i += step {
				p.examples[g.ID] = append(p.examples[g.ID], vectors[i])
			}
		}
	}
	var all []string
	p.index.Range(func(sha string, _ []Face) { all = append(all, sha) })
	p.add(all)
	p.built = true
}

// add puts the faces of files in with the named people and the groups, in the order the files were taken, so that
// a person's photos of one day meet in the recent window.
func (p *people) add(files []string) {
	type file struct {
		sha string
		at  time.Time
	}
	var ordered []file
	for _, sha := range files {
		p.seen[sha] = true
		if item, ok := p.store.Get(sha); ok {
			at := item.Sidecar.Facts.UploadedAt
			if item.Sidecar.Facts.TakenAt != nil {
				at = *item.Sidecar.Facts.TakenAt
			}
			ordered = append(ordered, file{sha, at})
		}
	}
	slices.SortFunc(ordered, func(a, b file) int {
		return cmp.Or(a.at.Compare(b.at), strings.Compare(a.sha, b.sha))
	})
	notFaces := p.notFaces()
	type namedFace struct {
		person string
		ref    media.FaceRef
	}
	named := map[string][]namedFace{} // the faces the user named, by file
	for _, person := range p.named {
		for _, ref := range person.Faces {
			named[ref.Sha256] = append(named[ref.Sha256], namedFace{person.ID, ref})
		}
	}
	for _, f := range ordered {
		item, _ := p.store.Get(f.sha)
		// Faces the user named in a file read after the people were put together (a model changed).
		for _, nf := range named[f.sha] {
			if id, ok := p.find(nf.ref); ok && p.of[id] == "" {
				p.of[id] = nf.person
				p.groups[nf.person].Faces = append(p.groups[nf.person].Faces, id)
			}
		}
		for n, face := range p.index.Faces(f.sha) {
			id := FaceID{f.sha, n}
			if _, taken := p.of[id]; taken || !good(item, face) {
				continue
			}
			if person := p.personFor(face.Vector, id, notFaces); person != "" {
				p.of[id] = person
				g := p.groups[person]
				g.Faces = append(g.Faces, id)
				continue
			}
			p.cluster(id, face.Vector)
		}
	}
}

// personFor is the named person face is, or "": of the people whose mean it is at least nearMatch like, the most
// like it that it is at least PersonMatch like (or exemplarMatch like one of their faces), and that the user hasn't
// said it is not (the face itself, or one it is more like than the person: notFor).
func (p *people) personFor(vector []float32, id FaceID, notFaces map[string]map[FaceID]bool) string {
	type candidate struct {
		person string
		score  float32
	}
	var candidates []candidate
	for person, m := range p.means {
		if notFaces[person][id] {
			continue
		}
		if s := dot(vector, m); s > nearMatch {
			candidates = append(candidates, candidate{person, s})
		}
	}
	slices.SortFunc(candidates, func(a, b candidate) int { return cmp.Or(cmp.Compare(b.score, a.score), strings.Compare(a.person, b.person)) })
	for _, c := range candidates {
		if p.notFor(vector, c.person) {
			continue
		}
		if c.score >= PersonMatch {
			return c.person
		}
		for _, e := range p.examples[c.person] {
			if dot(vector, e) >= exemplarMatch {
				return c.person
			}
		}
	}
	return ""
}

// notFor reports whether vector looks like a face the user said person is not (NotMatch or more) more than like
// any of the person's own faces (their exemplars).
func (p *people) notFor(vector []float32, person string) bool {
	var not float32 = -1
	for _, v := range p.nots[person] {
		not = max(not, dot(vector, v))
	}
	if not < NotMatch {
		return false
	}
	for _, e := range p.examples[person] {
		if dot(vector, e) >= not {
			return false
		}
	}
	return true
}

// cluster puts a face no one is named for in the first group it is like enough, or starts a group.
func (p *people) cluster(id FaceID, vector []float32) {
	join := func(c *cluster) {
		c.group.Faces = append(c.group.Faces, id)
		p.of[id] = c.group.ID
		for i, v := range vector {
			c.sum[i] += v
		}
		c.mean = unit(slices.Clone(c.sum))
	}
	for _, c := range p.clusters {
		if dot(vector, c.mean) >= GroupJoin {
			join(c)
			return
		}
	}
	for i, c := range p.recent {
		if dot(vector, c.mean) >= GroupJoin {
			join(c)
			p.recent = slices.Delete(p.recent, i, i+1)
			p.clusters = append(p.clusters, c)
			return
		}
	}
	g := &Group{ID: "g" + id.String(), Faces: []FaceID{id}}
	p.groups[g.ID] = g
	p.of[id] = g.ID
	p.recent = append(p.recent, &cluster{group: g, sum: slices.Clone(vector), mean: vector})
	if len(p.recent) > recentWindow {
		p.recent = p.recent[1:]
	}
}

// notFaces is the faces the user said each named person is not.
func (p *people) notFaces() map[string]map[FaceID]bool {
	not := map[string]map[FaceID]bool{}
	for _, person := range p.named {
		for _, ref := range person.NotFaces {
			if id, ok := p.find(ref); ok {
				if not[person.ID] == nil {
					not[person.ID] = map[FaceID]bool{}
				}
				not[person.ID][id] = true
			}
		}
	}
	return not
}

// find is the face found closest to ref's box in ref's file, if one overlaps it by half or more.
func (p *people) find(ref media.FaceRef) (FaceID, bool) {
	best, bestIoU := -1, 0.5
	for n, f := range p.index.Faces(ref.Sha256) {
		box := [4]float64{float64(f.Box[0]), float64(f.Box[1]), float64(f.Box[2]), float64(f.Box[3])}
		if o := iou(box, ref.Box); o >= bestIoU {
			best, bestIoU = n, o
		}
	}
	return FaceID{ref.Sha256, best}, best >= 0
}

// ref is how people.json names face id.
func (p *people) ref(id FaceID) media.FaceRef {
	f := p.index.Faces(id.Sha256)[id.N]
	return media.FaceRef{Sha256: id.Sha256, Box: [4]float64{float64(f.Box[0]), float64(f.Box[1]), float64(f.Box[2]), float64(f.Box[3])}}
}

// good reports whether face is clear enough to be put with others without the user saying so.
func good(item media.Item, face Face) bool {
	if face.Score < goodScore {
		return false
	}
	// The width of the preview the face was found in (the upright size, fitted in 1280 px); 1280 when unknown.
	width := float64(media.FacePreview.Width)
	if w, h := item.Sidecar.Facts.Width, item.Sidecar.Facts.Height; w != nil && h != nil && *w > 0 && *h > 0 {
		width = float64(*w) * min(1, width/float64(max(*w, *h)))
	}
	return float64(face.Box[2])*width >= goodPixels
}

func mean(vectors [][]float32) []float32 {
	sum := make([]float32, len(vectors[0]))
	for _, v := range vectors {
		for i, x := range v {
			sum[i] += x
		}
	}
	return unit(sum)
}

// change has fn change the named people, saves them, and puts the faces together again.
func (p *people) change(fn func(named []media.Person) ([]media.Person, error)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.built {
		p.rebuild()
	}
	named, err := fn(slices.Clone(p.named))
	if err != nil {
		return err
	}
	if err := p.store.SetPeople(named); err != nil {
		return err
	}
	p.named = named
	p.rebuild()
	return nil
}

func newPersonID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "p" + hex.EncodeToString(b)
}
