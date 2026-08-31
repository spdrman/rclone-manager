# OpenMediaVault distribution workflow

This is a Compose deployment profile run through OMV's Compose plugin, not an
omv-extras package, and §4A defers a native Workbench plugin. There is nothing to submit
anywhere, so the deliverable is the workflow below.

The platform's own documentation, cited so this file can be re-derived rather than trusted:
https://www.openmediavault.org/

The packaging metadata this workflow deploys is under `apps/openmediavault/`.

## Workflow

| Item | Where it is | State |
|---|---|---|
| install | `apps/openmediavault/README.md` | ready |
| update | `apps/openmediavault/README.md` | ready |
| remove | `docs/acceptance/openmediavault-provider-acceptance.md` | ready |
| recovery | `docs/recovery-without-a-terminal.md` | ready |
| support | `docs/submission/support-source-license.md` | ready |

`apps/common/packaging` reads this table on every commit and fails when a row is missing or
names a path that is not in the tree. The five rows are the five things an administrator
needs and the five things a listing would have had to state if there were a listing.

## Hard rules, verified against the built artifact

The absence of a store changes nothing about what is deployed. The same four rules are
checked against this target's packaged files as against a submitted one: no self-update
mechanism, no floating image tag, no privileged mode, and no mandatory telemetry. So is the
eight-element adapter drift gate. The current result is in
`docs/conformance/submission-preflight.md`.

## Before calling this target ready

- [ ] `container/release-manifest.json` pins a commit that is on the main branch.
- [ ] The acceptance run in `docs/acceptance/store-submission-preflight.md` has been done on
      real hardware and accepted.

### How to read the State column

Four values, and only four. `apps/common/packaging` parses this table on every commit and
fails the build on a fifth.

- `ready` — the material is in the tree at the path in the middle column, and that path is
  checked to exist.
- `operator` — the material is produced on real hardware by the procedure in the middle
  column. It is not missing; it is outstanding, and the readiness verdict records it as
  such rather than as a pass.
- `blocked #N` — another work package owns it. The issue is named so the row cannot mean
  "blocked forever".
- `not-applicable` — this target does not need it, which only a target with no store can
  say.

### Recovery and support

An administrator who cannot open a terminal is the normal case on this platform, so the
support material a reviewer follows has to reach steps that need one. It does:
`docs/recovery-without-a-terminal.md` covers the three failures that account for
almost every support request, and `docs/recovery.md` covers the same ground for somebody
who does have a shell.
