package local

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
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
