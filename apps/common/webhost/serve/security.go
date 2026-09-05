// security.go holds the two hop-level protections issue #87 (B5.1) added
// to this package: the identity-header boundary, and the browser response
// headers the admin console is served with.
package serve

import (
	"net/http"

	"github.com/spdrman/rclone-manager/apps/common/platform/profile"
)

// StripUntrustedIdentity removes provider-native identity headers from a
// request whose own direct peer has not been proven to be a trusted
// platform gateway, BEFORE the request reaches anything that could read
// one — a handler, a reverse proxy, a log line.
//
// # Why this has to live at the hop and not only at the authenticator
//
// apps/common/platform/profile's CompiledGateway.Authenticate already
// refuses an identity from an untrusted peer, and that refusal is
// correct. It is also the wrong and only place for the check in the
// topology this product actually ships. container/compose.yaml runs two
// services from one image: the engine, with no published port, and the UI
// host, which serves the shared bundle and reverse proxies /api/v1 to the
// engine, and which is the only service on a LAN-facing port. The
// engine's only possible direct peer is therefore the UI host, so a
// gateway profile's trusted-peer range HAS to contain the UI host or the
// deployment cannot authenticate at all — at which point every request
// the UI host forwards arrives from a peer the engine trusts, and an
// identity header any LAN client set on the published port is believed.
// Deleting X-Forwarded-For at that hop, which the proxy already did, is
// the same defence applied to one header out of two that need it.
//
// gateway is the peer set this hop may believe. nil means this hop faces
// nothing it can prove is a gateway, so every identity header is removed
// from every request: that is the correct default for the browser-facing
// edge, and the reason a gateway deployment has to say where its gateway
// is at the hop that actually meets it rather than at one further in.
//
// # What this does not solve
//
// Under Docker's userland port publishing, traffic arriving at a
// published port can reach the container from the bridge gateway address
// regardless of who sent it, which collapses "the platform gateway" and
// "any LAN client" into one peer as far as RemoteAddr can tell. A gateway
// profile therefore has to be deployed so the gateway is the only thing
// that can reach the published port at all — bind it to loopback on the
// host, or put the gateway and the UI host on a network nothing else
// joins. Peer-based trust is a boundary, not a substitute for one.
func StripUntrustedIdentity(gateway *profile.CompiledGateway) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if gateway == nil {
				profile.StripIdentityHeaders(r.Header)
			} else {
				gateway.Sanitize(r.Header, r.RemoteAddr)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Browser response headers. Values rather than a computed policy: there
// is one admin console, it is served same-origin from one bundle, and a
// configurable security header is a security header somebody turns off.
const (
	// FrameOptions and FrameAncestors are the same statement twice, for
	// the same reason every other double-submit pair in this repository
	// exists: one of them is what a browser released this decade reads,
	// the other is what an older one reads, and neither is worth omitting
	// to save a header.
	//
	// The console drives every destructive route this product has behind
	// a same-origin session cookie. Framed invisibly over an attacker's
	// page, that session is driven by the operator's own cursor, and the
	// double-submit CSRF token does nothing about it: a framed
	// same-origin document reads its own cookie perfectly well.
	FrameOptions   = "DENY"
	FrameAncestors = "frame-ancestors 'none'"

	// ContentTypeOptions stops a browser from second-guessing a declared
	// Content-Type. The API's error documents echo caller-supplied text
	// in places (a rejected hostname, an unsupported action), and JSON
	// sniffed as HTML is where that becomes a script rather than a
	// string.
	ContentTypeOptions = "nosniff"

	// ReferrerPolicy keeps the console's own URLs, which name backup sets
	// and hosts, out of the Referer header of anything it links out to.
	ReferrerPolicy = "no-referrer"
)

// SecurityHeaders sets the browser response headers above on every
// response the handler it wraps produces.
//
// Deliberately NOT a full Content-Security-Policy. frame-ancestors
// constrains only who may embed this document, so it cannot break a
// bundle; a script-src or style-src directive can, and the shared UI's
// browser behaviour has never been exercised against the real runtime
// (its Playwright suite runs against createMockApi). Shipping a
// resource-loading policy on that evidence would be trading a real
// clickjacking fix for an unmeasured chance of a blank console. The rest
// of the policy belongs with the first browser test that runs against the
// real server.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", FrameOptions)
		h.Set("X-Content-Type-Options", ContentTypeOptions)
		h.Set("Referrer-Policy", ReferrerPolicy)
		if existing := h.Get("Content-Security-Policy"); existing == "" {
			h.Set("Content-Security-Policy", FrameAncestors)
		}
		next.ServeHTTP(w, r)
	})
}
