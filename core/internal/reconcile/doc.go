// Package reconcile is the FR-17 implementation: on startup, before normal
// processing touches anything, I compare what the journal (internal/state)
// already believes against what the local filesystem and the remote
// backend actually show right now, and I bring the journal back in line
// wherever the two disagree.
//
// # The table
//
// FR-17 (docs/EPIC.md) tabulates every combination this package has to
// handle:
//
//	Remote   Local          Journal                Required behavior
//	exists   absent         DISCOVERED             transfer
//	exists   partial        TRANSFERRING           safe retry/restart
//	exists   final          COMMITTED              verify and proceed toward delete
//	absent   final          REMOTE_DELETE_PENDING  reconcile COMPLETE
//	absent   final          COMPLETE               no-op
//	exists   invalid final  any                    preserve remote; quarantine local
//	absent   invalid final  any                    quarantine, unrecoverable
//	changed  final          delete pending         refuse delete; investigate
//
// "transfer" and "safe retry/restart" are not actions this package takes
// itself: DISCOVERED and TRANSFERRING are already exactly the precondition
// the FR-11 pipeline's own Transfer step expects, .partial files are
// deliberately disposable (transfer.go clears any stale one before
// starting a fresh copy), and there is nothing durable to fix at either
// state. I report both as consistent, unchanged findings and leave driving
// the pipeline forward to normal processing, which FR-17 explicitly runs
// before ("on startup and before normal processing"), not this package.
// The same reasoning covers TRANSFERRED, VERIFYING, VERIFIED and
// COMMITTING too: every one of them still points at a .partial file, not a
// final one, so they get the identical no-op treatment TRANSFERRING does,
// even though the table only spells out TRANSFERRING by name.
//
// The remaining five rows are where I actually act, and they all turn on
// two questions: does a local final copy exist and is it valid
// (checkLocalFinal, localcheck.go), and does the remote object this
// artifact was discovered at still exist, and is it still the same object
// (statRemote and model.CompareIdentity, remote.go). COMMITTED and
// REMOTE_DELETE_PENDING are the only two states the FR-10 machine (see
// machine.go's Transitions table) admits into QUARANTINED at all, so "any"
// in the invalid-local rows means exactly those two, never DISCOVERED or
// TRANSFERRING, which never have a final local copy to find invalid in the
// first place.
//
// # The gap: "absent, invalid final"
//
// The original table had no row at all for a remote object that is gone
// and a local final copy that is bad. I did not invent this state to fill
// it in: it already existed as QUARANTINED_LOST (internal/lifecycle,
// state.go and machine.go), added while building FR-10, specifically
// because the two-state QUARANTINED design (its only exit is back to
// DISCOVERED) would ask this exact case to rediscover and re-transfer a
// source that machine.go's own reasoning already established is gone,
// which fails, lands in FAILED, and FAILED -> DISCOVERED sends it right
// back around forever. QUARANTINED_LOST has no route back into the
// pipeline and no automatic exit of any kind: reaching it means an operator
// has to act, not that another automatic retry will. (Since issue #220 an
// operator who can prove the durable local copy is intact after all can
// return it to the COMPLETE it came from, which is still not something this
// package or any other automatic pass ever does.)
//
// machine.go admits QUARANTINED_LOST from exactly one place: COMPLETE. So:
//
//   - COMPLETE, invalid final: I record QUARANTINED_LOST directly
//     (reconcileComplete).
//   - REMOTE_DELETE_PENDING, remote confirmed absent, invalid final: I
//     reconcile to COMPLETE first, the same write the plain "absent, final,
//     REMOTE_DELETE_PENDING" row already makes on its own, and then on to
//     QUARANTINED_LOST in the same call (reconcileDeletePending). Both
//     writes go through lifecycle.Advance, so if I ever got the order
//     wrong and tried to skip straight from REMOTE_DELETE_PENDING to
//     QUARANTINED_LOST, Validate would refuse it outright: there is no
//     such edge in the table, on purpose.
//
// COMMITTED with an invalid local copy always goes to ordinary
// QUARANTINED, never QUARANTINED_LOST, regardless of what the remote looks
// like, because machine.go has no COMMITTED -> QUARANTINED_LOST edge at
// all, and I am not willing to manufacture one by first recording
// REMOTE_DELETE_PENDING, an actual statement of delete intent FR-15 owns,
// as a side effect of a corruption finding this package made on its own.
// "Confirmed gone" in this project's own vocabulary is established through
// that delete-and-reconfirm pathway, not through a bare Stat call made
// before any intent to delete was ever recorded, so I do not treat one the
// same as the other here. That is why I never even call Stat for a
// COMMITTED artifact: no answer it could give changes which state this
// package is allowed to move to.
//
// # Idempotence
//
// I read every decision fresh from the current journal row on every call,
// never from a cache or a previous Report, so running Reconcile twice in a
// row is naturally a no-op the second time: whatever the first call fixed
// is no longer in the state that triggered the fix, and every row above
// that already agreed with reality stays a no-op both times. The one place
// that is not automatic is a crash between two calls to
// lifecycle.RecordTransition for the same artifact in the same pass (the
// REMOTE_DELETE_PENDING -> COMPLETE -> QUARANTINED_LOST chain above), or a
// restart that re-runs a whole Reconcile call against a row an earlier,
// interrupted call already partly moved. reconcileKey derives every
// transition's idempotency key from the artifact identity, the exact
// (From, To) edge, and the journal row's own UpdatedAt at the moment I
// read it, rather than a counter this package invents. UpdatedAt already
// strictly advances on every write RecordTransition makes to that row, so
// a retry that lands before anything else has touched the row reproduces
// the exact same key and the journal's own idempotency-key replay
// (internal/state/journal.go's RecordTransition) recognises it, rather
// than my package needing to persist an attempt counter of its own.
package reconcile
