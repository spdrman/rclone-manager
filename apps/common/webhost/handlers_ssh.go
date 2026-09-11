package webhost

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/backupdproject/backupd/core/service"
)

// The wizard's SSH steps: import a key, probe a host key, test a
// connection.
//
// Importing is two routes rather than one, because there are two ways to
// end up holding a key reference and they are different acts. POST
// /ssh-keys takes pasted material this host has never seen. POST
// /ssh-keys/from-candidate takes an opaque handle for a key this host
// already found, and no material crosses the network at all. They answer
// with the same response, so a client keeps one result type.
//
// What they have in common is that none of them ever puts key material
// back on the wire. An import answers with a reference; the server-side
// path the reference resolves to does not appear in the response either,
// because a path is a hint about the host's filesystem that a browser has
// no use for.
//
// The two probes look like reads and are not treated as reads. Each opens
// a real outbound TCP or SSH connection to a host and port the caller
// chose, which makes this server into a network-probing instrument aimed
// at whatever is reachable from it, and that is a side effect worth a CSRF
// token even though nothing is persisted. They used to be listed as
// CSRF-exempt, and that was the gap.
//
// The body limits differ per route on purpose: a pasted private key is
// the only thing here that legitimately runs to kilobytes, so the probes
// and the candidate selection get much tighter ceilings rather than
// inheriting one generous number.

// maxImportSSHKeyBodyBytes bounds POST /api/v1/ssh-keys' request body.
// The largest private key this project has any real reason to see (an
// RSA-4096 PEM) is a few kilobytes (core/internal/transport/rclone/
// keysource.go's identical maxResolvedKeySize reasoning); this is wide
// margin over that, not a realistic key-size ceiling.
const maxImportSSHKeyBodyBytes = 1 << 16 // 64 KiB

// maxImportSSHKeyFromCandidateBodyBytes bounds POST
// /api/v1/ssh-keys/from-candidate's request body, and it is three orders
// of magnitude tighter than its sibling above on purpose. That body
// carries a private key; this one carries one opaque handle, so it gets
// the probe routes' ceiling rather than inheriting the key-sized one. A
// request that arrives here holding kilobytes is not a key selection.
const maxImportSSHKeyFromCandidateBodyBytes = 1 << 12 // 4 KiB

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
//
// Passphrase is optional (#269): "" is every request before this field
// existed, and means PrivateKeyPEM is not passphrase-protected. When set,
// it is checked against PrivateKeyPEM right here, at import time -- see
// service.BackupService.ImportSSHKey's own doc for why that check exists
// and what it guarantees against a key.file/key.passphrase configuration
// reaching the same key later.
type importSSHKeyRequest struct {
	PrivateKeyPEM string `json:"private_key_pem"`
	Passphrase    string `json:"passphrase"`
}

// importSSHKeyFromCandidateRequest is POST /api/v1/ssh-keys/from-candidate's
// body: the wizard's "use the key that is already here" choice (#592).
//
// It is a SEPARATE operation from the paste above rather than a second
// mode of it, and there are two reasons, one about clients and one about
// the acts themselves.
//
// The client reason is the hard one. A single body with two optional
// halves has to stop requiring private_key_pem, and this contract's
// compatibility pin (core/tests/compat, FR-35 clause 3) may only grow:
// "private_key_pem is required" is a promise POST /ssh-keys already made,
// and withdrawing it is indistinguishable, to the gate and to anybody
// reading a diff months later, from a server that quietly began refusing
// something differently. Two operations means nothing is withdrawn.
//
// The honest-shape reason is that pasting material this host has never
// seen and selecting a key this host already found are different acts
// with different trust stories. The paste carries the private half across
// the network; this one carries an opaque handle and no material at all,
// and the key never leaves the machine that already holds it.
//
// The handle is never a path. It resolves only by matching against a
// fresh scan of the fixed locations core decides, so a path, a traversal
// and an id for a file outside those locations all fail the same way:
// they are not in the scan. The private half is then read once,
// server-side, through exactly the rclone.ValidateImportedPrivateKey a
// paste goes through, so a candidate cannot be imported through a check a
// paste would have failed, and the original file is left where it is,
// which is the promise `--ssh-key-file` already makes.
type importSSHKeyFromCandidateRequest struct {
	CandidateID string `json:"candidate_id"`
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

	ref, err := h.setup().ImportSSHKey(r.Context(), []byte(body.PrivateKeyPEM), body.Passphrase)
	if err != nil {
		if errors.Is(err, service.ErrInvalidRequest) {
			// Safe to echo: rclone.ValidateImportedPrivateKey's own doc
			// guarantees this never includes the key bytes themselves,
			// only the shape of the problem ("empty", "passphrase-
			// protected", "did not decrypt with the configured
			// passphrase", "does not parse", ...).
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		h.internalError(w, r, "INTERNAL", "failed to import SSH key", err)
		return
	}

	writeJSON(w, http.StatusCreated, importSSHKeyResponse{
		ID:          ref.ID,
		Algorithm:   ref.Algorithm,
		Fingerprint: ref.Fingerprint,
	})
}

// importSSHKeyFromCandidate is POST /api/v1/ssh-keys/from-candidate: the
// other way to end up holding a key reference (#592).
//
// Same tier as importSSHKey above and the same protections: CSRF because
// it writes, no destructive gate because importing a key destroys
// nothing. It answers with the SAME importSSHKeyResponse, so a client
// keeps one result type no matter which of the two it called, and a
// wizard that lets an operator switch between "paste" and "use this one"
// has one success path rather than two.
//
// There is deliberately no passphrase here. A passphrase belongs to
// material the caller is holding, a candidate's material is read on this
// machine, and #269 already refuses a passphrase-protected key that
// arrives without one. A field with nothing to apply it to would read
// like a way to select an encrypted candidate, which it is not.
func (h *handlers) importSSHKeyFromCandidate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportSSHKeyFromCandidateBodyBytes)

	var body importSSHKeyFromCandidateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxImportSSHKeyFromCandidateBodyBytes)
		return
	}
	if body.CandidateID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "candidate_id is required")
		return
	}

	ref, err := h.setup().ImportSSHKeyCandidate(r.Context(), body.CandidateID)
	if err != nil {
		if errors.Is(err, service.ErrSSHKeyCandidateNotFound) {
			// The listing the caller acted on is not the machine's
			// current state, which is an ordinary thing for a wizard pane
			// left open to run into, not a failure of this deployment.
			writeError(w, http.StatusBadRequest, "SSH_KEY_CANDIDATE_NOT_FOUND",
				"no such key candidate; scan again and select from the current listing")
			return
		}
		if errors.Is(err, service.ErrInvalidRequest) {
			// Safe to echo, for the reason importSSHKey's own branch
			// gives: the sentence describes the shape of the problem and
			// never the key bytes.
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		h.internalError(w, r, "INTERNAL", "failed to import SSH key", err)
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

	probe, err := h.setup().ProbeHostKey(r.Context(), body.Host, body.Port)
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
// request body, in one of two modes.
//
// Naming BackupSetID re-checks a backup set that already exists, and every
// other field must then be absent: the connection details come from the
// configuration. Leaving it empty checks a CANDIDATE before anything is
// persisted, and every field then mirrors backupSetRequest's own
// SSH-facing fields (handlers_backupsets.go) exactly, since this is meant
// to be called with the same values a subsequent POST /api/v1/backup-sets
// would carry.
//
// One route with two modes, rather than a second route keyed by id, is
// deliberate (issue #211). A client re-checking a persisted set does not
// know that set's key reference or its trusted host line, and must never
// have to send them back: making it do so would turn a read-only "does
// this still work" button into a request that could quietly test something
// else entirely. The mutual exclusion below is what keeps the two modes
// from being combined into exactly that.
type testConnectionRequest struct {
	BackupSetID    string `json:"backup_set_id"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	User           string `json:"user"`
	SSHKeyID       string `json:"ssh_key_id"`
	KnownHostsLine string `json:"known_hosts_line"`
	RemotePath     string `json:"remote_path"`
}

// namesACandidate reports whether body carries any candidate connection
// detail at all. RemotePath is included: it is optional in candidate mode,
// but supplying it alongside a backup set id is still a caller asking to
// test something other than what that set is configured with.
func (b testConnectionRequest) namesACandidate() bool {
	return b.Host != "" || b.Port != 0 || b.User != "" ||
		b.SSHKeyID != "" || b.KnownHostsLine != "" || b.RemotePath != ""
}

type testConnectionResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`

	// Checks is what the test actually did (issues #592 and #596): one
	// entry per step, always all of them, in the order they run.
	// Additive: `ok` and `message` mean exactly what they meant, so a
	// client reading only those keeps working.
	//
	// Populated in BOTH modes. There is one breakdown on this endpoint
	// and not one per mode, so a caller never has to remember which
	// request it sent to know which array it is reading.
	Checks []connectionCheckResponse `json:"checks,omitempty"`
}

// connectionCheckResponse is one step of a connection test on the wire.
// The fields are core/internal/sourcecheck's Check, flattened to
// strings: `outcome` is passed/failed/skipped, `category` is the
// machine-readable half a client branches on, and `detail` is a sentence
// the engine composed, never an underlying transport error's text.
type connectionCheckResponse struct {
	Step     string `json:"step"`
	Outcome  string `json:"outcome"`
	Category string `json:"category,omitempty"`
	Detail   string `json:"detail"`

	// DurationMS is omitted rather than sent as 0 on the steps that are
	// not measured on their own, which is what `omitempty` buys here:
	// authenticate and list come out of one call, and a "0 ms" beside a
	// green step reads as "instantly" when it means "never measured".
	DurationMS int `json:"duration_ms,omitempty"`
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

	var (
		result service.ConnectionTestResult
		err    error
	)
	switch {
	case body.BackupSetID != "" && body.namesACandidate():
		// Refused rather than resolved by precedence. A request that
		// names a persisted set AND carries its own connection details is
		// ambiguous about which one is being tested, and silently
		// preferring either one is how a caller ends up shown a green
		// result for something it did not ask about.
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"name backup_set_id to re-check a persisted backup set, or supply the connection details to check a candidate, but not both")
		return
	case body.BackupSetID != "":
		// h.backend, not h.setup(): re-checking a PERSISTED set needs the
		// configuration it was persisted into, and an unconfigured
		// deployment (the first-run surface, where setup() falls back to
		// a client that can only reach a candidate) has no backup sets at
		// all. Answering "no such backup set" is the honest reading of a
		// request that names one there.
		if h.backend == nil {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
		result, err = h.backend.TestBackupSetConnection(r.Context(), body.BackupSetID)
		if errors.Is(err, service.ErrBackupSetNotFound) {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
	default:
		// h.setup(), so the add-backup-set wizard's pre-save check still
		// works before this deployment has a configuration at all.
		result, err = h.setup().TestConnection(r.Context(), service.ConnectionTestRequest{
			Host:           body.Host,
			Port:           body.Port,
			User:           body.User,
			SSHKeyID:       body.SSHKeyID,
			KnownHostsLine: body.KnownHostsLine,
			RemotePath:     body.RemotePath,
		})
	}
	if err != nil {
		if errors.Is(err, service.ErrInvalidRequest) {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		if errors.Is(err, service.ErrSSHKeyNotFound) {
			writeError(w, http.StatusBadRequest, "SSH_KEY_NOT_FOUND", "the referenced ssh_key_id does not exist; import a key first")
			return
		}
		h.internalError(w, r, "INTERNAL", "failed to test connection", err)
		return
	}

	// The steps travel exactly as the service reported them, including
	// the skipped ones. Dropping a skipped step, or filtering to the
	// interesting ones, is how a client ends up drawing five steps and
	// letting a reader assume the sixth passed.
	out := testConnectionResponse{OK: result.OK, Message: result.Message}
	for _, c := range result.Checks {
		out.Checks = append(out.Checks, connectionCheckResponse{
			Step: c.Step, Outcome: c.Outcome, Category: c.Category, Detail: c.Detail,
			DurationMS: c.DurationMs,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
