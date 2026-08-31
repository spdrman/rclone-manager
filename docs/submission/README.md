# The submission bundle

Everything a store or catalog reviewer is handed, for every target this project
distributes. Work Package 5.4 (`docs/EPIC-B-multi-nas.md` §73), issue #90.

One bundle, not one per store. The seven shared materials below describe the same product
however it is packaged, and a per-store rewrite of any of them is a per-store opportunity
for one of them to be wrong. What varies per target is a single file: its checklist, or,
for a target with no store, its documented workflow.

## Shared materials

| File | What it is |
|---|---|
| `description.md` | The listing copy every target's short form is shortened from. |
| `icon.svg` | The listing mark. Not the in-app icon: see below. |
| `screenshots.md` | What to capture and how many, per store. The captures themselves are an operator step. |
| `release-notes.md` | 1.0.0. |
| `privacy-disclosure.md` | No personal data, no telemetry, nothing leaves the NAS. |
| `permission-rationale.md` | Why the application asks for as little as it does, in the form a store asks the question. |
| `support-source-license.md` | Support, source, and the licence row that is blocked on #88. |

## Per-target

| Target | File | Store or catalog |
|---|---|---|
| Synology DSM | `synology.md` | Synology Package Center |
| TrueNAS | `truenas.md` | TrueNAS Apps catalog |
| Unraid | `unraid.md` | Unraid Community Applications |
| UGOS Pro | `ugreen.md` | UGREEN App Center (EPIC D's to submit, #178) |
| Generic Docker | `generic.md` | none: documented workflow |
| OpenMediaVault | `openmediavault.md` | none: documented workflow |
| Proxmox VE | `proxmox.md` | none: documented workflow |

Each of those files carries a machine-readable table that `apps/common/packaging` parses on
every commit. A missing row fails; a `ready` row naming a path that is not in the tree
fails; a `blocked` row that does not name the issue that owns it fails. The prose around
the table is what a reviewer reads; the table is what stops the prose from rotting.

## Two icons on purpose

`ui/shared/public/icon.svg` is the in-app mark. It paints with `currentColor`, which is
right inside a themed page and wrong on a store listing: outside any colour context
`currentColor` resolves to the initial colour, so the same file that looks correct in the
shell renders as a flat black shape on a store's own page, or as very nearly nothing on a
dark one. It is the one icon defect that is invisible everywhere a developer looks and
visible in the only place that matters, so this bundle ships `icon.svg` with explicit
colours at the size the listings render, and the preflight fails a listing icon that
depends on `currentColor`.

## The recorded verdict

`docs/conformance/submission-preflight.md` holds the generated, per-target readiness
verdict, and EPIC D's #178 consumes it rather than re-running any of these checks. Nothing
in this directory is the answer to "is this ready"; that report is.
