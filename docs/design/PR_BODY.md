## Summary

Adds the Backup Manager frontend as one provider-neutral core plus seven thin
provider shells. No page, component, or token is duplicated per provider.

- `ui/shared/` — design system, 16 shared components, 10 pages, platform
  abstraction, typed API client, and a mock API covering every documented state.
- `apps/<provider>/frontend/` — bootstrap, bridge, capability flags, and (for
  three providers) a restrained accent/titlebar override. Nothing else.

## Architecture

```
apps/<provider>/frontend/bootstrap.tsx
  -> createApp(container, bridge)        # ui/shared/src/app/createApp.tsx
     -> PlatformProvider + ApiProvider + <App/>
```

The dependency arrow only ever points from `apps/` into `ui/shared/`.
`vite.config.ts` resolves `@platform-entry` from `VITE_PLATFORM`, so a build
contains exactly one provider.

### Invariants, and where they are enforced

| Invariant | Enforcement |
| --- | --- |
| Deleting any `apps/<provider>/` leaves everything else compiling | no shared file imports a provider; only `src/test/bridges.ts` imports all seven |
| `ui/shared/` knows nothing about any provider | no provider identifier appears outside `apps/` |
| No unsupported capability is presented as supported | `capabilities()` is opt-in from `NO_CAPABILITIES`; `providerConformance.test.tsx` asserts `notify()` exists only when claimed |
| Remote deletion follows commit | `buildPhases()` derives the timeline from `remoteSourceRemovedAt`; asserted in `safety.test.tsx` |
| Retention applies the reviewed plan or nothing | `applyRetention(planId)`; a stale plan is a server 409, never a recalculation |
| Status is never colour-only | every status renders glyph + label + sentence |

## Product decisions worth reviewing

- **Service health and backup health are separate facts.** The dashboard headline
  states the *backup* verdict; the running daemon is a supporting chip. A healthy
  service with 31-hour-old backups reads `BACKUPS DEGRADED`.
- **The page is "Backups", not "Restore points".** The product retains and
  verifies; it does not execute application restore, and the footer says so.
- **Wizard save is gated on the remote-deletion acknowledgement.** All three save
  actions stay disabled until the operator checks the box.
- **Destructive buttons name their consequence.** "Delete 4 backups", never "OK".
  Focus lands on Cancel.
- **Quarantine has no "delete remote anyway".** Deliberate omission.
- **Version mismatch disables management, keeps information.** Read-only mode is
  threaded through pages as a `readOnly` prop.

## Screens

Dashboard · Backup sets · Backup set detail · Add backup set (6 steps) ·
Backups · Backup detail with lifecycle timeline · Activity · Quarantine ·
Settings (service, notifications, platform, system info) · Catalog recovery ·
Login · First-run enrollment.

## Provider matrix

| Provider | Integration | Auth | Native notifications | Storage picker |
| --- | --- | --- | --- | --- |
| Generic Docker/Linux | Standalone | Local account | webhook | manual path |
| UGREEN UGOS Pro | Native | UGOS session | yes | yes |
| Synology DSM | Embedded web | Local account | webhook | manual path |
| TrueNAS | Container | Local account | webhook | manual path |
| Unraid | Container | Local account | webhook | manual path |
| OpenMediaVault | Container | Local account | webhook | manual path |
| Proxmox VE | Standalone | Local account | webhook | manual path |

DSM native auth is intentionally *not* claimed — no DSM authentication API is
fabricated. OMV is a Compose integration; a future native Workbench shell
replaces `apps/openmediavault/frontend/` only.

## Tests

```
src/test/providerConformance.test.tsx   provider matrix, capability honesty
src/test/safety.test.tsx               health separation, lifecycle ordering,
                                       stale plan refusal, destructive confirm,
                                       no private key in any contract
src/test/wizard.test.tsx               6 steps, acknowledgement gate, stable-size
                                       warning, capability-driven picker
```

### End-to-end (Playwright)

Thirteen specs against the mock API — see `ui/shared/e2e/README.md` for the
per-spec table.

```
shell / dashboard / backup-sets / wizard / backups / retention /
quarantine-activity / settings-recovery / auth        component behaviour
provider-treatment                                    per-provider capability honesty
responsive                                            small app window, no clipped columns
accessibility                                         labels, focus, semantics, 0 console errors
safety-invariants                                     the product's non-negotiables, every page
```

```bash
npm run test:all           # lint + unit + e2e
npm run e2e:all-providers  # provider matrix across all seven shells
```

## Design source

`docs/design/Backup Manager.dc.html` is the interactive design this was built
from — all screens, both themes, all seven provider treatments, and the risk
states. `docs/design/Logo Options.dc.html` holds the logo exploration; option
**1a (Cycle)** was selected and ships as `ui/shared/public/icon.svg`, tinted by
`--accent` so no provider needs its own asset.

## Not included

Restore execution, cloud telemetry, mobile layouts, and a Storybook instance
(the mock scenarios cover component states in-app instead).
