// This file is issue #211's read surface over the backups a deployment
// actually holds, plus the two operator actions a quarantined one has.
//
// Nothing here computes anything. Every value comes from
// core/service.BackupService's own read models, which in turn read the
// FR-9 journal `backup-manager artifacts` and `status` already print: the
// data has existed since the first migration, and what was missing was a
// boundary. That is why these routes could be built at all without
// inventing a second source of truth for what a backup is.
package webhost

import (
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
	default:
		// Deliberately not err.Error(): an unclassified error here can
		// carry filesystem text, the same reason writeBackupSetError's own
		// default case gives.
		writeError(w, http.StatusInternalServerError, "INTERNAL", fallback)
	}
}
