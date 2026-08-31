# Phase 4 cross-provider conformance matrix

Generated. Do not edit the region between the markers by hand: it is the output
of a real run of `apps/common/packaging`'s conformance suite, and the suite fails
if what is checked in differs from what a fresh run produces. To regenerate:

```bash
cd apps/common && CONFORMANCE_UPDATE=1 GOWORK=off go test ./packaging/ -run TestCrossProviderConformanceMatrix
```

This is the record section 68's INTEGRATION step asks for, and the answer to
issue #86's fourth acceptance criterion, "unsupported capabilities are explicitly
reported". Section 63A is the reason it exists in this shape:

> The conformance suite SHALL distinguish SUPPORTED / UNSUPPORTED /
> NOT_APPLICABLE rather than silently skipping missing provider features.

## How to read an outcome

| Outcome | Means |
| --- | --- |
| `PASS` | The check ran here, on this repository, and held. |
| `FAIL` | The check ran here and did not hold. Any `FAIL` is a red build. |
| `UNSUP` | The provider genuinely does not have this capability at its section 4A support tier. Declared, with a reason, never inferred from absence. |
| `N/A` | The capability does not apply to this provider's shape, usually because the platform expresses the same guarantee somewhere else. The `verifiedBy` field says where. |
| `BLOCKED` | The check is implemented and correct and cannot conclude today, for a reason tracked in an issue. Not a pass and not a fail. |
| `OPERATOR` | Supported, and decidable only on the real platform (section 68). The automated half held: the prewritten acceptance procedure exists and covers this capability. The hardware run has not happened. |

`UNSUP`, `N/A` and `BLOCKED` are declarations in
`apps/common/packaging/conformance.json`, and the suite checks them rather than
trusting them: every one of them still has its check run, and a declaration the
repository has outgrown fails the build. A provider cannot quietly drop a
capability by omitting it either, because omission is itself a failure.

## What is blocking today

- **#174** — `container/release-manifest.json` pins commit `c51a07f`, which is not
  an ancestor of `main` after the squash-merge rewrite. Every core binary hash
  parity cell is `BLOCKED` on it, for all seven providers: the check compares a
  provider's binaries against that manifest, and a manifest describing a build
  that is not in the history has nothing on the other side of the comparison.
  The check is written correctly and starts passing on its own the moment the
  manifest points at a reachable build.
- **#83** — work package 4.2's UGOS UPK moved out of this EPIC into EPIC D and is
  still open. `apps/ugos/` holds the frontend bridge and nothing else: no
  `project.yaml`, no Compose, no icon, no architecture image tar. So UGOS is the
  one Phase 4 Exit Gate provider with no package in this repository, and its
  packaging cells are `BLOCKED` rather than passing. **The Phase 4 Exit Gate is
  not met while that is true**, whatever the rest of this table says.

## Nothing here is a certification

Every `OPERATOR` row is a provider that is **build-supported and uncertified**
in section 68's own words. A green matrix proves the packaging metadata is
well-formed and mutually consistent. It proves nothing about how any of these
platforms behaves. `docs/acceptance/` is where that gets decided.

<!-- BEGIN GENERATED MATRIX -->

### Support tiers (§4A)

| Provider | Tier | Work package | Acceptance procedure |
|---|---|---|---|
| UGOS Pro | A | 4.2 | `docs/acceptance/ugos-local-notification.md` |
| Synology DSM | B | 4.4 | `docs/acceptance/synology-dsm-package-lifecycle.md` |
| TrueNAS | B | 4.3 | `docs/acceptance/truenas-provider-acceptance.md` |
| Unraid | B | 4.3 | `docs/acceptance/unraid-provider-acceptance.md` |
| Generic Docker | C | 4.1 | `none (automated instead)` |
| OpenMediaVault | C | 4.3 | `docs/acceptance/openmediavault-provider-acceptance.md` |
| Proxmox VE | C | 4.5 | `docs/acceptance/proxmox-ve-deployment.md` |

### Per-capability results

| Capability | UGOS Pro | Synology DSM | TrueNAS | Unraid | Generic Docker | OpenMediaVault | Proxmox VE |
|---|---|---|---|---|---|---|---|
| Provider identified correctly | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| Provider package metadata present | BLOCKED | PASS | PASS | PASS | PASS | PASS | PASS |
| Uses the exact canonical image | BLOCKED | N/A | PASS | PASS | N/A | PASS | PASS |
| Core binary hash parity | BLOCKED | BLOCKED | BLOCKED | BLOCKED | BLOCKED | BLOCKED | BLOCKED |
| Architecture claims match the build | BLOCKED | PASS | PASS | PASS | PASS | PASS | PASS |
| State path persists outside the container | BLOCKED | N/A | PASS | PASS | PASS | PASS | PASS |
| Backup root constrained | BLOCKED | N/A | PASS | PASS | PASS | PASS | PASS |
| Auth mode explicit and honest | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| No bundled secrets | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| No provider-specific lifecycle implementation | PASS | N/A | PASS | PASS | N/A | PASS | PASS |
| API reachable only through the intended path | BLOCKED | N/A | PASS | PASS | PASS | PASS | PASS |
| Provider removal does not alter core | PASS | PASS | PASS | PASS | N/A | PASS | PASS |
| Host management plane not modified | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| Install / update / remove semantics | BLOCKED | OPERATOR | OPERATOR | OPERATOR | N/A | OPERATOR | OPERATOR |
| UI launches | BLOCKED | OPERATOR | OPERATOR | OPERATOR | N/A | OPERATOR | OPERATOR |
| Upgrade preserves state | BLOCKED | OPERATOR | OPERATOR | OPERATOR | N/A | OPERATOR | OPERATOR |
| Removal does not delete retained backups | BLOCKED | OPERATOR | OPERATOR | OPERATOR | N/A | OPERATOR | OPERATOR |
| Native authentication | PASS | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP |
| Native notifications | PASS | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP |
| Embedded window | PASS | PASS | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP |
| App-store packaging | BLOCKED | PASS | PASS | PASS | UNSUP | UNSUP | UNSUP |
| Storage picker | PASS | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP |

### Totals

| Outcome | Cells |
|---|---|
| PASS | 78 |
| PENDING_OPERATOR | 20 |
| UNSUPPORTED | 26 |
| NOT_APPLICABLE | 12 |
| BLOCKED | 18 |
| FAIL | 0 |

### Every cell that is not a plain PASS

Section 63A's requirement in full: an unsupported capability is reported, with a
reason, rather than skipped. Every row below is a cell this run did not pass, and
why.

#### UGOS Pro (Tier A)

| Capability | Outcome | Why |
|---|---|---|
| Provider package metadata present | BLOCKED | #83 — Work package 4.2's UPK was moved out of this EPIC into EPIC D and is still open as #83. apps/ugos/ contains the frontend bridge and nothing else: no project.yaml, no compose, no icon, no image tar. Until #83 lands, UGOS is the one Phase 4 Exit Gate provider with no package in this repository. |
| Uses the exact canonical image | BLOCKED | #83 — Nothing in apps/ugos/ references an image yet, so there is no reference to compare. |
| Core binary hash parity | BLOCKED | #174 — Same unreachable release manifest as every other provider, and #83 on top of it. |
| Architecture claims match the build | BLOCKED | #83 — The UPK declares the architecture image tars (section 41); no UPK, no claim to check. |
| State path persists outside the container | BLOCKED | #83 — The UPK's compose declares the storage mapping (section 22); it does not exist yet. |
| Backup root constrained | BLOCKED | #83 — BACKUP_ROOT comes from the UPK's install parameters (section 20); they do not exist yet. |
| API reachable only through the intended path | BLOCKED | #83 — The UPK's compose decides which container publishes a port; it does not exist yet. |
| Install / update / remove semantics | BLOCKED | #83 — docs/acceptance/ugos-local-notification.md covers notifications only. The install/update/disable/uninstall/reinstall procedure is work package 4.2's, and belongs with #83. |
| UI launches | BLOCKED | #83 — Section 12's embedded provider window is delivered by the UPK. |
| Upgrade preserves state | BLOCKED | #83 — Section 46's upgrade behaviour needs the package that gets upgraded. |
| Removal does not delete retained backups | BLOCKED | #83 — Section 48's uninstall behaviour needs the package that gets uninstalled. |
| App-store packaging | BLOCKED | #83 — The bridge claims it, and section 4A promises it, but no UPK exists in this repository yet. Passing on the bridge flag alone would be exactly the kind of claim the store-artifact half of this check exists to refuse. |

#### Synology DSM (Tier B)

| Capability | Outcome | Why |
|---|---|---|
| Uses the exact canonical image | N/A | Synology is the one Phase 4 provider that cannot consume the OCI image: DSM's Package Center installs a native .spk. Section 3.7 makes the SPK a sibling of the image carrying the same core binary digest, so parity here is binary parity, not image parity. |
| Core binary hash parity | BLOCKED | #174 — spkctl verify re-derives each binary's SHA-256 out of a finished package and compares it against container/release-manifest.json. The manifest pins a commit that is not an ancestor of main, so the comparison has nothing real on the other side. |
| State path persists outside the container | N/A | DSM fixes the persistent location: /var/packages/<pkg>/var under the package FHS, not a bind mount this repository declares. |
| Backup root constrained | N/A | The backup root is a DSM shared folder the operator picks at install time, so there is no checked-in host path pair to compare. The separation itself is proven by the package never placing key material or auth state under it. |
| No provider-specific lifecycle implementation | N/A | DSM's package format MANDATES preinst/postinst/preuninst/postuninst/preupgrade/postupgrade and start-stop-status. Those scripts are the platform's contract, not a lifecycle engine of our own, and apps/synology holds them to wrapper-only behaviour. |
| API reachable only through the intended path | N/A | There is no two-container split to check: the SPK runs one process behind DSM's own reverse proxy, and the port comes from conf/resource rather than a compose ports list. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/synology-dsm-package-lifecycle.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/synology-dsm-package-lifecycle.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/synology-dsm-package-lifecycle.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/synology-dsm-package-lifecycle.md, not yet executed |
| Native authentication | UNSUP | Tier B. Section 4A makes DSM SSO a follow-on capability; the initial package uses section 13A local auth. |
| Native notifications | UNSUP | Tier B. No DSM notification adapter in v1; webhooks instead. |
| Storage picker | UNSUP | Tier B. The shared folder is chosen once at install time through DSM, not browsed from inside the app. |

#### TrueNAS (Tier B)

| Capability | Outcome | Why |
|---|---|---|
| Core binary hash parity | BLOCKED | #174 — container/release-manifest.json pins commit c51a07f, which is not an ancestor of main after the squash-merge rewrite, so there is no build to compare a hash against. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/truenas-provider-acceptance.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/truenas-provider-acceptance.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/truenas-provider-acceptance.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/truenas-provider-acceptance.md, not yet executed |
| Native authentication | UNSUP | Tier B. Section 4A gives TrueNAS the generic local auth; no middleware session adapter in v1. |
| Native notifications | UNSUP | Tier B. Webhooks instead of TrueNAS alerts. |
| Embedded window | UNSUP | Tier B. The Apps portal link opens the UI in a normal browser tab. |
| Storage picker | UNSUP | Tier B. questions.yaml asks for the dataset paths at install time; the running app does not browse pools. |

#### Unraid (Tier B)

| Capability | Outcome | Why |
|---|---|---|
| Core binary hash parity | BLOCKED | #174 — container/release-manifest.json pins commit c51a07f, which is not an ancestor of main after the squash-merge rewrite, so there is no build to compare a hash against. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/unraid-provider-acceptance.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/unraid-provider-acceptance.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/unraid-provider-acceptance.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/unraid-provider-acceptance.md, not yet executed |
| Native authentication | UNSUP | Tier B. Section 4A gives Unraid the generic local auth; no plugin is required for v1. |
| Native notifications | UNSUP | Tier B. Webhooks instead of Unraid notifications, which would need a plugin. |
| Embedded window | UNSUP | Tier B. The WebUI link opens a normal browser tab. |
| Storage picker | UNSUP | Tier B. Community Applications collects the paths at install time; the app does not browse shares. |

#### Generic Docker (Tier C)

| Capability | Outcome | Why |
|---|---|---|
| Uses the exact canonical image | N/A | container/compose.yaml BUILDS the canonical image from container/Dockerfile rather than pulling a published reference. It is the source of the image the other six profiles consume, so pinning it to its own output would be circular. |
| Core binary hash parity | BLOCKED | #174 — container/release-manifest.json pins commit c51a07f, which is not an ancestor of main after the squash-merge rewrite, so there is no build to compare a hash against. |
| No provider-specific lifecycle implementation | N/A | Two reasons, both structural. apps/generic IS the canonical web host (section 37), not a wrapper around it, so there is no wrapper for a second implementation to hide in. And container/compose.yaml carries a build: key, which the scanner treats as a violation everywhere else precisely because a Tier B/C package must reuse the canonical image rather than build one; here it is the file the canonical image is built FROM. |
| Provider removal does not alter core | N/A | scripts/architecture/verify-ui-shared-without-provider-sdks.sh says it directly: apps/generic is the vendor-neutral baseline the default Vite build targets, so it is not a provider SDK directory and ui/shared's own tests import its bridge on purpose. The rule applies to the six vendor providers. |
| Install / update / remove semantics | N/A | Automated rather than operator-verified: apps/generic/tests/dockercli drives the real docker CLI against the real image (section 67), so there is no hardware step to write a procedure for. |
| UI launches | N/A | Same as install-update-remove: covered by the Docker CLI suite and ui/shared's own tests, not by a hardware procedure. |
| Upgrade preserves state | N/A | No vendor updater to exercise; container replacement is what the Docker CLI suite already does. |
| Removal does not delete retained backups | N/A | No vendor uninstaller to exercise. The compose profile declares no named volume, so there is nothing for `down -v` to reach. |
| Native authentication | UNSUP | Tier C. No identity provider on a plain Docker host; section 13A local auth is the whole story. |
| Native notifications | UNSUP | Tier C. Webhook notifications instead. |
| Embedded window | UNSUP | Tier C. Opens in a standalone browser. |
| App-store packaging | UNSUP | Tier C. There is no store; this is the raw compose deployment. |
| Storage picker | UNSUP | Tier C. Manual path entry, because no host volume API exists to browse. |

#### OpenMediaVault (Tier C)

| Capability | Outcome | Why |
|---|---|---|
| Core binary hash parity | BLOCKED | #174 — container/release-manifest.json pins commit c51a07f, which is not an ancestor of main after the squash-merge rewrite, so there is no build to compare a hash against. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/openmediavault-provider-acceptance.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/openmediavault-provider-acceptance.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/openmediavault-provider-acceptance.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/openmediavault-provider-acceptance.md, not yet executed |
| Native authentication | UNSUP | Tier C. A native Workbench plugin is deferred by section 4A, and auth would come with it. |
| Native notifications | UNSUP | Tier C. Webhooks; OMV notifications would need the deferred plugin. |
| Embedded window | UNSUP | Tier C. There is no Workbench navigation entry, by design; the UI is reached on its own port. |
| App-store packaging | UNSUP | Tier C. A Compose deployment profile, not an omv-extras package. Section 4A defers the Debian plugin. |
| Storage picker | UNSUP | Tier C. Paths are set once in the env file; the app does not browse OMV filesystems. |

#### Proxmox VE (Tier C)

| Capability | Outcome | Why |
|---|---|---|
| Core binary hash parity | BLOCKED | #174 — container/release-manifest.json pins commit c51a07f, which is not an ancestor of main after the squash-merge rewrite, so there is no build to compare a hash against. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/proxmox-ve-deployment.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/proxmox-ve-deployment.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/proxmox-ve-deployment.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/proxmox-ve-deployment.md, not yet executed |
| Native authentication | UNSUP | Tier C. No PVE realm, PAM hook or API token; section 13A local auth only. |
| Native notifications | UNSUP | Tier C. Webhooks. PVE notification targets would mean touching the host management plane. |
| Embedded window | UNSUP | Tier C. Section 4A defers the PVE Web UI plugin indefinitely, so there is no window to embed in. |
| App-store packaging | UNSUP | Tier C, and structurally so: Proxmox VE has no third-party application store to package into. That is why this provider is a deployment profile at all. |
| Storage picker | UNSUP | Tier C. One host directory is shared into the guest and the paths under it are fixed in the env file. |

<!-- END GENERATED MATRIX -->
