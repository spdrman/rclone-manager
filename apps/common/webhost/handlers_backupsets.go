package webhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/core/service"
)

// maxCreateBackupSetBodyBytes bounds POST /api/v1/backup-sets' request
// body, the same rationale as maxSubmitOperationBodyBytes
// (handlers_operations.go): generous headroom over any legitimate
// request (a handful of short strings and a small include-pattern list),
// while still bounding how much of a malformed or hostile request this
// handler reads into memory before giving up.
const maxCreateBackupSetBodyBytes = 1 << 20 // 1 MiB

// backupSetRequest is POST /api/v1/backup-sets' request body: the
// add-backup-set wizard's (#98) Review step, translated into
// service.CreateBackupSetRequest. See that type's own doc for what
// SSHKeyID and KnownHostsLine reference (a prior POST /ssh-keys and
// POST /ssh/host-key-probe call respectively) and why neither carries
// key material or an unverified fingerprint directly.
type backupSetRequest struct {
	SourceName         string   `json:"source_name"`
	Name               string   `json:"name"`
	Host               string   `json:"host"`
	Port               int      `json:"port"`
	User               string   `json:"user"`
	SSHKeyID           string   `json:"ssh_key_id"`
	KnownHostsLine     string   `json:"known_hosts_line"`
	RemotePath         string   `json:"remote_path"`
	LocalPath          string   `json:"local_path"`
	Include            []string `json:"include"`
	CompletionStrategy string   `json:"completion_strategy"`
	StableForSeconds   int      `json:"stable_for_seconds"`
	StaleAfterSeconds  int      `json:"stale_after_seconds"`
	Disabled           bool     `json:"disabled"`
	// RunImmediately is the wizard's "Save, enable & run" tier (as
	// opposed to "Save & enable", RunImmediately: false, Disabled:
	// false, or "Save disabled", Disabled: true). See
	// service.CreateBackupSetRequest.RunImmediately's own doc for why
	// this is folded into create rather than a separate endpoint: this
	// issue's scope is exactly the four endpoints named in its own
	// title, and a per-backup-set-scoped run is not one of them.
	RunImmediately bool `json:"run_immediately"`
}

// backupSetResponse is the wire shape of one backup set, shared by
// GET /api/v1/backup-sets, GET /api/v1/backup-sets/{id} and the 201 POST
// /api/v1/backup-sets returns. It carries nothing service.BackupSet does
// not already have.
type backupSetResponse struct {
	ID                 string   `json:"id"`
	SourceName         string   `json:"source_name"`
	Name               string   `json:"name"`
	Host               string   `json:"host"`
	Port               int      `json:"port"`
	User               string   `json:"user"`
	RemotePath         string   `json:"remote_path"`
	LocalPath          string   `json:"local_path"`
	Include            []string `json:"include"`
	CompletionStrategy string   `json:"completion_strategy"`
	Disabled           bool     `json:"disabled"`
}

func toBackupSetResponse(bs service.BackupSet) backupSetResponse {
	return backupSetResponse{
		ID:                 bs.ID,
		SourceName:         bs.SourceName,
		Name:               bs.Name,
		Host:               bs.Host,
		Port:               bs.Port,
		User:               bs.User,
		RemotePath:         bs.RemotePath,
		LocalPath:          bs.LocalPath,
		Include:            bs.Include,
		CompletionStrategy: bs.CompletionStrategy,
		Disabled:           bs.Disabled,
	}
}

// createBackupSetResponse embeds backupSetResponse (its fields marshal
// inline, at the top level) plus, only when the request's
// RunImmediately was set and honoured, the run_cycle Operation it kicked
// off — the same shape POST /api/v1/operations already returns for one,
// so a client parses it identically either way.
type createBackupSetResponse struct {
	backupSetResponse
	Operation *operationResponse `json:"operation,omitempty"`
}

// listBackupSetsResponse is GET /api/v1/backup-sets' body: an object
// with one array field, not a bare JSON array at the top level, so a
// future field (pagination, a total count) can be added without
// breaking every existing client the way changing a top-level array's
// shape would.
type listBackupSetsResponse struct {
	BackupSets []backupSetResponse `json:"backup_sets"`
}

// createBackupSet is POST /api/v1/backup-sets: issue #146's
// create-backup-set endpoint, the write path the wizard's three Save
// buttons call. State-changing but non-destructive
// (docs/EPIC-B-multi-nas.md §50: "create/edit backup set"), so it is
// CSRF-protected (router.go) but not gated behind the destructive-ops
// gate (gate.go) — creating a backup set never touches, let alone
// deletes, remote or local backup data by itself.
func (h *handlers) createBackupSet(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBackupSetBodyBytes)

	var body backupSetRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxCreateBackupSetBodyBytes)
		return
	}

	req := service.CreateBackupSetRequest{
		SourceName:         body.SourceName,
		Name:               body.Name,
		Host:               body.Host,
		Port:               body.Port,
		User:               body.User,
		SSHKeyID:           body.SSHKeyID,
		KnownHostsLine:     body.KnownHostsLine,
		RemotePath:         body.RemotePath,
		LocalPath:          body.LocalPath,
		Include:            body.Include,
		CompletionStrategy: body.CompletionStrategy,
		StableFor:          secondsToDuration(body.StableForSeconds),
		StaleAfter:         secondsToDuration(body.StaleAfterSeconds),
		Disabled:           body.Disabled,
		RunImmediately:     body.RunImmediately,
		Actor:              actorFromContext(r.Context()),
	}

	result, err := h.backend.CreateBackupSet(r.Context(), req)
	if err != nil {
		writeBackupSetError(w, err)
		return
	}

	resp := createBackupSetResponse{backupSetResponse: toBackupSetResponse(result.Set)}
	if result.Operation != nil {
		op := toOperationResponse(*result.Operation)
		resp.Operation = &op
	}
	writeJSON(w, http.StatusCreated, resp)
}

// listBackupSets is GET /api/v1/backup-sets: read-only (§50), no CSRF, no
// destructive gate — see router.go.
func (h *handlers) listBackupSets(w http.ResponseWriter, r *http.Request) {
	sets, err := h.backend.ListBackupSets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list backup sets")
		return
	}
	resp := listBackupSetsResponse{BackupSets: make([]backupSetResponse, 0, len(sets))}
	for _, s := range sets {
		resp.BackupSets = append(resp.BackupSets, toBackupSetResponse(s))
	}
	writeJSON(w, http.StatusOK, resp)
}

// getBackupSet is GET /api/v1/backup-sets/{id}: read-only (§50). id is
// read from chi's "*" wildcard param, not a named {id} segment: a backup
// set's id is "source/name" (model.BackupSetID.String()), and a plain
// chi path parameter never matches a literal "/" within one segment (see
// router.go's route-registration comment for why {id:.*} does not work
// either).
func (h *handlers) getBackupSet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "*")
	set, err := h.backend.GetBackupSet(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrBackupSetNotFound) {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load backup set")
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetResponse(set))
}

// writeBackupSetError maps a CreateBackupSet (or, via the same sentinels,
// any other backupsets.go method) error to the HTTP status/code this
// package's other handlers already establish the vocabulary for
// (handlers_operations.go's identical switch is the direct precedent).
func writeBackupSetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidRequest):
		// Safe to echo: every ErrInvalidRequest this package returns is
		// built from its own field-description strings and the caller's
		// own request values (config.ValidationError, or backupsets.go's
		// own validateCreateRequest), never from state/rclone internals.
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, service.ErrSSHKeyNotFound):
		writeError(w, http.StatusBadRequest, "SSH_KEY_NOT_FOUND", "the referenced ssh_key_id does not exist; import a key first")
	case errors.Is(err, service.ErrConfigNotFileBacked):
		writeError(w, http.StatusInternalServerError, "INTERNAL", "this deployment has no configuration file to persist to")
	default:
		// Deliberately not err.Error(): an unclassified error could carry
		// filesystem or rclone-internal text (see handlers_operations.go's
		// identical default case for the same reasoning).
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to create backup set")
	}
}

// writeDecodeError maps a json.Decoder error the same way submitOperation
// (handlers_operations.go) already does for POST /api/v1/operations,
// factored out here so this file and that one do not each carry their own
// copy of the http.MaxBytesError-vs-anything-else switch.
func writeDecodeError(w http.ResponseWriter, err error, limit int64) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			fmt.Sprintf("request body exceeds the %d byte limit", limit))
		return
	}
	writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed JSON body")
}

// secondsToDuration turns a wire "N seconds" integer into a
// time.Duration; the wizard's JSON body carries seconds (a plain number)
// rather than a Go-duration-string, so the HTTP layer is the one place
// that conversion happens, before service.CreateBackupSetRequest sees a
// real time.Duration like the rest of this codebase already uses.
func secondsToDuration(s int) time.Duration {
	return time.Duration(s) * time.Second
}
