package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
)

// A realtime copy gets an upload a few seconds after it lands; copy report and copy (startCopy) go through the
// admin API.
func TestCopiesFollowUploadsAndReport(t *testing.T) {
	copies.RealtimeDelay = 10 * time.Millisecond
	folder := t.TempDir()
	env := newTestEnv(t, func(c *auth.AppConfig) {
		c.Users[0].Copies = []auth.CopyConfig{{Folder: folder, At: "03:30", Realtime: true}}
	})
	copiers := newCopiers(env.config, env.library)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startCopies(ctx, env.library, copiers)

	client := env.enrolledClient(t)
	env.uploadPhoto(t, client, []byte("a photo for the copy"), "IMG_1.jpg")
	var copied string
	deadline := time.Now().Add(5 * time.Second)
	for copied == "" {
		filepath.WalkDir(filepath.Join(folder, "media"), func(path string, d os.DirEntry, err error) error {
			if err == nil && d.Name() == "IMG_1.jpg.json" {
				copied = path
			}
			return nil
		})
		if time.Now().After(deadline) {
			t.Fatal("the upload wasn't copied")
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err := os.ReadFile(strings.TrimSuffix(copied, ".json"))
	if err != nil || string(data) != "a photo for the copy" {
		t.Fatalf("the copied original: %q %v", data, err)
	}

	admin := httptest.NewServer(newAdminMux(env.config, env.enrollment, "secret", env.refreshIndexes, copiers,
		func(c *copies.Copier) { runCopy(ctx, c) }))
	defer admin.Close()
	tokenFile := filepath.Join(t.TempDir(), "admin.token")
	os.WriteFile(tokenFile, []byte("secret\n"), 0o600)
	config := env.config
	config.AdminAddr = strings.TrimPrefix(admin.URL, "http://")
	config.AdminTokenFile = tokenFile

	report := func() adminCopy {
		t.Helper()
		var data struct {
			CopyReport []adminCopy `json:"copyReport"`
		}
		if err := adminQuery(config, `{ copyReport(user: "hen", paths: true) { `+copyFields+` paths } }`, nil, &data); err != nil {
			t.Fatal(err)
		}
		if len(data.CopyReport) != 1 {
			t.Fatalf("copyReport: %+v", data.CopyReport)
		}
		return data.CopyReport[0]
	}
	if r := report(); r.Originals != 0 || r.Sidecars != 0 || r.Folder != folder || !r.Realtime || len(r.Paths) != 0 {
		t.Errorf("report after the realtime copy: %+v", r)
	}

	// A file the realtime copy didn't get (its hook off) shows in the report, and copy (startCopy) copies it.
	env.library.Store("hen").OnWrite(nil)
	env.uploadPhoto(t, client, []byte("a second photo"), "IMG_2.jpg")
	if r := report(); r.Originals != 1 || r.Sidecars != 1 || len(r.Paths) != 2 {
		t.Errorf("report with a file not copied: %+v", r)
	}
	var started struct {
		StartCopy []adminCopy `json:"startCopy"`
	}
	if err := adminQuery(config, `mutation { startCopy(user: "hen") { user folder } }`, nil, &started); err != nil || len(started.StartCopy) != 1 {
		t.Fatalf("startCopy: %+v %v", started, err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		r := report()
		if r.Originals == 0 && r.Sidecars == 0 && r.LastPass != nil && r.Running == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pass didn't copy the file: %+v", r)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := adminQuery(config, `mutation { startCopy(user: "nobody") { user } }`, nil, &started); err == nil {
		t.Error("startCopy of a user without copies")
	}
}

func TestConfigRefusesACopyInsideTheDataFolder(t *testing.T) {
	dir := t.TempDir()
	write := func(copies string) error {
		path := filepath.Join(dir, "config.json")
		os.WriteFile(path, []byte(`{"users": [{"username": "hen", "data_folder": "`+filepath.ToSlash(filepath.Join(dir, "hen"))+
			`", "copies": [`+copies+`]}]}`), 0o644)
		_, err := auth.LoadConfig(path)
		return err
	}
	inside := filepath.ToSlash(filepath.Join(dir, "hen", "copy"))
	if err := write(`{"folder": "` + inside + `"}`); err == nil {
		t.Error("a copy inside the data folder was accepted")
	}
	if err := write(`{"folder": "` + filepath.ToSlash(dir) + `"}`); err == nil {
		t.Error("a copy holding the data folder was accepted")
	}
	if err := write(`{"folder": "` + filepath.ToSlash(filepath.Join(dir, "copy")) + `", "at": "25:00"}`); err == nil {
		t.Error("a copy at 25:00 was accepted")
	}
	if err := write(`{"folder": "` + filepath.ToSlash(filepath.Join(dir, "copy")) + `"}`); err != nil {
		t.Errorf("a good copy: %v", err)
	}
	config, _ := auth.LoadConfig(filepath.Join(dir, "config.json"))
	if at := config.Users[0].Copies[0].At; at != auth.DefaultCopyAt {
		t.Errorf("default at %q", at)
	}
}
