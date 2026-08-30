package csrf

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnsureCookie_IssuesACookieWhenNoneIsPresent(t *testing.T) {
	handler := EnsureCookie(func(*http.Request) bool { return false })(http.NotFoundHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != CookieName {
		t.Fatalf("cookies = %v, want exactly one %q cookie", cookies, CookieName)
	}
	if cookies[0].Value == "" {
		t.Error("issued cookie value is empty")
	}
}

func TestEnsureCookie_DoesNotReissueAnExistingCookie(t *testing.T) {
	handler := EnsureCookie(func(*http.Request) bool { return false })(http.NotFoundHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "already-have-one"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("cookies = %v, want none (a request that already carries one must not get a fresh Set-Cookie)", cookies)
	}
}

func TestEnsureCookie_SecureFlagComesFromTheCallback(t *testing.T) {
	for _, want := range []bool{true, false} {
		handler := EnsureCookie(func(*http.Request) bool { return want })(http.NotFoundHandler())
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		cookies := rec.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("cookies = %v, want exactly one", cookies)
		}
		if cookies[0].Secure != want {
			t.Errorf("Secure = %v, want %v (from the secure callback)", cookies[0].Secure, want)
		}
	}
}

func TestVerify_MissingCookieIsErrMissingCookie(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(HeaderName, "some-value")

	if err := Verify(req); !errors.Is(err, ErrMissingCookie) {
		t.Fatalf("Verify with no cookie = %v, want errors.Is(err, ErrMissingCookie)", err)
	}
}

func TestVerify_MissingHeaderIsErrHeaderMismatch(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "token-abc"})

	if err := Verify(req); !errors.Is(err, ErrHeaderMismatch) {
		t.Fatalf("Verify with no header = %v, want errors.Is(err, ErrHeaderMismatch)", err)
	}
}

func TestVerify_MismatchedHeaderIsErrHeaderMismatch(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "token-abc"})
	req.Header.Set(HeaderName, "not-the-same-value")

	if err := Verify(req); !errors.Is(err, ErrHeaderMismatch) {
		t.Fatalf("Verify with mismatched header = %v, want errors.Is(err, ErrHeaderMismatch)", err)
	}
}

func TestVerify_MatchingCookieAndHeaderSucceeds(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "token-abc"})
	req.Header.Set(HeaderName, "token-abc")

	if err := Verify(req); err != nil {
		t.Fatalf("Verify with matching cookie/header = %v, want nil", err)
	}
}
