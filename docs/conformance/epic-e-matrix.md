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

`BLOCKED` is a declaration, and like the phase 4 matrix's declarations it is
checked rather than trusted: `TestTheMatrixDoesNotCiteSuitesThatDoNotExist` in
`core/tests/compat` reads this file and fails if a `PASS` row cites a path the
repository does not have. A row cannot be quietly upgraded by editing a word.

## Where the checks live

| Thing | Path |
| --- | --- |
| The FR-35 medium-free surface corpus and its gate | `core/tests/compat` |
| The captured baseline | `core/tests/compat/testdata/medium-free-surfaces.json` |
| The planted violations, automated | `scripts/compat/selftest.sh` |
| The gate step that runs them | `scripts/ci-local.sh` |

Regenerating the corpus is `COMPAT_UPDATE=1 go test ./tests/compat/` from `core/`,
and every regeneration is a claimed behavior change that belongs in a commit
message. The one assertion a regeneration cannot silence is
`TestUpgradingAndInstallingFreshAgreeWithEachOther`, which compares two captures
from the same run rather than a capture against a file.

## Phase 1 exit gate

| # | Outcome | Certifies | Where | Falsification |
| --- | --- | --- | --- | --- |
| P1.1 | PASS | A config declaring an `s3` medium and a tier `medium` reference validates, round-trips through a settings save without injecting fields into a legacy config, and a config naming neither behaves identically to today. | `core/internal/config/storage_mediums_test.go`, `core/service/settings_mediums_test.go`, cell `01-config-validation` in `core/tests/compat` | A tier with no `medium` key resolving to no medium instead of `local`. Run, caught. |
| P1.2 | PASS | A config with an inline literal credential fails `Load` with an unknown-field error. | `core/tests/compat/testdata/configs/53-invalid-inline-medium-secret.yaml`, cell `01-config-validation` | The refusal text is pinned by the corpus; weakening `KnownFields(true)` or the message moves the cell. Run, caught (via the same cell's control). |
| P1.3 | BLOCKED (#235) | The rclone backend set is exactly `local`, `sftp`, `s3` required and `crypt` accepted, and the binary-size delta is recorded in the landing PR. | `core/internal/transport/rclone/backends.go` and its set test, once #235 lands | Registering a fourth backend without updating the set test. |
| P1.4 | BLOCKED (#235) | The MediumStore contract suite passes against the local backend in-tree and against a MinIO fixture, including the explicit capability refusal where attestation is unsupported. | `core/internal/transport/contract`, once #235 lands | An `attested` request silently degrading to `existence` instead of returning a capability result. |
| P1.5 | BLOCKED (#235) | The credential canary passes for file, env and command sources, and its planted violation fails it. | #235's canary test | A build that logs the resolved medium config verbatim. |
| P1.6 | PARTIAL (#236) | Migration 0004 backfills a local placement for every pre-existing artifact row, and the golden retention tests and the full existing suite pass unmodified against the migrated schema. | Cells `10-upgraded-artifact-rows` and `11-upgraded-retention-verdicts`, plus `TestUpgradingAndInstallingFreshAgreeWithEachOther`, in `core/tests/compat` | The mechanism is live and caught: a planted `0007` migration that rewrites `retention_tier` during backfill turns cell 10 red, and one that rewrites `discovered_at` turns cell 11 red. What is BLOCKED is the placement half: there is no placements table to check the backfill of until #236 lands, and one further half is blocked outright by #396, see U1 below. |
| P1.7 | BLOCKED (#237) | Revalidation reports `existence` class for a medium placement and never a stronger class it did not achieve. | #237's class-string assertion | A revalidation pass forced to `existence` reporting itself as content verification. |
| P1.8 | BLOCKED (#235, #236, #237) | Nothing in phase 1 can delete an artifact copy anywhere; the destructive-safety suite diff shows no new deletion path. | The destructive-safety suite, once the phase 1 code exists to diff | A `DeleteObject` call reachable from a phase 1 code path. |

## Phase 2 exit gate

| # | Outcome | Certifies | Where | Falsification |
| --- | --- | --- | --- | --- |
| P2.1 | BLOCKED (#238, #239, #241) | A three-tier chain (daily local, monthly `s3`, annual `s3` cold) runs end to end against MinIO, with the standing invariant (at least one ACTIVE verified placement per managed-complete artifact) asserted continuously by the harness rather than sampled. | The composed scenario, once the move engine, the retention integration and the archive classes exist | An engine that lets both placements be non-verified at the same instant. The harness has to observe it at that instant, which is why "continuously" is in the gate line and why sampling would not do. |
| P2.2 | BLOCKED (#238) | The crash matrix passes: a forced crash at every move phase boundary, restart reconciliation, and the move either completed or abandoned with the source intact. | `core/tests/crashmatrix`, extended by #238 | The spec's own: a mutation that issues the source delete before `VERIFIED` is durably recorded. |
| P2.3 | PARTIAL (#238) | Moving an artifact does not change its retention bucketing: verdicts before and after are bit-identical. | Cells `04-retention-verdicts` and `11-upgraded-retention-verdicts` in `core/tests/compat` | The invariance mechanism is live and caught: rewriting the journal's discovery timestamp turns cell 11 red. BLOCKED is the "across a move" half, because there is no move to run yet. When #238 lands, the same cell is what it has to be re-decided against. |
| P2.4 | BLOCKED (#239) | Prune against a medium refuses on identity mismatch, and the mandatory dry-run names the medium for every proposed deletion. | #239's prune extension | The spec's own: a fixture that swaps the object behind a key before prune. The local half of the same rule is live and caught: cell `05-prune-verdicts` goes red when prune is mutated to delete a file it could not stat instead of refusing. |
| P2.5 | BLOCKED (#240) | A tier-to-medium settings save without the disclosure acknowledgment is refused by the API, with allow and deny tests. | #240's settings handler | A save that carries no acknowledgment and is accepted. |
| P2.6 | PARTIAL (#241) | An artifact on an archive class shows `requires_restore`, a restore is a durable operation surviving restart, and no surface anywhere renders a cost figure or an invented ETA. | `TestTheContractServesNoCostFigureAndNoInventedETA` in `core/tests/compat` | The no-cost half is live and caught, in both directions: a cost field and an invented ETA plus a percentage each turn the check red, and the rule is tested against strings it must match and strings it must not. BLOCKED is the archive half, which needs #241's states and restore operation. |
| P2.7 | PASS | FR-35 holds: a deployment upgraded with a medium-free config shows zero behavioral difference through config validation, retention verdicts, API responses (minus additive fields) and CLI output. | `core/tests/compat`, eleven cells | The spec's own: a migration variant that rewrites `retention_tier` during backfill. Run, caught by cell `10-upgraded-artifact-rows`, and caught again by `TestUpgradingAndInstallingFreshAgreeWithEachOther` when the corpus is regenerated around it. |
| P2.8 | PARTIAL | `check-contract-drift.sh` and `check-client-paths.sh` pass with the new operations; the layer manifest classifies every new file; `verify-core-without-distribution.sh` still passes. | `scripts/api/check-contract-drift.sh`, `scripts/api/check-client-paths.sh`, `scripts/architecture/check-layer-manifest.sh`, `scripts/architecture/verify-core-without-distribution.sh` | All four already run in `scripts/ci-local.sh` and all four already have mutation self-tests. What is not yet true is "with the new operations", because there are none: this row goes to PASS when the API lane lands and these keep passing. |

## Section 4 planted violations

The spec names one violation per guard, and says the landing PR for each gate
records its planted violation actually failing. This is that ledger.

| # | Guard | Planted violation | Outcome |
| --- | --- | --- | --- |
| V1 | Source survives every move uncertainty | A mutation that issues the source delete before `VERIFIED` is durably recorded | BLOCKED (#238) |
| V2 | Medium data only ever adds (FR-32) | A mutation that admits S3 `LastModified` as a producer timestamp | BLOCKED (#237, #239). The composed analogue is live: a backfill that re-derives `discovered_at` is caught by cell `11-upgraded-retention-verdicts`. |
| V3 | Bucketing invariant under movement | A mutation that rewrites the journal's discovery timestamp | PASS, automated in `scripts/compat/selftest.sh` |
| V4 | Credential canary (FR-33) | A build that logs the resolved medium config verbatim | BLOCKED (#235) |
| V5 | Inline secret refusal (FR-33) | A config with a literal `secret_access_key:` | PASS, pinned by cell `01-config-validation` |
| V6 | Prune identity re-check on mediums (FR-30) | A fixture that swaps the object behind a key before prune | BLOCKED (#239). The local half is live: prune mutated to delete what it could not stat is caught by cell `05-prune-verdicts`. |
| V7 | Compatibility (FR-35) | A migration variant that rewrites `retention_tier` during backfill | PASS, automated in `scripts/compat/selftest.sh` |
| V8 | Verification honesty (FR-31) | A revalidation run forced to `existence` class | BLOCKED (#237) |

## Unexpected blockers found while building this

| # | What | Status |
| --- | --- | --- |
| U1 | Upgrading a populated journal fails outright. `state.Open` sets `PRAGMA foreign_keys = ON` before running migrations; migrations `0002` and `0006` recreate the `artifacts` table with `DROP TABLE`, and both carry a comment asserting that foreign keys are not enabled on that connection. `state_transitions.artifact_id` references `artifacts(id)`, so the drop trips FK enforcement the moment any transition row exists, which for a real deployment is always. Proven against a journal written by an actual pre-`0006` build of this product and then opened by the current one: `state: apply migration 6 (remote_retained): constraint failed: FOREIGN KEY constraint failed (787)`. Every migration test starts from an empty database, which is why nothing caught it. | Filed as #396. It is why cell `10-upgraded-artifact-rows` copies artifact rows without their transition history, and why the fuller upgrade this gate wants to run is not running. |

## What this matrix does not claim

It does not claim the composed end-to-end scenario exists. Job one of #242, the
three-tier chain against MinIO with the crash matrix run over it, is written here
as a specification and is BLOCKED on every issue it composes. Nothing in
`core/tests/compat` stands in for it, and nothing here should be read as though
it did: what is checked today is the compatibility half, the surfaces a
medium-free deployment shows, and the two planted violations that half can
already be shown to catch.
