package local

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiter_AllowsUpToMaxAttempts(t *testing.T) {
	r := NewRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !r.Allow("1.2.3.4") {
			t.Fatalf("attempt %d refused, want allowed (max is 3)", i+1)
		}
	}
}

func TestRateLimiter_RefusesTheAttemptAfterMax(t *testing.T) {
	r := NewRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		r.Allow("1.2.3.4")
	}
	if r.Allow("1.2.3.4") {
		t.Error("4th attempt within the window = allowed, want refused")
	}
}

func TestRateLimiter_TracksEachKeyIndependently(t *testing.T) {
	r := NewRateLimiter(1, time.Minute)
	if !r.Allow("1.2.3.4") {
		t.Fatal("first attempt from 1.2.3.4 refused, want allowed")
	}
	if !r.Allow("5.6.7.8") {
		t.Error("first attempt from a different IP (5.6.7.8) refused, want allowed - keys must not share a budget")
	}
	if r.Allow("1.2.3.4") {
		t.Error("second attempt from 1.2.3.4 = allowed, want refused (max is 1)")
	}
}

func TestRateLimiter_AllowsAgainAfterTheWindowElapses(t *testing.T) {
	now := time.Now()
	clock := &now
	r := NewRateLimiter(1, time.Minute)
	r.now = func() time.Time { return *clock }

	if !r.Allow("1.2.3.4") {
		t.Fatal("first attempt refused, want allowed")
	}
	if r.Allow("1.2.3.4") {
		t.Fatal("second attempt within the window = allowed, want refused")
	}

	*clock = now.Add(time.Minute + time.Second)
	if !r.Allow("1.2.3.4") {
		t.Error("attempt after the window elapsed = refused, want allowed")
	}
}

// TestRateLimiter_SweepsIdleKeysOnceTheirEntriesAllAge is issue #119's
// review finding that RateLimiter's own hits map never evicts a key once
// every entry in it has aged out: a client seen exactly once (the common
// case once Config.TrustForwardedHeaders makes this map's keys real
// external client addresses instead of always the same reverse proxy's
// own address) otherwise stays in this map forever, growing it without
// bound over a long-running process's lifetime. This proves sweepEvery's
// own housekeeping actually reclaims those entries rather than merely
// existing unreachably in source.
func TestRateLimiter_SweepsIdleKeysOnceTheirEntriesAllAge(t *testing.T) {
	now := time.Now()
	clock := &now
	r := NewRateLimiter(1, time.Minute)
	r.now = func() time.Time { return *clock }

	// A batch of one-off callers: each key is used exactly once and never
	// returns to have its own entry trimmed as a side effect of its own
	// next call.
	const idleBatch = sweepEvery + 10
	for i := 0; i < idleBatch; i++ {
		r.Allow(fmt.Sprintf("203.0.113.%d", i))
	}

	*clock = now.Add(2 * time.Minute) // every entry above is now expired

	// Enough further calls, from a disjoint set of keys, to cross the
	// sweep threshold again and trigger a pass.
	for i := 0; i < sweepEvery; i++ {
		r.Allow(fmt.Sprintf("198.51.100.%d", i))
	}

	r.mu.Lock()
	got := len(r.hits)
	r.mu.Unlock()

	if got > sweepEvery {
		t.Errorf("hits map has %d entries after a sweep pass, want at most %d (the first batch's now-expired entries should have been evicted)", got, sweepEvery)
	}
}

// TestRemoteIP_DefaultsToRemoteAddrRegardlessOfHeaders proves the safe
// default: without trustForwarded, an X-Forwarded-For header - which any
// direct caller can set to whatever it likes - is never consulted at all.
func TestRemoteIP_DefaultsToRemoteAddrRegardlessOfHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.5:54321"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := remoteIP(req, false); got != "10.0.0.5" {
		t.Errorf("remoteIP(trustForwarded=false) = %q, want %q (RemoteAddr, ignoring X-Forwarded-For)", got, "10.0.0.5")
	}
}

// TestRemoteIP_TrustsFirstForwardedForEntryWhenEnabled is issue #119's
// review's central regression test for the rate-limit collapse: with
// trustForwarded enabled (the only setting apps/generic's engine,
// container/compose.yaml's `rclone-manager` service, actually uses), two
// requests that share the same RemoteAddr (exactly what every request the
// engine sees looks like in the shipped two-container topology - always
// web-ui's own container address) but carry DIFFERENT X-Forwarded-For
// values must be treated as different clients, not collapsed into one.
func TestRemoteIP_TrustsFirstForwardedForEntryWhenEnabled(t *testing.T) {
	reqA := httptest.NewRequest(http.MethodPost, "/", nil)
	reqA.RemoteAddr = "172.18.0.3:9000" // web-ui's own address on the internal network
	reqA.Header.Set("X-Forwarded-For", "203.0.113.7")

	reqB := httptest.NewRequest(http.MethodPost, "/", nil)
	reqB.RemoteAddr = "172.18.0.3:9000" // SAME reverse-proxy peer address
	reqB.Header.Set("X-Forwarded-For", "203.0.113.99")

	gotA := remoteIP(reqA, true)
	gotB := remoteIP(reqB, true)

	if gotA != "203.0.113.7" {
		t.Errorf("remoteIP(reqA, true) = %q, want %q", gotA, "203.0.113.7")
	}
	if gotB != "203.0.113.99" {
		t.Errorf("remoteIP(reqB, true) = %q, want %q", gotB, "203.0.113.99")
	}
	if gotA == gotB {
		t.Fatal("two different X-Forwarded-For values resolved to the same key - this is the exact rate-limit collapse the fix is for")
	}
}

// TestRemoteIP_TrustedButHeaderAbsentFallsBackToRemoteAddr covers a
// trusted engine that, for whatever reason, receives a request with no
// X-Forwarded-For at all (e.g. a direct request in a test/dev setup that
// doesn't go through the reverse proxy): it must not treat an empty
// string as a valid key.
func TestRemoteIP_TrustedButHeaderAbsentFallsBackToRemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "192.168.1.1:1234"

	if got := remoteIP(req, true); got != "192.168.1.1" {
		t.Errorf("remoteIP(trustForwarded=true, no header) = %q, want %q", got, "192.168.1.1")
	}
}
