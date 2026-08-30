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
	// calls counts Allow invocations since the last sweep, so idle keys
	// (see sweepExpiredLocked) are cleaned up without a background
	// goroutine of their own.
	calls int
}

// sweepEvery bounds how often Allow's own housekeeping walks the whole
// hits map looking for keys whose every entry has aged out of window,
// deleting them. Without this, a key that is only ever seen once (a
// distinct client IP that never comes back - the common case once
// Config.TrustForwardedHeaders is enabled and this map starts being keyed
// by real external client addresses instead of always the same reverse
// proxy's own address) stays in this map forever: nothing else ever
// touches that key again to trim it. A plain counter, not a timer or a
// background goroutine, matching this type's own doc above ("the
// simplest thing that satisfies it without an external store") - a
// goroutine would need its own shutdown story this type does not
// otherwise have.
const sweepEvery = 256

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

	var allowed bool
	if len(kept) >= r.max {
		r.hits[key] = kept
		allowed = false
	} else {
		r.hits[key] = append(kept, now)
		allowed = true
	}

	r.calls++
	if r.calls >= sweepEvery {
		r.calls = 0
		r.sweepExpiredLocked(cutoff)
	}
	return allowed
}

// sweepExpiredLocked deletes every hits entry whose every retained
// timestamp is at or before cutoff - the only thing that bounds this
// map's size over a long-running process's lifetime (see sweepEvery's own
// doc). Callers must already hold r.mu.
func (r *RateLimiter) sweepExpiredLocked(cutoff time.Time) {
	for key, hits := range r.hits {
		stillLive := false
		for _, t := range hits {
			if t.After(cutoff) {
				stillLive = true
				break
			}
		}
		if !stillLive {
			delete(r.hits, key)
		}
	}
}

// remoteIP reports the client address rate limiting should key on for r.
//
// When trustForwarded is false (the default - see
// Config.TrustForwardedHeaders, service.go), this is always r.RemoteAddr
// (stripped of its port), i.e. the direct TCP peer - correct for a
// listener reachable directly by arbitrary clients, and safe by
// construction since RemoteAddr is not a header a client can forge.
//
// When trustForwarded is true, this instead reads the first entry of
// X-Forwarded-For, falling back to RemoteAddr if that header is absent.
// This is the fix for the two-container split's rate-limit collapse
// (issue #119's review): apps/generic/server.NewEngine's HTTP surface is,
// in the shipped topology, reachable ONLY from
// apps/generic/server.NewUI's reverse proxy, over a Docker network
// nothing else can join - every request this Service's handler sees
// therefore carries the proxy's own container address as RemoteAddr
// regardless of which real external client made it, which collapsed
// every client into one shared rate-limit bucket. See
// Config.TrustForwardedHeaders's own doc for exactly what makes trusting
// this header safe here specifically, and requestIsSecure (forwarded.go)
// for the identical reasoning applied to the Secure cookie flag.
func remoteIP(r *http.Request, trustForwarded bool) string {
	if trustForwarded {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if ip := firstForwardedValue(fwd); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
