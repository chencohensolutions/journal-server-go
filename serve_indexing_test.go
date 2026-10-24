package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/reading"
)

// fakeIndexProcesses stands in for journal-server index: each run reports that it read 3 files of what is left of
// 10, then waits to be stopped.
type fakeIndexProcesses struct {
	mu   sync.Mutex
	runs []string
}

func (f *fakeIndexProcesses) install(t *testing.T) {
	t.Helper()
	before := startIndexProcess
	t.Cleanup(func() { startIndexProcess = before })
	startIndexProcess = func(ctx context.Context, user string, onProgress func(reading.Progress)) error {
		f.mu.Lock()
		f.runs = append(f.runs, user)
		left := 10
		for _, u := range f.runs[:len(f.runs)-1] {
			if u == user {
				left -= 3
			}
		}
		f.mu.Unlock()
		onProgress(reading.Progress{User: user, Done: 3, Total: left, StartedAt: time.Now(),
			Indexes: []reading.IndexCount{{Name: "search", Read: 3}}})
		<-ctx.Done()
		return nil
	}
}

const indexingFields = `state nightly done total queuedBehind indexes { name read failed } lastRun { files stopped error } nextNightly`

type indexingResult struct {
	State        string
	Nightly      bool
	Done, Total  int
	QueuedBehind *int
	Indexes      []struct {
		Name         string
		Read, Failed int
	}
	LastRun *struct {
		Files   int
		Stopped bool
		Error   *string
	}
	NextNightly *time.Time
}

func indexing(t *testing.T, env *testEnv, phone *http.Client, field string) indexingResult {
	t.Helper()
	op := "query"
	if field != "indexing" {
		op = "mutation"
	}
	resp := env.graphql(t, phone, op+` { `+field+` { `+indexingFields+` } }`, nil)
	if len(resp.Errors) > 0 {
		t.Fatalf("%s: %v", field, resp.Errors)
	}
	var out indexingResult
	if err := json.Unmarshal(resp.Data[field], &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func waitForIndexing(t *testing.T, env *testEnv, phone *http.Client, ok func(indexingResult) bool) indexingResult {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		s := indexing(t, env, phone, "indexing")
		if ok(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("indexing never got there: %+v", s)
		}
	}
}

// With "index": "daily" a phone starts its user's run, follows it, and pauses and resumes it. Another user's run waits
// its turn, shows nothing of the first, and pausing it leaves the first alone.
func TestAPhoneStartsFollowsAndPausesItsUsersRun(t *testing.T) {
	var processes fakeIndexProcesses
	processes.install(t)
	env := newTestEnv(t, func(c *auth.AppConfig) {
		c.Index = auth.IndexDaily
		c.Users = append(c.Users, auth.UserConfig{Username: "dana", DataFolder: filepath.Join(t.TempDir(), "dana")})
	})
	hen, dana := env.enrolledClient(t), env.enrolledClientOf(t, "dana")

	resp := env.graphql(t, hen, `{ serverInfo { indexing } }`, nil)
	if string(resp.Data["serverInfo"]) != `{"indexing":true}` {
		t.Fatalf("serverInfo: %s %v", resp.Data["serverInfo"], resp.Errors)
	}
	if s := indexing(t, env, hen, "indexing"); s.State != "IDLE" || s.NextNightly == nil || s.LastRun != nil {
		t.Fatalf("before any run: %+v", s)
	}

	indexing(t, env, hen, "startIndexing")
	s := waitForIndexing(t, env, hen, func(s indexingResult) bool { return s.Done == 3 })
	if s.State != "RUNNING" || s.Nightly || s.Total != 10 || len(s.Indexes) != 1 || s.Indexes[0].Read != 3 {
		t.Fatalf("running: %+v", s)
	}

	if s := indexing(t, env, dana, "startIndexing"); s.State != "QUEUED" || s.QueuedBehind == nil || *s.QueuedBehind != 1 ||
		s.Done != 0 || s.Total != 0 || len(s.Indexes) != 0 {
		t.Fatalf("dana's run while hen's runs: %+v", s)
	}
	if s := indexing(t, env, dana, "pauseIndexing"); s.State != "PAUSED" {
		t.Fatalf("dana paused: %+v", s)
	}
	if s := indexing(t, env, hen, "indexing"); s.State != "RUNNING" {
		t.Fatalf("pausing dana's run changed hen's: %+v", s)
	}

	if s := indexing(t, env, hen, "pauseIndexing"); s.State != "PAUSED" || s.Done != 3 || s.Total != 10 {
		t.Fatalf("hen paused: %+v", s)
	}
	s = waitForIndexing(t, env, hen, func(s indexingResult) bool { return s.LastRun != nil })
	if s.State != "PAUSED" || !s.LastRun.Stopped || s.LastRun.Files != 3 {
		t.Fatalf("after hen's process ended: %+v", s)
	}
	processes.mu.Lock()
	runs := len(processes.runs)
	processes.mu.Unlock()
	if runs != 1 {
		t.Fatalf("%d processes ran, want hen's alone (dana's is paused)", runs)
	}

	// Resumed, the bar goes on from where it was: 3 before the pause and 3 of the 7 left.
	indexing(t, env, hen, "startIndexing")
	s = waitForIndexing(t, env, hen, func(s indexingResult) bool { return s.State == "RUNNING" && s.Done == 6 })
	if s.Total != 10 {
		t.Fatalf("resumed: %+v", s)
	}
	if s := indexing(t, env, dana, "indexing"); s.State != "PAUSED" || s.Done != 0 {
		t.Fatalf("dana's run, seen by dana: %+v", s)
	}
}

// Without a model there is no run to start: serverInfo says so, and the fields answer a coded error.
func TestIndexingIsOffWithoutAModel(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	resp := env.graphql(t, phone, `{ serverInfo { indexing } }`, nil)
	if string(resp.Data["serverInfo"]) != `{"indexing":false}` {
		t.Fatalf("serverInfo: %s %v", resp.Data["serverInfo"], resp.Errors)
	}
	resp = env.graphql(t, phone, `mutation { startIndexing { state } }`, nil)
	if len(resp.Errors) == 0 || resp.Errors[0].Extensions["code"] != "INDEXING_OFF" {
		t.Fatalf("startIndexing without a model: %+v", resp.Errors)
	}
}

// The progress lines journal-server index --progress writes come back as they were; other output goes to the log.
func TestProgressLinesRoundTrip(t *testing.T) {
	var got []reading.Progress
	pw := &progressWriter{onProgress: func(p reading.Progress) { got = append(got, p) }}
	write := writeProgress(pw, time.Hour)
	write(reading.Progress{User: "hen", Done: 0, Total: 3})
	write(reading.Progress{User: "hen", Done: 1, Total: 3}) // within the hour: left out
	write(reading.Progress{User: "hen", Done: 3, Total: 3, Ended: true, Indexes: []reading.IndexCount{{Name: "faces", Read: 2, Failed: 1}}})
	pw.Write([]byte("index: done in 1s\n"))
	if len(got) != 2 || got[0].Done != 0 || got[1].Done != 3 || got[1].Indexes[0].Failed != 1 {
		t.Fatalf("progress read back: %+v", got)
	}
	if args, err := parseIndexArgs([]string{"--user", "hen", "--progress", "--only", "faces"}); err != nil ||
		args.user != "hen" || !args.progress || !args.only["faces"] {
		t.Fatalf("parseIndexArgs: %+v %v", args, err)
	}
	if _, err := parseIndexArgs([]string{"--user"}); err == nil {
		t.Fatal("--user without a name was taken")
	}
}

// The nightly run queues every user's run; pausing one ends its process and starts the next user's, and resuming
// it keeps it nightly.
func TestTheNightlyRunGoesOneUserAtATime(t *testing.T) {
	var processes fakeIndexProcesses
	processes.install(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs := newDailyRuns(ctx, auth.AppConfig{IndexAt: "03:00"}, func() {})
	runs.nightly([]string{"hen", "dana"})
	if s := runs.Status("hen"); s.State != "RUNNING" || !s.Nightly {
		t.Fatalf("hen: %+v", s)
	}
	if s := runs.Status("dana"); s.State != "QUEUED" || !s.Nightly || *s.QueuedBehind != 1 {
		t.Fatalf("dana: %+v", s)
	}
	runs.Pause("hen")
	for deadline := time.Now().Add(5 * time.Second); runs.Status("dana").State != "RUNNING"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("dana's run didn't start after hen's was paused: %+v", runs.Status("dana"))
		}
	}
	if s, _ := runs.Start("hen"); s.State != "QUEUED" || !s.Nightly {
		t.Fatalf("hen resumed: %+v", s)
	}
}
