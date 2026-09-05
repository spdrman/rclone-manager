package local

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A truth table for one question: when may this process believe a header
// that says the connection was secure?
//
// The four cases exist because the answer depends on two independent
// inputs, real TLS and the trust flag, and three of the four combinations
// must come back false. The one that would be easiest to get wrong by
// accident is plaintext-plus-forged-header-without-trust: that is a caller
// who set X-Forwarded-Proto themselves, and a Secure cookie issued on the
// strength of it would be a claim of protection nothing is providing.

func TestRequestIsSecure_RealTLSIsAlwaysSecureRegardlessOfTrust(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.TLS = &tls.ConnectionState{}

	for _, trust := range []bool{true, false} {
		if !requestIsSecure(req, trust) {
			t.Errorf("requestIsSecure(realTLS, trustForwarded=%v) = false, want true", trust)
		}
	}
}

func TestRequestIsSecure_PlaintextWithoutTrustIsNeverSecure(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")

	// This is the whole point of defaulting TrustForwardedHeaders to
	// false: an untrusted caller can set this header to anything it
	// likes, and it must be completely ignored.
	if requestIsSecure(req, false) {
		t.Error("requestIsSecure(plaintext, trustForwarded=false) = true, want false even with a forged X-Forwarded-Proto: https header")
	}
}

// TestRequestIsSecure_TrustsForwardedProtoOnlyWhenEnabled is issue #119's
// review's central regression test for finding 4 (the Secure cookie flag
// can never be true behind the two-container split): with trust enabled,
// a plaintext request whose X-Forwarded-Proto says "https" (exactly what
// apps/common/webhost/serve.NewUI's reverse proxy sends, derived from the
// REAL browser<->web-ui connection, never from anything the browser
// itself sent - see ProxyRequest.SetXForwarded) must be treated as
// secure.
func TestRequestIsSecure_TrustsForwardedProtoOnlyWhenEnabled(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")

	if !requestIsSecure(req, true) {
		t.Error("requestIsSecure(plaintext, X-Forwarded-Proto=https, trustForwarded=true) = false, want true")
	}
}

func TestRequestIsSecure_TrustedButForwardedProtoIsHTTPIsNotSecure(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "http")

	if requestIsSecure(req, true) {
		t.Error("requestIsSecure(plaintext, X-Forwarded-Proto=http, trustForwarded=true) = true, want false")
	}
}

func TestRequestIsSecure_TrustedButHeaderAbsentIsNotSecure(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	if requestIsSecure(req, true) {
		t.Error("requestIsSecure(plaintext, no X-Forwarded-Proto, trustForwarded=true) = true, want false")
	}
}
