// Package capabilities defines the PlatformCapabilities/PlatformAdapter
// contract every provider app composes over (docs/EPIC-B-multi-nas.md
// §3.4). It has no dependency on core: the engine (core/) is not aware
// providers exist (§7.1 — core imports nothing from apps/), and this
// package is not aware backup-lifecycle internals exist either. It is a
// pure declaration of what a NAS platform can and cannot do for the
// product running on top of it.
package capabilities

import "context"

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
	ID         string
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
	Mode             string // "local-account" | "native-session"
	SessionExpiresAt string // RFC3339; empty when not applicable.
}

// AuthRequest is intentionally narrow: an Authenticator needs the signals
// a trusted-proxy or local-session adapter uses to identify a caller, not
// a dependency on net/http or any other transport-layer type.
type AuthRequest struct {
	Headers    map[string]string
	RemoteAddr string
}

// Authenticator resolves the identity of the caller for a provider that
// supplies (or emulates) authentication.
type Authenticator interface {
	Authenticate(ctx context.Context, r AuthRequest) (AuthContext, error)
}

// Notifier delivers an operator-facing notification through whatever
// channel the platform offers natively. A PlatformAdapter that reports
// NativeNotifications: false MUST return a nil Notifier from Notifier()
// rather than one that silently drops messages (§22 — unsupported
// capabilities are explicit, never emulated).
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
	ID() string
	Capabilities() PlatformCapabilities
	// Authenticator returns nil when Capabilities().NativeAuth is false.
	Authenticator() Authenticator
	// Notifier returns nil when Capabilities().NativeNotifications is false.
	Notifier() Notifier
	PlatformInfo(ctx context.Context) (PlatformInfo, error)
}
