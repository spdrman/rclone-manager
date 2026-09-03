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

// maxSubmitOperationBodyBytes bounds POST /api/v1/operations' request
// body (docs/EPIC-B-multi-nas.md §17: "enforce request-size limits").
// submitOperationRequest carries exactly two short strings, so 1 MiB is
// generous headroom over anything a legitimate client would ever send,
// while still bounding how much of a malformed or hostile request this
// handler will read into memory before giving up on it.
const maxSubmitOperationBodyBytes = 1 << 20 // 1 MiB

// submitOperationRequest is POST /api/v1/operations' request body. The
// idempotency key travels as a header (Idempotency-Key), not a body
// field: it is a property of the HTTP request/retry, not of the
// operation's own business parameters, matching the common REST
// convention for this exact concern.
type submitOperationRequest struct {
	Action         string `json:"action"`
	ConfigRevision string `json:"config_revision"`

	// Restore carries the restore_placement action's own parameters, and
	// is nil for every other action.
	//
	// A nested object rather than four more flat fields, and refused
	// outright when the action is not a restore: a body carrying restore
	// parameters for a run_cycle is a request that has confused two
	// operations, and a server that quietly ignores the extra fields
	// teaches a client that they are optional.
	Restore *restoreOperationRequest `json:"restore,omitempty"`
}

// restoreOperationRequest is POST /api/v1/operations' body when the action
// is restore_placement.
type restoreOperationRequest struct {
	ArtifactID   string `json:"artifact_id"`
	Medium       string `json:"medium"`
	WindowDays   int    `json:"window_days"`
	Acknowledged bool   `json:"acknowledged"`
}

// operationRestoreResponse is a restore operation's own wire block,
// present only on a restore and simply absent otherwise, exactly like
// operationProgressResponse.
//
// There is nowhere in here to put a percentage, a finishing time or a
// price, and that is the shape rather than an omission; see
// core/service.OperationRestore for the argument.
type operationRestoreResponse struct {
	ArtifactID    string `json:"artifact_id,omitempty"`
	Medium        string `json:"medium,omitempty"`
	StorageClass  string `json:"storage_class,omitempty"`
	WindowDays    int    `json:"window_days,omitempty"`
	Access        string `json:"access,omitempty"`
	Detail        string `json:"detail,omitempty"`
	RestoredUntil string `json:"restored_until,omitempty"`
	Wait          string `json:"wait,omitempty"`
	Billing       string `json:"billing,omitempty"`
}

// operationResponse is the wire shape of one operation, used both for the
// 202 response POST /api/v1/operations returns and for
// GET /api/v1/operations/{id} (docs/EPIC-B-multi-nas.md §14's example,
// §15.7's polling surface). Timestamp fields are omitted (not
// zero-valued) until the corresponding event has actually happened.
type operationResponse struct {
	OperationID    string `json:"operation_id"`
	Status         string `json:"status"`
	Actor          string `json:"actor,omitempty"`
	BackupSetID    string `json:"backup_set_id,omitempty"`
	ConfigRevision string `json:"config_revision,omitempty"`
	Action         string `json:"action,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	FinishedAt     string `json:"finished_at,omitempty"`
	Result         string `json:"result,omitempty"`
	Error          string `json:"error,omitempty"`

	// Progress is present only while the operation is executing in this
	// process, and is simply absent otherwise (see
	// core/service.OperationProgress). Absent is a different answer from
	// zero and a client must be able to tell them apart, which is why
	// this is a nested object that disappears rather than a set of
	// flat fields that would each have to carry some sentinel.
	Progress *operationProgressResponse `json:"progress,omitempty"`

	// Restore is present only on a restore_placement operation, for the
	// same reason and in the same shape.
	Restore *operationRestoreResponse `json:"restore,omitempty"`
}

// operationProgressResponse is the wire shape of one live progress
// reading (docs/EPIC-B-multi-nas.md §52, api/v1/openapi.json's
// OperationProgress).
//
// The three byte fields are pointers, not plain int64s with omitempty,
// because zero is a real reading here: "the copy has started and nothing
// has landed yet" and "no copy is being measured" are different facts, and
// omitempty on a plain int64 would send exactly the same bytes for both.
type operationProgressResponse struct {
	ObservedAt      string `json:"observed_at"`
	Sequence        int64  `json:"sequence"`
	Stage           string `json:"stage"`
	BackupSetID     string `json:"backup_set_id,omitempty"`
	BackupSetsDone  int    `json:"backup_sets_done"`
	BackupSetsTotal int    `json:"backup_sets_total"`
	Artifact        string `json:"artifact,omitempty"`
	ArtifactsDone   int    `json:"artifacts_done"`

	BytesTransferred *int64 `json:"bytes_transferred,omitempty"`
	BytesTotal       *int64 `json:"bytes_total,omitempty"`
	BytesPerSecond   *int64 `json:"bytes_per_second,omitempty"`
}

func toOperationResponse(op service.Operation) operationResponse {
	resp := operationResponse{
		OperationID:    op.ID,
		Status:         op.Status,
		Actor:          op.Actor,
		BackupSetID:    op.BackupSetID,
		ConfigRevision: op.ConfigRevision,
		Action:         op.Action,
		Result:         op.Result,
		Error:          op.Error,
	}
	if !op.CreatedAt.IsZero() {
		resp.CreatedAt = op.CreatedAt.Format(time.RFC3339Nano)
	}
	if !op.StartedAt.IsZero() {
		resp.StartedAt = op.StartedAt.Format(time.RFC3339Nano)
	}
	if !op.FinishedAt.IsZero() {
		resp.FinishedAt = op.FinishedAt.Format(time.RFC3339Nano)
	}
	if op.Restore != nil {
		r := op.Restore
		resp.Restore = &operationRestoreResponse{
			ArtifactID:    r.Artifact,
			Medium:        r.Medium,
			StorageClass:  r.Class,
			WindowDays:    r.WindowDays,
			Access:        r.Access,
			Detail:        r.Detail,
			Wait:          r.Wait,
			Billing:       r.Billing,
			RestoredUntil: formatTimePtr(r.RestoredUntil),
		}
	}
	if op.Progress != nil {
		p := op.Progress
		resp.Progress = &operationProgressResponse{
			ObservedAt:       p.ObservedAt.Format(time.RFC3339Nano),
			Sequence:         p.Sequence,
			Stage:            p.Stage,
			BackupSetID:      p.BackupSetID,
			BackupSetsDone:   p.BackupSetsDone,
			BackupSetsTotal:  p.BackupSetsTotal,
			Artifact:         p.Artifact,
			ArtifactsDone:    p.ArtifactsDone,
			BytesTransferred: p.BytesTransferred,
			BytesTotal:       p.BytesTotal,
			BytesPerSecond:   p.BytesPerSecond,
		}
	}
	return resp
}

// submitOperation is POST /api/v1/operations. It is the one mutating
// route this skeleton exposes, and the one route wrapped in
// requireDestructiveGate (see router.go): a run_cycle can advance backup
// sets all the way through remote deletion (FR-15), so it is treated as
// destructive, not merely "mutating", for gating purposes.
//
// It never blocks on the operation actually finishing: it returns as soon
// as core/service.BackupServiceClient.SubmitRunCycle has durably persisted
// the row (see that method's own doc for the decoupling guarantee this
// handler relies on rather than re-implements).
func (h *handlers) submitOperation(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "the Idempotency-Key header is required")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSubmitOperationBodyBytes)

	var body submitOperationRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				fmt.Sprintf("request body exceeds the %d byte limit", maxSubmitOperationBodyBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed JSON body")
		return
	}
	switch body.Action {
	case service.ActionRunCycle:
		if body.Restore != nil {
			// A run_cycle carrying restore parameters is a request that
			// has confused two operations. Ignoring the extra object
			// would teach the client that it is decorative, and the same
			// client will later send a restore and be surprised that
			// nothing was restored.
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				fmt.Sprintf("a %q submission carried restore parameters, which it has no use for", service.ActionRunCycle))
			return
		}
	case service.ActionRestorePlacement:
		h.submitRestore(w, r, idempotencyKey, body)
		return
	default:
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			fmt.Sprintf("unsupported action %q; this release supports %q and %q",
				body.Action, service.ActionRunCycle, service.ActionRestorePlacement))
		return
	}

	op, err := h.backend.SubmitRunCycle(r.Context(), service.RunCycleRequest{
		IdempotencyKey: idempotencyKey,
		Actor:          actorFromContext(r.Context()),
		ConfigRevision: body.ConfigRevision,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrConfigRevisionStale):
			// A structured, top-level config_revision field, not just prose
			// in the message a client is explicitly told not to rely on
			// (issue #118 item 5); see writeConfigRevisionStale's own doc.
			writeConfigRevisionStale(w, err.Error(), h.backend.ConfigRevision())
		case errors.Is(err, service.ErrIdempotencyKeyConflict):
			// Its own code, distinct from INVALID_REQUEST (issue #118 item
			// 10): "you reused an idempotency key across two different
			// logical requests" is not "your request body is malformed",
			// and a client needs to tell the two apart programmatically.
			// 409, matching the CONFIG_REVISION_STALE precedent above:
			// safe to echo, ErrIdempotencyKeyConflict's message is always
			// one of core/service's own deliberately generic strings.
			writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_CONFLICT", err.Error())
		case errors.Is(err, service.ErrOperationAlreadyRunning):
			// issue #118 item 1: a second, genuinely new run_cycle
			// submission that arrived while another was still executing.
			// Its own code, not folded into either sentinel above: this is
			// neither a malformed request nor a stale configuration
			// revision, and a client that understands this code
			// specifically knows to retry (with a fresh idempotency key)
			// once the deployment is no longer mid-cycle, rather than
			// assuming its request itself needs fixing.
			writeError(w, http.StatusConflict, "OPERATION_ALREADY_RUNNING", err.Error())
		case errors.Is(err, service.ErrInvalidRequest):
			// Safe to echo: ErrInvalidRequest's message is always one of
			// core/service's own deliberately generic strings (see that
			// sentinel's doc), never anything from core/internal.
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		default:
			// Deliberately NOT err.Error() here: an unclassified error is
			// exactly the case core/service.ErrInvalidRequest's doc warns
			// about, one that might otherwise carry state-layer/SQLite
			// text across this boundary.
			writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to submit operation")
		}
		return
	}

	writeJSON(w, http.StatusAccepted, toOperationResponse(op))
}

// getOperation is GET /api/v1/operations/{id}: authenticated polling
// (§15.7, §14's "the v1 frontend SHALL observe long-running operation
// state through authenticated polling"). It does not require the
// destructive gate — reading status is not a destructive action, and
// gating reads on #92 landing would make an already-submitted operation
// unobservable, not safer.
func (h *handlers) getOperation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	op, err := h.backend.GetOperation(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrOperationNotFound) {
			writeError(w, http.StatusNotFound, "OPERATION_NOT_FOUND", "no such operation")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load operation")
		return
	}
	writeJSON(w, http.StatusOK, toOperationResponse(op))
}

// listOperationsResponse is GET /api/v1/operations' body: an object with
// one array field, matching every other list route in this package.
type listOperationsResponse struct {
	Operations []operationResponse `json:"operations"`
}

// listOperations is GET /api/v1/operations: recent operations, newest
// first.
//
// It is the list counterpart of GET /api/v1/operations/{id}. A client that
// has just loaded, or has just reconnected, holds no operation id and
// therefore had no way at all to learn that anything was running: the
// polling read alone only helps a client that submitted the operation
// itself and kept the id. Read-only (§50), so no CSRF and no destructive
// gate, exactly like the single-operation read beside it. POST on the same
// path is the submit route, which is gated; a verb is not a synonym.
func (h *handlers) listOperations(w http.ResponseWriter, r *http.Request) {
	ops, err := h.backend.ListOperations(r.Context(), 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list operations")
		return
	}
	resp := listOperationsResponse{Operations: make([]operationResponse, 0, len(ops))}
	for _, op := range ops {
		resp.Operations = append(resp.Operations, toOperationResponse(op))
	}
	writeJSON(w, http.StatusOK, resp)
}

// formatTimePtr renders an optional instant as RFC3339Nano, or "" when the
// provider reported none.
//
// A separate helper from formatTime because the two absences are different
// facts. formatTime's zero time means "this event has not happened yet";
// a nil here means "the provider never told us", which FR-34 says must not
// be filled in with anything invented.
func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

// submitRestore is POST /api/v1/operations with a restore_placement
// action: make one archived copy readable again (EPIC E, FR-34).
//
// # Why it is on the same route, behind the same gate
//
// The issue that asks for this names the submitOperation family, and it is
// the right family: this is a durable, idempotency-keyed, configuration-
// revision-checked operation whose row outlives the request, which is
// exactly what that route was built for. Putting it on a route of its own
// would give this deployment two answers to "how is a long-running job
// started", and one of them would drift.
//
// It carries requireDestructiveGate for a reason worth stating, because a
// restore destroys nothing. The gate in this codebase stands in front of
// operations an operator cannot undo, and a restore is one: the provider
// accepts it, bills for it, and there is no call that cancels it. That is
// closer to a deletion in consequence than it is to a read, and the gate
// is the control an operator already has for exactly that.
func (h *handlers) submitRestore(w http.ResponseWriter, r *http.Request, idempotencyKey string, body submitOperationRequest) {
	if body.Restore == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			fmt.Sprintf("a %q submission has to say which copy to restore", service.ActionRestorePlacement))
		return
	}

	sub, err := h.backend.SubmitRestorePlacement(r.Context(), service.RestorePlacementRequest{
		IdempotencyKey: idempotencyKey,
		Actor:          actorFromContext(r.Context()),
		ConfigRevision: body.ConfigRevision,
		ArtifactID:     body.Restore.ArtifactID,
		Medium:         body.Restore.Medium,
		WindowDays:     body.Restore.WindowDays,
		Acknowledged:   body.Restore.Acknowledged,
	})
	if err != nil {
		writeRestoreError(w, err, h.backend.ConfigRevision())
		return
	}
	writeJSON(w, http.StatusAccepted, toOperationResponse(sub.Operation))
}

// writeRestoreError maps a restore's refusals onto their declared statuses.
//
// Every one of them is a state an operator reaches by clicking a button on
// a screen that has moved on, so every one is a typed refusal rather than a
// 500. The messages that ARE echoed are core/service's and
// core/internal/archive's own prose, which is the rule
// service.ErrInvalidRequest's doc sets out: never an unclassified error,
// which could carry endpoint or SQLite text.
func writeRestoreError(w http.ResponseWriter, err error, revision string) {
	switch {
	case errors.Is(err, service.ErrConfigRevisionStale):
		writeConfigRevisionStale(w, err.Error(), revision)
	case errors.Is(err, service.ErrIdempotencyKeyConflict):
		writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_CONFLICT", err.Error())
	case errors.Is(err, service.ErrRestoreUnavailable):
		// 503 rather than 409: nothing about the request would make this
		// work, so a client that retries the same body verbatim once the
		// deployment has a medium configured is doing the right thing.
		writeError(w, http.StatusServiceUnavailable, "RESTORE_UNAVAILABLE", err.Error())
	case errors.Is(err, service.ErrArtifactNotFound):
		writeError(w, http.StatusNotFound, "ARTIFACT_NOT_FOUND", "no such backup")
	case errors.Is(err, service.ErrCopyNotFound):
		// Its own code, not folded into ARTIFACT_NOT_FOUND: "check the
		// backup's id" and "that backup is not on that medium" send an
		// operator in opposite directions, and the second is what a
		// screen that has not been reloaded since a move produces.
		writeError(w, http.StatusNotFound, "COPY_NOT_FOUND", err.Error())
	case errors.Is(err, service.ErrRestoreRefused):
		writeError(w, http.StatusConflict, "RESTORE_REFUSED", err.Error())
	case errors.Is(err, service.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to submit the restore")
	}
}
