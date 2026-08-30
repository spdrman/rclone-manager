package local

import (
	"testing"
	"time"
)

func TestSessionManager_LookupFindsAFreshlyCreatedSession(t *testing.T) {
	m := newSessionManager(time.Now)
	token, expiresAt, err := m.create("bm-admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if expiresAt.Before(time.Now()) {
		t.Errorf("expiresAt = %v, want a time in the future", expiresAt)
	}

	username, ok := m.lookup(token)
	if !ok || username != "bm-admin" {
		t.Errorf("lookup(token) = (%q, %v), want (\"bm-admin\", true)", username, ok)
	}
}

func TestSessionManager_LookupRejectsAnUnknownToken(t *testing.T) {
	m := newSessionManager(time.Now)
	if _, ok := m.lookup("never-issued"); ok {
		t.Error("lookup(unknown token) = true, want false")
	}
}

func TestSessionManager_LookupRejectsAnExpiredSession(t *testing.T) {
	now := time.Now()
	clock := &now
	m := newSessionManager(func() time.Time { return *clock })

	token, _, err := m.create("bm-admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	*clock = now.Add(sessionTTL + time.Second)
	if _, ok := m.lookup(token); ok {
		t.Error("lookup(expired token) = true, want false")
	}
}

func TestSessionManager_RevokeInvalidatesTheSessionImmediately(t *testing.T) {
	m := newSessionManager(time.Now)
	token, _, err := m.create("bm-admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	m.revoke(token)
	if _, ok := m.lookup(token); ok {
		t.Error("lookup(revoked token) = true, want false")
	}
}

func TestSessionManager_TwoSessionsGetDifferentTokens(t *testing.T) {
	m := newSessionManager(time.Now)
	a, _, err := m.create("bm-admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, _, err := m.create("bm-admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a == b {
		t.Error("two sessions were issued the same token")
	}
}
