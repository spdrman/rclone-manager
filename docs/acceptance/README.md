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

`apps/common/packaging` is the automated half. It runs on every commit and checks
the parts of the Phase 4 TDD Gate that are decidable from the repository alone:

| Phase 4 gate item | Where it is checked |
| --- | --- |
| core version/hash parity | `apps/common/packaging` (canonical image reference is identical across all three platforms, and the architecture set matches `container/release-manifest.json`) |
| provider package metadata | `apps/common/packaging` (every metadata file parses, and carries the keys its platform requires) |
| architecture | `apps/common/packaging` |
| backup-root containment | `apps/common/packaging` (§19.2: private state, config and key material are never inside the backup root) |
| auth mode | `apps/common/packaging` (every platform declares `local-account`, none ships its own auth) |
| no bundled secrets | `apps/common/packaging` |
| no provider-specific lifecycle implementation | `apps/common/packaging` |
| state persistence | **here**, on hardware |
| install/update/remove semantics | **here**, on hardware |

The last two rows are the reason this directory exists.

## Procedures

| Provider | Procedure | Required evidence (§68) |
| --- | --- | --- |
| TrueNAS | [truenas-provider-acceptance.md](truenas-provider-acceptance.md) | current supported TrueNAS release VM or hardware |
| Unraid | [unraid-provider-acceptance.md](unraid-provider-acceptance.md) | current supported Unraid release VM or hardware |
| OpenMediaVault | [openmediavault-provider-acceptance.md](openmediavault-provider-acceptance.md) | current OMV 8.x Debian-based test system |

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
