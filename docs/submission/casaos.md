# CasaOS AppStore submission checklist

The submission bundle for CasaOS AppStore, in the form that store asks for it. The shared materials
are one set for every target, because a per-store rewrite of a privacy disclosure is a
per-store opportunity for one of them to be wrong; what is per-store is this file and the
store compose file under `apps/casaos/`.

That store's own published requirements: https://github.com/IceWhaleTech/CasaOS-AppStore

CasaOS installs a third-party application by reading one docker-compose file carrying an
`x-casaos` block, so `apps/casaos/compose/backup-manager.yml` is both the runtime
definition and the whole submission. Everything outside `x-casaos` is derived from
`container/compose.yaml` and held to it field by field; everything inside it is store
presentation and reaches no Go package and no shared UI module.

Nothing on a developer machine can run that store's own build validation, which is why it
is a step of `docs/acceptance/casaos-app-store-install.md` rather than a check here.

## Materials

| Item | Where it is | State |
|---|---|---|
| materials-description | `docs/submission/description.md` | ready |
| materials-icon | `docs/submission/icon.svg` | ready |
| materials-screenshots | `docs/acceptance/store-submission-preflight.md` | operator |
| materials-release-notes | `docs/submission/release-notes.md` | ready |
| materials-privacy-disclosure | `docs/submission/privacy-disclosure.md` | ready |
| materials-permission-rationale | `docs/submission/permission-rationale.md` | ready |
| materials-support-source-license | `docs/submission/support-source-license.md` | ready |

This table is not decoration. `distribution/packaging` reads it on every commit, fails when
a row is missing, fails when a `ready` row names a path that is not in the tree, and fails
when a `blocked` row does not name the issue that owns it.

## Screenshots

CasaOS AppStore asks for 2. See `docs/submission/screenshots.md` for what to capture and
`docs/acceptance/store-submission-preflight.md` for the procedure that captures them on real
hardware.

## The store icon

`apps/casaos/icon.svg` is the in-app mark the `x-casaos` block points at, and
`docs/submission/icon.svg` is the listing icon the shared bundle carries. They are two
assets for two surfaces, and the preflight checks the listing one.

## Hard rules, verified against the built artifact

Four things this submission claims and the preflight checks against the packaged files
rather than against this sentence: no self-update mechanism, no floating image tag, no
privileged mode, and no mandatory telemetry. The current result for this target is in
`docs/conformance/submission-preflight.md`.

## Before submitting

- [ ] `container/release-manifest.json` pins a commit that is on the main branch, so the
      bytes in this submission can be traced to a recorded build. The preflight decides
      this; the box is here because a submission is checked by a person, not only by a gate.
- [ ] The acceptance run in `docs/acceptance/store-submission-preflight.md` has been done on
      real hardware and accepted.
- [ ] The acceptance run in `docs/acceptance/casaos-app-store-install.md` has been done on
      real hardware and accepted.
- [ ] The screenshots exist and are of that run.

### How to read the State column

Four values, and only four. `distribution/packaging` parses this table on every commit and
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
