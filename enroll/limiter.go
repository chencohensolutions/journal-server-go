package enroll

import (
	"net/netip"
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

// Limiter blocks a client IP after a few wrong codes in a minute; peer invites use it too.
type Limiter = failureLimiter

func NewLimiter(now func() time.Time) *Limiter { return newFailureLimiter(now) }

func newFailureLimiter(now func() time.Time) *failureLimiter {
	return &failureLimiter{failures: map[string][]time.Time{}, now: now}
}

func (l *failureLimiter) Allow(ip string) bool {
	ip = limiterKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(ip)) < maxFailures
}

func (l *failureLimiter) Fail(ip string) {
	ip = limiterKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.failures) >= maxLimiterEntries {
		// Many addresses failing at once: drop those whose minute has passed, so the map can't grow without end.
		for other := range l.failures {
			l.recent(other)
		}
	}
	l.failures[ip] = append(l.recent(ip), l.now())
}

// maxLimiterEntries is how many addresses the limiter remembers before it clears out the old ones.
const maxLimiterEntries = 10000

// limiterKey is what failures are counted by: an IPv4 address, or an IPv6 address's /64, since one client has a
// whole /64 (or more) of addresses to move between.
func limiterKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return addr.String()
	}
	return prefix.String()
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
