package main

import (
	"context"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/reading"
)

// startIndexProcess runs journal-server index for one user (runIndexProcess); tests put a fake in its place.
var startIndexProcess = func(ctx context.Context, user string, onProgress func(reading.Progress)) error {
	return runIndexProcess(ctx, onProgress, "--user", user, "--progress")
}

// dailyRuns runs journal-server index for one user at a time, with "index": "daily": every user's at "index_at"
// (indexDaily), and a user's own when a phone asks (startIndexing). The runs wait in a queue, so only one process,
// with one set of models, is ever in memory. A phone pauses its user's run: the process finishes the file in hand and
// exits, which frees its models, and the next user's run starts. Each user's run, progress and pause are their own:
// a phone sees only its user's.
type dailyRuns struct {
	ctx     context.Context
	at      string // config.IndexAt
	refresh func() // reads what a run added to the caches
	now     func() time.Time

	mu      sync.Mutex
	queue   []queuedRun
	current *dailyRun
	paused  map[string]pausedRun
	last    map[string]*model.IndexingRun
	// Files read before a pause, by user: added to the next run's progress, so its bar goes on from there.
	carried map[string]int
}

type queuedRun struct {
	user         string
	nightly      bool
	resumedTotal int
}

type dailyRun struct {
	queuedRun
	cancel    context.CancelFunc
	startedAt time.Time
	progress  reading.Progress
	pausing   bool
	// A resumed run's total before its process says how many are left, so its bar doesn't jump back.
	resumedTotal int
}

type pausedRun struct {
	nightly     bool
	done, total int
	indexes     []reading.IndexCount
	startedAt   time.Time
}

func newDailyRuns(ctx context.Context, config auth.AppConfig, refresh func()) *dailyRuns {
	return &dailyRuns{ctx: ctx, at: config.IndexAt, refresh: refresh, now: time.Now,
		paused: map[string]pausedRun{}, last: map[string]*model.IndexingRun{}, carried: map[string]int{}}
}

// indexDaily queues every user's run at "index_at" each day, and every 10 minutes reads what a run started by hand
// (journal-server index in a terminal) added. When ctx ends a run in hand is stopped (it finishes the file it is
// reading, up to 30 s), and indexDaily returns once it has.
func indexDaily(ctx context.Context, runs *dailyRuns, users []string) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Minute):
				runs.refresh()
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			runs.wait()
			return
		case <-time.After(time.Until(nextRun(runs.at, runs.now()))):
		}
		runs.nightly(users)
	}
}

// nightly queues the run of every user not already running or waiting; a paused run starts again with it.
func (d *dailyRuns) nightly(users []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, user := range users {
		if d.current != nil && d.current.user == user || d.queuedAt(user) >= 0 {
			continue
		}
		delete(d.paused, user)
		delete(d.carried, user)
		d.queue = append(d.queue, queuedRun{user: user, nightly: true})
	}
	d.next()
}

// Start queues user's run (a paused one goes on), and starts it when nothing else runs.
func (d *dailyRuns) Start(user string) (*model.Indexing, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	running := d.current != nil && d.current.user == user && !d.current.pausing
	if !running && d.queuedAt(user) < 0 {
		p := d.paused[user]
		delete(d.paused, user)
		d.queue = append(d.queue, queuedRun{user: user, nightly: p.nightly, resumedTotal: p.total})
		d.next()
	}
	return d.status(user), nil
}

// Pause stops user's run after the file in hand, or takes it off the queue.
func (d *dailyRuns) Pause(user string) (*model.Indexing, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.current; c != nil && c.user == user && !c.pausing {
		c.pausing = true
		c.cancel()
		d.paused[user] = pausedRun{nightly: c.nightly, done: d.carried[user] + c.progress.Done,
			total: d.carried[user] + c.progress.Total, indexes: c.progress.Indexes, startedAt: c.startedAt}
	} else if i := d.queuedAt(user); i >= 0 {
		d.paused[user] = pausedRun{nightly: d.queue[i].nightly, done: d.carried[user], total: d.carried[user]}
		d.queue = slices.Delete(d.queue, i, i+1)
	}
	return d.status(user), nil
}

// Status is user's run as the indexing field shows it.
func (d *dailyRuns) Status(user string) *model.Indexing {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status(user)
}

func (d *dailyRuns) status(user string) *model.Indexing {
	next := nextRun(d.at, d.now())
	s := &model.Indexing{State: model.IndexingStateIdle, LastRun: d.last[user], NextNightly: &next,
		Indexes: []*model.IndexingCount{}}
	if p, ok := d.paused[user]; ok {
		s.State, s.Nightly, s.Done, s.Total, s.Indexes = model.IndexingStatePaused, p.nightly, p.done, p.total, counts(p.indexes)
		if !p.startedAt.IsZero() {
			s.StartedAt = &p.startedAt
		}
		return s
	}
	if c := d.current; c != nil && c.user == user {
		carried := d.carried[user]
		s.State, s.Nightly, s.StartedAt = model.IndexingStateRunning, c.nightly, &c.startedAt
		s.Done, s.Total, s.Indexes = carried+c.progress.Done, carried+c.progress.Total, counts(c.progress.Indexes)
		if c.progress.Total == 0 && c.resumedTotal > s.Total {
			s.Total = c.resumedTotal
		}
		return s
	}
	if i := d.queuedAt(user); i >= 0 {
		ahead := i
		if d.current != nil {
			ahead++
		}
		s.State, s.Nightly, s.QueuedBehind = model.IndexingStateQueued, d.queue[i].nightly, &ahead
		s.Done, s.Total = d.carried[user], d.carried[user]
	}
	return s
}

func (d *dailyRuns) queuedAt(user string) int {
	return slices.IndexFunc(d.queue, func(q queuedRun) bool { return q.user == user })
}

// next starts the first queued run when none is in hand. With d.mu held.
func (d *dailyRuns) next() {
	if d.current != nil || len(d.queue) == 0 || d.ctx.Err() != nil {
		return
	}
	q := d.queue[0]
	d.queue = d.queue[1:]
	ctx, cancel := context.WithCancel(d.ctx)
	run := &dailyRun{queuedRun: q, cancel: cancel, startedAt: d.now(), resumedTotal: q.resumedTotal}
	d.current = run
	go d.run(ctx, run)
}

// run runs one user's process to its end, keeps how it ended, and starts the next.
func (d *dailyRuns) run(ctx context.Context, run *dailyRun) {
	err := startIndexProcess(ctx, run.user, func(p reading.Progress) {
		d.mu.Lock()
		run.progress = p
		d.mu.Unlock()
	})
	run.cancel()
	d.refresh()
	d.mu.Lock()
	defer d.mu.Unlock()
	carried := d.carried[run.user]
	failed := 0
	for _, c := range run.progress.Indexes {
		failed += c.Failed
	}
	last := &model.IndexingRun{StartedAt: run.startedAt, FinishedAt: d.now(), Files: carried + run.progress.Done,
		Failed: failed, Stopped: run.pausing || run.progress.Done < run.progress.Total || d.ctx.Err() != nil}
	if err != nil {
		msg := err.Error()
		last.Error, last.Stopped = &msg, true
	}
	d.last[run.user] = last
	if run.pausing {
		d.carried[run.user] = last.Files
		// The process finished the files in hand after the pause: the paused run shows where it really stopped.
		if p, ok := d.paused[run.user]; ok {
			p.done, p.total, p.indexes = last.Files, carried+run.progress.Total, run.progress.Indexes
			d.paused[run.user] = p
		}
	} else {
		delete(d.carried, run.user)
	}
	d.current = nil
	d.next()
	if err != nil {
		log.Printf("Indexing: %s's run: %v", run.user, err)
	}
}

// wait waits for the run in hand (stopped by d.ctx) to end, up to the 30 s a process is given.
func (d *dailyRuns) wait() {
	for deadline := time.Now().Add(31 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		d.mu.Lock()
		idle := d.current == nil
		d.mu.Unlock()
		if idle {
			return
		}
	}
}

// readerRuns is the indexing fields with "index": "server": the server's own reader reads every user's files as
// they come (reading.Reader.Run), and a phone pauses or wakes its user's reading. The models stay in memory, since
// new uploads are read with them.
type readerRuns struct{ reader *reading.Reader }

func (r readerRuns) Status(user string) *model.Indexing {
	s := &model.Indexing{State: model.IndexingStateIdle, Indexes: []*model.IndexingCount{}}
	if l, ok := r.reader.LastRun(user); ok {
		s.LastRun = &model.IndexingRun{StartedAt: l.StartedAt, FinishedAt: l.FinishedAt, Files: l.Files,
			Failed: l.Failed, Stopped: l.Stopped}
	}
	if p, ok := r.reader.Progress(user); ok {
		s.State, s.Done, s.Total, s.StartedAt, s.Indexes = model.IndexingStateRunning, p.Done, p.Total, &p.StartedAt, counts(p.Indexes)
	}
	if r.reader.Paused(user) {
		s.State = model.IndexingStatePaused
		if l, ok := r.reader.LastRun(user); ok && l.Stopped {
			s.Done, s.Total, s.StartedAt = l.Files, l.Total, &l.StartedAt
		}
	}
	return s
}

func (r readerRuns) Start(user string) (*model.Indexing, error) {
	r.reader.Resume(user)
	return r.Status(user), nil
}

func (r readerRuns) Pause(user string) (*model.Indexing, error) {
	r.reader.Pause(user)
	return r.Status(user), nil
}

func counts(in []reading.IndexCount) []*model.IndexingCount {
	out := make([]*model.IndexingCount, 0, len(in))
	for _, c := range in {
		out = append(out, &model.IndexingCount{Name: c.Name, Read: c.Read, Failed: c.Failed})
	}
	return out
}
