# EPIC E conformance matrix

Every gate line EPIC E declares, as a cell: what it certifies, where it is
asserted, and the planted violation that has to make it red. One row per line of
the spec's two exit gates and one per entry of its section 4 planted-violation
table, with nothing dropped for not being ready yet.

`docs/EPIC-E-alternative-storage.md` is the contract. This file is the account of
which parts of it are currently checked by something that has been watched to
fail, which parts are checked by nothing, and which issue owns each gap.

## Why this file exists in this shape

A conformance suite is the last thing standing between a plausible-looking EPIC
and a data-loss bug, so the bar is higher than "the tests pass". Three checks in
this repository were found in a single day that could not fail: a rule that could
never fire on a backup set with history because settled rows were counted as
throughput, a mutation that disabled two mechanisms at once so a ten-second
budget looked safe when it was not, and a guard that matched a flag in the
comment explaining the flag rather than in the command. A matrix of assertions
like those is worse than no matrix, because it converts "untested" into
"certified".

So every row below carries a **falsification**: the specific mutation that must
turn that cell red. A row whose falsification has actually been run and watched
fail is `PASS`. A row whose falsification cannot be run yet, because the code it
would mutate does not exist, is `BLOCKED`, with the issue that unblocks it. There
is no third reading, and in particular there is no row that is green because
nobody has looked.

## How to read an outcome

| Outcome | Means |
| --- | --- |
| `PASS` | The check exists, runs in `scripts/ci-local.sh`, and has been shown to go red against a real planted violation. The falsification column names the mutation and it is automated in `scripts/compat/selftest.sh`. |
| `PARTIAL` | Part of the claim is checked and shown to fire; the rest waits on an unlanded issue. The row says which half is which. |
| `BLOCKED` | The check is specified here and cannot conclude today, because the code it certifies is not merged. Not a pass, and not a fail. The row names the issue. |

Two of the twelve cells are compared additively rather than exactly, and the
reason is the same in both cases: the surface has a direction in which growth is
routine and harmless, and an exact comparison there would have people
regenerating the corpus without reading it. `03-migrated-schema` may gain tables
and migrations because FR-29 adds both. `06b-cli-usage-block` may gain lines
because a new subcommand adds them, which #350 did on the first main this gate
met. Neither may change or lose a line it already has, and the violation the CLI
family exists to catch, an additive column rendered where there is no non-local
placement, lands in the artifact detail, which is compared exactly.

`BLOCKED` is a declaration, and like the phase 4 matrix's declarations it is
checked rather than trusted: `TestTheMatrixDoesNotCiteSuitesThatDoNotExist` in
`core/tests/compat` reads this file and fails if a `PASS` or `PARTIAL` row cites
a path the repository does not have, and fails if no row is BLOCKED at all. A row cannot be quietly upgraded by editing a word.

## Where the checks live

| Thing | Path |
| --- | --- |
| The FR-35 medium-free surface corpus and its gate | `core/tests/compat` |
| The captured baseline | `core/tests/compat/testdata/medium-free-surfaces.json` |
| The composed three-tier scenario over MinIO, and its continuous invariant watcher | `core/tests/conformance` |
| The crash matrix, against a directory bucket and against a real S3 endpoint | `core/tests/movecrash`, `core/tests/conformance/crashmatrix_test.go` |
| The planted violations, automated | `scripts/compat/selftest.sh`, `scripts/conformance/selftest.sh` |
| The gate step that runs them | `scripts/ci-local.sh` |

Regenerating the corpus is `COMPAT_UPDATE=1 go test ./tests/compat/` from `core/`,
and every regeneration is a claimed behavior change that belongs in a commit
message. The one assertion a regeneration cannot silence is
`TestUpgradingAndInstallingFreshAgreeWithEachOther`, which compares two captures
from the same run rather than a capture against a file.

## What has landed since this file was written

#236 (E1.4, placement records) merged, bringing `core/migrations/0007_placements.sql`
with it. The FR-35 gate ran against it unchanged and stayed green: the schema cell
picked up `placements`, `placement_moves` and three indexes as additions, and every
cell compared line for line was byte identical, including the two that migrate an
existing populated journal. So the placements backfill makes no observable
difference to a medium-free deployment, which is the half of #236's fourth
acceptance criterion that lives here.

The spec still calls that migration `0004_placements.sql`, and `0004` has been
`backup_set_halts` since before EPIC E started. Only the prose is stale; the code
uses `0007`.

Phase 2's four issues (#238, #239, #240, #241) are composed and #242's own
suite runs over them: `core/tests/conformance` builds the exact three-tier
chain this gate names from a real `config.yaml`, seeds a backup set across
two years, and then advances the clock and lets `GFSDecide`,
`PlanHomeMoves` and the move engine decide what has to happen. Nothing in it
tells the engine where an artifact belongs.

Composing them turned up two things no unit suite could have, and both are
now issues rather than rows: an archive-class tier can never take delivery
of an artifact (#428), and a chain with two medium tiers needs a
medium-to-medium move, which the engine refuses (#429). Neither loses data.

#428 has since been answered at the engine by #437: a move to an
archive-class destination is refused at PLAN time, for nothing, and
reported again every cycle rather than abandoned and retried. That answer
is also what made the composed scenario's own annual rung untestable, since
the rung stopped producing anything to observe, so the scenario's annual
tier is now an ordinary `s3` medium and the archive pairing has a cell of
its own that asserts the refusal, the zero move rows and the zero objects.
What the suite can and cannot claim about a cold class as a result is
spelled out in P2.1 and P2.6, and again at the bottom of this file. #442,
which would move the same refusal into `config.Validate`, is the next place
it belongs, and the conformance suite no longer stands in its way: no cell
needs a tier on an archive class to LOAD, and the one that builds such a
chain says in its own failure message that it is the cell to move.

#239's rows (P2.4 and V6) are PASS as of this run, and what changed is more
than a word. Its code is in the tree. The prune cell that used to pin
"REFUSE, because this build has no medium-aware prune" now pins the delete
itself, over MinIO, on an artifact the chain really moved, with three
different verdicts coming out of one pass over artifacts on three different
mediums. The spec's own planted violation for V6, an object swapped behind
its key, is run rather than described. Four falsifications are automated in
`scripts/conformance/selftest.sh`, and every one has been watched to go red.

Phase 1's rows have NOT been re-read, and that is deliberate rather than an
oversight. #234, #235, #236 and #237 are all closed and their code is in the
tree, so "BLOCKED (#235)" no longer describes why P1.3 to P1.5 are not PASS.
What is missing is not the code, it is an automated falsification: each of
those violations was planted once, by hand, in the landing PR (#369 records
the credential canary and the backend set, #383 records the verification
honesty half), and this file's own definition of PASS asks for one that
lives in a self-test and runs every time. Whoever automates them owns
re-reading the rows, and doing it from #242's lane would mean upgrading four
cells on evidence read out of a pull request description.

## Phase 1 exit gate

| # | Outcome | Certifies | Where | Falsification |
| --- | --- | --- | --- | --- |
| P1.1 | PASS | A config declaring an `s3` medium and a tier `medium` reference validates, round-trips through a settings save without injecting fields into a legacy config, and a config naming neither behaves identically to today. | `core/internal/config/storage_mediums_test.go`, `core/service/settings_mediums_test.go`, cell `01-config-validation` in `core/tests/compat` | A tier with no `medium` key resolving to no medium instead of `local`. Run, caught. |
| P1.2 | PASS | A config with an inline literal credential fails `Load` with an unknown-field error. | `core/tests/compat/testdata/configs/53-invalid-inline-medium-secret.yaml`, cell `01-config-validation` | The refusal text is pinned by the corpus; weakening `KnownFields(true)` or the message moves the cell. Run, caught (via the same cell's control). |
| P1.3 | BLOCKED (#235) | The rclone backend set is exactly `local`, `sftp`, `s3` required and `crypt` accepted, and the binary-size delta is recorded in the landing PR. | `core/internal/transport/rclone/backends.go` and its set test, once #235 lands | Registering a fourth backend without updating the set test. |
| P1.4 | BLOCKED (#235) | The MediumStore contract suite passes against the local backend in-tree and against a MinIO fixture, including the explicit capability refusal where attestation is unsupported. | `core/internal/transport/contract`, once #235 lands | An `attested` request silently degrading to `existence` instead of returning a capability result. |
| P1.5 | BLOCKED (#235) | The credential canary passes for file, env and command sources, and its planted violation fails it. | #235's canary test | A build that logs the resolved medium config verbatim. |
| P1.6 | PASS | Migration 0007 backfills a local placement for every pre-existing artifact row, and the golden retention tests and the full existing suite pass unmodified against the migrated schema. | `core/internal/state/placements_test.go` and `core/internal/state/placementcrash_test.go` for the backfill itself (#236, merged); cells `10-upgraded-artifact-rows` and `11-upgraded-retention-verdicts` plus `TestUpgradingAndInstallingFreshAgreeWithEachOther` in `core/tests/compat` for the "unmodified" half | The spec's own, and it fires: a planted migration that rewrites `retention_tier` during backfill turns cell 10 red, and one that rewrites `discovered_at` turns cell 11 red, both automated in `scripts/compat/selftest.sh` and both run against a tree that now carries the real `0007_placements.sql`. The upgrade this cell runs now copies the transition history alongside the artifact rows, which #396 had blocked and #381 fixed, see U1. |
| P1.7 | BLOCKED (#237) | Revalidation reports `existence` class for a medium placement and never a stronger class it did not achieve. | #237's class-string assertion | A revalidation pass forced to `existence` reporting itself as content verification. |
| P1.8 | BLOCKED (#235, #236, #237) | Nothing in phase 1 can delete an artifact copy anywhere; the destructive-safety suite diff shows no new deletion path. | `core/internal/app/destructive_b34_test.go`, once the phase 1 code exists to diff against | A `DeleteObject` call reachable from a phase 1 code path. Deliberately not attempted here as an inventory of every `os.Remove` in `core/`: most of them are temp-file cleanup, the list churns on every refactor across ten active lanes, and a cell people learn to regenerate is the thing this matrix exists to avoid. |

## Phase 2 exit gate

| # | Outcome | Certifies | Where | Falsification |
| --- | --- | --- | --- | --- |
| P2.1 | PARTIAL (#429) | A three-tier chain (daily local, monthly `s3`, annual `s3` cold) runs end to end against MinIO, with the standing invariant (at least one ACTIVE verified placement per managed-complete artifact) asserted continuously by the harness rather than sampled. | `core/tests/conformance` (`threetier_test.go` runs the chain, `watcher_test.go` is the watcher, `sampler_test.go` is the control that gives "continuously" its meaning) | The gate line's own, and it fires: the source released one phase early opens a window in which neither copy is verified, and the SAME run judged two ways has the sampler seeing nothing and the watcher failing at "after the journal wrote phase COPIED". A completed move that never removes the source copy is caught too (`still has a local copy after a completed move to "annual_s3"`), which is what holds the third rung's delivery to more than a journal row. Both automated in `scripts/conformance/selftest.sh`. What is now PASS-shaped: all THREE rungs take delivery in one run, each from a local copy, with the bytes read back off each bucket. Two things keep it PARTIAL. The hop from one medium rung to the next is medium-to-medium and the engine refuses it (#429), so an artifact reaches any rung from local and never walks along the chain. And the annual rung is not a cold class here and cannot be, for two independent reasons that are each written down as a check: this MinIO refuses every archive class (`archiveboundary_test.go`), and the product refuses a tier-to-archive move before it costs anything (#428, answered by #437, asserted in `TestAnArchiveClassTierIsRefusedBeforeItCostsAnything`). |
| P2.2 | PASS | The crash matrix passes: a forced crash at every move phase boundary, restart reconciliation, and the move either completed or abandoned with the source intact. | `core/tests/movecrash` (eleven cells against rclone's local backend) and `core/tests/conformance/crashmatrix_test.go` (the same eleven against MinIO, through the same harness binary) | The spec's own, run and caught: the source delete issued before `VERIFIED` is durably recorded, which takes three edits (the phase edge, the driver's case, and the phase `intendSourceDelete` names as the one it is leaving) and is refused as an illegal write with only two. Automated in `scripts/conformance/selftest.sh`, together with a destination trusted instead of re-verified, which turns the two hostile-endpoint cells red on the bytes rather than on the phase. |
| P2.3 | PASS | Moving an artifact does not change its retention bucketing: verdicts before and after are bit-identical. | `TestAMoveDoesNotChangeARetentionVerdict` in `core/tests/conformance` for the across-a-move half, plus cells `04-retention-verdicts` and `11-upgraded-retention-verdicts` in `core/tests/compat` for the migration half | Both halves caught. Rewriting the journal's discovery timestamp turns cell 11 red (`scripts/compat/selftest.sh`), and bucketing an artifact by when its copy was VERIFIED rather than when it was discovered, which is the value a move itself writes and therefore the likeliest way FR-32 would be broken by one, turns the across-a-move check red (`scripts/conformance/selftest.sh`). The comparison is bit-identical rather than tier-for-tier, because a move that shifted a bucket by a month would keep the artifact under a tier name that still looked right and change which artifact each bucket keeps. |
| P2.4 | PASS | Prune against a medium refuses on identity mismatch, and the mandatory dry-run names the medium for every proposed deletion. | `core/tests/conformance/prune_test.go` for both halves composed over MinIO, on an artifact this same process really moved onto a bucket; `core/internal/placement/reclaim_test.go` for the FR-16 re-check against a double, including the same-length swap a size comparison cannot see and this endpoint cannot show; `core/internal/retention/pruneonmedium_test.go` for the refusal-first decision table; `core/cmd/backup-manager/retention_placement_test.go` for the dry-run's `medium=` field and the control that a local artifact's line did not grow one | The spec's own, run and caught: a fixture that swaps the object behind a key before prune, and the apply has to refuse and leave the object where it is. Four mutations automated in `scripts/conformance/selftest.sh` and all four watched red: the medium copy sent down FR-20's local path (`is REFUSE, want DELETE`), a medium delete reported but never made (`PruneApply asked the medium pruner for []`), the FR-16 re-check run and its answer ignored (`prune's verdict is DELETE, not REFUSE`), and a dry-run that stops naming the medium (`does not say where its deletion would happen`). The composed cell's own control is that one pass produces three different answers over artifacts on three different mediums, so a build that refused everything or deleted everything fails it. The local half of the same rule stays live: cell `05-prune-verdicts` goes red when prune is mutated to delete a file it could not stat instead of refusing. |
| P2.5 | PASS | A tier-to-medium settings save without the disclosure acknowledgment is refused by the API, with allow and deny tests. The same gate stands in front of a backup set's own chain (`PUT /backup-sets/{source}/{set}/retention`) and the CLI's `backup-set retention --policy-file`, since an override can name a medium per tier too. | `core/service/placements_test.go` (the deny, allow, does-not-ask-again and per-set halves against the real service), `apps/common/webhost/placements_contract_test.go` (the typed `MEDIUM_DISCLOSURE_REQUIRED` refusal on both routes, and the acknowledgment crossing the seam), `core/cmd/backup-manager/backupsetretention_test.go` (the CLI half) | The spec's own: the gate removed from `core/service.UpdateSettings` and from `SetBackupSetRetention`, each run and caught by its refuses-a-first-mapping test; the per-set gate reading the deployment's chain instead of the set's own, run, caught; the handler mapping the refusal onto `INVALID_REQUEST`, run, caught; the acknowledgment dropped at the HTTP seam or echoed back out, each run, caught; the CLI flag not wired, run, caught. |
| P2.6 | PARTIAL (#428) | An artifact on an archive class shows `requires_restore`, a restore is a durable operation surviving restart, and no surface anywhere renders a cost figure or an invented ETA. | `TestTheContractServesNoCostFigureAndNoInventedETA` in `core/tests/compat`, and `core/tests/conformance/archiveboundary_test.go` for the access-state derivation and the ceiling that follows from it | Three things are now caught. The no-cost half fires in both directions: a cost field, and an invented ETA plus a percentage. An archived copy allowed to claim content class turns the ceiling check red, and an existence-class placement allowed to satisfy the standing invariant turns its own check red; both automated in `scripts/conformance/selftest.sh`. A fourth is now caught: an archive-class destination refused after planning instead of before turns `TestAnArchiveClassTierIsRefusedBeforeItCostsAnything` red on `for a move it refused before planning`, and the mutant shows exactly the shape #437 measured, a move row per cycle accumulating for ever. So what an archive tier DOES is asserted rather than described: the chain plans it, the engine refuses it as a policy refusal naming `GLACIER`, no move row is written, no object is created, and the next cycle says the same thing and spends the same nothing. PARTIAL is the END TO END archive half, and it is not blocked on code that has not been written: it cannot be run here at all. This MinIO takes 1 of the 7 storage classes the config accepts and answers `InvalidStorageClass` to the other six, so no archived object can be created to observe, let alone restored, and #428 records that the product could not complete the move even against an endpoint that took it. That is written down as a check rather than a caveat, so the day either fact changes this suite says so. |
| P2.7 | PASS | FR-35 holds: a deployment upgraded with a medium-free config shows zero behavioral difference through config validation, retention verdicts, API responses (minus additive fields) and CLI output. | `core/tests/compat`, twelve cells | The spec's own: a migration variant that rewrites `retention_tier` during backfill. Run, caught by cell `10-upgraded-artifact-rows`, and caught again by `TestUpgradingAndInstallingFreshAgreeWithEachOther` when the corpus is regenerated around it. |
| P2.8 | PASS | `check-contract-drift.sh` and `check-client-paths.sh` pass with the new operations; the layer manifest classifies every new file; `verify-core-without-distribution.sh` still passes. | `scripts/api/check-contract-drift.sh`, `scripts/api/check-client-paths.sh`, `scripts/architecture/check-layer-manifest.sh`, `scripts/architecture/verify-core-without-distribution.sh` | All four run in `scripts/ci-local.sh` and all four have mutation self-tests (`scripts/api/selftest.sh` for the first two). #240 landed the API lane: five schemas, four new fields on existing shapes and one new error code, all additive, and deliberately no new client path (placements ride the artifact surface, mediums ride settings, the consent rides the two retention writes), so `check-client-paths.sh` had nothing new to admit and `check-contract-drift.sh` regenerated cleanly. The contract-level falsification that did fire on the way in: the no-cost gate (P2.6) refused two field names on the new ladder schema, `cost` and `costs_egress`, and they were renamed rather than exempted. |

## Section 4 planted violations

The spec names one violation per guard, and says the landing PR for each gate
records its planted violation actually failing. This is that ledger.

| # | Guard | Planted violation | Outcome |
| --- | --- | --- | --- |
| V1 | Source survives every move uncertainty | A mutation that issues the source delete before `VERIFIED` is durably recorded | PASS, automated in `scripts/conformance/selftest.sh`. Caught by the continuous watcher at the instant, and by the crash matrix's byte assertion in the variant where the destination is trusted instead of re-read. |
| V2 | Medium data only ever adds (FR-32) | A mutation that admits S3 `LastModified` as a producer timestamp | PARTIAL. The literal mutation is still unreachable, because nothing carries `LastModified` into a record at all; `transport.ObjectInfo.ModTime` exists and no retention path can see one. Two composed analogues are live and caught: a backfill that re-derives `discovered_at` (cell `11-upgraded-retention-verdicts`, `scripts/compat/selftest.sh`) and bucketing by a placement's `verified_at`, which is a medium-shaped timestamp a move genuinely writes (`scripts/conformance/selftest.sh`). |
| V3 | Bucketing invariant under movement | A mutation that rewrites the journal's discovery timestamp | PASS, automated in `scripts/compat/selftest.sh` |
| V4 | Credential canary (FR-33) | A build that logs the resolved medium config verbatim | BLOCKED (#235) |
| V5 | Inline secret refusal (FR-33) | A config with a literal `secret_access_key:` | PASS, pinned by cell `01-config-validation` |
| V6 | Prune identity re-check on mediums (FR-30) | A fixture that swaps the object behind a key before prune | PASS, automated in `scripts/conformance/selftest.sh` as `fr16-recheck-result-ignored`, and run composed rather than against a double: `TestPruneRefusesAnObjectThatIsNoLongerTheOneTheJournalRecorded` moves an artifact onto a real bucket, replaces the object behind its key, and the apply refuses with `... is 93 bytes, but 122 was recorded for this placement` while the local artifact in the same pass is still deleted. The size is the whole of the proof against this endpoint, because rclone's s3 backend attests no full-object checksum; the same-length swap, which size alone cannot see, is covered against an attesting double in `core/internal/placement/reclaim_test.go`. The local half is live too: prune mutated to delete what it could not stat is caught by cell `05-prune-verdicts`. |
| V7 | Compatibility (FR-35) | A migration variant that rewrites `retention_tier` during backfill | PASS, automated in `scripts/compat/selftest.sh` |
| V8 | Verification honesty (FR-31) | A revalidation run forced to `existence` class | BLOCKED (#237) |

## Unexpected blockers found while building this

| # | What | Status |
| --- | --- | --- |
| U1 | Upgrading a populated journal failed outright. `state.Open` set `PRAGMA foreign_keys = ON` before running migrations; migrations `0002` and `0006` recreate the `artifacts` table with `DROP TABLE`, and both carry a comment asserting that foreign keys are not enabled on that connection. `state_transitions.artifact_id` references `artifacts(id)`, so the drop tripped FK enforcement the moment any transition row existed, which for a real deployment is always. Proven against a journal written by an actual pre-`0006` build and then opened by the current one: `state: apply migration 6 (remote_retained): constraint failed: FOREIGN KEY constraint failed (787)`. Every migration test started from an empty database, which is why nothing caught it. | **FIXED** in #381 and closed as #396. `migrate` now suspends foreign key enforcement for the run and re-checks with `PRAGMA foreign_key_check` inside each migration's own transaction, which is stronger than the state before the bug. Cell `10-upgraded-artifact-rows` now copies the transition history too, so the full upgrade this gate wanted to run is running. The two migration comments are still literally false and are staying that way on purpose: the files are checksummed and editing one stops every existing deployment from opening, which `TestShippedMigrationsAreImmutable` now enforces. |

## What this matrix does not claim

It does not claim the three-tier chain works end to end, and it is now
precise about which half is which. All three rungs take delivery, each
from a local copy, in one run against a real endpoint, with the bytes read
back off each bucket. What does not happen is an artifact walking from one
medium rung to the next, because that is a medium-to-medium move and the
engine refuses it (#429). The refusal keeps the artifact and keeps the
invariant, and it is asserted rather than hoped for, so the day #429 is
fixed the check goes red and the row gets re-read.

It does not claim the annual rung is a cold class, which is what the gate
line literally asks for. It cannot be one here and it should not be one
here, and those are two different statements. It cannot, because this MinIO
refuses every archive class outright. It should not, because a retention
tier on an archive class can never take delivery of an artifact at all
(#428) and the product now refuses that pairing before it costs anything
(#437), so a scenario built on it would demonstrate a rung that no
deployment anywhere can use while failing to demonstrate the one it can.
The archive pairing is a cell of its own instead, asserting the refusal,
and #442 is where the same refusal wants to move next.

It does not claim anything about an archive storage class beyond two
refusals and one fixture fact. The archive rows rest on the product's own
definitions (the class table, the ceiling, the invariant), on the plan-time
refusal, and on the recorded fact that this fixture will not hold an
archived object, and not on any emulation of Glacier semantics. A double
that pretended to be Glacier would have made P2.6 green and certified
nothing.

It does not claim the daemon drives any of this. The wiring from a retention
pass to `placement.Engine.RunCycle` is #239's, and `core/tests/conformance`
composes the same functions in the same order one layer below the scheduler
while that lands. Every decision inside the loop is the product's; the loop
is the suite's, and it says so.

It does not claim prune's medium half is finished, only that what P2.4 and
V6 say is now checked. The verdict names the medium and the CLI dry-run
renders it; the HTTP surface does not carry the per-deletion medium yet,
which is #430 and is a different claim from this row's.
