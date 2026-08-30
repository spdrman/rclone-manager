package webhost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSubmitOperation_SurvivesClientDisconnect is issue #94's INTEGRATION
// requirement: "Run the disconnect test against a real HTTP server
// (httptest.Server or equivalent), not an in-process fake, to prove
// request lifetime really is decoupled from operation lifetime."
//
// httptest.NewServer, unlike httptest.NewRecorder, actually listens on a
// real TCP socket and drives requests through the real net/http server
// loop, so Request.Context() behaves exactly as it would on a real
// deployment: it is genuinely canceled once ServeHTTP returns or the
// client's connection goes away — net/http's documented behavior, not
// this test's assumption. A handler that (incorrectly) started its
// background work with r.Context() instead of a context independent of
// the request would have that work canceled almost immediately, well
// before this test's deliberate delay elapses; asyncFakeBackend's own
// goroutine, gated on a channel this test controls, is what turns that
// into a deterministic assertion instead of a timing-dependent one.
//
// asyncFakeBackend, not core/service's real BackupService, provides the
// operation execution here. That keeps this test about the ONE thing it
// is actually responsible for proving — that webhost's own HTTP handling
// never re-couples an already-decoupled operation back to the request
// that submitted it — deterministically and without standing up a real
// SQLite journal and rclone transport. core/service's own test suite
// (core/service/service_test.go) separately proves BackupService itself
// keeps its execution goroutine on context.Background(); see this
// package's introducing PR description for that split.
func TestSubmitOperation_SurvivesClientDisconnect(t *testing.T) {
	backend := newAsyncFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       backend,
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})

	srv := httptest.NewServer(router)
	defer srv.Close()

	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, srv.URL+"/api/v1/operations",
		strings.NewReader(`{"action":"run_cycle","config_revision":"rev-1"}`))
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "idem-disconnect")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("submit request: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	var submitted map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&submitted); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	_ = resp.Body.Close()
	operationID, _ := submitted["operation_id"].(string)
	if operationID == "" {
		t.Fatal("submitted operation_id is empty")
	}
	if submitted["status"] != "queued" {
		t.Fatalf("status = %v, want %q (must be persisted before this test disconnects anything)", submitted["status"], "queued")
	}

	// Kill the client connection now that the response has been received,
	// standing in for a browser tab closing right after submitting: cancel
	// the request's own context AND force-close every connection the test
	// server's client pool holds, so nothing is left for the operation's
	// background work to still be attached to.
	cancel()
	srv.CloseClientConnections()

	// Give the server every chance to have canceled anything reachable
	// from that now-dead connection before letting the operation's
	// background work actually proceed.
	time.Sleep(20 * time.Millisecond)
	backend.release()

	deadline := time.Now().Add(2 * time.Second)
	for {
		op, err := backend.GetOperation(context.Background(), operationID)
		if err == nil && op.Status == "completed" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %q never completed after the client disconnected (last status %q, err %v)", operationID, op.Status, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
