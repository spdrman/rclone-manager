# ADR 0004: Reinstating a quarantined backup, and what it costs the remote source

## Status

Accepted and implemented (issue #220, EPIC B #81). Extends FR-10's state
machine and FR-15's delete gate. Nothing here supersedes an earlier decision.

## Context

`POST /api/v1/quarantine/{id}/revalidate` re-runs the durable-local-copy checks
against a quarantined backup and reports a verdict. It writes nothing, and that
was deliberate rather than an oversight: `core/internal/lifecycle`'s
`Transitions` table gave QUARANTINED exactly one outgoing edge, back to
DISCOVERED, and gave QUARANTINED_LOST none at all.

So a passing revalidate could not restore anything. The operator's only lever
was "Retry ingestion", QUARANTINED to DISCOVERED, a full re-fetch from the
remote source. That is the right answer when the local copy really is bad and
the source is still there. It is the wrong answer in two cases the product
meets:

- **The local copy is fine and the quarantine was the mistake.** A misconfigured
  validator, a restore-test hook that failed for an environmental reason, a
  checksum recorded against the wrong algorithm. Re-transferring gigabytes
  re-establishes a fact a local re-check has just established.
- **The remote source is gone and the local copy is intact.** An artifact sits
  in QUARANTINED (not QUARANTINED_LOST) whenever the remote delete was never
  confirmed, so a producer that cleaned up its own output leaves a perfectly
  good restore point that can only be re-ingested from something that no longer
  exists.

In both cases the only remaining option was to leave the artifact quarantined
forever, which means FR-24 keeps reporting the set as attention-needed and
FR-19's last-known-good protection keeps skipping it.

There is a third case the issue does not name and the code makes worse. A local
volume that is not mounted when FR-17 reconciliation runs makes every COMPLETE
artifact in the set fail its local check, and COMPLETE routes to
QUARANTINED_LOST, which had no exit at all. A five-minute mount problem could
therefore write off an entire backup set irrecoverably.

## The four questions, and a panel

The issue named four genuinely open questions rather than a plumbing change:
what evidence is enough to re-trust an artifact and whether a passing revalidate
qualifies; whether the transition is operator-confirmed or automatic; what it
means for the remote-delete decision, given that COMMITTED promises the remote
is untouched; and what the audit record says, so a later reader can tell a
re-trusted artifact from one that was never distrusted.

Five perspectives argued this out: Data Safety and Recovery, Lifecycle and
State Machine Design, Security (FR-8 treats remote metadata as untrusted),
Operations (the person looking at a stuck quarantine at 2am), and Test and QA.
Three viable options survived.

### Option 1: operator attestation

Add QUARANTINED to COMMITTED, taken on an explicit operator confirmation with a
mandatory note. No evidence requirement: the human is the authority.

Operations liked it, because it works in every case including the ones where
nothing can be proved locally (a backup set configured for transfer
verification alone records no local hash baseline, so there is nothing to
compare against). Data Safety and Security killed it. COMMITTED is the only
state from which a remote delete can be reached, so under this option one click
puts a possibly-corrupt artifact back on the path to having its remote copy
destroyed. That is precisely the failure the repository exists to prevent, and
"the operator said so" is not a check, it is the absence of one.

### Option 2: evidence-gated reinstatement, with permanent remote-delete forfeiture

Add QUARANTINED to COMMITTED and QUARANTINED_LOST to COMPLETE. Each is
operator-triggered, each requires evidence gathered in the same call, and taking
either one permanently forfeits the artifact's remote delete: FR-15's
`DeleteRemote` refuses any artifact whose append-only transition log contains a
reinstatement edge.

### Option 3: a separate trusted state

Same evidence rule, but the trust lands in one or two brand new lifecycle states
(say REINSTATED and REINSTATED_COMPLETE) that the rest of the system treats as
restore points and that have no edge to REMOTE_DELETE_PENDING at all. The
forfeiture becomes structural: `Predecessors(RemoteDeletePending)` stays exactly
`[Committed]`, and `TestQuarantineCannotShortcutToSuccess` survives verbatim.

### Why option 2 won

Lifecycle Design made the strongest case for option 3 and it is worth recording,
because option 2 does pay for its win. Option 3 keeps the safety property
provable from the transition table alone, which is how every other safety
property in this package is proved. Option 2 moves that proof from the table to
a gate.

What decided it was blast radius against the value of that difference. Option 3
needs a schema migration to widen the `artifacts` CHECK constraint, and it needs
every "is this a durable restore point" predicate in the tree to learn the new
names: `internal/health`'s `decideState`, `internal/retention`'s
`gfsIsManagedComplete` and FR-19 eligibility, `internal/revalidate`'s
`eligibleStates`, `internal/reconcile`'s dispatch, `internal/app`'s
`ValidateArtifact`, the metrics surface, the service read model and the web UI.
That is a large change to the meaning of a state vocabulary, in service of a
property option 2 also delivers.

And option 2 does not merely assert the property, it derives it.
`ReinstatementEdges()` is computed from the `Transitions` table itself, so an
edge from a quarantine state into a durable restore point is covered by the
delete gate the moment it is declared, and
`TestEveryQuarantineExitIntoADurableStateForfeitsRemoteDeletion` walks the real
table and proves the two cannot drift apart. The gate is also the right place
for it: `DeleteRemote` is the only call site in the repository allowed to invoke
`Transport.DeleteRemote`, and its whole design is already "revalidate everything
from scratch on every attempt, never trust that an earlier pass checked it".

Test and QA added the condition that made option 2 acceptable: the refusal must
be provable at the gate that destroys data, with a positive control showing the
same fixture deletes when the quarantine detour is removed. Without that control
a "the transport was never called" assertion passes just as happily for a
fixture that was undeletable for some unrelated reason.

A fourth option was raised and rejected outright: reinstate only into a world
where the remote is provably gone, by having the manager `Stat` the remote and
requiring absence. It answers the delete question by having nothing to delete,
but it does not serve the first case at all, and it makes the manager record
COMPLETE ("the remote is confirmed deleted") on the strength of a single remote
read. A transient auth failure that reads as absence would then route a later
local failure to QUARANTINED_LOST instead of the recoverable QUARANTINED. FR-8's
"remote metadata is untrusted" applies, and the failure mode is worse than the
problem.

## Decision

**Two new edges, each returning an artifact to the state it already held.**
QUARANTINED goes back to COMMITTED, QUARANTINED_LOST goes back to COMPLETE, and
nowhere else. `ReinstateFromQuarantine` proves the artifact previously entered
that state by reading the append-only transition log before it writes anything,
so an artifact quarantined out of VERIFYING, whose recorded local path is still
a `.partial`, is refused: it never had a durable copy to re-trust, and its way
back is the ordinary re-ingest.

There is deliberately no QUARANTINED to REMOTE_DELETE_PENDING edge, even though
an artifact can be quarantined from there. COMMITTED must remain the only
predecessor of REMOTE_DELETE_PENDING, which
`TestOnlyCommittedPrecedesRemoteDeletePending` pins, so an artifact quarantined
out of REMOTE_DELETE_PENDING reinstates one step further back, which is the more
conservative of the two.

**Operator-triggered, never automatic.** Nothing in the cycle, the scheduler or
`internal/revalidate` takes either edge. Quarantine's documented meaning is
"this needs a human", and an automatic reinstatement on a passing check would
let a flapping validator or an intermittent mount oscillate an artifact's
trustworthiness with no one watching.

**The checks and the write are one operation.** `ReinstateQuarantined` re-runs
the checks itself rather than accepting a verdict from an earlier
`revalidate` request. A verdict is a fact about the moment it was measured, and
the window between "the operator read a pass" and "the operator clicked
reinstate" is exactly when a failing disk keeps failing.

**A pass has to be something that could have failed.** The local-copy check runs
unconditionally, so a backup with no recorded hash baseline and no configured
validator "passes" on nothing more than the file still being present at its
recorded path. That is not evidence. At least one of these is required:

- a hash baseline recorded at VERIFIED that the durable local copy still
  matches, which proves the bytes are the bytes this manager itself verified,
  and which rests on this manager's own journal rather than on anything the
  remote said; or
- the backup set's configured application validator running now and passing,
  which proves the artifact still restores.

**The evidence has to answer the reason for the distrust.** When the artifact's
own record carries a validator rejection, hash evidence alone is refused: a
matching hash proves the bytes are unchanged, and the unchanged bytes are
exactly what the validator refused. The validator itself has to run and pass.
Only a validator that ran and passed may overwrite the record's validation
verdict; a hash-carried reinstatement leaves it alone, because a hash comparison
says nothing about whether the artifact restores.

**Reinstating forfeits the remote delete, permanently.** `DeleteRemote` refuses
any artifact whose transition log contains a reinstatement edge, before it
records intent and before it touches the transport, and returns the existing
typed `*RemoteDeleteRefusalError` with `Check` = `"quarantine reinstatement"`.
This is not a cooling-off period and it is not conditional on how strong the
evidence was. The evidence that justifies a reinstatement is a local re-check;
that is a weaker thing than the full FR-13 verification chain the artifact
passed on its way to COMMITTED the first time, and it is not enough to authorise
destroying the last remaining source. Preserving a remote copy costs storage.
Deleting the source of an artifact that should not have been re-trusted costs
the backup.

**The audit record is the edge itself.** `state_transitions` is append-only and
idempotency-keyed, so the QUARANTINED to COMMITTED row is permanent in a way no
column on the `artifacts` row could be, and it is what distinguishes a
re-trusted artifact from one that was never distrusted (both simply read
COMMITTED). `state.Journal.LastTransition` is the read, `RecentActivity` already
surfaces the row with its detail, and the detail names which evidence carried
the reinstatement plus the operator's note, so a later reader can judge the
decision and not merely see that one was made.

## What this means for remote deletion, specifically

Before: an artifact reached COMMITTED, and FR-15's four revalidations plus
WP3.2's completion-strategy gate decided whether its remote source could be
deleted.

After: an artifact that has been reinstated out of quarantine can never have its
remote source deleted by this manager, regardless of how completely it passes
those checks afterwards. The remote copy is preserved indefinitely, and
releasing it becomes an operator's decision made outside this manager.

That preservation has a real operational cost, which the FR-15 package doc
already names for a different reason: an archive that never prunes its remote
side eventually fills the source disk. It is the same cost the project already
accepts routinely, because against its own recommended hardened SFTP posture
`model.CompareIdentity` cannot usually reach strong confidence, and the expected
outcome of a correctly functioning delete gate in that deployment is already a
refusal. The refusal is loud in the same way: a typed error, logged with its
check name.

The failure direction is the point. A reinstatement that should not have
happened costs disk on the source. A delete that should not have happened costs
the backup.

## What was deliberately not weakened

`TestOnlyCommittedPrecedesRemoteDeletePending` is unchanged and still passes:
COMMITTED remains the sole predecessor of REMOTE_DELETE_PENDING.

`TestQuarantineCannotShortcutToSuccess` keeps every state it named except
COMMITTED, including the two that stand between an artifact and a destroyed
remote source, REMOTE_DELETE_PENDING and COMPLETE, and quarantine still cannot
re-enter the middle of the pipeline.

FR-13's "a required validator failure must prevent source deletion" proof got
stronger rather than weaker. It now forces the artifact onto COMMITTED with a
bare `Advance`, bypassing every rule `ReinstateFromQuarantine` enforces, and
shows the remote delete is still refused, because the refusal reads the
transition log rather than trusting the writer to have come through the front
door.

QUARANTINED_LOST's terminality was narrowed rather than removed, and stated more
precisely than before. It still has no path back into the pipeline: its only
exit is to the COMPLETE it came from, and `Validate` refuses every other target.
The livelock the original design guarded against needed neither a passing check
nor a human (rediscover something that no longer exists, fail, retry, forever);
this exit needs both, every time round. The test that pinned "no successors at
all" now pins "no exit anything automatic can take", for both quarantine states
rather than one, which is a stronger statement about a larger surface.

## Consequences

- An operator has a real answer for a quarantined backup whose local copy is
  provably intact, including when the remote source is gone.
- A backup set that lost a mount for five minutes is recoverable instead of
  written off.
- Reinstated artifacts count as restore points again, so FR-24 stops reporting
  them as attention-needed and FR-19's last-known-good protection stops skipping
  them.
- Reinstated artifacts accumulate remote copies that this manager will never
  release. Surfacing that (a count of reinstated artifacts whose remote source
  is still present, alongside FR-24's existing counts) is worth doing and is not
  in this change.
- A backup set configured for transfer verification alone, with no local hash
  baseline recorded and no validator configured, cannot be reinstated at all.
  That is the correct refusal: there is genuinely nothing to prove with. The
  remedy is to configure a validator, or to re-ingest.
