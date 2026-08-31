package serve

import (
	"context"
	"net/http"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
	"github.com/spdrman/rclone-manager/apps/common/platform/profile"
	"github.com/spdrman/rclone-manager/apps/common/webhost"
)

// EngineConfig is everything NewEngine needs to build one provider's
// engine-container HTTP surface.
type EngineConfig struct {
	// Platform supplies both the Authenticator every /api/v1 request is
	// checked against and the capabilities GET /api/v1/system/capabilities
	// reports - forwarded to webhost.NewRouter unchanged
	// (docs/EPIC-B-multi-nas.md §3.4). This is the one seam that lets a
	// provider with no local authentication of its own (a future
	// native-session platform) reuse NewEngine without ever depending on
	// apps/common/auth/local, or on any other provider's own
	// PlatformAdapter implementation.
	Platform capabilities.PlatformAdapter

	// AuthRoutes, when non-nil, is mounted at /api/v1/auth/ with that
	// prefix stripped - e.g. apps/common/auth/local.Service.Handler() for
	// a provider using local authentication. A provider whose
	// Authenticator never needs a login/enroll/logout HTTP surface of its
	// own (e.g. one that authenticates purely off a trusted-proxy
	// identity header) leaves this nil: no /api/v1/auth/* route is
	// registered at all, rather than NewEngine panicking on a nil
	// http.Handler.
	AuthRoutes http.Handler

	// TrustForwardedHeaders controls the CSRF cookie's Secure flag issued
	// in front of this handler, the same way
	// apps/common/auth/local.Config.TrustForwardedHeaders controls the
	// session cookie's own Secure flag - see that field's doc for exactly
	// when this is safe to set true. Kept as its own explicit field,
	// rather than read off Platform or AuthRoutes, so a caller can never
	// end up with the CSRF cookie's trust setting silently out of sync
	// with whatever Authenticator/AuthRoutes it actually wired up (a
	// caller using apps/common/auth/local should always pass its
	// Service's own TrustForwardedHeaders() here, exactly as
	// apps/generic/cmd/backup-manager-web does).
	TrustForwardedHeaders bool

	// Backend is the core/service.BackupService adapter (or a test
	// double) every apps/common/webhost handler ultimately calls into.
	Backend webhost.BackupServiceClient

	// Gate decides whether POST /api/v1/operations may run; nil means
	// webhost.NewRouter's own NotYetImplementedGate default.
	Gate webhost.DestructiveGate

	// FirstRun is the setup surface of an instance that may have no
	// configuration yet (issue #176). Set it, leave Backend nil, and
	// build the engine with NewFirstRunEngine (firstrun.go) rather than
	// NewEngine: that is what makes a fresh app-store install serve a
	// setup flow instead of refusing to start.
	FirstRun webhost.FirstRunClient

	// Activate opens a real backend against the configuration the
	// first-run flow has just written, and returns it together with the
	// cleanup that releases it (the same pair core/service.Open already
	// returns, which is what a provider passes straight through). Called
	// at most once, and only by a FirstRunEngine.
	Activate func(ctx context.Context) (webhost.BackupServiceClient, func() error, error)

	BinaryVersion string
	Commit        string
}

// NewEngine composes cfg into the engine container's whole HTTP surface:
// optional local/native authentication routes plus the versioned /api/v1
// API, and nothing else - no static UI (see NewUI for that half of the
// two-container split this package's doc comment describes).
func NewEngine(cfg EngineConfig) http.Handler {
	mustIdentityBoundary(cfg)
	return newEngineHandler(cfg, cfg.Backend, nil)
}

// mustIdentityBoundary resolves the trusted-peer boundary cfg's adapter
// authenticates against, and refuses a deployment that declares native
// sessions but resolves none (issue #87's review, M2).
//
// The nil is SAFE in the security direction and total in the functional
// one: StripUntrustedIdentity reads it as "trust nobody", so a
// native-session deployment would authenticate nobody, with no message
// anywhere saying why. That is exactly the operator experience the
// serve-ui startup refusal was added to prevent one hop over, so this hop
// refuses the same way rather than starting into a console nobody can
// sign in to.
//
// A panic and not an error return because there is no request in flight
// and nothing to degrade to: this is process wiring, it is wrong before a
// listener is open, and every shipped caller builds its adapter through
// profile.Profile.Adapter, which cannot produce this state. The way an
// adapter that decorates its Authenticator satisfies this is
// IdentityBoundaryCarrier below, not deleting the check.
//
// It is called from the two CONSTRUCTORS, NewEngine and NewFirstRunEngine,
// rather than from newEngineHandler which they share. FirstRunEngine
// builds a second handler from the same cfg during activation, on a live
// request, and a panic there would take the process down at the moment an
// operator submits their first configuration. The inputs are cfg.Platform
// both times, so construction-time is the same answer, earlier.
func mustIdentityBoundary(cfg EngineConfig) *profile.CompiledGateway {
	boundary := gatewayOf(cfg.Platform)
	if cfg.Platform != nil && cfg.Platform.Capabilities().NativeAuth && boundary == nil {
		// Fail closed, loudly, at construction (issue #87's review, M2).
		panic("serve: NewEngine: this platform adapter declares NativeAuth but no trusted-peer boundary resolves from its Authenticator, " +
			"so every identity header would be stripped and nobody could authenticate; " +
			"implement serve.IdentityBoundaryCarrier on the adapter or its Authenticator")
	}
	return boundary
}

// IdentityBoundaryCarrier is how a platform adapter, or an Authenticator
// that decorates one, DECLARES the trusted-peer boundary it enforces.
//
// It exists because the alternative was discovery by type assertion, and
// a discarded one at that: `gw, _ := platform.Authenticator().(*profile.
// CompiledGateway)` answers nil for every authenticator that is not
// literally that concrete type, and nil means strip-everything. Wrapping
// is not hypothetical. A gateway with a local fallback is what the
// first-run auth bootstrap wants, and audit, metrics and rate-limit
// decorators are the usual next step; each of them would silently turn a
// working gateway deployment into one that authenticates nobody.
//
// Declared rather than widened into capabilities.PlatformAdapter on
// purpose: that interface is implemented directly by six providers, and
// adding a method to it makes every one of them change to say something
// only a native-session profile has an answer for. An optional interface
// costs one type switch here and nothing at all to an adapter that has no
// gateway.
type IdentityBoundaryCarrier interface {
	// IdentityBoundary returns the peer set this adapter's Authenticator
	// will believe an identity header from, or nil when it believes none.
	IdentityBoundary() *profile.CompiledGateway
}

// gatewayOf reports the trusted-peer boundary the running profile
// authenticates against, or nil for a profile that has none.
//
// It is READ OFF the adapter's own Authenticator rather than configured
// separately, and that is the point: the thing that decides whether to
// believe an identity header and the thing that decides whether to strip
// one have to be the same object, or a deployment eventually ends up with
// an engine stripping against one peer list and authenticating against
// another. A second EngineConfig field would have made that drift
// possible; there is nothing here to set inconsistently.
//
// nil is never "skip the check". StripUntrustedIdentity reads nil as "no
// peer is trusted at this hop", which for a profile with no gateway is
// exactly right: a generic-profile engine has no reason to forward a UGOS
// identity header inwards, and every reason not to on the day somebody
// restarts it with --profile=ugos.
func gatewayOf(platform capabilities.PlatformAdapter) *profile.CompiledGateway {
	if platform == nil {
		return nil
	}
	// The declared answer first, asked of the adapter and then of the
	// authenticator it hands out, so a decorated Authenticator can still
	// say what it enforces. The concrete type is the fallback rather than
	// the mechanism: it is what profile.Profile.Adapter returns today,
	// and it keeps this working for an adapter written before the
	// interface existed.
	if carrier, ok := platform.(IdentityBoundaryCarrier); ok {
		return carrier.IdentityBoundary()
	}
	auth := platform.Authenticator()
	if carrier, ok := auth.(IdentityBoundaryCarrier); ok {
		return carrier.IdentityBoundary()
	}
	if gw, ok := auth.(*profile.CompiledGateway); ok {
		return gw
	}
	return nil
}
