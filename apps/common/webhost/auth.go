package webhost

import (
	"context"
	"net/http"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
)

// actorContextKey is an unexported type so this package's context value
// can never collide with, or be set by, any other package's key.
type actorContextKey struct{}

// actorFromContext returns the authenticated caller's identity, as
// resolved by authMiddleware below. Empty if called from anywhere that
// did not go through that middleware — every handler in this package
// does, since NewRouter wires it across the whole /api/v1 group (see
// router.go).
func actorFromContext(ctx context.Context) string {
	actor, _ := ctx.Value(actorContextKey{}).(string)
	return actor
}

// authMiddleware requires a caller to be authenticated, using whatever
// Authenticator platform.Authenticator() returns, before letting a
// request reach the handler it wraps. This is the whole of this package's
// auth abstraction: it never implements a credential check itself (that
// is #106's reserved apps/common/auth/local for the local-auth mode, or
// #92 for platform-auth), it only enforces that SOME Authenticator was
// consulted and said yes.
//
// A nil platform, or a nil Authenticator() (still a legal
// capabilities.PlatformAdapter return value — see that package's own
// doc), is treated as "deny", the same as an Authenticator that returns
// Authenticated: false: there is no code path in this function that lets
// a request through without an explicit, affirmative Authenticated: true.
// This is what makes the whole /api/v1 surface fail closed the moment a
// provider has not wired a real Authenticator in yet, which today is
// every provider (see router_test.go's noAuthWiredAdapter).
func authMiddleware(platform capabilities.PlatformAdapter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if platform == nil {
				writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "no platform adapter is configured")
				return
			}
			authenticator := platform.Authenticator()
			if authenticator == nil {
				writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "no authenticator is configured for this platform")
				return
			}

			authCtx, err := authenticator.Authenticate(r.Context(), capabilities.AuthRequest{
				Headers:    r.Header,
				RemoteAddr: r.RemoteAddr,
			})
			if err != nil || !authCtx.Authenticated {
				writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
				return
			}

			ctx := context.WithValue(r.Context(), actorContextKey{}, authCtx.Username)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// requireDestructiveGate refuses a request unless gate.Passed() reports
// true, independent of and applied after authentication (see router.go's
// route wiring): a caller must be told who they are before being told
// whether the server is even willing to consider destructive operations
// from anyone yet.
func requireDestructiveGate(gate DestructiveGate) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if gate == nil || !gate.Passed() {
				writeError(w, http.StatusForbidden, "DESTRUCTIVE_OPERATIONS_DISABLED",
					"destructive operations are disabled until the trusted-proxy authentication gate (issue #92) has been verified for this deployment")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
