package serve

import (
	"net/http"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
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

	BinaryVersion string
	Commit        string
}

// NewEngine composes cfg into the engine container's whole HTTP surface:
// optional local/native authentication routes plus the versioned /api/v1
// API, and nothing else - no static UI (see NewUI for that half of the
// two-container split this package's doc comment describes).
func NewEngine(cfg EngineConfig) http.Handler {
	apiRouter := webhost.NewRouter(webhost.RouterConfig{
		Platform:      cfg.Platform,
		Backend:       cfg.Backend,
		Gate:          cfg.Gate,
		BinaryVersion: cfg.BinaryVersion,
		Commit:        cfg.Commit,
	})

	mux := http.NewServeMux()
	if cfg.AuthRoutes != nil {
		mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", cfg.AuthRoutes))
	}
	mux.Handle("/health/", apiRouter)
	mux.Handle("/api/v1/", apiRouter)

	return local.EnsureCSRFCookie(cfg.TrustForwardedHeaders)(mux)
}
