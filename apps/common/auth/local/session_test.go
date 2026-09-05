// Session lifetime, exercised through the manager rather than over HTTP,
// so that expiry can be tested by moving an injected clock instead of
// waiting a day.
//
// The two token-distinctness and unknown-token cases look trivial and are
// not: a manager that returned the same token twice would silently share
// one session between two logins, and one that accepted an unknown token
// would authenticate anybody who sent a plausible-looking string. Both are
// the kind of thing an optimisation can introduce without touching any
// route.
package local

import (
	"sync"
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

// TestSessionManager_RotateSessionReplacesEverySessionWithOneNewOne is
// password rotation's session-invalidation guarantee (handler.go's
// handleRotatePassword) at the sessionManager level: a stolen or
// forgotten-open session must not survive a password change just because
// nobody explicitly logged it out, while the caller performing the
// rotation gets a fresh, valid session out of the very same call.
func TestSessionManager_RotateSessionReplacesEverySessionWithOneNewOne(t *testing.T) {
	m := newSessionManager(time.Now)
	a, _, err := m.create("bm-admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, _, err := m.create("bm-admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	newToken, expiresAt, err := m.rotateSession("bm-admin")
	if err != nil {
		t.Fatalf("rotateSession: %v", err)
	}
	if expiresAt.Before(time.Now()) {
		t.Errorf("expiresAt = %v, want a time in the future", expiresAt)
	}

	if _, ok := m.lookup(a); ok {
		t.Error("lookup(a) = true after rotateSession, want false")
	}
	if _, ok := m.lookup(b); ok {
		t.Error("lookup(b) = true after rotateSession, want false")
	}
	username, ok := m.lookup(newToken)
	if !ok || username != "bm-admin" {
		t.Errorf("lookup(newToken) = (%q, %v), want (\"bm-admin\", true)", username, ok)
	}
}

// TestSessionManager_ConcurrentRotateSessionNeverLeavesZeroLiveSessions is
// a regression test for the race handleRotatePassword used to be exposed
// to when it composed a revoke-all step and create() as two
// separately-locked calls: a second, concurrent rotation's revoke-all
// could fire after the first rotation's create() had already installed
// its new session, wiping out the very session the first rotation just
// successfully issued and leaving the admin who triggered it locked out
// by their own action. rotateSession folds both steps under one lock
// acquisition, so no interleaving of concurrent calls can ever produce a
// state with zero live sessions - each call installs exactly one, and
// whichever runs last wins outright.
func TestSessionManager_ConcurrentRotateSessionNeverLeavesZeroLiveSessions(t *testing.T) {
	m := newSessionManager(time.Now)
	if _, _, err := m.create("bm-admin"); err != nil {
		t.Fatalf("create: %v", err)
	}

	const attempts = 200
	tokens := make([]string, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			tokens[i], _, errs[i] = m.rotateSession("bm-admin")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("rotateSession[%d]: %v", i, err)
		}
	}

	live := 0
	for _, tok := range tokens {
		if _, ok := m.lookup(tok); ok {
			live++
		}
	}
	if live != 1 {
		t.Errorf("live sessions after %d concurrent rotations = %d, want exactly 1", attempts, live)
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
