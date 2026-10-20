package media

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyHashesTheOriginalAgain(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	good := f.upload([]byte("a photo that stays intact"), "GOOD.jpg")
	damaged := f.upload([]byte("a photo that rots on disk"), "BAD.jpg")
	gone := f.upload([]byte("a photo someone deleted"), "GONE.jpg")
	shortened := f.upload([]byte("a photo that gets cut short"), "SHORT.jpg")

	path := func(item Item) string { return filepath.Join(f.root, filepath.FromSlash(item.Path)) }
	if err := os.WriteFile(path(damaged), []byte("a photo that rots on dusk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path(gone)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path(shortened), []byte("a photo"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		sha  string
		want Verdict
	}{
		{good.Sha256(), VerifyOK},
		{damaged.Sha256(), VerifyMismatch},
		{gone.Sha256(), VerifyMissing},
		{shortened.Sha256(), VerifyMismatch},
		{shaOf([]byte("never uploaded")), VerifyMissing},
	} {
		got, err := f.store.Verify(ctx, c.sha)
		if err != nil || got != c.want {
			t.Errorf("Verify(%s) = %q, %v; want %q", c.sha[:8], got, err, c.want)
		}
	}

	if err := os.Remove(path(good) + ".json"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.store.Verify(ctx, good.Sha256()); got != VerifyMissing {
		t.Errorf("without its sidecar: %q", got)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.store.Verify(cancelled, shortened.Sha256()); err == nil {
		t.Error("a cancelled check still answered")
	}
}

func TestVerifyHandler(t *testing.T) {
	f := newFixture(t)
	item := f.upload([]byte("a photo"), "IMG.jpg")
	mux := http.NewServeMux()
	mux.Handle("POST /media/{sha256}/verify", VerifyHandler(func(*http.Request) *Store { return f.store }))

	post := func(sha string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/media/"+sha+"/verify", nil))
		return w
	}
	for sha, want := range map[string]string{item.Sha256(): "ok", shaOf([]byte("other")): "missing"} {
		w := post(sha)
		var body map[string]string
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["result"] != want || body["sha256"] != sha {
			t.Errorf("%s: %d %s; want %q", sha[:8], w.Code, w.Body, want)
		}
	}
	if w := post("not-a-sha"); w.Code != http.StatusBadRequest {
		t.Errorf("bad sha: %d", w.Code)
	}
}
