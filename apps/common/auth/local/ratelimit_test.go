package local

import (
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
