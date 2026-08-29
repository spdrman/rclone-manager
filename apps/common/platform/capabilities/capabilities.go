// Package capabilities defines the PlatformCapabilities/PlatformAdapter
// contract every provider app composes over (docs/EPIC-B-multi-nas.md
// §3.4). It has no dependency on core: the engine (core/) is not aware
// providers exist (§7.1 — core imports nothing from apps/), and this
// package is not aware backup-lifecycle internals exist either. It is a
// pure declaration of what a NAS platform can and cannot do for the
// product running on top of it.
package capabilities

import (
	"context"
	"errors"
	"net/http"
)

// PlatformID is the stable provider identifier every PlatformAdapter.ID()
// implementation returns. The values mirror ui/shared's PlatformId
// literal-union type (ui/shared/src/types/platform.ts) exactly: nothing
// catches a Go provider returning a value the TS side does not also
// recognize until it fails at runtime, in production, so grep both sides
// before adding a value here.
type PlatformID string

const (
	PlatformGeneric        PlatformID = "generic"
	PlatformUGOS           PlatformID = "ugos"
	PlatformSynology       PlatformID = "synology"
	PlatformTrueNAS        PlatformID = "truenas"
	PlatformUnraid         PlatformID = "unraid"
	PlatformOpenMediaVault PlatformID = "openmediavault"
	PlatformProxmox        PlatformID = "proxmox"
)

// AuthMode identifies how a caller's identity was established. The two
// values mirror ui/shared's AuthMode literal-union type
// (ui/shared/src/types/platform.ts) exactly, for the same cross-language
// reason PlatformID above does.
type AuthMode string

const (
	AuthModeLocalAccount  AuthMode = "local-account"
	AuthModeNativeSession AuthMode = "native-session"
)

// PlatformCapabilities is a least-privilege declaration: every field
// defaults to false, and a provider adapter must opt in explicitly.
// Presenting an unsupported capability as supported is the one thing this
// contract exists to prevent (§22, §45) — mirrors the shared frontend's
// PlatformCapabilities (ui/shared/src/types/platform.ts) field-for-field,
// modulo Go/TS naming convention.
type PlatformCapabilities struct {
	NativeAuth          bool
	NativeNotifications bool
	StoragePicker       bool
	EmbeddedWindow      bool
	AppStorePackaging   bool
}

// PlatformInfo is read-only, provider-reported information about the host
// the product is currently running on.
type PlatformInfo struct {
	ID         PlatformID
	Name       string
	Deployment string
	Version    string
}

// AuthContext mirrors the shared frontend's AuthContext
// (ui/shared/src/types/platform.ts) so a Go-side auth adapter and the
// TS-side PlatformBridge describe the same shape.
type AuthContext struct {
	Authenticated    bool
	Username         string
	Mode             AuthMode
	SessionExpiresAt string // RFC3339; empty when not applicable.
}

// AuthRequest is intentionally narrow: an Authenticator needs the signals
// a trusted-proxy or local-session adapter uses to identify a caller, not
// a dependency on the rest of net/http's request type. Headers is
// http.Header (not a bare map[string]string) specifically because this
// type exists to support the trusted-proxy identity check landing in
// #92/B1.3 (verified network source, reject spoofed headers from
// untrusted peers): a bare map cannot represent a repeated header or
// canonicalize a header name by case, and losing either of those
// semantics is exactly the kind of gap that could silently defeat that
// check. http.Header gets both from the standard library for free.
type AuthRequest struct {
	Headers    http.Header
	RemoteAddr string
}

// Authenticator resolves the identity of the caller for a provider that
// supplies (or emulates) authentication.
type Authenticator interface {
	Authenticate(ctx context.Context, r AuthRequest) (AuthContext, error)
}

// Notifier delivers an operator-facing notification through whatever
// channel the platform offers natively. A PlatformAdapter that reports
// NativeNotifications: false MUST return a non-emulating Notifier from
// Notifier() — one that fails rather than silently drops messages (§22 —
// unsupported capabilities are explicit, never emulated). See
// BasePlatformAdapter for the recommended way to get this for free.
type Notifier interface {
	Notify(ctx context.Context, title, message string) error
}

// PlatformAdapter is the seam every provider app (apps/<provider>)
// implements once, and the only shape core or a generic host is ever
// allowed to depend on when it needs something platform-specific. core/
// never imports this package (§7.1); it lives at the apps layer, above
// core, alongside the providers that implement it.
type PlatformAdapter interface {
	// ID is the stable provider identifier (matches ui/shared's PlatformId).
	ID() PlatformID
	Capabilities() PlatformCapabilities
	// Authenticator returns the caller's authenticator, or a null object
	// whose Authenticate always fails with ErrCapabilityUnsupported, when
	// Capabilities().NativeAuth is false. Embed BasePlatformAdapter to get
	// that null object for free — a bare nil is technically still a legal
	// implementation of this method, but it panics at the natural call
	// site (adapter.Authenticator().Authenticate(...)) instead of
	// surfacing a typed error, so BasePlatformAdapter is the safer
	// default for every new provider.
	Authenticator() Authenticator
	// Notifier returns the platform's notifier, or a null object whose
	// Notify always fails with ErrCapabilityUnsupported, when
	// Capabilities().NativeNotifications is false. Same nil-vs-null-object
	// reasoning as Authenticator above.
	Notifier() Notifier
	PlatformInfo(ctx context.Context) (PlatformInfo, error)
}

// ErrCapabilityUnsupported is the error a null-object Authenticator or
// Notifier returns for a capability a provider has not opted into (see
// BasePlatformAdapter). Callers can `errors.Is(err, ErrCapabilityUnsupported)`
// to distinguish "this platform doesn't support that" from any other
// failure, instead of a bare nil that panics at the call site.
var ErrCapabilityUnsupported = errors.New("capability not supported by this platform adapter")

type unsupportedAuthenticator struct{}

func (unsupportedAuthenticator) Authenticate(context.Context, AuthRequest) (AuthContext, error) {
	return AuthContext{}, ErrCapabilityUnsupported
}

type unsupportedNotifier struct{}

func (unsupportedNotifier) Notify(context.Context, string, string) error {
	return ErrCapabilityUnsupported
}

// BasePlatformAdapter is embedded by a PlatformAdapter implementation to
// get safe, non-nil defaults for every optional capability accessor:
// unless a provider overrides Authenticator()/Notifier() itself (because
// it DOES support that capability), embedding this type means the
// unsupported case returns a typed ErrCapabilityUnsupported from the
// natural call site instead of a nil interface value that panics.
//
// It also gives PlatformAdapter room to grow: six independent providers
// implement this interface directly today, so any future method added to
// PlatformAdapter would otherwise break all six simultaneously — the
// opposite of what the epic wants from a seam every provider composes
// over. A future accessor added here with a default implementation on
// BasePlatformAdapter does not break a provider that already embeds it,
// the same non-breaking-growth pattern gRPC's generated
// UnimplementedXServer types use.
//
// BasePlatformAdapter does NOT implement ID(), Capabilities(), or
// PlatformInfo() — those have no sensible platform-neutral default, and
// every provider must supply them itself.
type BasePlatformAdapter struct{}

func (BasePlatformAdapter) Authenticator() Authenticator { return unsupportedAuthenticator{} }

func (BasePlatformAdapter) Notifier() Notifier { return unsupportedNotifier{} }
