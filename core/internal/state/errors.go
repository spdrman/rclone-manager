package state

import "errors"

var (
	// ErrUnknownSchemaVersion is returned by Open when the database already
	// has a migration recorded in schema_migrations that this binary's
	// embedded migrations do not know about. That means the database was
	// created or migrated by a build newer than (or otherwise different
	// from) this one. Applying further migrations, or simply reading and
	// writing, on top of a schema this binary cannot account for would be
	// guessing, not verifying, so the migration runner refuses instead.
	ErrUnknownSchemaVersion = errors.New("state: database schema is newer than this binary's migrations")

	// ErrSchemaDrift is returned by Open when a migration this binary does
	// recognise was already applied to the database, but the checksum
	// recorded for it does not match the migration file compiled into this
	// binary. That means the migration file's content changed after it was
	// applied to this particular database, which the runner refuses to
	// paper over by reapplying or ignoring.
	ErrSchemaDrift = errors.New("state: applied migration does not match this binary's migration file")

	// ErrAlreadyDiscovered is returned by Discover (and by RecordTransition
	// with From == "") when an artifact with the same identity already has
	// a journal row recorded under a different idempotency key. This is not
	// the crash-and-retry case (that returns Outcome.Applied == false with
	// no error); it means something tried to discover the same artifact
	// identity twice as two logically distinct attempts. Callers should Get
	// the existing record rather than treat this as a hard failure.
	ErrAlreadyDiscovered = errors.New("state: artifact already discovered")

	// ErrArtifactNotFound is returned when a transition or query names an
	// artifact that has no journal row.
	ErrArtifactNotFound = errors.New("state: artifact not found")

	// ErrStateMismatch is returned when a transition's expected From state
	// does not match the artifact's current recorded state. This is
	// distinct from idempotent replay (a repeated Key is recognised and
	// short-circuited before this check ever runs): it means the caller
	// asked to transition an artifact out of a state it is not actually in,
	// which is either a race between two writers or a bug in the caller's
	// own sequencing.
	ErrStateMismatch = errors.New("state: artifact is not in the expected state")

	// ErrIdempotencyKeyReused is returned when the same idempotency key is
	// presented for a transition whose artifact or target state does not
	// match what that key was first recorded against. A fresh key must be
	// used per logical attempt; reusing one across different attempts
	// defeats the guarantee RecordTransition otherwise provides.
	ErrIdempotencyKeyReused = errors.New("state: idempotency key already used for a different transition")

	// ErrOperationNotFound is returned by GetOperation, MarkOperationRunning,
	// CompleteOperation and FailOperation when no operation row matches the
	// given operation_id.
	ErrOperationNotFound = errors.New("state: operation not found")

	// ErrOperationIdempotencyKeyReused is CreateOperation's equivalent of
	// ErrIdempotencyKeyReused above: the same idempotency key was presented
	// for a request whose action or configuration revision does not match
	// what that key was first recorded against. Unlike RecordTransition's
	// artifact journal, silently serving back an unrelated operation here
	// would mean telling a caller "your request is already in flight" about
	// a request it never actually made, so this is refused rather than
	// papered over.
	ErrOperationIdempotencyKeyReused = errors.New("state: idempotency key already used for a different operation")
)
