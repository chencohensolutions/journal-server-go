package enroll

import (
	"sync"
	"time"
)

const (
	maxFailures   = 5
	failureWindow = time.Minute
)

// failureLimiter blocks a client IP after maxFailures wrong codes within failureWindow.
type failureLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
	now      func() time.Time
}

func newFailureLimiter(now func() time.Time) *failureLimiter {
	return &failureLimiter{failures: map[string][]time.Time{}, now: now}
}

func (l *failureLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(ip)) < maxFailures
}

func (l *failureLimiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[ip] = append(l.recent(ip), l.now())
}

// recent drops failures older than the window. Callers hold mu.
func (l *failureLimiter) recent(ip string) []time.Time {
	cutoff := l.now().Add(-failureWindow)
	times := l.failures[ip]
	kept := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, ip)
		return nil
	}
	l.failures[ip] = kept
	return kept
}
