package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

func postSSHKeyImport(t *testing.T, router http.Handler, body string, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ssh-keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		attachValidCSRF(req)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestImportSSHKey_Success_Returns201WithFingerprintNeverTheKey is the
// RED plan's response-contract case for the import endpoint: it must
// report id/algorithm/fingerprint and must NEVER echo the key material
// or a filesystem path back to the caller (service.SSHKeyRef's own doc:
// the server-side path stays server-side).
func TestImportSSHKey_Success_Returns201WithFingerprintNeverTheKey(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	pem := "-----BEGIN OPENSSH PRIVATE KEY-----\nFAKETESTKEYMATERIAL\n-----END OPENSSH PRIVATE KEY-----"
	rec := postSSHKeyImport(t, tr.router, `{"private_key_pem":"`+strings.ReplaceAll(pem, "\n", "\\n")+`"}`, true)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["id"] == "" || body["id"] == nil {
		t.Error("id is missing/empty")
	}
	if body["fingerprint"] == "" || body["fingerprint"] == nil {
		t.Error("fingerprint is missing/empty")
	}
	if _, hasKeyFile := body["key_file"]; hasKeyFile {
		t.Error("response leaked key_file (a server-side filesystem path)")
	}
	if strings.Contains(rec.Body.String(), "FAKETESTKEYMATERIAL") {
		t.Error("response echoed the imported key material back")
	}
}

func TestImportSSHKey_EmptyBodyReturns400(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := postSSHKeyImport(t, tr.router, `{"private_key_pem":""}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestImportSSHKey_InvalidKeyFromBackendReturns400(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	tr.backend.errOnImport = service.ErrInvalidRequest
	rec := postSSHKeyImport(t, tr.router, `{"private_key_pem":"not a key"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestImportSSHKey_MissingCSRFCookieReturns403(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := postSSHKeyImport(t, tr.router, `{"private_key_pem":"x"}`, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

func TestImportSSHKey_RequiresAuthentication(t *testing.T) {
	backend := newBackupSetFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      noAuthWiredAdapter{},
		Backend:       backend,
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})
	rec := postSSHKeyImport(t, router, `{"private_key_pem":"x"}`, true)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func postHostKeyProbe(t *testing.T, router http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ssh/host-key-probe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestProbeHostKey_Success_ReturnsFingerprintAndKnownHostsLine covers
// probeHostKey's response contract: the wizard's "Verify server" step
// needs both a human-facing fingerprint and the exact known_hosts line
// it will later carry into CreateBackupSet.
func TestProbeHostKey_Success_ReturnsFingerprintAndKnownHostsLine(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := postHostKeyProbe(t, tr.router, `{"host":"prod-db-01.internal","port":22}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["fingerprint"] == "" || body["fingerprint"] == nil {
		t.Error("fingerprint is missing/empty")
	}
	if body["known_hosts_line"] == "" || body["known_hosts_line"] == nil {
		t.Error("known_hosts_line is missing/empty")
	}
}

func TestProbeHostKey_MissingHostReturns400(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := postHostKeyProbe(t, tr.router, `{"port":22}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestProbeHostKey_ProbeFailureReturns400WithItsOwnCode proves an
// unreachable/misconfigured host is reported as an ordinary 400
// (HOST_KEY_PROBE_FAILED), not a 500 — probing a host the operator typoed
// mid-wizard is an expected outcome, not a server fault.
func TestProbeHostKey_ProbeFailureReturns400WithItsOwnCode(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	tr.backend.errOnProbe = errBoom
	rec := postHostKeyProbe(t, tr.router, `{"host":"unreachable.invalid","port":22}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "HOST_KEY_PROBE_FAILED" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "HOST_KEY_PROBE_FAILED")
	}
}

// TestProbeHostKey_DoesNotRequireCSRF proves this route is deliberately
// exempt (docs/EPIC-B-multi-nas.md §50: "probe host key" is read-only) —
// unlike createBackupSet/importSSHKey, no CSRF cookie is attached here at
// all, and the request must still succeed.
func TestProbeHostKey_DoesNotRequireCSRF(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := postHostKeyProbe(t, tr.router, `{"host":"prod-db-01.internal","port":22}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (read-only per §50, no CSRF token attached), body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestProbeHostKey_RequiresAuthentication(t *testing.T) {
	backend := newBackupSetFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      noAuthWiredAdapter{},
		Backend:       backend,
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})
	rec := postHostKeyProbe(t, router, `{"host":"prod-db-01.internal","port":22}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func postTestConnection(t *testing.T, router http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backup-sets/test-connection", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

const validTestConnectionBody = `{
	"host": "prod-db-01.internal",
	"port": 22,
	"user": "backup-agent",
	"ssh_key_id": "key_test_1",
	"known_hosts_line": "prod-db-01.internal ssh-ed25519 AAAAfaketest",
	"remote_path": "/backups/postgresql"
}`

func TestTestConnection_Success_ReturnsOKTrue(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := postTestConnection(t, tr.router, validTestConnectionBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
}

// TestTestConnection_Failure_ReturnsOKFalseNotAnHTTPError proves a failed
// connection test is reported as a normal 200 with ok:false, not an HTTP
// error status: it is an expected outcome (bad host/key), and the
// wizard's pre-save flow needs to render it inline, not catch an
// exception.
func TestTestConnection_Failure_ReturnsOKFalseNotAnHTTPError(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	tr.backend.connectionResult = service.ConnectionTestResult{OK: false, Message: "could not connect and list the remote path"}
	rec := postTestConnection(t, tr.router, validTestConnectionBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["ok"] != false {
		t.Errorf("ok = %v, want false", body["ok"])
	}
	if body["message"] == "" || body["message"] == nil {
		t.Error("message is missing/empty on a failed connection test")
	}
}

func TestTestConnection_SSHKeyNotFoundReturns400(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	tr.backend.errOnConnect = service.ErrSSHKeyNotFound
	rec := postTestConnection(t, tr.router, validTestConnectionBody)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestTestConnection_DoesNotRequireCSRF(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := postTestConnection(t, tr.router, validTestConnectionBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (read-only per §50, no CSRF token attached), body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestTestConnection_RequiresAuthentication(t *testing.T) {
	backend := newBackupSetFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      noAuthWiredAdapter{},
		Backend:       backend,
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})
	rec := postTestConnection(t, router, validTestConnectionBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}
