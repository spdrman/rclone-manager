package lifecycle

// This file has no code. It writes down the crash-safety reasoning FR-10
// asks for: for the transitions that matter, what's on disk if the process
// dies immediately before, during, and immediately after each one. It's
// kept as its own file, rather than folded into machine.go's comments,
// because it's reasoning about the *sequencing contract* future callers
// (the FR-11 transfer loop, the FR-16/17 delete-and-reconcile code) must
// hold, not about this package's own code. This package cannot enforce I/O
// ordering it doesn't perform, only the state graph around it.
//
// # The one fact that makes all of this tractable
//
// Every transition here is, at the lifecycle-package level, a single write
// to the FR-9 journal's state column, and SQLite transactions are atomic.
// So "during" a journal write never produces a torn or partial value: a
// restart observes either the old state (the write never landed) or the new
// one (it did). That collapses every "before / during / after" question
// into two real cases, except where a transition also straddles a call to
// something outside the journal (the local filesystem, or the remote).
// Those cases are called out explicitly below.
//
// # COMMITTING -> COMMITTED
//
// This is FR-11's "durably commit local file" step: fsync the local file,
// rename it from its .partial name to its final name, fsync the directory,
// then record COMMITTED.
//
//   - Before: the journal still reads VERIFIED. The fsync/rename/fsync
//     sequence may be partially done or not started at all. Safe: nothing
//     has told any other component the artifact is committed, so a restart
//     re-runs the whole durable-commit step from scratch. Both fsync and a
//     rename to the same final name are safe to repeat.
//   - During (the journal write itself): atomic. A restart observes
//     VERIFIED (the write never landed, retry the commit step, which is
//     safe per "before") or COMMITTED (it landed, see "after").
//   - After: the journal reads COMMITTED. By the sequencing contract, the
//     local file was already durably at its final name before this write
//     was even attempted, that has to be true first, never assumed after
//     the fact. The remote is still completely untouched. This is the safe
//     state the whole design exists to reach before anything irreversible
//     can happen to the remote copy.
//
// # COMMITTED -> REMOTE_DELETE_PENDING
//
// Recording *intent* to delete the remote object, strictly before any
// delete call is issued.
//
//   - Before: the journal reads COMMITTED, the remote is untouched, no
//     delete has been attempted. Safe: retry the transition.
//   - During: atomic. A restart observes COMMITTED (retry) or
//     REMOTE_DELETE_PENDING (see "after"). The remote delete call has not
//     been issued in either case, because the contract this package's
//     graph exists to protect is that the state write happens strictly
//     before the delete call, never after or concurrently with it.
//   - After: the journal reads REMOTE_DELETE_PENDING, the remote object
//     still exists. This is a durable statement of intent, recorded before
//     anything destructive happens, so a crash right here just means the
//     delete hasn't been tried yet.
//
// # REMOTE_DELETE_PENDING -> COMPLETE
//
// This is the one genuinely three-way window in the whole machine, because
// it spans a call to a system this package does not control: the remote
// delete may not have been issued yet, may be in flight, or may have
// already succeeded on the remote side without the caller having recorded
// that yet (for example a network partition after the remote applied the
// delete but before the caller received the acknowledgement).
//
//   - Before the remote delete call: the journal reads REMOTE_DELETE_PENDING,
//     the remote object still exists, untouched. Safe: nothing has happened
//     yet.
//   - During the remote delete call: unknown from the caller's side whether
//     it took effect. This is fine because FR-16 requires re-comparing the
//     remote object's identity against what was recorded at discovery
//     immediately before (re-)issuing the delete. A restart that finds the
//     journal still at REMOTE_DELETE_PENDING re-runs that check:
//     -- same identity still present: issue the delete again if this is a
//     retry (deleting a positively re-identified object is safe to
//     repeat);
//     -- object already absent: reconcile straight to COMPLETE without
//     ever calling delete again (FR-17's own "absent, final,
//     REMOTE_DELETE_PENDING -> reconcile COMPLETE" row; this is exactly
//     the case where the delete had actually already succeeded before the
//     crash);
//     -- a *different* object now at that identity: refuse and flag for
//     investigation (FR-17's "changed identity -> refuse delete;
//     investigate" row), never delete something that wasn't positively
//     re-identified.
//     What never happens on any retry is treating "the journal already
//     says REMOTE_DELETE_PENDING" as license to skip that re-identification
//     and delete blind.
//   - During the journal write to COMPLETE: atomic, and only ever attempted
//     after the remote side of the story above is already resolved. A
//     restart observes REMOTE_DELETE_PENDING (re-run the check-then-maybe-
//     delete step above, which is idempotent) or COMPLETE (done).
//   - After: the journal reads COMPLETE. The remote object is confirmed
//     gone; the local durable copy is retained. This is the fully safe
//     terminal outcome, and it was never at risk, because COMMITTED was
//     durably recorded before REMOTE_DELETE_PENDING, which was durably
//     recorded before any delete call. That ordering is what FR-10 exists
//     to guarantee, and TestOnlyCommittedPrecedesRemoteDeletePending proves
//     it at the graph level.
//
// # COMPLETE -> QUARANTINED_LOST
//
// This transition doesn't protect a copy, it records that one is gone. By
// the time COMPLETE is reached the remote is already confirmed deleted, so
// this edge only fires when a later integrity check (periodic
// re-verification, or reconciliation on restart) finds the durable local
// copy corrupted with nothing left anywhere to recover from.
//
//   - Before: the journal reads COMPLETE. The local file may already be
//     corrupted on disk (bit rot doesn't wait for a state transition to
//     happen), but nothing has recorded that yet. Safe: the artifact still
//     reports as a good restore point until the check runs and the write
//     lands, so a crash here just means the corruption is found again on
//     the next check.
//   - During: atomic, like every journal write. A restart observes COMPLETE
//     (the corruption finding wasn't recorded, so it gets found again) or
//     QUARANTINED_LOST (it was).
//   - After: the journal reads QUARANTINED_LOST. FR-19 already excludes
//     QUARANTINED from last-known-good eligibility; QUARANTINED_LOST must
//     be excluded the same way, because unlike QUARANTINED there is no
//     retry that could ever make this artifact good again. This state has
//     no declared successors (TestNoStateIsALeak documents why that's not
//     itself a leak), so nothing here can loop; it sits as a durable,
//     visible record of loss until an operator resolves it out of band, for
//     example by confirming a different backup-set generation still
//     satisfies last-known-good.
//
// # Reading this against the two failure shapes that would be a design bug
//
// "The artifact is stuck" can't happen: every state this package defines
// has at least one declared outgoing edge, FAILED, QUARANTINED and
// QUARANTINED_LOST included, and TestNoStateIsALeak walks all of them. The
// two quarantine states are the ones whose every exit is an operator
// decision rather than an automatic one, which is deliberate and is what
// avoids the other failure shape below.
//
// "The remote may be deleted without a committed local copy" can't happen,
// because REMOTE_DELETE_PENDING, the only state a delete call may ever be
// issued from, has exactly one legal predecessor, COMMITTED
// (TestOnlyCommittedPrecedesRemoteDeletePending), and COMMITTED is only
// ever reached after the local file is durably fsynced and renamed to its
// final name.
