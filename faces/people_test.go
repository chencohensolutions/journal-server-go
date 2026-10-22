package faces

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

// fixture is a data folder with files whose faces the test makes up: vectors near one of a few people's, or of
// strangers.
type fixture struct {
	t      testing.TB
	store  *media.Store
	index  *Index
	rng    *rand.Rand
	people map[string][]float32 // a made-up person's own vector
	files  int
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	root := t.TempDir()
	store, err := media.OpenStore(root, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	index, err := OpenIndex(root, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close() })
	return &fixture{t: t, store: store, index: index, rng: rand.New(rand.NewPCG(1, 2)), people: map[string][]float32{}}
}

func (f *fixture) randomVector() []float32 {
	v := make([]float32, Dim)
	for i := range v {
		v[i] = float32(f.rng.NormFloat64())
	}
	return unit(v)
}

// faceOf is a face of who: near their vector (about 0.8 like it), or a stranger's when who is "".
func (f *fixture) faceOf(who string) Face {
	if who == "" {
		return Face{Box: [4]float32{0.1, 0.1, 0.1, 0.1}, Score: 0.95, Vector: f.randomVector()}
	}
	base := f.people[who]
	if base == nil {
		base = f.randomVector()
		f.people[who] = base
	}
	noise := f.randomVector()
	v := make([]float32, Dim)
	for i := range v {
		v[i] = base[i] + 0.7*noise[i]
	}
	return Face{Box: [4]float32{0.1, 0.1, 0.1, 0.1}, Score: 0.95, Vector: unit(v)}
}

// file adds a file, taken one hour after the one before, whose faces haven't been read, and returns its sha256.
func (f *fixture) file() string {
	f.t.Helper()
	f.files++
	content := []byte(fmt.Sprintf("photo %d", f.files))
	path := filepath.Join(f.t.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	taken := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(f.files) * time.Hour)
	_, _, err := f.store.Import(path, media.UploadRequest{Sha256: sha, Size: int64(len(content)),
		FileName: fmt.Sprintf("IMG_%04d.jpg", f.files), MimeType: "image/jpeg", Kind: media.KindPhoto, TakenAt: &taken,
		UploadedBy: "test"})
	if err != nil {
		f.t.Fatal(err)
	}
	return sha
}

// photo adds a file with the faces of who ("" for a stranger), taken one hour after the one before, and returns its
// sha256.
func (f *fixture) photo(who ...string) string {
	f.t.Helper()
	sha := f.file()
	var faces []Face
	for i, w := range who {
		face := f.faceOf(w)
		face.Box[0] = 0.1 + 0.3*float32(i) // side by side
		faces = append(faces, face)
	}
	if err := f.index.Add(sha, faces); err != nil {
		f.t.Fatal(err)
	}
	return sha
}

func (f *fixture) open() *people {
	f.t.Helper()
	p, err := newPeople(f.store, f.index)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func all(string) bool { return true }

// filesOf is the files person id is in, sorted.
func filesOf(t *testing.T, p *people, id string) []string {
	t.Helper()
	files, err := p.files([]string{id})
	if err != nil {
		t.Fatal(err)
	}
	var list []string
	for sha := range files {
		list = append(list, sha)
	}
	slices.Sort(list)
	return list
}

func sorted(list ...string) []string {
	list = slices.Clone(list)
	slices.Sort(list)
	return list
}

func TestAlikeFacesAreGroupedAndStrangersLeftOut(t *testing.T) {
	f := newFixture(t)
	var mom, dad []string
	for range 5 {
		mom = append(mom, f.photo("mom"))
		f.photo("")
	}
	for range 4 {
		dad = append(dad, f.photo("dad"))
	}
	both := f.photo("mom", "dad")
	mom, dad = append(mom, both), append(dad, both)

	p := f.open()
	list := p.list(all, true)
	if len(list) != 2 {
		t.Fatalf("got %d groups, want 2 (strangers are each alone): %+v", len(list), list)
	}
	if list[0].Files != 6 || list[1].Files != 5 {
		t.Errorf("groups of %d and %d files, want 6 and 5 (the most files first)", list[0].Files, list[1].Files)
	}
	if got := filesOf(t, p, list[0].ID); !slices.Equal(got, sorted(mom...)) {
		t.Errorf("the first group is in %v, want mom's %v", got, sorted(mom...))
	}
	if len(p.list(all, false)) != 0 {
		t.Error("groups are listed without withGroups")
	}
	// The face in the photo of both says whom it is.
	infos := p.facesIn(both)
	if len(infos) != 2 || infos[0].Person == "" || infos[0].Person == infos[1].Person {
		t.Errorf("faces in the photo of both: %+v", infos)
	}
	// Files with both of them.
	files, err := p.files([]string{list[0].ID, list[1].ID})
	if err != nil || len(files) != 1 || !files[both] {
		t.Errorf("files with both: %v, %v", files, err)
	}
}

func TestNamingAPersonKeepsTheirFacesComing(t *testing.T) {
	f := newFixture(t)
	var mom []string
	for range 4 {
		mom = append(mom, f.photo("mom"))
	}
	for range 3 {
		f.photo("dad")
	}
	p := f.open()
	group := p.list(all, true)[0].ID
	id, err := p.name(group, " Mom ")
	if err != nil {
		t.Fatal(err)
	}
	list := p.list(all, true)
	if list[0].ID != id || list[0].Name != "Mom" || list[0].Files != 4 {
		t.Fatalf("named people come first: %+v", list)
	}

	// A new photo of her is hers as soon as it is read.
	mom = append(mom, f.photo("mom"))
	p.mu.Lock()
	p.update()
	p.mu.Unlock()
	if got := filesOf(t, p, id); !slices.Equal(got, sorted(mom...)) {
		t.Errorf("Mom is in %v, want %v", got, sorted(mom...))
	}

	// The names are in people.json, and found again by a server started over.
	again := f.open()
	if got := filesOf(t, again, id); !slices.Equal(got, sorted(mom...)) {
		t.Errorf("after a restart Mom is in %v, want %v", got, sorted(mom...))
	}
	if again.list(all, true)[0].Name != "Mom" {
		t.Error("the name was not kept")
	}

	// Renaming keeps the ID; a group given her name (ignoring case) joins her.
	if renamed, err := p.name(id, "Mother"); err != nil || renamed != id {
		t.Fatalf("rename: %s, %v", renamed, err)
	}
	dadGroup := p.list(all, true)[1].ID
	if joined, err := p.name(dadGroup, "mother"); err != nil || joined != id {
		t.Fatalf("a group named like a person: %s, %v", joined, err)
	}
	if list := p.list(all, true); len(list) != 1 || list[0].Files != 8 {
		t.Errorf("after joining: %+v", list)
	}
	if _, err := p.name("g-nothing", "X"); err == nil {
		t.Error("named a group that isn't there")
	}
	if _, err := p.name(id, " "); err == nil {
		t.Error("an empty name was taken")
	}
}

func TestAFaceMovedAwayStaysAway(t *testing.T) {
	f := newFixture(t)
	var mom []string
	for range 5 {
		mom = append(mom, f.photo("mom"))
	}
	p := f.open()
	id, err := p.name(p.list(all, true)[0].ID, "Mom")
	if err != nil {
		t.Fatal(err)
	}
	wrong := FaceID{mom[2], 0}
	if err := p.move(wrong, ""); err != nil {
		t.Fatal(err)
	}
	if got := filesOf(t, p, id); slices.Contains(got, mom[2]) || len(got) != 4 {
		t.Errorf("Mom is still in %v", got)
	}
	// Not taken for her again when the faces are put together again, or after a restart.
	if got := filesOf(t, f.open(), id); slices.Contains(got, mom[2]) {
		t.Errorf("after a restart Mom is in %v", got)
	}

	// Given to another person, it is theirs.
	f.photo("dad")
	f.photo("dad")
	f.photo("dad")
	var dadGroup string
	for _, person := range p.list(all, true) {
		if person.Name == "" {
			dadGroup = person.ID
		}
	}
	dad, err := p.name(dadGroup, "Dad")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.move(wrong, dad); err != nil {
		t.Fatal(err)
	}
	if got := filesOf(t, p, dad); !slices.Contains(got, mom[2]) {
		t.Errorf("Dad is in %v, without the face given to him", got)
	}
	if err := p.move(FaceID{mom[0], 5}, ""); err == nil {
		t.Error("moved a face that isn't there")
	}
	if err := p.move(wrong, "p-nobody"); err == nil {
		t.Error("moved a face to someone who isn't there")
	}
}

func TestMergingAndForgettingPeople(t *testing.T) {
	f := newFixture(t)
	for range 3 {
		f.photo("a")
		f.photo("b")
		f.photo("c")
	}
	p := f.open()
	list := p.list(all, true)
	if len(list) != 3 {
		t.Fatalf("got %+v", list)
	}
	a, err := p.name(list[0].ID, "A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.name(list[1].ID, "B")
	if err != nil {
		t.Fatal(err)
	}
	var c string
	for _, person := range p.list(all, true) {
		if person.Name == "" {
			c = person.ID
		}
	}
	if err := p.merge(c, []string{a}); err == nil {
		t.Error("merged into a group no one named")
	}
	if err := p.merge(a, []string{b, c}); err != nil {
		t.Fatal(err)
	}
	list = p.list(all, true)
	if len(list) != 1 || list[0].ID != a || list[0].Files != 9 {
		t.Fatalf("after merging: %+v", list)
	}
	if err := p.forget(a); err != nil {
		t.Fatal(err)
	}
	list = p.list(all, true)
	if len(list) != 3 || list[0].Name != "" {
		t.Errorf("after forgetting, the faces are in groups again: %+v", list)
	}
	if err := p.forget(a); err == nil {
		t.Error("forgot someone twice")
	}
}

func TestListCountsOnlyFilesKept(t *testing.T) {
	f := newFixture(t)
	var mom []string
	for range 4 {
		mom = append(mom, f.photo("mom"))
	}
	p := f.open()
	hidden := func(n int) func(string) bool {
		return func(sha string) bool { return !slices.Contains(mom[:n], sha) }
	}
	if list := p.list(hidden(1), true); len(list) != 1 || list[0].Files != 3 || list[0].Cover.Sha256 == mom[0] {
		t.Errorf("one file hidden: %+v", list)
	}
	// A group left with fewer than MinGroup faces is not shown; a named person is, while they have any.
	if list := p.list(hidden(2), true); len(list) != 0 {
		t.Errorf("a group of 2: %+v", list)
	}
	id, _ := p.name(p.list(all, true)[0].ID, "Mom")
	if list := p.list(hidden(3), true); len(list) != 1 || list[0].ID != id {
		t.Errorf("Mom with one file shown: %+v", list)
	}
	if list := p.list(hidden(4), true); len(list) != 0 {
		t.Errorf("Mom with no file shown: %+v", list)
	}
}

func TestSimilarFindsTheSamePersonFirst(t *testing.T) {
	f := newFixture(t)
	var mom []string
	for range 5 {
		mom = append(mom, f.photo("mom"))
		f.photo("")
		f.photo("dad")
	}
	p := f.open()
	hits, err := p.similar(FaceID{mom[0], 0}, 4, all)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hits {
		got = append(got, h.ID.Sha256)
	}
	if !slices.Equal(sorted(got...), sorted(mom[1:]...)) {
		t.Errorf("the 4 most like Mom's face: %v, want her other photos %v", got, mom[1:])
	}
	if _, err := p.similar(FaceID{mom[0], 3}, 4, all); err == nil {
		t.Error("found faces like one that isn't there")
	}
}

// Faces the detector is unsure of, or too small, are not put in groups.
func TestUnclearFacesAreLeftAlone(t *testing.T) {
	f := newFixture(t)
	for range 3 {
		f.photo("mom")
	}
	sha := f.photo()
	weak := f.faceOf("mom")
	weak.Score = 0.7
	tiny := f.faceOf("mom")
	tiny.Box[2] = 0.01
	f.index.Add(sha, []Face{weak, tiny})
	p := f.open()
	if list := p.list(all, true); len(list) != 1 || list[0].Faces != 3 {
		t.Errorf("got %+v", list)
	}
	if infos := p.facesIn(sha); len(infos) != 2 || infos[0].Person != "" || infos[1].Person != "" {
		t.Errorf("unclear faces: %+v", infos)
	}
}

func TestFaceIDs(t *testing.T) {
	sha := "aa0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"
	id, err := ParseFaceID(sha + ":3")
	if err != nil || id != (FaceID{sha, 3}) || id.String() != sha+":3" {
		t.Errorf("got %v, %v", id, err)
	}
	for _, bad := range []string{"", sha, sha + ":", sha + ":-1", "abc:1"} {
		if _, err := ParseFaceID(bad); err == nil {
			t.Errorf("%q was taken for a face", bad)
		}
	}
}

// BenchmarkRebuild puts a library's faces together: 2000 photos, four in five of one of 50 people, the others of
// strangers. (10000 photos of 100 people took 0.44 s on the owner's PC, Oct 23, 2026.)
func BenchmarkRebuild(b *testing.B) {
	f := newFixture(b)
	for i := range 2000 {
		who := ""
		if i%5 != 0 {
			who = fmt.Sprintf("p%d", f.rng.IntN(50))
		}
		f.photo(who)
	}
	p := f.open()
	b.ResetTimer()
	for range b.N {
		p.built = false
		p.mu.Lock()
		p.update()
		p.mu.Unlock()
	}
	b.StopTimer()
	b.ReportMetric(float64(len(p.list(all, true))), "groups")
}
