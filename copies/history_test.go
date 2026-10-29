package copies

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/auth"
)

// clock is a copier's time, moved by the test.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// Each pass that replaces a changed file keeps the copy's version in history/; restore --as-of brings back the version
// of a day, on a disk and in an encrypted copy alike.
func TestPreviousVersionsComeBackAsOfADay(t *testing.T) {
	h := newTestHost(t)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 3, 30, 0, 0, time.UTC) }
	for _, kind := range []string{"folder", "encrypted"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := newCopier(t)
			addPhoto(t, store, "IMG_1.jpg", "a photo")
			people := filepath.Join(store.Root(), "people.json")
			at := &clock{day(20)}
			var c *Copier
			var key *KeyFile
			var src Target
			if kind == "folder" {
				folder := t.TempDir()
				c = NewTo("hen", store, auth.CopyConfig{Folder: folder}, nil, at.now)
				src = Folder(folder)
			} else {
				k, identity, _ := NewCopyKey("hen", time.Now())
				e, err := OpenEncrypted(h.peer(t, h.enroll(t, "hen-"+t.Name()[len(t.Name())-4:], 1<<30), "hen"), k, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				c = NewTo("hen", store, auth.CopyConfig{Peer: "dana", Encrypt: true}, e, at.now)
				src, key = e.Inner, ptr(keyFileOf(t, k, identity))
			}
			sidecar := c.source(c.items()[0].Path) + ".json"
			good, _ := os.ReadFile(sidecar)
			write := func(file, text string, d int) {
				os.WriteFile(file, []byte(text), 0o644)
				os.Chtimes(file, day(d), day(d))
			}
			// Oct 20: the first pass. Oct 22: people.json changed. Oct 25: the sidecar damaged, people.json again.
			write(people, `{"v":1,"names":["Noa"]}`, 20)
			if pass := c.Run(context.Background()); pass.Err != nil {
				t.Fatal(pass.Err)
			}
			at.t = day(22)
			write(people, `{"v":1,"names":["Noa","Dan"]}`, 22)
			c.Run(context.Background())
			at.t = day(25)
			write(people, `{"v":1,"names":["Noa","Dan","Tal"]}`, 25)
			damaged := strings.Replace(string(good), `"sha256"`, `"sha256" `, 1) // the same length would do too
			if damaged == string(good) {
				t.Fatal("the sidecar wasn't changed")
			}
			write(sidecar, damaged, 25)
			c.Run(context.Background())

			restore := func(asOf time.Time) (string, string) {
				t.Helper()
				to := filepath.Join(t.TempDir(), "restored")
				if result, err := RestoreAsOf(context.Background(), src, key, to, asOf, nil); err != nil || result.Failed != 0 {
					t.Fatalf("as of %v: %+v %v", asOf, result, err)
				}
				p, _ := os.ReadFile(filepath.Join(to, "people.json"))
				rel, _ := filepath.Rel(store.Root(), sidecar)
				s, _ := os.ReadFile(filepath.Join(to, rel))
				return string(p), string(s)
			}
			if p, s := restore(time.Time{}); !strings.Contains(p, "Tal") || s != damaged {
				t.Errorf("now: %s, sidecar damaged %v", p, s == damaged)
			}
			if p, s := restore(day(23)); !strings.Contains(p, "Dan") || strings.Contains(p, "Tal") || s != string(good) {
				t.Errorf("as of Oct 23: %s, sidecar good %v", p, s == string(good))
			}
			if p, _ := restore(day(21)); strings.Contains(p, "Dan") || !strings.Contains(p, "Noa") {
				t.Errorf("as of Oct 21: %s", p)
			}
		})
	}
}
