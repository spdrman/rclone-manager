package local

import (
	"testing"
	"time"
)

// The bootstrap token's three refusals, one per way it can stop being
// valid: already used, expired, or not the one that was issued.
//
// Single-use is the property with the sharpest failure mode, because a
// token that can be redeemed twice is a token an attacker can redeem after
// the operator did. The test for it consumes successfully first and then
// asserts the second attempt fails, which is the only ordering that can
// tell "single-use" apart from "always refuses".
//
// Time is injected rather than waited on. A test that slept for the real
// TTL would take half an hour, and one that shortened the TTL would be
// testing a constant nothing ships with.

func TestBootstrapIssuer_ConsumeAcceptsTheIssuedTokenOnce(t *testing.T) {
	b := newBootstrapIssuer(time.Now)
	token, err := b.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if !b.consume(token) {
		t.Fatal("consume(issued token) = false, want true")
	}
	if b.consume(token) {
		t.Error("consume(already-used token) = true, want false (single-use)")
	}
}

func TestBootstrapIssuer_ConsumeRejectsAnUnknownToken(t *testing.T) {
	b := newBootstrapIssuer(time.Now)
	if _, err := b.issue(); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if b.consume("not-the-real-token") {
		t.Error("consume(wrong token) = true, want false")
	}
}

func TestBootstrapIssuer_ConsumeRejectsAnEmptyCandidate(t *testing.T) {
	b := newBootstrapIssuer(time.Now)
	if _, err := b.issue(); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if b.consume("") {
		t.Error("consume(\"\") = true, want false")
	}
}

func TestBootstrapIssuer_ConsumeRejectsAnExpiredToken(t *testing.T) {
	now := time.Now()
	clock := &now
	b := newBootstrapIssuer(func() time.Time { return *clock })
	token, err := b.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	*clock = now.Add(bootstrapTokenTTL + time.Second)
	if b.consume(token) {
		t.Error("consume(expired token) = true, want false")
	}
}

func TestBootstrapIssuer_IssuingAgainInvalidatesThePreviousToken(t *testing.T) {
	b := newBootstrapIssuer(time.Now)
	first, err := b.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	second, err := b.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if first == second {
		t.Fatal("two issued tokens are identical")
	}
	if b.consume(first) {
		t.Error("consume(superseded first token) = true, want false")
	}
	if !b.consume(second) {
		t.Error("consume(current second token) = false, want true")
	}
}
