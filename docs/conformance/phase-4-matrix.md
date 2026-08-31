# Phase 4 cross-provider conformance matrix

Generated. Do not edit the region between the markers by hand: it is the output
of a real run of `apps/common/packaging`'s conformance suite, and the suite fails
if what is checked in differs from what a fresh run produces. To regenerate:

```bash
cd apps/common && CONFORMANCE_UPDATE=1 GOWORK=off go test ./packaging/ -count=1 -run TestCrossProviderConformanceMatrix
```

`-count=1` is not decoration. The checks read files all over the tree and the test
cache keys on this module's own inputs, so a run after editing a provider can be
served from the cache and quietly regenerate nothing.

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

## Whose gate a column counts towards

Every column also declares the EPIC whose gate consumes it. Six of them are EPIC
B's, and the Phase 4 Exit Gate is computed over those six and over nothing else:
Generic Docker, TrueNAS, Unraid, OpenMediaVault, Synology DSM and Proxmox VE, the
same six #86 and #81 name.

UGOS is EPIC D's. Its packaging is #83 (D1.2) since the UGOS split, so it cannot
be part of an EPIC B gate: an EPIC B phase that waits on a package built on
hardware nobody in this repository owns is a phase that cannot close. It is still
a column here, and still checked on exactly the same terms as every other one,
because the alternative was deleting it. A deleted column reports no blockers, it
reports nothing, and nothing reads as clean. The two-directional store-packaging
check is the concrete case: it is what caught UGOS claiming app-store packaging
with no UPK behind it, and it only works while there is a UGOS column for it to
read.

So drift in a UGOS declaration is still a red build, and it is still EPIC D's to
fix. What it is not is a Phase 4 result.

## What is blocking today

- **#174** — `container/release-manifest.json` pins commit `c51a07f`, which is not
  an ancestor of `main` after the squash-merge rewrite, so its hashes describe a
  build that is not in this history. That is what `release-manifest-integrity`
  reports, for all seven providers, because it is a repository-wide fact and this
  table says so rather than dressing it up as seven measurements. The separate
  per-provider row, `core-binary-hash-parity`, asks a different question: do the
  binaries THIS provider ships hash to what the manifest recorded? No provider
  checks a binary into this repository, so that row is `N/A` everywhere except
  Synology, whose `.spk` really is hashed against the manifest by
  `TestVerify_BinaryHashParity` in `apps/synology`'s own module, and UGOS, which
  is meant to ship an artifact and does not yet. Neither row can go green without
  the comparison it names actually happening.
- **#180** — `ui/shared/vite.config.ts` picks the frontend shell at build time
  from `VITE_PLATFORM`, defaulting to `generic`, and `serve-ui` serves one
  `go:embed`ed bundle. Nothing in the release build selects a provider, so every
  artifact anyone installs, the canonical image and the `.spk` alike, runs the
  generic bridge. A capability flag in `apps/<provider>/frontend/platform.ts` is
  therefore a statement of repository intent, not of deployed behaviour, and
  every cell resolved from one is `BLOCKED` on this rather than `PASS`.
- **#83** — work package 4.2's UGOS UPK moved out of this EPIC into EPIC D and is
  still open. `apps/ugos/` holds the frontend bridge and nothing else: no
  `project.yaml`, no Compose, no icon, no architecture image tar, so its packaging
  cells are `BLOCKED` rather than passing. Those blockers are EPIC D's, and the
  Phase 4 Exit Gate below is not computed over them.

What holds the Phase 4 Exit Gate open, then, is `#174` and `#180`, and both are
EPIC B's own work. No cell of the six providers EPIC B claims fails: the suite
reddens the build if one does, so a `FAIL` in the table below cannot survive
long enough to be read here. The generated **Phase 4 Exit Gate** section states
the verdict over those six, and lists every cell holding it open with the issue
tracking it.

## Nothing here is a certification

Every `OPERATOR` row is a provider that is **build-supported and uncertified**
in section 68's own words. A green matrix proves the packaging metadata is
well-formed and mutually consistent. It proves nothing about how any of these
platforms behaves. `docs/acceptance/` is where that gets decided.

<!-- BEGIN GENERATED MATRIX -->

### Support tiers (§4A)

| Provider | Tier | Gated by | Work package | Acceptance procedure |
|---|---|---|---|---|
| UGOS Pro | A | EPIC D (reported here, gated there) | 4.2 | `docs/acceptance/ugos-local-notification.md` |
| Synology DSM | B | EPIC B (Phase 4) | 4.4 | `docs/acceptance/synology-dsm-package-lifecycle.md` |
| TrueNAS | B | EPIC B (Phase 4) | 4.3 | `docs/acceptance/truenas-provider-acceptance.md` |
| Unraid | B | EPIC B (Phase 4) | 4.3 | `docs/acceptance/unraid-provider-acceptance.md` |
| Generic Docker | C | EPIC B (Phase 4) | 4.1 | `none (automated instead)` |
| OpenMediaVault | C | EPIC B (Phase 4) | 4.3 | `docs/acceptance/openmediavault-provider-acceptance.md` |
| Proxmox VE | C | EPIC B (Phase 4) | 4.5 | `docs/acceptance/proxmox-ve-deployment.md` |

### Per-capability results

| Capability | UGOS Pro (EPIC D) | Synology DSM | TrueNAS | Unraid | Generic Docker | OpenMediaVault | Proxmox VE |
|---|---|---|---|---|---|---|---|
| Provider identified correctly | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| Provider package metadata present | BLOCKED | PASS | PASS | PASS | PASS | PASS | PASS |
| Uses the exact canonical image | BLOCKED | N/A | PASS | PASS | N/A | PASS | PASS |
| Release manifest well-formed and reachable (repository-wide) | BLOCKED | BLOCKED | BLOCKED | BLOCKED | BLOCKED | BLOCKED | BLOCKED |
| Core binary hash parity (this provider's own shipped bytes) | BLOCKED | N/A | N/A | N/A | N/A | N/A | N/A |
| This provider's own architecture claim matches the build | BLOCKED | PASS | N/A | N/A | N/A | N/A | N/A |
| State path persists outside the container | BLOCKED | N/A | PASS | PASS | PASS | PASS | PASS |
| Backup root constrained | BLOCKED | N/A | PASS | PASS | PASS | PASS | PASS |
| Auth mode explicit and honest | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| No bundled secrets | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| No provider-specific lifecycle implementation | PASS | N/A | PASS | PASS | N/A | PASS | PASS |
| API reachable only through the intended path | BLOCKED | PASS | PASS | PASS | PASS | PASS | PASS |
| Provider removal does not alter core | PASS | PASS | PASS | PASS | N/A | PASS | PASS |
| Host management plane not modified | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| Install / update / remove semantics | BLOCKED | OPERATOR | OPERATOR | OPERATOR | N/A | OPERATOR | OPERATOR |
| UI launches | BLOCKED | OPERATOR | OPERATOR | OPERATOR | N/A | OPERATOR | OPERATOR |
| Upgrade preserves state | BLOCKED | OPERATOR | OPERATOR | OPERATOR | N/A | OPERATOR | OPERATOR |
| Removal does not delete retained backups | BLOCKED | OPERATOR | OPERATOR | OPERATOR | N/A | OPERATOR | OPERATOR |
| Native authentication | BLOCKED | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP |
| Native notifications | BLOCKED | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP |
| Embedded window | BLOCKED | BLOCKED | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP |
| App-store packaging | BLOCKED | BLOCKED | BLOCKED | BLOCKED | UNSUP | UNSUP | UNSUP |
| Storage picker | BLOCKED | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP |

### Totals

| Outcome | Cells |
|---|---|
| PASS | 66 |
| PENDING_OPERATOR | 20 |
| UNSUPPORTED | 26 |
| NOT_APPLICABLE | 22 |
| BLOCKED | 27 |
| FAIL | 0 |

### Phase 4 Exit Gate

Computed over the 6 providers EPIC B claims, and over nothing else: Synology DSM, TrueNAS, Unraid, Generic Docker, OpenMediaVault, Proxmox VE.

**Not met.** 0 cell(s) failed and 10 could not be decided, every one of them in a column EPIC B claims:

| Provider | Capability | Outcome | Tracked by |
|---|---|---|---|
| Synology DSM | Release manifest well-formed and reachable (repository-wide) | BLOCKED | #174 |
| Synology DSM | Embedded window | BLOCKED | #180 |
| Synology DSM | App-store packaging | BLOCKED | #180 |
| TrueNAS | Release manifest well-formed and reachable (repository-wide) | BLOCKED | #174 |
| TrueNAS | App-store packaging | BLOCKED | #180 |
| Unraid | Release manifest well-formed and reachable (repository-wide) | BLOCKED | #174 |
| Unraid | App-store packaging | BLOCKED | #180 |
| Generic Docker | Release manifest well-formed and reachable (repository-wide) | BLOCKED | #174 |
| OpenMediaVault | Release manifest well-formed and reachable (repository-wide) | BLOCKED | #174 |
| Proxmox VE | Release manifest well-formed and reachable (repository-wide) | BLOCKED | #174 |

**UGOS Pro is EPIC D's column** (work package 4.2).
All 23 of its cells are decided by the same runner, on the same terms as every
other column, and reported in full below; 17 are blocked today, on #174 and #83.
None of them is in the verdict above. A capability EPIC D owns cannot hold
EPIC B's Phase 4 open, and an EPIC D column that goes green cannot close it.

### Every cell that is not a plain PASS

Section 63A's requirement in full: an unsupported capability is reported, with a
reason, rather than skipped. Every row below is a cell this run did not pass, and
why.

#### UGOS Pro (Tier A, reported here, gated by EPIC D)

| Capability | Outcome | Why |
|---|---|---|
| Provider package metadata present | BLOCKED | #83 — Work package 4.2's UPK was moved out of this EPIC into EPIC D and is still open as #83. apps/ugos/ contains the frontend bridge and nothing else: no project.yaml, no compose, no icon, no image tar. Until #83 lands, UGOS is the one Phase 4 Exit Gate provider with no package in this repository. |
| Uses the exact canonical image | BLOCKED | #83 — Nothing in apps/ugos/ references an image yet, so there is no reference to compare. |
| Release manifest well-formed and reachable (repository-wide) | BLOCKED | #174 — container/release-manifest.json pins commit c51a07f, which is not an ancestor of main after the squash-merge rewrite, so its hashes describe a build that is not in this history. This row is repository-wide by design: it reads no provider metadata and returns the same verdict in all seven columns, which is why it is named for what it decides rather than for the per-provider parity claim it used to be mistaken for. |
| Core binary hash parity (this provider's own shipped bytes) | BLOCKED | #83 — The UPK is what would carry the architecture image tars (section 41), and there is no UPK, so there is no shipped byte to hash. Unlike the six providers that consume the OCI image by reference, this one is not not-applicable: UGOS is meant to ship its own artifact, so the cell stays blocked until #83 produces one. |
| This provider's own architecture claim matches the build | BLOCKED | #83 — The UPK declares the architecture image tars (section 41); no UPK, no claim of its own to check. |
| State path persists outside the container | BLOCKED | #83 — The UPK's compose declares the storage mapping (section 22); it does not exist yet. |
| Backup root constrained | BLOCKED | #83 — BACKUP_ROOT comes from the UPK's install parameters (section 20); they do not exist yet. |
| API reachable only through the intended path | BLOCKED | #83 — The UPK's compose decides which container publishes a port; it does not exist yet. |
| Install / update / remove semantics | BLOCKED | #83 — docs/acceptance/ugos-local-notification.md covers notifications only. The install/update/disable/uninstall/reinstall procedure is work package 4.2's, and belongs with #83. |
| UI launches | BLOCKED | #83 — The UI an operator would launch is the UPK's, and docs/acceptance/ugos-local-notification.md covers notifications only. Section 12's embedded provider window is delivered by the UPK too, which is why embedded-window below is blocked on the same issue rather than declared supported: both cannot be true at once. |
| Upgrade preserves state | BLOCKED | #83 — Section 46's upgrade behaviour needs the package that gets upgraded. |
| Removal does not delete retained backups | BLOCKED | #83 — Section 48's uninstall behaviour needs the package that gets uninstalled. |
| Native authentication | BLOCKED | #83 — The bridge opts in, but nothing this repository produces loads the UGOS bridge: there is no UPK (#83), and even once there is one, serve-ui embeds a single bundle chosen at build time (#180). A capability flag is a statement of intent until an installed artifact runs it. |
| Native notifications | BLOCKED | #83 — Same as native-auth: the flag is set in apps/ugos/frontend/platform.ts and no artifact loads that file. |
| Embedded window | BLOCKED | #83 — Section 12's embedded provider window is delivered by the UPK, which is exactly what ui-launch above says. Declaring this supported while blocking ui-launch on the same sentence was a contradiction: both rows are the same missing package. |
| App-store packaging | BLOCKED | #83 — The bridge claims it, and section 4A promises it, but no UPK exists in this repository yet. Passing on the bridge flag alone would be exactly the kind of claim the store-artifact half of this check exists to refuse. |
| Storage picker | BLOCKED | #83 — Same as native-auth: declared in a bridge no shipped artifact loads. |

#### Synology DSM (Tier B, gated by EPIC B's Phase 4)

| Capability | Outcome | Why |
|---|---|---|
| Uses the exact canonical image | N/A | Synology is the one Phase 4 provider that cannot consume the OCI image: DSM's Package Center installs a native .spk. Section 3.7 makes the SPK a sibling of the image carrying the same core binary digest, so parity here is binary parity, not image parity. |
| Release manifest well-formed and reachable (repository-wide) | BLOCKED | #174 — container/release-manifest.json pins commit c51a07f, which is not an ancestor of main after the squash-merge rewrite, so its hashes describe a build that is not in this history. This row is repository-wide by design: it reads no provider metadata and returns the same verdict in all seven columns, which is why it is named for what it decides rather than for the per-provider parity claim it used to be mistaken for. |
| Core binary hash parity (this provider's own shipped bytes) | N/A | The .spk is not in this repository; cmd/spkctl builds it. The byte comparison this row demands is real and it does run: spkctl verify re-derives each binary's SHA-256 out of a finished package and compares it against container/release-manifest.json, and TestVerify_BinaryHashParity is the test that proves it, including the negative case. apps/common/packaging cannot execute it without importing across the apps/synology module boundary that scripts/architecture/*.sh enforces, so this cell records where the comparison happens instead of pretending to do it here. |
| State path persists outside the container | N/A | DSM fixes the persistent location: /var/packages/<pkg>/var under the package FHS, not a bind mount this repository declares. |
| Backup root constrained | N/A | The backup root is a DSM shared folder the operator picks at install time: conf/resource's data-share worker declares the share by name and carries no path, so there is no checked-in host path pair for this check to compare. What IS decided in this repository is the other side of the same rule, that the package places no key material or auth state anywhere (no-bundled-secrets) and that its lifecycle scripts delete nothing outside the package footprint. The containment itself is step 5 of the procedure, which puts a canary in the share and diffs a listing across the uninstall. |
| No provider-specific lifecycle implementation | N/A | DSM's package format MANDATES preinst/postinst/preuninst/postuninst/preupgrade/postupgrade and start-stop-status. Those scripts are the platform's contract, not a lifecycle engine of our own, and apps/synology holds them to wrapper-only behaviour. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/synology-dsm-package-lifecycle.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/synology-dsm-package-lifecycle.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/synology-dsm-package-lifecycle.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/synology-dsm-package-lifecycle.md, not yet executed |
| Native authentication | UNSUP | Tier B. Section 4A makes DSM SSO a follow-on capability; the initial package uses section 13A local auth. |
| Native notifications | UNSUP | Tier B. No DSM notification adapter in v1; webhooks instead. |
| Embedded window | BLOCKED | #180 — apps/synology/frontend/platform.ts opts in, but the .spk serves the same go:embed'ed bundle the canonical image does, and ui/shared/vite.config.ts defaults that bundle to the generic shell. apps/synology/README.md's first known gap says it outright and step 3.6 of the acceptance procedure tells the operator to expect the generic bridge, so recording PASS here contradicted two documents in the same tree. #180 tracks the missing serve-side selection. |
| App-store packaging | BLOCKED | #180 — The store artifacts are real and checked in, and the bridge opts in, but no artifact this repository produces loads that bridge: ui/shared/vite.config.ts picks the shell at build time and defaults to generic, and serve-ui serves one go:embed'ed bundle with no flag to serve another. So a user who installs through the platform's own store is still told this is a Docker Compose deployment, which is the defect this check was written to catch. #180 tracks giving serve-ui a way to select a bundle. |
| Storage picker | UNSUP | Tier B. The shared folder is chosen once at install time through DSM, not browsed from inside the app. |

#### TrueNAS (Tier B, gated by EPIC B's Phase 4)

| Capability | Outcome | Why |
|---|---|---|
| Release manifest well-formed and reachable (repository-wide) | BLOCKED | #174 — container/release-manifest.json pins commit c51a07f, which is not an ancestor of main after the squash-merge rewrite, so its hashes describe a build that is not in this history. This row is repository-wide by design: it reads no provider metadata and returns the same verdict in all seven columns, which is why it is named for what it decides rather than for the per-provider parity claim it used to be mistaken for. |
| Core binary hash parity (this provider's own shipped bytes) | N/A | This provider consumes the canonical OCI image by reference and checks in no core binary of its own, so there is no second copy of the bytes here to hash against the release manifest. Parity for a provider of this shape is the image reference it names, which canonical-image-parity decides. A cell here can only go green once a provider declares a binaryArtifacts entry and the file behind it hashes to what the manifest recorded. |
| This provider's own architecture claim matches the build | N/A | This provider makes no architecture claim of its own: it names one multi-arch canonical image and lets the runtime pick. The claim that the built architecture set matches canonical.json is repository-wide and is release-manifest-integrity's row, not seven copies of itself. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/truenas-provider-acceptance.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/truenas-provider-acceptance.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/truenas-provider-acceptance.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/truenas-provider-acceptance.md, not yet executed |
| Native authentication | UNSUP | Tier B. Section 4A gives TrueNAS the generic local auth; no middleware session adapter in v1. |
| Native notifications | UNSUP | Tier B. Webhooks instead of TrueNAS alerts. |
| Embedded window | UNSUP | Tier B. The Apps portal link opens the UI in a normal browser tab. |
| App-store packaging | BLOCKED | #180 — The store artifacts are real and checked in, and the bridge opts in, but no artifact this repository produces loads that bridge: ui/shared/vite.config.ts picks the shell at build time and defaults to generic, and serve-ui serves one go:embed'ed bundle with no flag to serve another. So a user who installs through the platform's own store is still told this is a Docker Compose deployment, which is the defect this check was written to catch. #180 tracks giving serve-ui a way to select a bundle. |
| Storage picker | UNSUP | Tier B. questions.yaml asks for the dataset paths at install time; the running app does not browse pools. |

#### Unraid (Tier B, gated by EPIC B's Phase 4)

| Capability | Outcome | Why |
|---|---|---|
| Release manifest well-formed and reachable (repository-wide) | BLOCKED | #174 — container/release-manifest.json pins commit c51a07f, which is not an ancestor of main after the squash-merge rewrite, so its hashes describe a build that is not in this history. This row is repository-wide by design: it reads no provider metadata and returns the same verdict in all seven columns, which is why it is named for what it decides rather than for the per-provider parity claim it used to be mistaken for. |
| Core binary hash parity (this provider's own shipped bytes) | N/A | This provider consumes the canonical OCI image by reference and checks in no core binary of its own, so there is no second copy of the bytes here to hash against the release manifest. Parity for a provider of this shape is the image reference it names, which canonical-image-parity decides. A cell here can only go green once a provider declares a binaryArtifacts entry and the file behind it hashes to what the manifest recorded. |
| This provider's own architecture claim matches the build | N/A | This provider makes no architecture claim of its own: it names one multi-arch canonical image and lets the runtime pick. The claim that the built architecture set matches canonical.json is repository-wide and is release-manifest-integrity's row, not seven copies of itself. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/unraid-provider-acceptance.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/unraid-provider-acceptance.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/unraid-provider-acceptance.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/unraid-provider-acceptance.md, not yet executed |
| Native authentication | UNSUP | Tier B. Section 4A gives Unraid the generic local auth; no plugin is required for v1. |
| Native notifications | UNSUP | Tier B. Webhooks instead of Unraid notifications, which would need a plugin. |
| Embedded window | UNSUP | Tier B. The WebUI link opens a normal browser tab. |
| App-store packaging | BLOCKED | #180 — The store artifacts are real and checked in, and the bridge opts in, but no artifact this repository produces loads that bridge: ui/shared/vite.config.ts picks the shell at build time and defaults to generic, and serve-ui serves one go:embed'ed bundle with no flag to serve another. So a user who installs through the platform's own store is still told this is a Docker Compose deployment, which is the defect this check was written to catch. #180 tracks giving serve-ui a way to select a bundle. |
| Storage picker | UNSUP | Tier B. Community Applications collects the paths at install time; the app does not browse shares. |

#### Generic Docker (Tier C, gated by EPIC B's Phase 4)

| Capability | Outcome | Why |
|---|---|---|
| Uses the exact canonical image | N/A | container/compose.yaml BUILDS the canonical image from container/Dockerfile rather than pulling a published reference. It is the source of the image the other six profiles consume, so pinning it to its own output would be circular. |
| Release manifest well-formed and reachable (repository-wide) | BLOCKED | #174 — container/release-manifest.json pins commit c51a07f, which is not an ancestor of main after the squash-merge rewrite, so its hashes describe a build that is not in this history. This row is repository-wide by design: it reads no provider metadata and returns the same verdict in all seven columns, which is why it is named for what it decides rather than for the per-provider parity claim it used to be mistaken for. |
| Core binary hash parity (this provider's own shipped bytes) | N/A | container/ BUILDS the canonical image from container/Dockerfile rather than consuming a published one, so its binaries are compiled during the build and nothing here is a checked-in artifact to hash. The bytes are decided by the build itself, and apps/generic/tests/dockercli drives the real image the build produces. |
| This provider's own architecture claim matches the build | N/A | This provider makes no architecture claim of its own: it names one multi-arch canonical image and lets the runtime pick. The claim that the built architecture set matches canonical.json is repository-wide and is release-manifest-integrity's row, not seven copies of itself. |
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

#### OpenMediaVault (Tier C, gated by EPIC B's Phase 4)

| Capability | Outcome | Why |
|---|---|---|
| Release manifest well-formed and reachable (repository-wide) | BLOCKED | #174 — container/release-manifest.json pins commit c51a07f, which is not an ancestor of main after the squash-merge rewrite, so its hashes describe a build that is not in this history. This row is repository-wide by design: it reads no provider metadata and returns the same verdict in all seven columns, which is why it is named for what it decides rather than for the per-provider parity claim it used to be mistaken for. |
| Core binary hash parity (this provider's own shipped bytes) | N/A | This provider consumes the canonical OCI image by reference and checks in no core binary of its own, so there is no second copy of the bytes here to hash against the release manifest. Parity for a provider of this shape is the image reference it names, which canonical-image-parity decides. A cell here can only go green once a provider declares a binaryArtifacts entry and the file behind it hashes to what the manifest recorded. |
| This provider's own architecture claim matches the build | N/A | This provider makes no architecture claim of its own: it names one multi-arch canonical image and lets the runtime pick. The claim that the built architecture set matches canonical.json is repository-wide and is release-manifest-integrity's row, not seven copies of itself. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/openmediavault-provider-acceptance.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/openmediavault-provider-acceptance.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/openmediavault-provider-acceptance.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/openmediavault-provider-acceptance.md, not yet executed |
| Native authentication | UNSUP | Tier C. A native Workbench plugin is deferred by section 4A, and auth would come with it. |
| Native notifications | UNSUP | Tier C. Webhooks; OMV notifications would need the deferred plugin. |
| Embedded window | UNSUP | Tier C. There is no Workbench navigation entry, by design; the UI is reached on its own port. |
| App-store packaging | UNSUP | Tier C. A Compose deployment profile, not an omv-extras package. Section 4A defers the Debian plugin. |
| Storage picker | UNSUP | Tier C. Paths are set once in the env file; the app does not browse OMV filesystems. |

#### Proxmox VE (Tier C, gated by EPIC B's Phase 4)

| Capability | Outcome | Why |
|---|---|---|
| Release manifest well-formed and reachable (repository-wide) | BLOCKED | #174 — container/release-manifest.json pins commit c51a07f, which is not an ancestor of main after the squash-merge rewrite, so its hashes describe a build that is not in this history. This row is repository-wide by design: it reads no provider metadata and returns the same verdict in all seven columns, which is why it is named for what it decides rather than for the per-provider parity claim it used to be mistaken for. |
| Core binary hash parity (this provider's own shipped bytes) | N/A | This provider consumes the canonical OCI image by reference and checks in no core binary of its own, so there is no second copy of the bytes here to hash against the release manifest. Parity for a provider of this shape is the image reference it names, which canonical-image-parity decides. A cell here can only go green once a provider declares a binaryArtifacts entry and the file behind it hashes to what the manifest recorded. |
| This provider's own architecture claim matches the build | N/A | This provider makes no architecture claim of its own: it names one multi-arch canonical image and lets the runtime pick. The claim that the built architecture set matches canonical.json is repository-wide and is release-manifest-integrity's row, not seven copies of itself. |
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
