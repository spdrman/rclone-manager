package webhost

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/spdrman/rclone-manager/core/service"
)

// maxImportSSHKeyBodyBytes bounds POST /api/v1/ssh-keys' request body.
// The largest private key this project has any real reason to see (an
// RSA-4096 PEM) is a few kilobytes (core/internal/transport/rclone/
// keysource.go's identical maxResolvedKeySize reasoning); this is wide
// margin over that, not a realistic key-size ceiling.
const maxImportSSHKeyBodyBytes = 1 << 16 // 64 KiB

// maxHostKeyProbeBodyBytes and maxTestConnectionBodyBytes bound their own
// requests generously: both bodies are a handful of short fields, never
// anything resembling secret material.
const (
	maxHostKeyProbeBodyBytes   = 1 << 12 // 4 KiB
	maxTestConnectionBodyBytes = 1 << 16 // 64 KiB
)

// importSSHKeyRequest is POST /api/v1/ssh-keys' request body: the
// wizard's "Import key" step (#98), sent once, straight to the backend,
// per that step's own on-screen copy.
type importSSHKeyRequest struct {
	PrivateKeyPEM string `json:"private_key_pem"`
}

// importSSHKeyResponse never carries KeyFile (the server-side path
// service.SSHKeyRef resolves to) or the key material itself — see
// service.SSHKeyRef's doc for why the path stays server-side.
type importSSHKeyResponse struct {
	ID          string `json:"id"`
	Algorithm   string `json:"algorithm"`
	Fingerprint string `json:"fingerprint"`
}

// importSSHKey is POST /api/v1/ssh-keys: issue #146's SSH-key-import
// endpoint. State-changing but non-destructive (§50: "generate SSH
// key" — importing one is the same tier), so it is CSRF-protected
// (router.go) but not gated behind the destructive-ops gate.
func (h *handlers) importSSHKey(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportSSHKeyBodyBytes)

	var body importSSHKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxImportSSHKeyBodyBytes)
		return
	}
	if body.PrivateKeyPEM == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "private_key_pem is required")
		return
	}

	ref, err := h.backend.ImportSSHKey(r.Context(), []byte(body.PrivateKeyPEM))
	if err != nil {
		if errors.Is(err, service.ErrInvalidRequest) {
			// Safe to echo: rclone.ValidateImportedPrivateKey's own doc
			// guarantees this never includes the key bytes themselves,
			// only the shape of the problem ("empty", "passphrase-
			// protected", "does not parse", ...).
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to import SSH key")
		return
	}

	writeJSON(w, http.StatusCreated, importSSHKeyResponse{
		ID:          ref.ID,
		Algorithm:   ref.Algorithm,
		Fingerprint: ref.Fingerprint,
	})
}

// hostKeyProbeRequest is POST /api/v1/ssh/host-key-probe's request body:
// the wizard's "Verify server" step asking "what host key does this
// server offer right now".
type hostKeyProbeRequest struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type hostKeyProbeResponse struct {
	Algorithm      string `json:"algorithm"`
	Fingerprint    string `json:"fingerprint"`
	KnownHostsLine string `json:"known_hosts_line"`
}

// probeHostKey is POST /api/v1/ssh/host-key-probe: issue #146's
// host-key-probe endpoint. Read-only in the destructive-gate sense (§50
// lists "probe host key" under read-only/low-risk actions) — it never
// trusts or persists anything — but it DOES open a real outbound SSH
// connection to a caller-supplied host:port, which is exactly the side
// effect CSRF protection exists for; this route carries requireCSRF
// (router.go) for that reason (mandatory review finding M5, PR #155).
func (h *handlers) probeHostKey(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxHostKeyProbeBodyBytes)

	var body hostKeyProbeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxHostKeyProbeBodyBytes)
		return
	}
	if body.Host == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "host is required")
		return
	}

	probe, err := h.backend.ProbeHostKey(r.Context(), body.Host, body.Port)
	if err != nil {
		// A probe failure (unreachable host, no SSH service on that port,
		// a timeout, ...) is an ordinary, expected outcome of an operator
		// typing a wrong hostname mid-wizard, not an internal error —
		// reported as 400/HOST_KEY_PROBE_FAILED with err's own message,
		// which internal/transport/rclone.ProbeHostKey's doc guarantees
		// only ever describes the connection attempt itself (dial/
		// handshake failure text), never anything from this process's own
		// internals or filesystem.
		writeError(w, http.StatusBadRequest, "HOST_KEY_PROBE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, hostKeyProbeResponse{
		Algorithm:      probe.Algorithm,
		Fingerprint:    probe.Fingerprint,
		KnownHostsLine: probe.KnownHostsLine,
	})
}

// testConnectionRequest is POST /api/v1/backup-sets/test-connection's
// request body: a candidate source, checked before anything is
// persisted. Every field mirrors backupSetRequest's own SSH-facing
// fields (handlers_backupsets.go) exactly, since this is meant to be
// called with the same values a subsequent POST /api/v1/backup-sets
// would carry.
type testConnectionRequest struct {
	Host           string `json:"host"`
	Port           int    `json:"port"`
	User           string `json:"user"`
	SSHKeyID       string `json:"ssh_key_id"`
	KnownHostsLine string `json:"known_hosts_line"`
	RemotePath     string `json:"remote_path"`
}

type testConnectionResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// testConnection is POST /api/v1/backup-sets/test-connection: issue
// #146's connection-test endpoint. Read-only in the destructive-gate
// sense (§50: "test SSH") — it writes nothing anywhere — but it lists a
// remote path over a real SFTP session against a caller-supplied
// host:port, the same real-outbound-connection side effect probeHostKey
// above has; it carries requireCSRF (router.go) for the same reason
// (mandatory review finding M5, PR #155).
func (h *handlers) testConnection(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTestConnectionBodyBytes)

	var body testConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxTestConnectionBodyBytes)
		return
	}

	result, err := h.backend.TestConnection(r.Context(), service.ConnectionTestRequest{
		Host:           body.Host,
		Port:           body.Port,
		User:           body.User,
		SSHKeyID:       body.SSHKeyID,
		KnownHostsLine: body.KnownHostsLine,
		RemotePath:     body.RemotePath,
	})
	if err != nil {
		if errors.Is(err, service.ErrInvalidRequest) {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		if errors.Is(err, service.ErrSSHKeyNotFound) {
			writeError(w, http.StatusBadRequest, "SSH_KEY_NOT_FOUND", "the referenced ssh_key_id does not exist; import a key first")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to test connection")
		return
	}

	writeJSON(w, http.StatusOK, testConnectionResponse{OK: result.OK, Message: result.Message})
}
