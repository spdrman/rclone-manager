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

// The provider identifiers this product recognises. The set is closed on
// purpose: an adapter cannot invent one, because a value the shared
// frontend's literal union does not also carry reaches the browser as an
// unhandled platform and degrades to nothing useful. Synology has an
// entry despite shipping as a native .spk rather than an OCI image
// (apps/synology/spk) because what the SPK wraps is the same binary, and
// the binary still has to say which host it believes it is on.
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

// The two ways a caller's identity can have been established. There is no
// third: either this product's own local-account service verified a
// password (apps/common/auth/local), or the platform did and told us over
// a header from a verified peer (apps/common/platform/profile's Gateway).
// An anonymous caller has no AuthMode at all, it has Authenticated:false,
// which is why there is no "none" here for a handler to accidentally
// treat as a mode it recognises.
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

// unsupportedAuthenticator and unsupportedNotifier are the null objects
// BasePlatformAdapter hands out. They are unexported so that no caller can
// type-assert for them: "is this platform capable" is a question answered
// by Capabilities(), and the failure is recognised with
// errors.Is(err, ErrCapabilityUnsupported). A caller that could reach the
// concrete type would start branching on it, and then a provider that
// supplies its own equally-unsupported implementation would take a
// different path for the same fact.
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

// Authenticator returns the null-object authenticator, which refuses every
// caller with ErrCapabilityUnsupported. A provider that genuinely
// authenticates overrides this; the value of the default is that
// forgetting to override is a typed refusal at the call site rather than a
// nil-interface panic, and a refusal to authenticate fails in the safe
// direction.
func (BasePlatformAdapter) Authenticator() Authenticator { return unsupportedAuthenticator{} }

// Notifier returns the null-object notifier, which fails every send with
// ErrCapabilityUnsupported rather than accepting the message and dropping
// it. Silently succeeding is the specific failure §22 is about: an
// operator who is never told is worse off than one whose alerting is
// visibly off, because the first has no way to find out.
func (BasePlatformAdapter) Notifier() Notifier { return unsupportedNotifier{} }
