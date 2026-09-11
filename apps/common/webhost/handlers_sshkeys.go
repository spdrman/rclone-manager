package webhost

import (
	"errors"
	"net/http"
	"time"

	"github.com/backupdproject/backupd/core/service"
)

// The two reads the SSH surface never had (issue #592).
//
// Everything in handlers_ssh.go next door is a write or a probe, and that
// is exactly the gap: an imported key's id crossed the wire once, in the
// 201 that created it, and nothing could ever ask for it again. So
// `--ssh-key-id` and the edit box both took a value this product would
// not tell anybody, and the create wizard's "use a managed key" radio had
// nothing behind it.
//
// # The two listings answer the path question differently, on purpose
//
// The key store carries no path at all. service.SSHKeyRef.KeyFile has
// been kept off the wire since #146 so an API caller never learns this
// process's filesystem layout, and an inventory is exactly the shape
// where a path column looks helpful and is not: the file it would name
// lives inside a distroless container the operator has no shell in, so it
// is a fact about us that helps nobody.
//
// The candidate scan DOES carry paths, because a candidate's path is its
// identity to an operator: "which of these three files do you mean" has
// no other answer. What makes that safe is decided in core, not here. The
// locations are a closed, constant set (core/service/sshkeys.go), never
// caller-supplied and never walked recursively, so a browser cannot widen
// the search, and every path reported is one the operator installed or
// configured themselves.
//
// The handle travelling the OTHER way is opaque either way. Selecting a
// candidate sends a candidate id, and that id only resolves against a
// fresh scan of the same fixed locations, so there is no request shape
// that names a file for this process to read.
//
// Both are GETs and carry neither CSRF nor the destructive gate: they
// open no outbound connection and write nothing, which is what separates
// them from the probes next door (docs/EPIC-B-multi-nas.md §50's
// read-only bucket, the same tier as "view configuration").

// sshKeyResponse is one key in this deployment's store. It never carries
// KeyFile or key material; see this file's own doc.
type sshKeyResponse struct {
	ID          string `json:"id"`
	Algorithm   string `json:"algorithm"`
	Fingerprint string `json:"fingerprint"`

	// PublicKey is the authorized_keys line, which is public material by
	// definition and the one string here an operator has to be able to
	// copy: a verification that reaches the host, matches its key and
	// then fails to authenticate is fixed by pasting exactly this into
	// the remote account's authorized_keys.
	PublicKey string `json:"public_key"`

	// ImportedAt is RFC 3339, or "" when this deployment cannot report
	// it. Never omitted: a caller has to be able to tell an unknown time
	// from a build that does not report one.
	ImportedAt string `json:"imported_at"`

	PassphraseProtected bool `json:"passphrase_protected"`

	// Problem is why a row could not be described, and never names a
	// path.
	Problem string `json:"problem,omitempty"`

	// UsedBy is every backup set id pointing at this key. Never omitted:
	// "used by nothing" is a real and important answer, and a client that
	// could not tell it from an absent field would render the two the
	// same way.
	UsedBy []string `json:"used_by"`
}

type listSSHKeysResponse struct {
	Keys []sshKeyResponse `json:"keys"`
}

// sshKeyCandidateResponse is one private key file this engine can see.
type sshKeyCandidateResponse struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	Location    string `json:"location"`
	Algorithm   string `json:"algorithm"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
	Mode        string `json:"mode"`
	InStore     bool   `json:"in_store"`
	InStoreID   string `json:"in_store_id,omitempty"`
	Selectable  bool   `json:"selectable"`
	Reason      string `json:"reason,omitempty"`
}

// sshKeyDiscoveryLocationResponse is one place the scan looked, reported
// whether or not it held anything.
type sshKeyDiscoveryLocationResponse struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Found   int    `json:"found"`
	Problem string `json:"problem,omitempty"`
}

// listSSHKeyCandidatesResponse carries the locations beside the
// candidates, in one response, so a client cannot render one without the
// other. An empty candidate list on a packaged install means "the engine
// is a distroless container that cannot see your home directory", and a
// response that made the locations optional would let a client show the
// other sentence.
type listSSHKeyCandidatesResponse struct {
	Locations  []sshKeyDiscoveryLocationResponse `json:"locations"`
	Candidates []sshKeyCandidateResponse         `json:"candidates"`
}

// listSSHKeys is GET /api/v1/ssh-keys.
func (h *handlers) listSSHKeys(w http.ResponseWriter, r *http.Request) {
	setup := h.setup()
	if setup == nil {
		// Neither a backend nor the first-run surface, which is a router
		// wired with neither and therefore a fault in this process rather
		// than anything the caller did. INTERNAL, not a new code: a code
		// exists so a client can BEHAVE differently, and there is nothing
		// a client does about this that it does not already do about 500.
		h.internalError(w, r, "INTERNAL", "failed to list SSH keys",
			errors.New("webhost: this router has neither a backend nor a first-run surface, so there is no key store to list"))
		return
	}
	keys, err := setup.ListSSHKeys(r.Context())
	if err != nil {
		if errors.Is(err, service.ErrConfigNotFileBacked) {
			// A deployment with no configuration file has no store to
			// list, which is an empty listing rather than a failure: the
			// caller asked what is there and the honest answer is
			// nothing.
			writeJSON(w, http.StatusOK, listSSHKeysResponse{Keys: []sshKeyResponse{}})
			return
		}
		h.internalError(w, r, "INTERNAL", "failed to list SSH keys", err)
		return
	}

	out := listSSHKeysResponse{Keys: make([]sshKeyResponse, 0, len(keys))}
	for _, key := range keys {
		imported := ""
		if !key.ImportedAt.IsZero() {
			imported = key.ImportedAt.UTC().Format(time.RFC3339)
		}
		usedBy := key.UsedBy
		if usedBy == nil {
			usedBy = []string{}
		}
		out.Keys = append(out.Keys, sshKeyResponse{
			ID:                  key.ID,
			Algorithm:           key.Algorithm,
			Fingerprint:         key.Fingerprint,
			PublicKey:           key.PublicKey,
			ImportedAt:          imported,
			PassphraseProtected: key.PassphraseProtected,
			Problem:             key.Problem,
			UsedBy:              usedBy,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// listSSHKeyCandidates is GET /api/v1/ssh/key-candidates.
func (h *handlers) listSSHKeyCandidates(w http.ResponseWriter, r *http.Request) {
	setup := h.setup()
	if setup == nil {
		h.internalError(w, r, "INTERNAL", "failed to scan for SSH keys",
			errors.New("webhost: this router has neither a backend nor a first-run surface, so there is nowhere to scan for keys"))
		return
	}
	found, err := setup.DiscoverSSHKeyCandidates(r.Context())
	if err != nil {
		h.internalError(w, r, "INTERNAL", "failed to scan for SSH keys", err)
		return
	}

	out := listSSHKeyCandidatesResponse{
		Locations:  make([]sshKeyDiscoveryLocationResponse, 0, len(found.Locations)),
		Candidates: make([]sshKeyCandidateResponse, 0, len(found.Candidates)),
	}
	for _, loc := range found.Locations {
		out.Locations = append(out.Locations, sshKeyDiscoveryLocationResponse{
			Path: loc.Path, Kind: loc.Kind, Found: loc.Found, Problem: loc.Problem,
		})
	}
	for _, c := range found.Candidates {
		out.Candidates = append(out.Candidates, sshKeyCandidateResponse{
			ID: c.ID, Path: c.Path, Location: c.Location,
			Algorithm: c.Algorithm, Fingerprint: c.Fingerprint, PublicKey: c.PublicKey,
			Mode: c.Mode, InStore: c.InStore, InStoreID: c.InStoreID,
			Selectable: c.Selectable, Reason: c.Reason,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
