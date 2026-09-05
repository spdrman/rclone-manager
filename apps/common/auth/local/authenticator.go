// The read half of a session: given a request, is somebody signed in, and
// who.
//
// It is separate from session.go because the two answer to different
// callers with different shapes. session.go serves this package's own HTTP
// handlers, which hold a *http.Request; this file serves
// apps/common/webhost's authMiddleware through capabilities.Authenticator,
// which was deliberately given only headers and a remote address so that
// the contract does not drag net/http's whole request type across the
// provider seam. The cost of that narrowing is paid here, once, in
// cookieValue.
//
// Nothing in this file can create or extend a session, only recognise one.
// That is what lets webhost consult it on every single /api/v1 request
// without any risk of a read silently refreshing a credential.
package local

import (
	"context"
	"net/http"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
)

// sessionAuthenticator adapts sessionManager's cookie-based session
// lookup to capabilities.Authenticator, the seam apps/common/webhost's
// authMiddleware consults for every /api/v1 request (see that package's
// auth.go). It never checks a username/password itself - only whether
// the caller's session cookie currently names a live session; Login and
// Enroll (handler.go) are what establish one in the first place.
type sessionAuthenticator struct {
	sessions *sessionManager
}

// Authenticate implements capabilities.Authenticator.
func (a sessionAuthenticator) Authenticate(_ context.Context, r capabilities.AuthRequest) (capabilities.AuthContext, error) {
	token := cookieValue(r.Headers.Get("Cookie"), SessionCookieName)
	username, ok := a.sessions.lookup(token)
	if !ok {
		return capabilities.AuthContext{}, nil
	}
	return capabilities.AuthContext{
		Authenticated: true,
		Username:      username,
		Mode:          capabilities.AuthModeLocalAccount,
	}, nil
}

// cookieValue parses name's value out of a raw Cookie header string.
// capabilities.AuthRequest carries only http.Header (not a
// *http.Request), so this can't call (*http.Request).Cookie directly;
// building a throwaway *http.Request around just that one header lets
// this reuse net/http's own cookie-parsing logic (quoting, multiple
// cookies, etc.) instead of re-implementing it.
func cookieValue(cookieHeader, name string) string {
	header := http.Header{}
	if cookieHeader != "" {
		header.Set("Cookie", cookieHeader)
	}
	req := &http.Request{Header: header}
	c, err := req.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}
