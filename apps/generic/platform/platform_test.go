package platform

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
)

// These tests are about the wiring, not about generic's answers. What ID()
// and Capabilities() return is asserted in the profile package's own
// tests, against the table row that defines them; repeating those
// assertions here would only pin the same constant twice.
//
// What is unique to this package is that Adapter is a forwarder, and a
// forwarder's characteristic failure is being wired to the wrong thing (or
// to nothing) while still answering plausibly. So the authenticator test
// deliberately drives a real local.Service through a real request rather
// than checking the returned interface is non-nil: a stand-in would also
// be non-nil, and would also refuse a caller with no cookie, but it would
// not be the same service the process later logs people in against.
//
// The package is platform rather than platform_test because New panics
// instead of returning an error, so there is nothing an external test
// could observe that an internal one cannot.

func TestAdapter_ReportsGenericIdentityAndNoCapabilities(t *testing.T) {
	auth, err := local.New(local.Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	adapter := New(auth)

	if got := adapter.ID(); got != capabilities.PlatformGeneric {
		t.Errorf("ID() = %q, want %q", got, capabilities.PlatformGeneric)
	}

	caps := adapter.Capabilities()
	if caps != (capabilities.PlatformCapabilities{}) {
		t.Errorf("Capabilities() = %+v, want every field false (this platform has no native integration)", caps)
	}

	info, err := adapter.PlatformInfo(context.Background())
	if err != nil {
		t.Fatalf("PlatformInfo: %v", err)
	}
	if info.ID != capabilities.PlatformGeneric || info.Name == "" {
		t.Errorf("PlatformInfo() = %+v, want ID=%q and a non-empty Name", info, capabilities.PlatformGeneric)
	}
}

func TestAdapter_AuthenticatorDelegatesToLocalService(t *testing.T) {
	auth, err := local.New(local.Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	adapter := New(auth)

	// A request with no session cookie must be refused - proves this is
	// actually wired to the same local.Service, not a stand-in.
	authCtx, err := adapter.Authenticator().Authenticate(context.Background(), capabilities.AuthRequest{})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if authCtx.Authenticated {
		t.Error("Authenticate(no session cookie) = authenticated, want false")
	}
}
