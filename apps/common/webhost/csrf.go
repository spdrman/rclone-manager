package webhost

import (
	"errors"
	"net/http"

	"github.com/spdrman/rclone-manager/apps/common/csrf"
)

// This package verifies CSRF tokens and never issues them.
//
// That split is the thing to know before reading the function below. The
// cookie has to exist before any request can echo it, and the response
// that seeds it is usually the very first page load, which this package
// does not serve; whichever caller composes the outer handler is
// responsible for wrapping EnsureCSRFCookie around everything, and
// apps/common/webhost/serve does exactly that for both of its handlers.
// The failure mode when somebody forgets is a uniform 403 on every
// mutating route, which reads like a bug in this file and is not.
//
// The check itself lives in apps/common/csrf because
// apps/common/auth/local needs the identical one, and one of the two
// packages having a slightly different idea of what counts as a match is
// the drift this arrangement exists to make impossible. What stays here is
// only the translation into this package's error envelope, which is not
// the same as that package's.

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
