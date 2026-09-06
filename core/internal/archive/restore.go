package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is FR-34's restore, and it is the one operation in this
// product whose work does not happen in this process.
//
// Everything else in the operations table is executed by a goroutine
// here, so a process that dies mid-operation really has abandoned it and
// the startup sweep is right to fail the row. A restore is asked for at
// the provider and then takes hours, and this deployment stopping,
// restarting or being replaced changes nothing about it whatever. So the
// two halves of an operation come apart, and the whole file is arranged
// around that: the row records what was ASKED FOR, which never changes,
// and where it has GOT TO is re-derived from the endpoint every time
// anybody asks (Derive).
//
// What that arrangement buys is the only property that matters here,
// which is that a restore this product started is never invisible. Submit
// writes the row before the provider is asked, so the worst crash leaves a
// row describing a restore that may not have started, and asking again
// costs a request. The other ordering leaves a restore running at the
// provider that nothing in this deployment knows about, and that one is
// billed and cannot be found.
//
// What it costs is a rule every declaration below has to keep: nothing
// here invents a fact about somebody else's system. No price, no
// percentage, no completion time, and nothing concluded from a provider
// that said nothing at all. Each of those is refused somewhere specific,
// and each place says why it is refusing rather than defaulting.

// ActionRestore is the durable operation's action name, and it is the
// string internal/state stores in operations.action.
//
// It joins run_cycle in the same table for a reason worth stating: a
// restore is a thing an operator asked for that outlives the request that
// asked for it, which is exactly what that table was built to hold. What
// it is NOT is a variant of run_cycle. Nothing about it runs in this
// process, so nothing about it is cancelled by this process stopping.
const ActionRestore = "restore_placement"

// The window a restored copy stays readable for, in days.
//
// The floor is one because zero is not a shorter restore, it is a restore
// that is billed and then immediately unavailable, which is the worst of
// both. The ceiling is thirty because a restored copy is billed for the
// whole window as additional storage, and a caller that fat-fingers a
// number should hit a refusal rather than a month of double billing. Both
// numbers are policy rather than a provider limit, and an operator who
// genuinely wants longer asks twice.
const (
	MinWindowDays = 1
	MaxWindowDays = 30
)

// The refusals Submit can return. Each is its own sentinel because each
// means something different to a caller: one is "you asked for the wrong
// thing", one is "you asked for it wrongly", and one is "somebody already
// asked".
var (
	// ErrNotArchived refuses a restore of a copy that does not need one.
	//
	// It is not pedantry. A restore request against a STANDARD object is
	// a request that has confused two artifacts or two mediums, and
	// answering it with a cheerful success teaches an operator that
	// restore is a thing you sprinkle on anything, which is how the
	// expensive version of the same mistake gets made later.
	ErrNotArchived = errors.New("archive: this copy does not need a restore")

	// ErrNotAcknowledged refuses a restore nobody explicitly asked for.
	ErrNotAcknowledged = errors.New("archive: a restore costs money and takes hours, so it has to be asked for explicitly")

	// ErrWindowOutOfRange refuses a restore window outside the bounds
	// above.
	ErrWindowOutOfRange = errors.New("archive: restore window out of range")

	// ErrAlreadyRestoring refuses a second restore of an object the
	// provider already says it is restoring. Asking twice does not make
	// it faster, and on some providers it is billed twice.
	ErrAlreadyRestoring = errors.New("archive: a restore of this copy is already running")

	// ErrInvalidRequest is the catch-all for a malformed request.
	ErrInvalidRequest = errors.New("archive: invalid restore request")
)

// Store is the slice of a storage medium this package needs: one call that
// asks about a restore, and one that starts one.
//
// Stating it here rather than taking transport.MediumStore whole is the
// same decision internal/placement made for its own Store, and it carries
// the same statement of intent. There is no upload here, no delete, and
// no read: a package about making bytes reachable has no business holding
// a method that can destroy them.
//
// The implementation is the rclone s3 adapter's, over that backend's own
// restore and restore-status commands, and it lands with the medium
// boundary rather than here; transport.MediumStore's doc already names
// this pair as #241's to define, which is what this interface does.
type Store interface {
	// RestoreStatus reports what the medium says about a restore of the
	// object at key, or nil when it reports no restore status at all,
	// which is what S3 returns for an object nobody has asked about.
	RestoreStatus(ctx context.Context, medium transport.Medium, key string) (*RestoreState, error)

	// InitiateRestore asks the medium to make the object at key readable
	// for windowDays days. It returns once the request is accepted, which
	// is long before the object is readable.
	InitiateRestore(ctx context.Context, medium transport.Medium, key string, windowDays int) error
}

// Journal is the slice of internal/state this operation needs.
// *state.Journal satisfies it.
//
// GetOperationByIdempotencyKey is the one method here that is not obvious
// from the operation's shape, and it is what lets Submit keep both of its
// promises at once. Resolving a retry key is otherwise the same call as
// writing a row (CreateOperation), and a restore has to answer "have I
// already been given this exact request" BEFORE it asks a provider for
// anything, while still leaving no row behind when it goes on to refuse.
// A read is the only thing that is both.
type Journal interface {
	CreateOperation(ctx context.Context, req state.OperationRequest) (state.OperationOutcome, error)
	GetOperation(ctx context.Context, operationID string) (state.Operation, error)
	GetOperationByIdempotencyKey(ctx context.Context, key string) (state.Operation, error)
	MarkOperationRunning(ctx context.Context, operationID string, startedAt time.Time) error
	CompleteOperation(ctx context.Context, operationID string, finishedAt time.Time, result string) error
	FailOperation(ctx context.Context, operationID string, finishedAt time.Time, errMsg string) error
}

// Parameters is what the operation row records about the restore it
// describes, serialised into operations.parameters.
//
// The row has to be able to answer, on its own, after a restart, in a
// process that has lost every bit of memory of the request: which object
// was this, on which medium, and for how long. Anything the row cannot
// answer is a thing a restarted process would have to guess, and guessing
// here means either re-billing a restore that is already running or
// reporting one that never started.
type Parameters struct {
	Artifact     string `json:"artifact"`
	Medium       string `json:"medium"`
	Key          string `json:"key"`
	StorageClass string `json:"storage_class"`
	WindowDays   int    `json:"window_days"`
}

// Request is one operator's ask for one copy to be made readable.
type Request struct {
	// IdempotencyKey is the caller's retry key, exactly as run_cycle uses
	// it: a retried submission finds the original row rather than
	// starting a second restore.
	//
	// It holds for both retries, and the second one is the one worth
	// naming. A submission that never reached the provider is the easy
	// case. A submission that WAS accepted is the case an operator
	// actually hits, because a restore this product started is precisely
	// one the provider now reports in progress, and for a while that came
	// back as ErrAlreadyRestoring: a conflict, reported to the person
	// whose request it was. Submit resolves this key before it asks the
	// provider anything, so a replay is answered from the row.
	//
	// What the key is not is a namespace. A key first used by a different
	// actor, for a different action, or against a different configuration
	// revision is refused with state.ErrOperationIdempotencyKeyReused
	// rather than answered, because handing back somebody else's
	// operation is an information leak dressed up as a convenience.
	IdempotencyKey string

	// Actor is who asked, recorded on the row.
	Actor string

	// ConfigRevision is the configuration the caller believed it was
	// acting against.
	ConfigRevision string

	// Artifact is the artifact id this copy belongs to.
	Artifact string

	// Copy is the copy to restore, with the class and access state the
	// caller derived for it.
	Copy Copy

	// Medium is the descriptor the store needs to reach it.
	Medium transport.Medium

	// WindowDays is how long the restored copy should stay readable.
	WindowDays int

	// Acknowledged is the operator saying, in one field, that they know
	// this is billed and takes hours.
	//
	// It is a required true rather than a defaulted one, and that is the
	// whole mechanism behind "make an accidental restore hard". A caller
	// that forgot to ask a human gets a refusal, not a bill, because the
	// zero value of a bool is the answer that costs nothing. Compare the
	// alternative, a Cancel or Force field defaulting to false: there the
	// forgetful caller is the one that spends the money.
	Acknowledged bool
}

// Restorer submits and tracks restore operations.
type Restorer struct {
	journal Journal
	store   Store
	now     func() time.Time
	newID   func() string
}

// NewRestorer builds a Restorer. now and newID may be nil, in which case
// the real clock and a time-ordered id are used.
func NewRestorer(j Journal, s Store, now func() time.Time, newID func() string) *Restorer {
	if now == nil {
		now = time.Now
	}
	if newID == nil {
		newID = func() string { return fmt.Sprintf("op_restore_%d", time.Now().UnixNano()) }
	}
	return &Restorer{journal: j, store: s, now: now, newID: newID}
}

// Submitted is what Submit returns: the durable row, plus the plain words
// about what was just started.
//
// Note what is not in here. No percentage, no completion time, no price.
// The one thing it says about duration is the class's published figure,
// carried through from the class table, and worded there so it cannot be
// read as a countdown.
type Submitted struct {
	// OperationID is the durable row's public id.
	OperationID string

	// Created is false when this was a replay of an idempotency key that
	// already had a row, in which case nothing new was started.
	Created bool

	// WindowDays is the window that was actually asked for.
	WindowDays int

	// Wait is the class's published restore time, in plain words.
	Wait string

	// Billing is the plain statement that this is billed, with no figure,
	// because this product holds no price list.
	Billing string
}

// Submit records a restore operation and then starts it, in that order.
//
// The ordering is the durability contract the operations table was built
// for and it is not negotiable: the row exists before anything at the
// provider has been asked for, so a crash between the two leaves a row
// describing a restore that never started (recoverable: ask again) rather
// than a restore running at the provider that nothing in this deployment
// knows about (not recoverable: it is billed and invisible).
//
// Everything that can refuse, refuses before the row is written, so a
// refused request leaves nothing behind at all.
//
// # Why the idempotency key is resolved first
//
// Two promises meet here and the order is the only thing that keeps both.
// A replayed key has to find the original row (Request.IdempotencyKey),
// and a refused request has to leave no row behind (above). Asking the
// provider first satisfies the second and breaks the first, because a
// restore this product started is one the provider now reports in
// progress, so an operator retrying their own request was told it
// conflicted with itself. Writing the row first to resolve the key
// satisfies the first and breaks the second.
//
// Neither is necessary: resolving the key is a READ
// (Journal.GetOperationByIdempotencyKey), so it can happen before the
// provider is asked and still write nothing. A replay returns the row it
// finds and stops; anything else falls through to the provider check with
// nothing recorded yet, and a refusal there still leaves an empty table.
// CreateOperation's own idempotency branch stays below as the race
// backstop, for the row that appears between that read and the insert.
//
// # The crash point, named
//
// There is exactly one, between marking the row running and the provider
// accepting the request, and the row is moved to running BEFORE the ask
// rather than after it on purpose. A crash there leaves a row saying a
// restore might be running, which is the true state of somebody's
// knowledge at that instant: the request may have been accepted and the
// answer lost. The alternative ordering would leave a row saying nothing
// was started, about a restore that may well have been, which is the
// version that gets billed invisibly. Derive resolves it by asking the
// provider, and concludes nothing from silence.
func (r *Restorer) Submit(ctx context.Context, req Request) (Submitted, error) {
	behaviour, err := r.validate(req)
	if err != nil {
		return Submitted{}, err
	}
	if r.store == nil {
		return Submitted{}, fmt.Errorf("%w: this deployment has no way to reach a storage medium", ErrInvalidRequest)
	}

	// Resolve the retry key before anything else is asked or written. A
	// key that already has a row is this same request arriving twice, and
	// the answer to it is that row: nothing is asked of the provider,
	// nothing is billed, and in particular the check below is never
	// reached, which is what stops a restore this product started being
	// reported back to its own submitter as a conflict.
	existing, replayed, err := r.resolveReplay(ctx, req)
	if err != nil {
		return Submitted{}, err
	}
	if replayed {
		return replayOf(existing)
	}

	// Ask the provider whether it is already restoring this object before
	// writing anything. A second restore of an object already being
	// restored is billed again on some providers and buys nothing on any
	// of them.
	//
	// By here the request is known NOT to be a replay, so a restore in
	// progress is somebody else's: another operator, another deployment,
	// or a lifecycle rule. Refusing it is the right answer and it happens
	// with the operations table still untouched.
	current, err := r.store.RestoreStatus(ctx, req.Medium, req.Copy.Placement.Location)
	if err != nil {
		return Submitted{}, fmt.Errorf("archive: asking %q about %q before restoring it: %w",
			req.Copy.Placement.Medium, req.Copy.Placement.Location, err)
	}
	if current != nil && current.InProgress {
		return Submitted{}, fmt.Errorf("%w: %q on %q", ErrAlreadyRestoring,
			req.Copy.Placement.Location, req.Copy.Placement.Medium)
	}

	params, err := json.Marshal(Parameters{
		Artifact:     req.Artifact,
		Medium:       req.Copy.Placement.Medium,
		Key:          req.Copy.Placement.Location,
		StorageClass: req.Copy.Class,
		WindowDays:   req.WindowDays,
	})
	if err != nil {
		return Submitted{}, fmt.Errorf("%w: recording the restore's parameters: %w", ErrInvalidRequest, err)
	}

	operationID := r.newID()
	outcome, err := r.journal.CreateOperation(ctx, state.OperationRequest{
		OperationID:    operationID,
		IdempotencyKey: req.IdempotencyKey,
		Actor:          req.Actor,
		ConfigRevision: req.ConfigRevision,
		Action:         ActionRestore,
		Parameters:     string(params),
		CreatedAt:      r.now(),
	})
	if err != nil {
		return Submitted{}, fmt.Errorf("archive: recording the restore operation: %w", err)
	}
	if !outcome.Created {
		// A row appeared under this key between resolveReplay's read
		// above and this insert, which needs two writers on one journal
		// file. It is the same answer as a replay, reached later:
		// nothing new is started, and in particular no second restore is
		// initiated, which is the entire reason this branch returns here
		// rather than falling through.
		return replayOf(outcome.Operation)
	}

	if err := r.journal.MarkOperationRunning(ctx, operationID, r.now()); err != nil {
		return Submitted{}, fmt.Errorf("archive: marking the restore operation running: %w", err)
	}
	if err := r.store.InitiateRestore(ctx, req.Medium, req.Copy.Placement.Location, req.WindowDays); err != nil {
		reason := fmt.Sprintf("the medium refused the restore request: %v", err)
		if failErr := r.journal.FailOperation(context.WithoutCancel(ctx), operationID, r.now(), reason); failErr != nil {
			return Submitted{}, fmt.Errorf("archive: the restore was refused (%v) and recording that failed: %w", err, failErr)
		}
		return Submitted{}, fmt.Errorf("archive: %s", reason)
	}

	return Submitted{
		OperationID: operationID,
		Created:     true,
		WindowDays:  req.WindowDays,
		Wait:        behaviour.RestoreWait,
		Billing:     BillingStatement(behaviour),
	}, nil
}

// resolveReplay asks the journal whether this exact request has already
// been accepted, and answers without writing anything.
//
// The match it insists on is the same one CreateOperation makes on a
// replay, and it is repeated here rather than delegated because this is
// the layer that short-circuits on the answer. A key first used by a
// different actor, for a different action, or against a different
// configuration revision is not this caller's request, and handing back
// the row would tell them a restore of theirs is running when it is
// somebody else's, result text included. Refusing costs a caller a fresh
// key; not refusing costs somebody their privacy.
//
// The refusal is state's own sentinel rather than a new one, because
// core/service already maps it (to ErrIdempotencyKeyConflict) and a caller
// cannot tell, and should not care, which of the two reads noticed.
func (r *Restorer) resolveReplay(ctx context.Context, req Request) (state.Operation, bool, error) {
	existing, err := r.journal.GetOperationByIdempotencyKey(ctx, req.IdempotencyKey)
	if errors.Is(err, state.ErrOperationNotFound) {
		return state.Operation{}, false, nil
	}
	if err != nil {
		return state.Operation{}, false, fmt.Errorf("archive: resolving the restore's idempotency key: %w", err)
	}
	if existing.Actor != req.Actor || existing.Action != ActionRestore || existing.ConfigRevision != req.ConfigRevision {
		return state.Operation{}, false, fmt.Errorf("%w: key %q", state.ErrOperationIdempotencyKeyReused, req.IdempotencyKey)
	}
	return existing, true, nil
}

// replayOf describes a row that already exists, for a submission that
// started nothing.
//
// Every figure in it is read back out of the ROW rather than copied from
// the request that replayed the key, and that is the difference between
// handing back the original request and handing back the new one wearing
// the original's id. The two only differ when a caller replays a key with
// different parameters, which is allowed (a key's identity is its actor,
// its action and the configuration revision, not the window), and in that
// case the row is the only thing that knows what the provider was actually
// asked for. Reporting the replayed window instead would describe a
// restore nobody ever requested.
func replayOf(op state.Operation) (Submitted, error) {
	params, err := ParametersOf(op.Parameters)
	if err != nil {
		return Submitted{}, fmt.Errorf("archive: replaying restore operation %q: %w", op.OperationID, err)
	}
	behaviour, err := Of(params.StorageClass)
	if err != nil {
		return Submitted{}, fmt.Errorf("archive: replaying restore operation %q: %w", op.OperationID, err)
	}
	return Submitted{
		OperationID: op.OperationID,
		Created:     false,
		WindowDays:  params.WindowDays,
		Wait:        behaviour.RestoreWait,
		Billing:     BillingStatement(behaviour),
	}, nil
}

// validate is every refusal Submit makes, in one place, so a reader can
// see the whole list without following the happy path.
func (r *Restorer) validate(req Request) (Behaviour, error) {
	if req.IdempotencyKey == "" {
		return Behaviour{}, fmt.Errorf("%w: a restore request needs an idempotency key", ErrInvalidRequest)
	}
	if req.Artifact == "" {
		return Behaviour{}, fmt.Errorf("%w: a restore request has to name its artifact", ErrInvalidRequest)
	}
	if req.Copy.Placement.Location == "" {
		return Behaviour{}, fmt.Errorf("%w: the copy on %q records no location, so there is nothing to restore",
			ErrInvalidRequest, req.Copy.Placement.Medium)
	}
	if req.Copy.Placement.Medium == state.MediumLocal {
		return Behaviour{}, fmt.Errorf("%w: %q is the local copy, which is a file", ErrNotArchived, req.Copy.Placement.Medium)
	}
	if !req.Acknowledged {
		return Behaviour{}, fmt.Errorf("%w: this request did not say so", ErrNotAcknowledged)
	}
	if req.WindowDays < MinWindowDays || req.WindowDays > MaxWindowDays {
		return Behaviour{}, fmt.Errorf("%w: %d days, and this accepts %d to %d",
			ErrWindowOutOfRange, req.WindowDays, MinWindowDays, MaxWindowDays)
	}
	behaviour, err := Of(req.Copy.Class)
	if err != nil {
		return Behaviour{}, err
	}
	if !behaviour.Archive {
		return Behaviour{}, fmt.Errorf("%w: %q is on %s, which reads on demand",
			ErrNotArchived, req.Copy.Placement.Location, behaviour.Class)
	}
	return behaviour, nil
}

// BillingStatement is the whole truth this product holds about what a
// restore costs, which is that it costs something.
//
// FR-34 forbids a figure, and forbids it for a good reason rather than out
// of caution: this deployment has no price list, no region-specific rates
// and no idea what the operator negotiated, so any number it printed would
// be invented, and an invented number is worse than none because people
// budget against it.
func BillingStatement(b Behaviour) string {
	if !b.RetrievalBilled {
		return ""
	}
	return fmt.Sprintf("the provider bills for retrieving an object from %s, and this product has no price list, so it cannot and will not tell you the amount", b.Class)
}

// Status is what a restore operation looks like right now, re-derived from
// the provider rather than remembered.
//
// There is no percentage field, no estimated-completion field and no cost
// field, and there is nowhere to add one without editing this comment.
// S3 reports a restore as running or finished and nothing else, so a
// struct with somewhere to put a percentage would be a struct somebody
// eventually fills in with a guess.
type Status struct {
	// OperationID is the durable row's id.
	OperationID string

	// Recorded is the operation row's own status: queued, running,
	// completed or failed.
	Recorded string

	// Access is the copy's access state, derived from what the medium
	// says right now.
	Access State

	// Restore is what the medium reports, or nil when it reports nothing.
	Restore *RestoreState

	// Detail is the plain-words sentence for a surface to print.
	Detail string

	// Parameters is what the row recorded about the restore, which is how
	// a process that has just started knows what this operation was even
	// about.
	Parameters Parameters
}

// Derive re-reads a restore operation and works out where it has actually
// got to, by asking the medium rather than by trusting a status this
// process wrote before it was restarted.
//
// # Why the row is not the answer
//
// A restore runs at the provider, over hours, and this process is not part
// of it. Every other operation in this table is executed by a goroutine
// here, so a process that dies mid-operation really has abandoned it and
// the startup sweep is right to fail the row. A restore is the exact
// opposite: the process dying changes nothing about it at all, and a sweep
// that marked it failed would be this product inventing a fact about
// somebody else's system. So the row records what was ASKED FOR, which
// never changes, and where it has GOT TO comes from here, every time.
//
// # What it will and will not conclude
//
// It moves the row to completed only when the provider says the object is
// restored and gives an expiry that has not passed. It never concludes
// anything from silence: a provider that reports no restore status leaves
// the row exactly where it was, described in those words, because "it has
// not started yet" and "it finished and expired" and "I asked the wrong
// bucket" all look identical from here and picking one would be a guess.
func (r *Restorer) Derive(ctx context.Context, operationID string, medium transport.Medium) (Status, error) {
	op, err := r.journal.GetOperation(ctx, operationID)
	if err != nil {
		return Status{}, err
	}
	if op.Action != ActionRestore {
		return Status{}, fmt.Errorf("%w: operation %q is a %s, not a restore", ErrInvalidRequest, operationID, op.Action)
	}

	var params Parameters
	if err := json.Unmarshal([]byte(op.Parameters), &params); err != nil {
		return Status{}, fmt.Errorf("archive: operation %q recorded parameters this build cannot read: %w", operationID, err)
	}

	st := Status{OperationID: operationID, Recorded: op.Status, Parameters: params}

	if op.Status == state.OperationCompleted || op.Status == state.OperationFailed {
		// A terminal row is a decision that was already made and durably
		// recorded, and re-deriving it would let a later reading quietly
		// overwrite it. The access state still comes from the medium,
		// because that is a fact about now rather than about the
		// operation.
		st.Access, st.Restore, st.Detail = r.observe(ctx, medium, params)
		return st, nil
	}

	st.Access, st.Restore, st.Detail = r.observe(ctx, medium, params)

	if st.Restore != nil && !st.Restore.InProgress && st.Access.Retrievable() {
		result := fmt.Sprintf("the copy on %s is restored and readable", params.Medium)
		if st.Restore.ExpiresAt != nil {
			result = fmt.Sprintf("%s until %s", result, st.Restore.ExpiresAt.UTC().Format(time.RFC3339))
		}
		if err := r.journal.CompleteOperation(ctx, operationID, r.now(), result); err != nil {
			return Status{}, fmt.Errorf("archive: recording the finished restore: %w", err)
		}
		st.Recorded = state.OperationCompleted
		st.Detail = result
	}

	return st, nil
}

// observe asks the medium about one object and turns the answer into an
// access state, a restore reading and a sentence.
//
// A medium that will not answer is Unreachable and nothing more. It is
// explicitly not an error out of Derive: the operation is fine, the row is
// fine, and the only thing that has happened is that a bucket did not
// answer this one time, which is a thing to report and not a thing to fail
// an operator's restore over.
func (r *Restorer) observe(ctx context.Context, medium transport.Medium, params Parameters) (State, *RestoreState, string) {
	if r.store == nil {
		return Unreachable, nil, "this deployment has no way to reach a storage medium, so nothing could be asked about this copy"
	}
	rs, err := r.store.RestoreStatus(ctx, medium, params.Key)
	if err != nil {
		return Unreachable, nil, fmt.Sprintf("%s did not answer when asked about %s", params.Medium, params.Key)
	}
	obs := Observation{Probe: Answered, Restore: rs}
	access, accessErr := Access(params.Medium, params.StorageClass, obs, r.now())
	if accessErr != nil {
		return Unreachable, rs, accessErr.Error()
	}
	return access, rs, Describe(access, params.StorageClass, rs)
}

// ParametersOf re-reads what an operation row recorded about the restore
// it describes.
//
// It is exported because the row is the only thing that knows which medium
// a restore was against, and a caller that has just restarted needs that
// before it can ask the provider anything. Keeping the JSON shape private
// and handing out a parsed struct is the difference between one definition
// of that row and two.
func ParametersOf(raw string) (Parameters, error) {
	var p Parameters
	if raw == "" {
		return Parameters{}, fmt.Errorf("%w: this operation row records no parameters", ErrInvalidRequest)
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Parameters{}, fmt.Errorf("archive: operation parameters this build cannot read: %w", err)
	}
	return p, nil
}
