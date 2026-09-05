package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// ActionRestorePlacement is the durable operation an operator submits to
// make an archived copy readable again (EPIC E, FR-34).
//
// It is re-exported from internal/archive so that a caller outside core/,
// which cannot import an internal package, can still name the action it is
// looking at without spelling the string a second time.
const ActionRestorePlacement = archive.ActionRestore

// externallyExecutedActions are the operation actions whose work happens
// somewhere other than this process.
//
// The startup sweep exists because an operation left at queued or running
// by a dead process really was abandoned by it, and nothing else would
// ever move that row. That reasoning holds for every action executed by a
// goroutine here, and it is exactly backwards for a restore: the provider
// carries on restoring whether or not this process is alive, so the row is
// not stale, it is simply not finished, and its true state comes from
// asking the provider rather than from a sweep's assumption.
//
// A list rather than one string because the next action of this shape (a
// provider-side lifecycle transition, say) belongs here too, and the
// alternative is somebody discovering the sweep the hard way.
var externallyExecutedActions = []string{ActionRestorePlacement}

// ErrCopyNotFound is returned when the artifact exists but records no copy
// on the medium a restore names.
//
// Its own sentinel rather than ErrArtifactNotFound, because the two send an
// operator in opposite directions: one means "check the backup's id", the
// other means "that backup is not on that medium", and the second is what
// a stale screen produces after a move.
var ErrCopyNotFound = errors.New("service: this backup has no copy on that medium")

// ErrRestoreRefused wraps every refusal internal/archive makes about a
// restore REQUEST: a copy that reads on demand, a window out of range, a
// request that did not acknowledge the cost, a restore already running.
//
// One sentinel for the family, with the specific reason in the message,
// because every member is the same thing from the caller's point of view:
// nothing was started, nothing was billed, nothing was written, and the
// remedy is to send a different request. The messages are internal/archive's
// own prose and carry no filesystem or endpoint text, which is what makes
// them safe to hand back verbatim (see ErrInvalidRequest's doc for the rule
// this is an instance of).
var ErrRestoreRefused = errors.New("service: the restore was refused")

// ErrRestoreUnavailable is returned when this deployment cannot reach a
// storage medium at all, so no restore can be asked for.
//
// It is separated from ErrRestoreRefused because it is not about the
// request. Nothing the caller changes about what they asked for will make
// it work; something about the deployment has to change first.
var ErrRestoreUnavailable = errors.New("service: this deployment has no way to reach a storage medium")

// RestorePlacementRequest is one operator's ask for one archived copy to be
// made readable again (EPIC E, FR-34).
type RestorePlacementRequest struct {
	// IdempotencyKey identifies this exact logical request, exactly as
	// RunCycleRequest's does: a retry finds the original row rather than
	// starting, and paying for, a second restore.
	IdempotencyKey string

	// Actor is the authenticated caller's identity, recorded on the row.
	Actor string

	// ConfigRevision must equal ConfigRevision() at the moment of the
	// call. Required for RunCycleRequest's reason, and for one more of its
	// own: this request names a medium by id, and a configuration that has
	// changed underneath the caller may have repointed that id at a
	// different bucket.
	ConfigRevision string

	// ArtifactID is the "source/set/name" identity of the backup.
	ArtifactID string

	// Medium is the id of the medium holding the copy to restore.
	Medium string

	// WindowDays is how long the restored copy should stay readable.
	WindowDays int

	// Acknowledged is the caller saying, in one field, that they know this
	// is billed and takes hours. A false gets a refusal rather than a
	// bill; see archive.Request.Acknowledged for why it is shaped this way
	// round.
	Acknowledged bool
}

// RestoreSubmission is what SubmitRestorePlacement returns: the durable
// operation, plus the plain words about what was just started.
//
// No percentage, no completion time, no price, and nowhere to put one.
type RestoreSubmission struct {
	// Operation is the durable row, in the same shape every other
	// operation reaches a caller in.
	Operation Operation

	// Created is false when this was a replay of an idempotency key that
	// already had a row, in which case nothing new was started and nothing
	// new was billed.
	Created bool

	// WindowDays is the window that was actually asked for.
	WindowDays int

	// Wait is the storage class's own published restore time, in plain
	// words. It is a documented property of the class and NEVER an
	// estimate for this particular restore; see archive.Behaviour.RestoreWait.
	Wait string

	// Billing is the plain statement that a bill exists, with no amount,
	// because this deployment holds no price list.
	Billing string
}

// restorer builds the archive.Restorer for this service, or nil when this
// deployment's transport cannot reach a storage medium.
//
// The capability is discovered by asking, not declared in a constructor
// signature. BackupService is handed a transport.Transport; the rclone
// adapter that satisfies it also satisfies archive.Store, and a build or a
// test that wires something narrower simply has no restore. That is the
// honest shape: a deployment with no medium boundary genuinely cannot
// restore anything, and archive.Restorer already refuses in exactly those
// words when its store is nil.
func (b *BackupService) restorer() *archive.Restorer {
	store, _ := b.state.Load().inner.Transport.(archive.Store)
	if store == nil {
		return nil
	}
	return archive.NewRestorer(b.journal, store, now, func() string { return "op_" + uuid.NewString() })
}

// SubmitRestorePlacement records and starts a restore of one archived copy.
//
// # Everything it does before anything is billed
//
// It resolves the artifact, finds the copy on the named medium, resolves
// that medium to its configured bucket and storage class, and only then
// hands the request to internal/archive, which refuses everything it is
// going to refuse BEFORE it writes a row or asks the provider anything. So
// a refused request leaves no operation row, no provider job and no bill.
//
// The configuration revision is checked first, ahead of every one of those
// lookups. A caller acting on a stale screen may be naming a medium id
// that now points at a different bucket, and a restore against the wrong
// bucket is not a no-op: it is a retrieval charge against somebody's
// objects.
func (b *BackupService) SubmitRestorePlacement(ctx context.Context, req RestorePlacementRequest) (RestoreSubmission, error) {
	if req.IdempotencyKey == "" {
		return RestoreSubmission{}, fmt.Errorf("%w: an idempotency key is required", ErrInvalidRequest)
	}
	if req.ConfigRevision == "" {
		return RestoreSubmission{}, fmt.Errorf("%w: a configuration revision is required", ErrInvalidRequest)
	}
	st := b.state.Load()
	if req.ConfigRevision != st.revision {
		return RestoreSubmission{}, fmt.Errorf("%w: request names %q, running configuration is %q",
			ErrConfigRevisionStale, req.ConfigRevision, st.revision)
	}

	restorer := b.restorer()
	if restorer == nil {
		return RestoreSubmission{}, ErrRestoreUnavailable
	}

	copyOf, medium, err := b.locateCopy(ctx, req.ArtifactID, req.Medium)
	if err != nil {
		return RestoreSubmission{}, err
	}

	submitted, err := restorer.Submit(ctx, archive.Request{
		IdempotencyKey: req.IdempotencyKey,
		Actor:          req.Actor,
		ConfigRevision: req.ConfigRevision,
		Artifact:       req.ArtifactID,
		Copy:           copyOf,
		Medium:         medium,
		WindowDays:     req.WindowDays,
		Acknowledged:   req.Acknowledged,
	})
	if err != nil {
		return RestoreSubmission{}, mapRestoreError(err)
	}

	op, err := b.GetOperation(ctx, submitted.OperationID)
	if err != nil {
		return RestoreSubmission{}, err
	}
	if op.Restore == nil {
		op.Restore = &OperationRestore{}
	}
	op.Restore.Wait = submitted.Wait
	op.Restore.Billing = submitted.Billing
	op.Restore.WindowDays = submitted.WindowDays
	return RestoreSubmission{
		Operation:  op,
		Created:    submitted.Created,
		WindowDays: submitted.WindowDays,
		Wait:       submitted.Wait,
		Billing:    submitted.Billing,
	}, nil
}

// locateCopy resolves an artifact id and a medium id into the two things a
// restore needs: the copy as internal/archive sees it, and the descriptor
// the transport needs to reach it.
//
// The access state it fills in is derived with no network call at all
// (archive.Observation's zero value), which is FR-34's "a read never
// initiates a restore as a side effect" holding one level up as well: the
// SUBMIT path does not get to trigger a probe either. Nothing downstream
// reads it for the decision to restore; what decides that is the class,
// which is a fact about configuration.
func (b *BackupService) locateCopy(ctx context.Context, artifactID, mediumID string) (archive.Copy, transport.Medium, error) {
	parsed, err := app.ParseArtifactID(artifactID)
	if err != nil {
		return archive.Copy{}, transport.Medium{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, artifactID)
	}
	rec, err := b.journal.Get(ctx, parsed)
	if err != nil {
		if errors.Is(err, state.ErrArtifactNotFound) {
			return archive.Copy{}, transport.Medium{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, artifactID)
		}
		return archive.Copy{}, transport.Medium{}, fmt.Errorf("service: loading artifact %s: %w", artifactID, err)
	}

	var placement state.Placement
	found := false
	for _, p := range rec.Placements {
		if p.Medium == mediumID {
			placement = p
			found = true
			break
		}
	}
	if !found {
		return archive.Copy{}, transport.Medium{}, fmt.Errorf("%w: %s is not on %q", ErrCopyNotFound, artifactID, mediumID)
	}

	medium, class, err := app.MediumFor(b.state.Load().inner.Config, mediumID)
	if err != nil {
		// A medium the journal names and the configuration does not. The
		// remedy is a configuration change, so it is reported as the
		// caller's request being wrong about the world rather than as an
		// internal failure.
		return archive.Copy{}, transport.Medium{}, fmt.Errorf("%w: %v", ErrCopyNotFound, err)
	}

	access, err := archive.Access(mediumID, class, archive.Observation{}, now())
	if err != nil {
		return archive.Copy{}, transport.Medium{}, fmt.Errorf("%w: %v", ErrRestoreRefused, err)
	}
	return archive.Copy{Placement: placement, Class: class, Access: access}, medium, nil
}

// mapRestoreError turns internal/archive's refusals into this package's
// own, so nothing below this boundary is named to a caller outside core/.
func mapRestoreError(err error) error {
	switch {
	case errors.Is(err, archive.ErrNotArchived),
		errors.Is(err, archive.ErrNotAcknowledged),
		errors.Is(err, archive.ErrWindowOutOfRange),
		errors.Is(err, archive.ErrAlreadyRestoring),
		errors.Is(err, archive.ErrUnknownClass),
		errors.Is(err, archive.ErrInvalidRequest):
		return fmt.Errorf("%w: %v", ErrRestoreRefused, err)
	case errors.Is(err, state.ErrOperationIdempotencyKeyReused):
		return fmt.Errorf("%w: %v", ErrIdempotencyKeyConflict, err)
	default:
		// Deliberately not wrapped with a message of its own: an
		// unclassified error here can carry state-layer or endpoint text,
		// and the HTTP layer's default arm is what stops it being echoed.
		return err
	}
}

// deriveRestore re-reads a restore operation's real state from the
// provider and, when the provider says the copy is readable, moves the row
// to completed.
//
// # Why a read does this at all
//
// Because nothing else can. A restore runs at the storage provider over
// hours; no goroutine here is executing it, so no goroutine here will ever
// write its terminal outcome. The row records what was ASKED FOR, which
// never changes, and where it has GOT TO is re-derived every time somebody
// looks. That is also what makes it restart-safe: the answer does not
// depend on this process having been alive when the restore was submitted.
//
// A provider that will not answer leaves the row exactly where it was and
// says so in words. It is not an error out of here: the operation is fine,
// the row is fine, and a bucket that did not answer once is a thing to
// report rather than a reason to fail somebody's restore.
func (b *BackupService) deriveRestore(ctx context.Context, op Operation) Operation {
	restorer := b.restorer()
	if restorer == nil {
		return op
	}
	rec, err := b.journal.GetOperation(ctx, op.ID)
	if err != nil {
		return op
	}
	params, err := archive.ParametersOf(rec.Parameters)
	if err != nil {
		return op
	}
	medium, _, err := app.MediumFor(b.state.Load().inner.Config, params.Medium)
	if err != nil {
		return op
	}
	status, err := restorer.Derive(ctx, op.ID, medium)
	if err != nil {
		// The provider did not answer, or the row is not readable as a
		// restore. Either way the row stands, and what IS known about it
		// (which backup, which medium, what was asked for) is still worth
		// serving: a surface with no restore block at all reads as "this
		// is not a restore", which is a worse answer than "this is a
		// restore and nobody could reach the medium just now".
		op.Restore = restoreFactsOf(params, "", "")
		return op
	}
	op.Status = status.Recorded
	op.Restore = restoreFactsOf(status.Parameters, string(status.Access), status.Detail)
	if status.Restore != nil && status.Restore.ExpiresAt != nil {
		expiry := *status.Restore.ExpiresAt
		op.Restore.RestoredUntil = &expiry
	}
	if status.Recorded == state.OperationCompleted && op.Result == "" {
		op.Result = status.Detail
	}
	return op
}

// restoreFactsOf assembles the restore block from the row's own parameters
// plus whatever the provider just said.
//
// Wait and Billing come from the class table rather than from the
// submission, so they survive a restart: a process that has just come up
// and is asked about a restore it never submitted still says how long that
// class takes and that it is billed, because both are properties of the
// class rather than of the request.
func restoreFactsOf(params archive.Parameters, access, detail string) *OperationRestore {
	out := &OperationRestore{
		Artifact:   params.Artifact,
		Medium:     params.Medium,
		Class:      params.StorageClass,
		WindowDays: params.WindowDays,
		Access:     access,
		Detail:     detail,
	}
	if b, err := archive.Of(params.StorageClass); err == nil {
		out.Wait = b.RestoreWait
		out.Billing = archive.BillingStatement(b)
	}
	return out
}
