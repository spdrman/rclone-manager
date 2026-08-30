package local

import (
	"testing"
	"time"
)

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
