# Provider acceptance procedures

Section 68 of `docs/EPIC-B-multi-nas.md` (the Provider Test Matrix) says provider
acceptance procedures are written and version-controlled **before** anyone executes
them manually. This directory is where they live.

Every procedure in here covers behaviour that no test on a developer laptop can
reach: installing a package through a real NAS platform's own app store, letting
that platform's own updater replace the container, and proving retained backup
data survived. Nothing in this directory runs in CI, and nothing in it is
"verified" until an operator has executed it against the hardware or VM named in
its own header and filled in the evidence table at the bottom.

## The rule this directory exists to enforce

Section 68:

> A provider/architecture SHALL be described as **build-supported but uncertified**
> until its required acceptance test is completed.

So: a green `ci-local: ok` proves the metadata is *well-formed*. It never proves a
platform *accepted* it. Until the evidence table in a procedure is filled in and
committed, that provider is build-supported and uncertified, and any PR, release
note, or README that says otherwise is wrong.

## What CI can and cannot prove

`distribution/packaging` is the automated half. It runs on every commit and checks
the parts of the Phase 4 TDD Gate that are decidable from the repository alone:

| Phase 4 gate item | Where it is checked |
| --- | --- |
| core version parity | `distribution/packaging` (one canonical image reference, identical across every platform and across the TrueNAS catalog as an install renders it) |
| core binary hash parity | **not claimed**. Nothing in `distribution/packaging` derives a hash from any artifact. It checks only that `container/release-manifest.json` records a non-empty SHA-256 per binary per architecture, which cannot detect a stale or wrong hash. #174 closed the worse half of that gap: the manifest no longer pins a commit that has left `main`'s history, and `release-manifest-integrity` passes for every provider. A reachable manifest is still not a byte comparison, so this row stays unclaimed. The only place binary hashes are verified against real bytes is `spkctl verify` against a built `.spk` in `apps/synology`. |
| provider package metadata | `distribution/packaging` (every metadata file parses, and carries the keys its platform requires) |
| architecture | `distribution/packaging` (the claimed set equals what `container/release-manifest.json` records as built) |
| backup-root containment | `distribution/packaging` (§19.2: private state, config and key material are never inside the backup root, and the declared storage mount IS the backup root, so the rule has one reading rather than three) |
| auth mode | `distribution/packaging` (every platform declares `local-account`, none ships its own auth) |
| no bundled secrets | `distribution/packaging` |
| no provider-specific lifecycle implementation | `distribution/packaging` |
| state persistence | **here**, on hardware |
| install/update/remove semantics | **here**, on hardware |

The last two rows are the reason this directory exists. The second row is the
reason the first one is worded so narrowly: a gate item marked as covered by a
check that cannot fail for the reason the item names is worse than an openly
unclaimed gap, because it stops anyone looking.

## What these procedures prove about your data

Every procedure ends by asking whether the backup root survived removal
untouched. That question is only answerable against a baseline, so each one
writes an 8 MiB canary of known content into the backup root during the storage
step and records its SHA-256 and a full file listing **outside** the backup root,
then verifies both immediately after removal, before anything else is inspected.
A procedure that claims the backup root is untouched byte for byte without
recording that baseline is a red test in `distribution/packaging`, as is any
`chown -R` that reaches the backup root or a parent of it: step 0 is what an
operator re-runs on a reinstall, by which point that tree is the retained backup
store.

## Which provider actually gets which of those

The table above answers one question: where a gate item is decided, here on a
laptop or over there on hardware. It deliberately does not answer the other one,
which provider each item holds for, because a hand-written table that tried to
would drift from the suite inside a release. One did. It folded version parity
and binary-hash parity into a single row and told the reader both were checked on
every commit, while a matrix generated in the same repository recorded the
release manifest as `BLOCKED` for all seven providers and passed hash parity for
none of them.

So the second question has exactly one answer and it is generated. The same
package runs the cross-provider conformance matrix (§63A) over all seven
providers on every commit and records the result in
[`../conformance/phase-4-matrix.md`](../conformance/phase-4-matrix.md), one
outcome per provider per capability, generated and then checked against a real
run. A test holds the table above to that matrix in both directions, so a row
here can neither claim a capability the matrix passes for nobody nor disclaim one
it passes. Read the matrix for coverage; read the table for where coverage is
decided.

What the outcomes there mean:

- `PASS` / `FAIL`: decided from the repository alone, on any laptop.
- `OPERATOR`: the capability is supported and the procedure below covers it, but
  deciding it needs the real platform. The automated half held; the hardware half
  has not run. That is the same claim this directory makes, said once, in a form
  a test enforces.
- `UNSUPPORTED` / `N/A`: the provider does not have this at its §4A tier, or
  expresses the same guarantee somewhere the matrix names.
- `BLOCKED`: the check is implemented and correct and cannot conclude today, for
  a reason tracked in a numbered issue.

State persistence and install/update/remove semantics are the two rows that can
never be anything but `OPERATOR`, per provider, until someone runs a procedure
from this directory and records what happened.

## Procedures

| Provider | Procedure | Required evidence (§68) |
| --- | --- | --- |
| TrueNAS | [truenas-provider-acceptance.md](truenas-provider-acceptance.md) | current supported TrueNAS release VM or hardware |
| Unraid | [unraid-provider-acceptance.md](unraid-provider-acceptance.md) | current supported Unraid release VM or hardware |
| OpenMediaVault | [openmediavault-provider-acceptance.md](openmediavault-provider-acceptance.md) | current OMV 8.x Debian-based test system |
| Synology DSM | [synology-dsm-package-lifecycle.md](synology-dsm-package-lifecycle.md) | a representative DSM 7.x model per claimed architecture |
| Proxmox VE | [proxmox-ve-deployment.md](proxmox-ve-deployment.md) | current PVE release test host or VM environment |
| UGOS | [ugos-local-notification.md](ugos-local-notification.md) | a real authorized UGREEN NAS. Covers notifications only; the install/update/uninstall procedure belongs with the UPK, which is #83 |

Generic Docker has no procedure here on purpose: `apps/generic/tests/dockercli`
drives the real `docker` CLI against the real image (§67), so there is no
hardware step left to write down.

## Recording evidence

Every procedure ends with the same evidence table, whose rows are section 68's own
required fields:

- provider / OS version
- hardware or model where relevant
- architecture
- package / image version
- install result
- auth result
- storage result
- update result
- uninstall / removal result
- retained-backup safety
- evidence (log paths, screenshots, command transcripts)

Fill it in **in the same commit** that flips a provider from uncertified to
certified, so the claim and its evidence never live apart.

## Credentials

No procedure in this directory ever asks anyone to commit a credential. Where a
step produces one (the one-time enrollment token, the administrator password, an
SSH private key), the procedure says so and says to keep it off the repository.
Paste command transcripts with those values redacted.
