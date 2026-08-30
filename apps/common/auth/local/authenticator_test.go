package local

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
)

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
