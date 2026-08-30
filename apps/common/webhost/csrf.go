package webhost

import (
	"errors"
	"net/http"

	"github.com/spdrman/rclone-manager/apps/common/csrf"
)

// requireCSRF refuses a mutating request unless it carries a valid
// double-submit CSRF token (apps/common/csrf.Verify) - the same
// primitive apps/common/auth/local applies to its own login/enroll/logout
// routes. Before this, POST /api/v1/operations had no CSRF check of its
// own at all: harmless only because NotYetImplementedGate (gate.go)
// always denies it first, and a gap issue #119's review flagged
// specifically because there was no SHARED CSRF primitive this package
// (or a future non-local-auth backend) could reach for - verification
// lived privately inside apps/common/auth/local's own route table.
//
// This package only ever verifies, never issues: the cookie itself is set
// by whichever EnsureCookie/EnsureCSRFCookie call wraps the outermost
// handler for a given deployment (apps/common/webhost/serve does this, for
// both NewEngine and NewUI, using apps/common/auth/local's own
// EnsureCSRFCookie - this package doesn't need its own issuance path as
// long as SOME caller upstream of it guarantees one).
func requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := csrf.Verify(r)
		switch {
		case err == nil:
			next.ServeHTTP(w, r)
		case errors.Is(err, csrf.ErrMissingCookie):
			writeError(w, http.StatusForbidden, "CSRF_TOKEN_MISSING", "missing CSRF cookie; reload the page and try again")
		default:
			writeError(w, http.StatusForbidden, "CSRF_TOKEN_MISMATCH", "missing or mismatched "+csrf.HeaderName+" header")
		}
	})
}
