package local

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// RateLimiter is a simple fixed-window limiter keyed by an arbitrary
// string (this package always keys it by remote IP, see remoteIP below):
// at most max attempts inside window, per key, before further attempts
// are refused until the oldest one in the window ages out.
// §3.6/§13A require SOME brute-force/rate-limit protection on
// login/enrollment, not a specific algorithm, and this is deliberately
// the simplest thing that satisfies it without an external store: a
// single administrator's login/enroll traffic is never high-QPS.
type RateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	hits   map[string][]time.Time
}

// NewRateLimiter returns a limiter allowing at most max attempts per key
// within window.
func NewRateLimiter(max int, window time.Duration) *RateLimiter {
	return &RateLimiter{max: max, window: window, now: time.Now, hits: map[string][]time.Time{}}
}

// Allow records one attempt for key at the current time and reports
// whether it is within the limit. A refused attempt (Allow returning
// false) is NOT counted as a fresh hit itself, so a caller hammering an
// already-limited key doesn't reset its own window by attempting again.
func (r *RateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	cutoff := now.Add(-r.window)

	kept := r.hits[key][:0]
	for _, t := range r.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= r.max {
		r.hits[key] = kept
		return false
	}

	r.hits[key] = append(kept, now)
	return true
}

// remoteIP extracts the client IP from r.RemoteAddr, stripping the port
// when present. Falls back to the raw value if it isn't a host:port pair
// (e.g. a unix socket address in a test), which is still a stable,
// per-connection rate-limit key even if it isn't strictly an IP.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
