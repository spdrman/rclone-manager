// This file is issue #211's read surface over the backups a deployment
// actually holds, plus the three operator actions a quarantined one has
// (the third, reinstate, is issue #220's).
//
// Nothing here computes anything. Every value comes from
// core/service.BackupService's own read models, which in turn read the
// FR-9 journal `backup-manager artifacts` and `status` already print: the
// data has existed since the first migration, and what was missing was a
// boundary. That is why these routes could be built at all without
// inventing a second source of truth for what a backup is.
package webhost

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/core/service"
)

// artifactResponse is one backup's wire shape, a field-for-field mirror of
// core/service.Artifact. Timestamps are RFC3339Nano strings and are
// OMITTED rather than zero-valued when the event they name has not
// happened, exactly like operationResponse's (handlers_operations.go): a
// "0001-01-01T00:00:00Z" reaching a screen is a date, and it renders like
// one.
type artifactResponse struct {
	ID          string `json:"id"`
	BackupSetID string `json:"backup_set_id"`
	SourceName  string `json:"source_name"`
	SetName     string `json:"set_name"`
	Name        string `json:"name"`

	RemotePath string `json:"remote_path"`
	LocalPath  string `json:"local_path"`

	// Placements is every durable copy this backup currently has.
	//
	// An empty array means there is no copy anywhere yet, which is the
	// honest answer for a backup still arriving, and is emphatically not
	// "we could not work it out". local_path above is the path ingestion
	// landed on and is not evidence that a readable file is sitting
	// there: a client asking where the bytes are reads this.
	Placements []placementResponse `json:"placements"`

	State        string `json:"state"`
	DiscoveredAt string `json:"discovered_at"`
	UpdatedAt    string `json:"updated_at"`
	SizeBytes    int64  `json:"size_bytes"`

	Checksum          string `json:"checksum,omitempty"`
	ChecksumAlgorithm string `json:"checksum_algorithm,omitempty"`

	// Validation is "passed", "failed" or "pending". Never empty: the
	// tri-state is spelled out so a client is not left deciding what an
	// absent value means about a backup's trustworthiness.
	Validation       string `json:"validation"`
	ValidationDetail string `json:"validation_detail,omitempty"`

	RemoteSourceRemovedAt string `json:"remote_source_removed_at,omitempty"`

	Quarantined             bool   `json:"quarantined"`
	QuarantineIrrecoverable bool   `json:"quarantine_irrecoverable"`
	QuarantineReason        string `json:"quarantine_reason,omitempty"`

	RetentionTier string `json:"retention_tier,omitempty"`
}

// placementResponse is one durable copy on the wire, a field-for-field
// mirror of core/service.Placement.
//
// Three fields are omitted rather than emptied, and each omission is a
// distinct statement: storage_class is absent for a local copy, which has
// no such thing; verification_class is absent when NOTHING has verified
// this copy, which is a different fact from a weak pass and must never be
// rendered as one; verified_at is absent exactly when verification_class
// is. A zero-valued spelling of any of the three would hand a client a
// value to render, and every value it could render would be a claim
// nobody made.
//
// size_bytes is a pointer for the reason core/service.Placement.SizeBytes
// is one: an artifact can genuinely be zero bytes, so a zero must stay
// distinguishable from nothing recorded.
type placementResponse struct {
	Medium     string `json:"medium"`
	MediumType string `json:"medium_type"`
	Location   string `json:"location"`
	SizeBytes  *int64 `json:"size_bytes,omitempty"`

	StorageClass      string `json:"storage_class,omitempty"`
	VerificationClass string `json:"verification_class,omitempty"`
	VerifiedAt        string `json:"verified_at,omitempty"`

	Access string `json:"access"`
	Status string `json:"status"`
}

// listArtifactsResponse is the body of both GET /api/v1/backups and GET
// /api/v1/quarantine: an object with one array field, matching
// listBackupSetsResponse's shape so a future field can be added without
// breaking a client that parsed a bare top-level array.
type listArtifactsResponse struct {
	Artifacts []artifactResponse `json:"artifacts"`
}

// artifactCheckResponse is POST /api/v1/quarantine/{id}/revalidate's body.
type artifactCheckResponse struct {
	Checked bool   `json:"checked"`
	Passed  bool   `json:"passed"`
	Reason  string `json:"reason,omitempty"`
}

// artifactReinstateResponse is POST /api/v1/quarantine/{id}/reinstate's
// body.
//
// Reinstated and Passed are separate fields rather than one. A caller told
// only "it did not work" cannot tell "the durable local copy is bad", which
// no configuration change fixes, from "the copy is fine but nothing that
// could have failed was actually checked", which the operator can fix by
// repairing the validator the backup set names. Those call for opposite
// next steps, so the wire shape says both.
type artifactReinstateResponse struct {
	Reinstated bool   `json:"reinstated"`
	Checked    bool   `json:"checked"`
	Passed     bool   `json:"passed"`
	State      string `json:"state,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func toArtifactResponse(a service.Artifact) artifactResponse {
	resp := artifactResponse{
		ID:                      a.ID,
		BackupSetID:             a.BackupSetID,
		SourceName:              a.SourceName,
		SetName:                 a.SetName,
		Name:                    a.Name,
		RemotePath:              a.RemotePath,
		LocalPath:               a.LocalPath,
		State:                   a.State,
		SizeBytes:               a.SizeBytes,
		Checksum:                a.Checksum,
		ChecksumAlgorithm:       a.ChecksumAlgorithm,
		Validation:              a.Validation,
		ValidationDetail:        a.ValidationDetail,
		Quarantined:             a.Quarantined,
		QuarantineIrrecoverable: a.QuarantineIrrecoverable,
		QuarantineReason:        a.QuarantineReason,
		RetentionTier:           a.RetentionTier,
		Placements:              toPlacementResponses(a.Placements),
	}
	resp.DiscoveredAt = formatTime(a.DiscoveredAt)
	resp.UpdatedAt = formatTime(a.UpdatedAt)
	resp.RemoteSourceRemovedAt = formatTime(a.RemoteSourceRemovedAt)
	return resp
}

// formatTime renders t as RFC3339Nano, or "" for the zero time. One
// helper rather than a repeated `if !t.IsZero()` at every call site: the
// omitted-not-zeroed rule is a property of this package's wire shapes, and
// the one place it is decided should be one place.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func writeArtifactsResponse(w http.ResponseWriter, artifacts []service.Artifact) {
	resp := listArtifactsResponse{Artifacts: make([]artifactResponse, 0, len(artifacts))}
	for _, a := range artifacts {
		resp.Artifacts = append(resp.Artifacts, toArtifactResponse(a))
	}
	writeJSON(w, http.StatusOK, resp)
}

// listArtifacts is GET /api/v1/backups, optionally narrowed by a setId
// query parameter. Read-only (§50's "list sources"/"view configuration"
// bucket), so no CSRF and no destructive gate.
//
// A setId naming no configured backup set is refused with 404
// BACKUP_SET_NOT_FOUND rather than answered with an empty list. That is
// the rule issue #187 established for the same filter on the CLI side,
// and it holds here for the same reason: an empty list has to keep
// meaning one thing, "this backup set exists and has no backups yet". If
// it also meant "there is no such backup set", a rename that reached the
// configuration but not a bookmarked URL would read to an operator as
// "your backups are gone", and those two call for opposite responses.
func (h *handlers) listArtifacts(w http.ResponseWriter, r *http.Request) {
	artifacts, err := h.backend.ListArtifacts(r.Context(), service.ArtifactFilter{
		BackupSetID: r.URL.Query().Get("setId"),
	})
	if err != nil {
		if errors.Is(err, service.ErrBackupSetNotFound) {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list backups")
		return
	}
	writeArtifactsResponse(w, artifacts)
}

// listQuarantine is GET /api/v1/quarantine: the same read, narrowed to
// the backups being held for a human. Read-only.
func (h *handlers) listQuarantine(w http.ResponseWriter, r *http.Request) {
	artifacts, err := h.backend.ListArtifacts(r.Context(), service.ArtifactFilter{QuarantinedOnly: true})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list quarantined backups")
		return
	}
	writeArtifactsResponse(w, artifacts)
}

// getArtifact is GET /api/v1/backups/{id}. Read-only.
func (h *handlers) getArtifact(w http.ResponseWriter, r *http.Request) {
	artifact, err := h.backend.GetArtifact(r.Context(), artifactIDFrom(r))
	if err != nil {
		if errors.Is(err, service.ErrArtifactNotFound) {
			writeError(w, http.StatusNotFound, "ARTIFACT_NOT_FOUND", "no such backup")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load backup")
		return
	}
	writeJSON(w, http.StatusOK, toArtifactResponse(artifact))
}

// revalidateArtifact is POST /api/v1/quarantine/{id}/revalidate: re-run
// the checks against one quarantined backup and report the verdict.
//
// It carries requireCSRF and NOT requireDestructiveGate. Nothing it can
// reach writes anything: core/service.RevalidateArtifact re-reads the
// durable local copy and returns a verdict, and it cannot move the backup
// anywhere, because the lifecycle graph has no edge out of quarantine
// except back into the pipeline (which is the other route below). CSRF
// applies anyway for the same reason it applies to host-key-probe: this
// opens real work against real files on a caller-supplied id, which is a
// side effect regardless of its destructive tier.
func (h *handlers) revalidateArtifact(w http.ResponseWriter, r *http.Request) {
	result, err := h.backend.RevalidateArtifact(r.Context(), artifactIDFrom(r))
	if err != nil {
		writeArtifactActionError(w, err, "failed to revalidate the backup")
		return
	}
	writeJSON(w, http.StatusOK, artifactCheckResponse{
		Checked: result.Checked,
		Passed:  result.Passed,
		Reason:  result.Reason,
	})
}

// reinstateArtifact is POST /api/v1/quarantine/{id}/reinstate: re-check
// one quarantined backup and, if what is found is enough to trust it
// again, return it to the state it already held (issue #220).
//
// It carries requireCSRF and NOT requireDestructiveGate, and the second
// half of that is worth spelling out, because this is the one quarantine
// action that changes what the manager believes about a backup's
// trustworthiness.
//
// The destructive gate exists for operations that can destroy backup data.
// This one destroys nothing: it touches no local file and no remote
// object, and it moves a journal row from a quarantine state back to the
// durable state that same row already held. What it CANNOT do is make the
// backup's remote source deletable, which is the thing the gate is there
// to stand in front of. The opposite is true: a reinstated artifact is
// refused by FR-15's delete gate forever afterwards (see
// core/internal/lifecycle's DeleteRemote), so taking this action strictly
// reduces the set of remote objects this manager will ever delete.
//
// A verdict of "the checks did not pass" comes back 200 with reinstated
// false, exactly like revalidate reports a failing verdict: that is a fact
// about the backup, not a failed request. Only a refusal the operator has
// to change something to clear is a 409.
func (h *handlers) reinstateArtifact(w http.ResponseWriter, r *http.Request) {
	result, err := h.backend.ReinstateArtifact(r.Context(), artifactIDFrom(r), "")
	if err != nil {
		writeArtifactActionError(w, err, "failed to reinstate the backup")
		return
	}
	writeJSON(w, http.StatusOK, artifactReinstateResponse{
		Reinstated: result.Reinstated,
		Checked:    result.Checked,
		Passed:     result.Passed,
		State:      result.State,
		Reason:     result.Reason,
	})
}

// retryArtifactIngestion is POST /api/v1/quarantine/{id}/retry: return one
// quarantined backup to the pipeline so it is attempted again.
//
// This is state-changing but not destructive (§50's "create/edit" bucket):
// it moves a journal row from QUARANTINED back to DISCOVERED and touches
// no backup data, no local file and no remote object. It carries
// requireCSRF and not requireDestructiveGate, for exactly that reason.
//
// A backup with no remaining source to re-ingest from is refused by name
// (409 ARTIFACT_IRRECOVERABLE) rather than attempted: see
// core/internal/app.RetryQuarantinedIngestion for why a retry there
// livelocks instead of recovering.
func (h *handlers) retryArtifactIngestion(w http.ResponseWriter, r *http.Request) {
	if err := h.backend.RetryArtifactIngestion(r.Context(), artifactIDFrom(r)); err != nil {
		writeArtifactActionError(w, err, "failed to return the backup to the pipeline")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxRetryFailedBodyBytes bounds POST /api/v1/backups/{id}/retry's body.
// The only field is an operator's own sentence, so this is orders of
// magnitude more headroom than any legitimate request needs while still
// bounding how much of a malformed or hostile body is read before giving
// up.
const maxRetryFailedBodyBytes = 8 << 10 // 8 KiB

// retryFailedIngestionRequest is POST /api/v1/backups/{id}/retry's
// optional body. Nothing in it changes what the retry does: the note is
// recorded alongside the transition so a later failure of the same backup
// carries the context of what was tried last time.
type retryFailedIngestionRequest struct {
	Note string `json:"note,omitempty"`
}

// retryFailedIngestion is POST /api/v1/backups/{id}/retry: put one failed
// backup back into the pipeline so it is attempted again (issue #419).
//
// FAILED declares two exits in the lifecycle graph and nothing in this
// product had ever taken either, so a backup that reached it stopped being
// worked on permanently. This is the first of the two, and it is
// deliberately something an operator asks for rather than something a
// cycle does: see core/internal/lifecycle/retryfailed.go for why a blind
// re-transfer is a cost this product does not take on its own.
//
// It carries requireCSRF and NOT requireDestructiveGate, for
// retryArtifactIngestion's reason one state along: it moves a journal row
// from FAILED back to DISCOVERED and touches no backup data, no local file
// and no remote object. It cannot reach a remote delete at all, because
// FAILED is only reachable before COMMITTED and COMMITTED is the only
// state a delete can be reached from.
//
// An empty body is legitimate and is what a client with no note to add
// sends, so a missing body is not a 400.
func (h *handlers) retryFailedIngestion(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRetryFailedBodyBytes)

	var req retryFailedIngestionRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeDecodeError(w, err, maxRetryFailedBodyBytes)
			return
		}
	}
	if err := h.backend.RetryFailedArtifact(r.Context(), artifactIDFrom(r), req.Note); err != nil {
		if errors.Is(err, service.ErrArtifactNotFailed) {
			writeError(w, http.StatusConflict, "ARTIFACT_NOT_FAILED",
				"this backup is not stuck: it is making progress, already finished, or quarantined and waiting for a judgement")
			return
		}
		writeArtifactActionError(w, err, "failed to return the backup to the pipeline")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// artifactIDFrom rebuilds the three-part "source/set/name" identity from
// chi's three path parameters.
//
// The contract spells this path parameter as a single {id} that spans
// segments, the way getBackupSet's does, because that is what the identity
// IS. The router registers three parameters instead, because chi matches a
// parameter per path segment; contract_test.go's routerPattern field is
// where those two spellings are reconciled. Three named segments rather
// than getBackupSet's catch-all, because unlike a backup set id this one
// has a fixed arity: model.NewArtifactID refuses a name containing "/", so
// an artifact id is always exactly three segments, and a route that says
// so gives a malformed request a 404 from the router instead of a
// confusing refusal from a handler.
func artifactIDFrom(r *http.Request) string {
	return strings.Join([]string{
		chi.URLParam(r, "source"),
		chi.URLParam(r, "set"),
		chi.URLParam(r, "name"),
	}, "/")
}

// writeArtifactActionError maps the two quarantine actions' refusals onto
// their declared statuses. Both are ordinary outcomes an operator can
// reach by clicking a button on a stale screen, so both are typed
// refusals rather than 500s.
func writeArtifactActionError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, service.ErrArtifactNotFound):
		writeError(w, http.StatusNotFound, "ARTIFACT_NOT_FOUND", "no such backup")
	case errors.Is(err, service.ErrArtifactIrrecoverable):
		writeError(w, http.StatusConflict, "ARTIFACT_IRRECOVERABLE",
			"this backup has no remaining source to re-ingest from, so retrying cannot recover it")
	case errors.Is(err, service.ErrArtifactNotQuarantined):
		writeError(w, http.StatusConflict, "ARTIFACT_NOT_QUARANTINED",
			"this backup is not quarantined")
	case errors.Is(err, service.ErrBackupSetNotFound):
		// Issue #391. The backup exists; the set that owned it does not,
		// any more. The same code every other surface answers for a
		// removed set, so a client already knows what it means, and the
		// remedy is the one the removal itself named.
		writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND",
			"this backup's set is no longer configured; create a backup set with the same source and name to act on it again")
	case errors.Is(err, service.ErrReinstatementRefused):
		// The one refusal whose text an operator genuinely needs: it says
		// which evidence was missing, and repairing that is the whole
		// remedy. It is safe to pass through because core/service builds
		// it from this project's own typed lifecycle refusals, never from
		// a filesystem or transport error, which is exactly the case the
		// default arm below exists for.
		writeError(w, http.StatusConflict, "REINSTATEMENT_REFUSED", err.Error())
	default:
		// Deliberately not err.Error(): an unclassified error here can
		// carry filesystem text, the same reason writeBackupSetError's own
		// default case gives.
		writeError(w, http.StatusInternalServerError, "INTERNAL", fallback)
	}
}

// toPlacementResponses projects an artifact's copies onto the wire.
//
// The result is always a non-nil slice, so a backup with no copy serves
// [] rather than null. That is not cosmetic: a client that has to handle
// null as well as [] is a client with two code paths for one fact, and
// the one somebody forgets is the one that renders "no copies" as a
// crash or, worse, as nothing at all.
func toPlacementResponses(placements []service.Placement) []placementResponse {
	out := make([]placementResponse, 0, len(placements))
	for _, p := range placements {
		out = append(out, placementResponse{
			Medium:            p.Medium,
			MediumType:        p.MediumType,
			Location:          p.Location,
			SizeBytes:         p.SizeBytes,
			StorageClass:      p.StorageClass,
			VerificationClass: p.VerificationClass,
			VerifiedAt:        formatTime(p.VerifiedAt),
			Access:            p.Access,
			Status:            p.Status,
		})
	}
	return out
}
