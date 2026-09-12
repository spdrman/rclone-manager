package local

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/backupdproject/backupd/apps/common/platform/capabilities"
)

// Three cases, and only the first one is about success.
//
// The read side of a session is where a bug is silent: an authenticator
// that answered "yes" to everything would let every test that logs in
// first pass, and would hand /api/v1 to anyone. So the two refusals (no
// cookie at all, and a cookie naming a session this process never issued)
// are the assertions that carry the weight here, and both check that the
// returned AuthContext is empty rather than only that Authenticated is
// false: a handler that reads Username without checking the flag must not
// find a name in there.

func TestAuthenticator_AuthenticatesARequestCarryingALiveSessionCookie(t *testing.T) {
	svc, err := New(Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token, _, err := svc.sessions.create("bm-admin")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	authCtx, err := svc.Authenticator().Authenticate(context.Background(), capabilities.AuthRequest{
		Headers: map[string][]string{"Cookie": {SessionCookieName + "=" + token}},
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !authCtx.Authenticated || authCtx.Username != "bm-admin" || authCtx.Mode != capabilities.AuthModeLocalAccount {
		t.Errorf("Authenticate(live session) = %+v, want Authenticated=true Username=bm-admin Mode=local-account", authCtx)
	}
}

func TestAuthenticator_RefusesARequestWithNoSessionCookie(t *testing.T) {
	svc, err := New(Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	authCtx, err := svc.Authenticator().Authenticate(context.Background(), capabilities.AuthRequest{})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if authCtx.Authenticated {
		t.Errorf("Authenticate(no cookie) = %+v, want Authenticated=false", authCtx)
	}
}

func TestAuthenticator_RefusesAnUnknownSessionCookie(t *testing.T) {
	svc, err := New(Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	authCtx, err := svc.Authenticator().Authenticate(context.Background(), capabilities.AuthRequest{
		Headers: map[string][]string{"Cookie": {SessionCookieName + "=not-a-real-token"}},
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if authCtx.Authenticated {
		t.Errorf("Authenticate(unknown token) = %+v, want Authenticated=false", authCtx)
	}
}

// #794's read-compat window, from the seam webhost actually consults.
//
// The rename changed the name every session cookie is WRITTEN under.
// Anything already in a browser or a cookie jar at upgrade time still
// carries the old one, and a read that only knew the new name would
// answer "nobody is signed in" to a caller holding a perfectly live
// session - a rename presenting as a mass logout. So the old name is
// still accepted here, and the new one still wins when both arrive, so a
// stale cookie left behind by the compat window cannot shadow the
// session that was actually just issued.

func TestAuthenticator_AcceptsTheLegacySessionCookieName(t *testing.T) {
	svc, err := New(Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token, _, err := svc.sessions.create("bm-admin")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	authCtx, err := svc.Authenticator().Authenticate(context.Background(), capabilities.AuthRequest{
		Headers: map[string][]string{"Cookie": {LegacySessionCookieName + "=" + token}},
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !authCtx.Authenticated || authCtx.Username != "bm-admin" {
		t.Errorf("Authenticate(legacy %s cookie) = %+v, want Authenticated=true Username=bm-admin",
			LegacySessionCookieName, authCtx)
	}
}

func TestAuthenticator_PrefersTheCurrentSessionCookieOverALegacyLeftover(t *testing.T) {
	svc, err := New(Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token, _, err := svc.sessions.create("bm-admin")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// The leftover names a session this process revoked; if it were read
	// in preference, the live session alongside it would be refused.
	authCtx, err := svc.Authenticator().Authenticate(context.Background(), capabilities.AuthRequest{
		Headers: map[string][]string{"Cookie": {
			LegacySessionCookieName + "=revoked-leftover; " + SessionCookieName + "=" + token,
		}},
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !authCtx.Authenticated || authCtx.Username != "bm-admin" {
		t.Errorf("Authenticate(both names) = %+v, want the current cookie to win (Authenticated=true)", authCtx)
	}
}

// An empty value is what a cleared cookie a client keeps echoing back
// looks like, and it must not shadow the legacy name still carrying a
// live token - otherwise logging out in one tab would strand the other.
func TestAuthenticator_AnEmptyCurrentCookieDoesNotShadowTheLegacyOne(t *testing.T) {
	svc, err := New(Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token, _, err := svc.sessions.create("bm-admin")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	authCtx, err := svc.Authenticator().Authenticate(context.Background(), capabilities.AuthRequest{
		Headers: map[string][]string{"Cookie": {
			SessionCookieName + "=; " + LegacySessionCookieName + "=" + token,
		}},
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !authCtx.Authenticated {
		t.Errorf("Authenticate(empty current, live legacy) = %+v, want Authenticated=true", authCtx)
	}
}
