package webhost

import (
	"net/http/httptest"
	"testing"
)

// TestWriteError_SetsCorrelationIdHeader is issue #119's review finding
// that ui/shared/src/api/client.ts already reads X-Correlation-Id off
// every non-2xx response, but neither of this repository's two
// error-writing paths ever actually sent it - made concrete for this
// package's own path.
func TestWriteError_SetsCorrelationIdHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, 400, "SOME_CODE", "a message")

	if got := rec.Header().Get("X-Correlation-Id"); got == "" {
		t.Error("X-Correlation-Id header is empty, want a generated correlation id on every error response")
	}
}

func TestWriteConfigRevisionStale_SetsCorrelationIdHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	writeConfigRevisionStale(rec, "stale", "rev-2")

	if got := rec.Header().Get("X-Correlation-Id"); got == "" {
		t.Error("X-Correlation-Id header is empty, want a generated correlation id on every error response")
	}
}
