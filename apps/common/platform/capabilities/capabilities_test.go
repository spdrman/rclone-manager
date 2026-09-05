package capabilities_test

import (
	"context"
	"errors"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
)

// These tests are the contract's only executable specification: nothing
// else in this repository can tell a provider author that returning a bare
// nil from Authenticator() is legal but discouraged, or that embedding
// BasePlatformAdapter is enough on its own.
//
// So two adapters live here side by side and neither is redundant.
// fakeAdapter returns nil for a capability it does not have, which is the
// original convention and still a valid PlatformAdapter; minimalAdapter
// embeds BasePlatformAdapter and returns the null object instead. Both
// conventions are in the wild, and a change that quietly broke either one
// would be a source-compatible change that panics in somebody's provider,
// which is the shape of breakage this file exists to make loud.
//
// The package is capabilities_test rather than capabilities: the contract
// is what a provider outside this package sees, so the tests are written
// from outside it too, and cannot accidentally lean on an unexported
// helper a provider would not have.

// fakeAdapter is a minimal PlatformAdapter used only to prove the contract
// is implementable without pulling in any real provider or core code. Every
// provider app under apps/<provider>/ implements this same interface (EPIC
// B §3.4); core/ never imports this package (§7.1).
//
// It returns nil from Authenticator()/Notifier() directly, by hand, rather
// than embedding BasePlatformAdapter, on purpose: that is still a legal
// PlatformAdapter implementation, and this file's tests exercise both that
// legacy nil convention (below) and BasePlatformAdapter's null-object
// convention (see minimalAdapter further down) side by side.
type fakeAdapter struct {
	caps capabilities.PlatformCapabilities
}

func (f fakeAdapter) ID() capabilities.PlatformID { return "fake" }

func (f fakeAdapter) Capabilities() capabilities.PlatformCapabilities { return f.caps }

func (f fakeAdapter) Authenticator() capabilities.Authenticator {
	if !f.caps.NativeAuth {
		return nil
	}
	return fakeAuthenticator{}
}

func (f fakeAdapter) Notifier() capabilities.Notifier {
	if !f.caps.NativeNotifications {
		return nil
	}
	return fakeNotifier{}
}

func (f fakeAdapter) PlatformInfo(_ context.Context) (capabilities.PlatformInfo, error) {
	return capabilities.PlatformInfo{ID: "fake", Name: "Fake Platform"}, nil
}

type fakeAuthenticator struct{}

func (fakeAuthenticator) Authenticate(_ context.Context, _ capabilities.AuthRequest) (capabilities.AuthContext, error) {
	return capabilities.AuthContext{Authenticated: true, Mode: capabilities.AuthModeNativeSession}, nil
}

type fakeNotifier struct{}

func (fakeNotifier) Notify(_ context.Context, _ string, _ string) error { return nil }

// Compile-time proof that fakeAdapter actually satisfies PlatformAdapter.
var _ capabilities.PlatformAdapter = fakeAdapter{}

// minimalAdapter embeds BasePlatformAdapter and supplies only the three
// methods it has no sensible default for. It never claims NativeAuth or
// NativeNotifications, so it never overrides Authenticator()/Notifier() —
// proving BasePlatformAdapter alone is enough to satisfy PlatformAdapter
// for a provider with no native capabilities at all (item 4).
type minimalAdapter struct {
	capabilities.BasePlatformAdapter
}

func (minimalAdapter) ID() capabilities.PlatformID { return capabilities.PlatformGeneric }

func (minimalAdapter) Capabilities() capabilities.PlatformCapabilities {
	return capabilities.PlatformCapabilities{}
}

func (minimalAdapter) PlatformInfo(_ context.Context) (capabilities.PlatformInfo, error) {
	return capabilities.PlatformInfo{ID: capabilities.PlatformGeneric, Name: "Minimal"}, nil
}

var _ capabilities.PlatformAdapter = minimalAdapter{}

func TestZeroValueCapabilitiesDenyEveryCapability(t *testing.T) {
	// Least-privilege default (§22, §45): a provider must opt IN to every
	// capability. A capability that is never explicitly claimed must never
	// read as supported.
	var caps capabilities.PlatformCapabilities

	if caps.NativeAuth {
		t.Error("zero-value PlatformCapabilities.NativeAuth = true, want false")
	}
	if caps.NativeNotifications {
		t.Error("zero-value PlatformCapabilities.NativeNotifications = true, want false")
	}
	if caps.StoragePicker {
		t.Error("zero-value PlatformCapabilities.StoragePicker = true, want false")
	}
	if caps.EmbeddedWindow {
		t.Error("zero-value PlatformCapabilities.EmbeddedWindow = true, want false")
	}
	if caps.AppStorePackaging {
		t.Error("zero-value PlatformCapabilities.AppStorePackaging = true, want false")
	}
}

func TestAdapterWithoutNativeAuthReturnsNilAuthenticator(t *testing.T) {
	a := fakeAdapter{caps: capabilities.PlatformCapabilities{NativeAuth: false}}

	// An unsupported capability must be explicit, never emulated (§3.4): a
	// caller that only checks Capabilities().NativeAuth before deciding
	// whether to call Authenticator() must never receive a non-nil value
	// that then does the wrong thing.
	if a.Authenticator() != nil {
		t.Error("Authenticator() is non-nil for an adapter that does not claim NativeAuth")
	}
}

func TestAdapterWithNativeAuthReturnsAnAuthenticator(t *testing.T) {
	a := fakeAdapter{caps: capabilities.PlatformCapabilities{NativeAuth: true}}

	if a.Authenticator() == nil {
		t.Fatal("Authenticator() is nil for an adapter that claims NativeAuth")
	}

	ctx, err := a.Authenticator().Authenticate(context.Background(), capabilities.AuthRequest{})
	if err != nil {
		t.Fatalf("Authenticate() error = %v, want nil", err)
	}
	if !ctx.Authenticated {
		t.Error("Authenticate() returned an AuthContext with Authenticated = false")
	}
}

func TestAdapterWithoutNativeNotificationsReturnsNilNotifier(t *testing.T) {
	a := fakeAdapter{caps: capabilities.PlatformCapabilities{NativeNotifications: false}}

	if a.Notifier() != nil {
		t.Error("Notifier() is non-nil for an adapter that does not claim NativeNotifications")
	}
}

func TestAdapterWithNativeNotificationsReturnsANotifier(t *testing.T) {
	a := fakeAdapter{caps: capabilities.PlatformCapabilities{NativeNotifications: true}}

	if a.Notifier() == nil {
		t.Fatal("Notifier() is nil for an adapter that claims NativeNotifications")
	}
	if err := a.Notifier().Notify(context.Background(), "title", "message"); err != nil {
		t.Fatalf("Notify() error = %v, want nil", err)
	}
}

func TestPlatformInfoReportsTheAdapterIdentity(t *testing.T) {
	a := fakeAdapter{}
	info, err := a.PlatformInfo(context.Background())
	if err != nil {
		t.Fatalf("PlatformInfo() error = %v, want nil", err)
	}
	if info.ID != "fake" {
		t.Errorf("PlatformInfo().ID = %q, want %q", info.ID, "fake")
	}
}

// The remaining tests exercise BasePlatformAdapter directly (item 4): a
// provider that embeds it and never overrides Authenticator()/Notifier()
// gets a non-nil, typed-failure default instead of a nil that panics at
// the natural call site.

func TestBasePlatformAdapterAuthenticatorIsNeverNil(t *testing.T) {
	a := minimalAdapter{}

	auth := a.Authenticator()
	if auth == nil {
		t.Fatal("BasePlatformAdapter.Authenticator() is nil, want a non-nil null object")
	}

	_, err := auth.Authenticate(context.Background(), capabilities.AuthRequest{})
	if !errors.Is(err, capabilities.ErrCapabilityUnsupported) {
		t.Errorf("Authenticate() error = %v, want errors.Is(err, ErrCapabilityUnsupported)", err)
	}
}

func TestBasePlatformAdapterNotifierIsNeverNil(t *testing.T) {
	a := minimalAdapter{}

	notifier := a.Notifier()
	if notifier == nil {
		t.Fatal("BasePlatformAdapter.Notifier() is nil, want a non-nil null object")
	}

	err := notifier.Notify(context.Background(), "title", "message")
	if !errors.Is(err, capabilities.ErrCapabilityUnsupported) {
		t.Errorf("Notify() error = %v, want errors.Is(err, ErrCapabilityUnsupported)", err)
	}
}

func TestBasePlatformAdapterDoesNotOverrideAProviderThatSupportsTheCapability(t *testing.T) {
	// fakeAdapter does not embed BasePlatformAdapter, but the point of the
	// embeddable-default pattern is that a provider which DOES support a
	// capability overrides the method with its own real implementation —
	// prove that override still wins, using the existing fakeAdapter as
	// the stand-in for "a provider with its own Authenticator".
	a := fakeAdapter{caps: capabilities.PlatformCapabilities{NativeAuth: true}}

	ctx, err := a.Authenticator().Authenticate(context.Background(), capabilities.AuthRequest{})
	if err != nil {
		t.Fatalf("Authenticate() error = %v, want nil", err)
	}
	if errors.Is(err, capabilities.ErrCapabilityUnsupported) {
		t.Error("Authenticate() returned ErrCapabilityUnsupported for a provider that supports NativeAuth")
	}
	if ctx.Mode != capabilities.AuthModeNativeSession {
		t.Errorf("Authenticate().Mode = %q, want %q", ctx.Mode, capabilities.AuthModeNativeSession)
	}
}
