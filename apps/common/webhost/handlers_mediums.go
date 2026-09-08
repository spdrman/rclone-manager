package webhost

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/core/service"
)

// Prove a declared storage medium works, before a cycle carrying a real
// backup finds out for the operator.
//
// The whole route is a write followed by a delete of the object it just
// wrote, which is what puts it in the CSRF tier without putting it in the
// destructive one: it has a real side effect on somebody's bucket, and the
// only object it can reach is its own probe.
//
// What the response deliberately does not carry is as important as what it
// does. There is no field for key material and there will not be, and the
// endpoint's own text of whatever came back never reaches this struct
// either: a provider's error string can contain a signed URL, a bucket
// listing or an account identifier, so the checks here carry a step, an
// outcome and a category, and the raw sentence goes to the manager's log
// where the operator already has the trust to read it.

// mediumPreflightResponse is POST
// /api/v1/storage-mediums/{id}/preflight's body.
//
// There is no field here for key material of any kind, and there never
// will be (FR-33). What each check carries is a step, an outcome, the
// transport category a failure classified as, and one of the engine's own
// sentences: see core/internal/mediumcheck's package doc for why the text
// of what actually came back never reaches this struct and goes to the
// manager's log instead.
type mediumPreflightResponse struct {
	Medium string                 `json:"medium"`
	OK     bool                   `json:"ok"`
	Checks []mediumPreflightCheck `json:"checks"`
}

type mediumPreflightCheck struct {
	Step     string `json:"step"`
	Outcome  string `json:"outcome"`
	Category string `json:"category,omitempty"`
	Detail   string `json:"detail"`
}

// preflightStorageMedium is POST /api/v1/storage-mediums/{id}/preflight:
// prove one declared storage medium actually works, before a cycle
// carrying a real backup does it for the operator (issue #443).
//
// It carries requireCSRF and NOT requireDestructiveGate, and both halves
// are worth stating.
//
// CSRF, because this is not a read. It writes a small probe object to
// somebody's bucket and deletes it again, which is a real side effect on
// real storage against a caller-supplied id, exactly the reason
// host-key-probe carries CSRF despite being a read of a public key.
//
// Not the destructive gate, because the gate stands in front of operations
// that can destroy BACKUP DATA (docs/EPIC-B-multi-nas.md §50). Nothing
// this can reach touches a backup: the only object it writes is one it
// generated a random key for, under a reserved key segment no configured
// artifact can produce, and the only object it deletes is that same one.
// It moves no journal row, it changes no configuration, and it cannot
// reach a remote source at all.
//
// A medium that does not work is a 200 with ok false, exactly like a
// failed backup-set connection test: a bucket that is not there is what an
// operator did, not what broke. The error path is for a medium this
// configuration does not declare.
func (h *handlers) preflightStorageMedium(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	result, err := h.backend.PreflightStorageMedium(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrMediumNotFound) {
			writeError(w, http.StatusNotFound, "MEDIUM_NOT_FOUND",
				"this configuration declares no storage medium with that id")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to check the storage medium")
		return
	}

	writeJSON(w, http.StatusOK, toMediumPreflightResponse(result))
}

// toMediumPreflightResponse projects one report onto the wire, in one
// place, so the by-id preflight and the candidate preflight below cannot
// render the same report two ways. Every check is carried, in the
// engine's own order, including the skipped ones: a surface that dropped
// them would be showing an operator a shorter list on a failure than on a
// success, which is the one moment the full list matters most.
func toMediumPreflightResponse(result service.MediumPreflight) mediumPreflightResponse {
	body := mediumPreflightResponse{Medium: result.Medium, OK: result.OK, Checks: make([]mediumPreflightCheck, 0, len(result.Checks))}
	for _, c := range result.Checks {
		body.Checks = append(body.Checks, mediumPreflightCheck{
			Step:     c.Step,
			Outcome:  c.Outcome,
			Category: c.Category,
			Detail:   c.Detail,
		})
	}
	return body
}

// ------------------------------------------- declaring one (G2.2, #594) ---
//
// Everything below is the write half a storage destination did not have,
// and the candidate probe that has to come before it.
//
// Two rules run through all of it.
//
// Credential MATERIAL crosses this boundary exactly once, at
// importStorageCredentials, in one direction. Nothing here returns it,
// echoes it, or derives anything displayable from it, and there is no
// field on any response type below that one could travel in. That is the
// same one-way door importSSHKey already is, and it is stricter in one
// respect: an SSH import answers with an algorithm and a fingerprint,
// because a public key's fingerprint is a safe thing to show a person,
// and nothing derived from an S3 credential is (FR-33 treats the access
// key id as secret alongside the secret access key).
//
// A destination that does not work is a 200 with ok false, and the error
// path is for a request this deployment could not resolve into a
// destination at all. That is preflightStorageMedium's rule above,
// unchanged, applied to a candidate.

// maxStorageMediumBodyBytes and maxImportStorageCredentialsBodyBytes bound
// their own requests.
//
// Both are generous rather than tight, and neither is a realistic
// ceiling: a medium is a handful of short strings, and a credential is
// two. They are here because an unbounded body on an authenticated route
// is an unbounded allocation, not because anything legitimate approaches
// them.
const (
	maxStorageMediumBodyBytes            = 1 << 14 // 16 KiB
	maxImportStorageCredentialsBodyBytes = 1 << 13 // 8 KiB
)

// importStorageCredentialsRequest is POST /api/v1/storage-credentials'
// body: the wizard's step 2 "paste a key and let me store it", sent once
// and discarded from the page the instant an id comes back.
//
// Every field is write-only. There is no response shape that carries any
// of them and no operation anywhere in this contract that reads one back.
type importStorageCredentialsRequest struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
}

// importStorageCredentialsResponse is the reference, and nothing else.
type importStorageCredentialsResponse struct {
	ID string `json:"id"`
}

// storageMediumCredentialsReference is the credentials block on a medium
// write or a candidate probe: a reference in four spellings, never
// material.
//
// credentials_id is the spelling a browser uses, and it is the one that
// makes verify-before-save possible at all: it is opaque, minted by this
// deployment, and it names no path and no variable on this host, which is
// the objection core/internal/app/mediumpreflight.go raised against a
// candidate probe and the way that objection is answered.
type storageMediumCredentialsReference struct {
	CredentialsID string   `json:"credentials_id"`
	File          string   `json:"file"`
	Env           string   `json:"env"`
	Command       []string `json:"command"`
}

// storageMediumRequest is one destination as a caller describes it, for
// all three of create, replace and probe.
//
// One shape for the three deliberately: what is proven and what is saved
// must be the same destination, and the interesting bug in this feature
// is a medium that verifies green and then saves as something slightly
// different.
type storageMediumRequest struct {
	ID                 string                            `json:"id"`
	Type               string                            `json:"type"`
	Region             string                            `json:"region"`
	Endpoint           string                            `json:"endpoint"`
	Bucket             string                            `json:"bucket"`
	Prefix             string                            `json:"prefix"`
	StorageClass       string                            `json:"storage_class"`
	UploadVerification string                            `json:"upload_verification"`
	Credentials        storageMediumCredentialsReference `json:"credentials"`
}

// spec turns the wire shape into the service's, which is the only place
// this package translates it.
func (b storageMediumRequest) spec() service.StorageMediumSpec {
	return service.StorageMediumSpec{
		ID:                 b.ID,
		Type:               b.Type,
		Region:             b.Region,
		Endpoint:           b.Endpoint,
		Bucket:             b.Bucket,
		Prefix:             b.Prefix,
		StorageClass:       b.StorageClass,
		UploadVerification: b.UploadVerification,
		Credentials: service.StorageMediumCredentials{
			ID:      b.Credentials.CredentialsID,
			File:    b.Credentials.File,
			Env:     b.Credentials.Env,
			Command: b.Credentials.Command,
		},
	}
}

// listStorageMediumsResponse wraps the list, matching every other list on
// this API. A bare array has nowhere to grow a field.
type listStorageMediumsResponse struct {
	Mediums []storageMediumBody `json:"mediums"`
}

// storageMediumUsageResponse is FR-30's report: what is on a destination
// right now, listed rather than only counted.
type storageMediumUsageResponse struct {
	Medium     string                            `json:"medium"`
	Placements int                               `json:"placements"`
	BackupSets []storageMediumUsageBySetResponse `json:"backup_sets"`
}

type storageMediumUsageBySetResponse struct {
	Set          string `json:"set"`
	Placements   int    `json:"placements"`
	OnlyCopyHere int    `json:"only_copy_here"`
}

// importStorageCredentials is POST /api/v1/storage-credentials.
//
// CSRF, because it writes a file to this host. Not the destructive gate,
// because nothing it can reach is backup data: it creates one new file at
// a name it generated and touches nothing that already exists. That is
// the same tier POST /ssh-keys sits in, for the same reason.
func (h *handlers) importStorageCredentials(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportStorageCredentialsBodyBytes)

	var body importStorageCredentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxImportStorageCredentialsBodyBytes)
		return
	}
	if body.AccessKeyID == "" || body.SecretAccessKey == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "access_key_id and secret_access_key are both required")
		return
	}

	imported, importErr := h.backend.ImportStorageCredentials(r.Context(), body.AccessKeyID, body.SecretAccessKey, body.SessionToken)
	if importErr != nil {
		if errors.Is(importErr, service.ErrInvalidRequest) {
			// Safe to echo: rclone.RenderImportedMediumCredentials'
			// contract is that a refusal reports the SHAPE of the problem
			// (empty, whitespace inside a value, not credentials text at
			// all) and never quotes the bytes that failed. That is the
			// same guarantee importSSHKey relies on above.
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", importErr.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to import storage credentials")
		return
	}
	writeJSON(w, http.StatusCreated, importStorageCredentialsResponse{ID: imported.ID})
}

// listStorageMediums is GET /api/v1/storage-mediums.
func (h *handlers) listStorageMediums(w http.ResponseWriter, r *http.Request) {
	mediums, listErr := h.backend.ListStorageMediums(r.Context())
	if listErr != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to read the storage destinations")
		return
	}
	out := listStorageMediumsResponse{Mediums: make([]storageMediumBody, 0, len(mediums))}
	for _, m := range mediums {
		out.Mediums = append(out.Mediums, toStorageMediumBody(m))
	}
	writeJSON(w, http.StatusOK, out)
}

// getStorageMedium is GET /api/v1/storage-mediums/{id}.
func (h *handlers) getStorageMedium(w http.ResponseWriter, r *http.Request) {
	m, getErr := h.backend.GetStorageMedium(r.Context(), chi.URLParam(r, "id"))
	if getErr != nil {
		if errors.Is(getErr, service.ErrMediumNotFound) {
			writeError(w, http.StatusNotFound, "MEDIUM_NOT_FOUND",
				"this configuration declares no storage medium with that id")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to read the storage destination")
		return
	}
	writeJSON(w, http.StatusOK, toStorageMediumBody(m))
}

// getStorageMediumUsage is GET /api/v1/storage-mediums/{id}/usage: the
// FR-30 report.
//
// It answers for an id the configuration no longer declares too, on
// purpose. A removed medium is precisely the case where an operator needs
// to know what was left on it, and a 404 would answer a different
// question from the one being asked.
func (h *handlers) getStorageMediumUsage(w http.ResponseWriter, r *http.Request) {
	usage, usageErr := h.backend.StorageMediumUsage(r.Context(), chi.URLParam(r, "id"))
	if usageErr != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to read what is on the storage destination")
		return
	}
	body := storageMediumUsageResponse{
		Medium:     usage.Medium,
		Placements: usage.Placements,
		BackupSets: make([]storageMediumUsageBySetResponse, 0, len(usage.BackupSets)),
	}
	for _, s := range usage.BackupSets {
		body.BackupSets = append(body.BackupSets, storageMediumUsageBySetResponse{
			Set: s.Set, Placements: s.Placements, OnlyCopyHere: s.OnlyCopyHere,
		})
	}
	writeJSON(w, http.StatusOK, body)
}

// preflightStorageMediumCandidate is POST
// /api/v1/storage-mediums/preflight: prove a destination that has not
// been saved.
//
// This is the route that did not exist and that made a setup wizard
// impossible. It writes nothing whatever the report says, so it is
// exactly as safe as the by-id preflight beside it and sits in the same
// CSRF-but-not-destructive tier: the only object it can reach is the
// probe it generated a random key for.
//
// It is registered as a STATIC path, before the "{id}" routes, so
// "preflight" can never be read as a medium id.
func (h *handlers) preflightStorageMediumCandidate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxStorageMediumBodyBytes)

	var body storageMediumRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxStorageMediumBodyBytes)
		return
	}
	result, checkErr := h.backend.PreflightStorageMediumCandidate(r.Context(), body.spec())
	if checkErr != nil {
		writeStorageMediumWriteError(w, checkErr, "failed to check the storage destination")
		return
	}
	writeJSON(w, http.StatusOK, toMediumPreflightResponse(result))
}

// createStorageMedium is POST /api/v1/storage-mediums.
//
// CSRF, and not the destructive gate. Declaring a destination MOVES
// NOTHING: artifacts arrive on a medium only once a retention tier names
// it, which is a separate write with a disclosure of its own. Putting
// this behind the destructive gate would train an operator to click
// through the acknowledgment that actually matters.
func (h *handlers) createStorageMedium(w http.ResponseWriter, r *http.Request) {
	h.writeStorageMedium(w, r, false)
}

// updateStorageMedium is PUT /api/v1/storage-mediums/{id}.
//
// The path id wins over any id in the body, and a body naming a different
// one is refused rather than resolved by precedence: a request that says
// two things about which destination it is editing is ambiguous, and
// silently preferring either is how a caller ends up shown a success for
// a change to something else. That is testConnection's own rule for its
// two modes.
func (h *handlers) updateStorageMedium(w http.ResponseWriter, r *http.Request) {
	h.writeStorageMedium(w, r, true)
}

func (h *handlers) writeStorageMedium(w http.ResponseWriter, r *http.Request, update bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxStorageMediumBodyBytes)

	var body storageMediumRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxStorageMediumBodyBytes)
		return
	}
	if update {
		id := chi.URLParam(r, "id")
		if body.ID != "" && body.ID != id {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				"the id in the path and the id in the body name different storage destinations")
			return
		}
		body.ID = id
	}

	var (
		medium   service.StorageMediumSummary
		writeErr error
		status   = http.StatusCreated
	)
	if update {
		status = http.StatusOK
		medium, writeErr = h.backend.UpdateStorageMedium(r.Context(), body.spec())
	} else {
		medium, writeErr = h.backend.CreateStorageMedium(r.Context(), body.spec())
	}
	if writeErr != nil {
		writeStorageMediumWriteError(w, writeErr, "failed to save the storage destination")
		return
	}
	writeJSON(w, status, toStorageMediumBody(medium))
}

// removeStorageMedium is DELETE /api/v1/storage-mediums/{id}: FR-30's
// refusal, at the surface that could otherwise break the invariant with
// one click.
//
// A medium that still holds copies answers 409 and changes nothing.
// Removing the declaration would not delete those copies; it would leave
// this deployment with no bucket, no endpoint and no credential to reach
// them with, so they would read as unreachable and no prune could ever
// run against them.
func (h *handlers) removeStorageMedium(w http.ResponseWriter, r *http.Request) {
	if removeErr := h.backend.RemoveStorageMedium(r.Context(), chi.URLParam(r, "id")); removeErr != nil {
		writeStorageMediumWriteError(w, removeErr, "failed to remove the storage destination")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeStorageMediumWriteError maps this file's four named refusals onto
// their status codes, once, so five handlers cannot disagree about which
// refusal is which.
//
// ErrStorageMediumInUse is a 409 rather than a 400 because the request
// was understood perfectly and is being declined on the state of the
// deployment, which is what 409 means; a 400 would read as "you sent me
// something malformed" and send an operator to check their JSON.
func writeStorageMediumWriteError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case service.AsStorageMediumInUse(err):
		// The message is this package's own sentence plus a count and a
		// list of backup set ids. No path, no credential and no endpoint
		// error text can reach it: see core/service's
		// storageMediumInUseRefusal.
		writeError(w, http.StatusConflict, "MEDIUM_IN_USE", err.Error())
	case errors.Is(err, service.ErrStorageMediumExists):
		writeError(w, http.StatusConflict, "MEDIUM_EXISTS", err.Error())
	case errors.Is(err, service.ErrMediumCredentialNotFound):
		writeError(w, http.StatusNotFound, "STORAGE_CREDENTIAL_NOT_FOUND",
			"the referenced credentials_id does not exist; import the credentials first")
	case errors.Is(err, service.ErrMediumNotFound):
		writeError(w, http.StatusNotFound, "MEDIUM_NOT_FOUND",
			"this configuration declares no storage medium with that id")
	case errors.Is(err, service.ErrInvalidRequest):
		// Safe to echo, on core/service's own guarantee: a
		// config.ValidationError's text is built from that package's field
		// descriptions and the caller's own submitted values, never from a
		// state or rclone error string, and there is no field on this
		// boundary a credential could have arrived in.
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "INTERNAL", fallback)
	}
}
