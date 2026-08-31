# Provider store and catalog submission preflight

The recorded answer to "is this fit to hand a store reviewer", per target, for every target
EPIC B distributes, plus EPIC B Phase 6's adapter conformance drift gate. Issue #90 (Work
Package 5.4, `docs/EPIC-B-multi-nas.md` §73), with the drift gate from #81.

**Everything below the marker is generated.** It is produced by a real run of
`TestProviderStoreSubmissionPreflight` in `apps/common/packaging` and compared against this
file on every commit, so it is a record of what a run actually decided rather than of what
somebody believed when they wrote it. Regenerate it with:

```
cd apps/common && CONFORMANCE_UPDATE=1 GOWORK=off go test ./packaging/ -count=1 -run TestProviderStoreSubmissionPreflight
```

## What this is, and what it is not

This is not a second conformance suite. `docs/conformance/phase-4-matrix.md` answers "does
this target's package hold together"; this answers "is that package fit to submit", which
is a different question with a different audience. A target registers once, in
`apps/common/packaging/conformance.json`, and is registered with this preflight by that
same act: `submission.json` adds only the store it goes to, the files its package carries,
and its own declarations.

Four of the drift gate's eight elements are already decided per target by the Phase 4
matrix, and they resolve here by consuming that verdict rather than by a second
implementation of the same rule. Only a PASS counts. A matrix that declined to decide an
element does not get laundered into a green drift cell, which is precisely the move a drift
gate exists to stop.

## Two things this report is careful about

**Blocked is not a pass and not a failure.** One rule cannot reach a verdict today and it
is not this work package's to fix: the licence, the software bill of materials and the
third-party inventory are B5.2's (#88), and until a `LICENSE` is in the tree the support,
source and licence material is two thirds of a material. Reporting that as a pass would
claim something nobody checked, and reporting it as a failure would blame this work
package for somebody else's. It is reported undecided, with the issue that owns it.

**A declaration is re-derived, never re-stated.** `container/release-manifest.json` used to
pin a commit that was not an ancestor of the main branch, which held `artifact-provenance`
undecided for all six targets. #182 repinned it, the check now passes, and the six
declarations were re-derived rather than left standing, because a blocked declaration whose
reason has been fixed underneath it is a documented reason not to look. That is what the
staleness guard turns red, and it is why the same thing will happen to the four
`materials-support-source-license` cells the day #88's `LICENSE` lands: whichever change
merges second re-derives them.

**UGREEN is in the mechanism and out of the gate.** UGOS Pro's column is decided by exactly
these rules and reported in full below. While EPIC D's #83 has produced no `.UPK` it has no
artifact, so its artifact-dependent rules record not-yet-applicable rather than failing, and
its shared listing materials record ready, because they are ready. Nothing in that column
can hold EPIC B's Phase 5 open, and nothing in it going green can close it. EPIC D's #178
consumes the verdict and the bundle; it does not re-run any of this.

## External approval

Whether a store accepts a submission is outside this repository's control (§75). A target
recorded READY_TO_SUBMIT here is ready to be submitted, which is the only half of that
sentence anything in this repository can decide.

<!-- BEGIN GENERATED PREFLIGHT -->

### Recorded readiness verdicts

This is the table EPIC D's #178 consumes. A target it cannot find a row for here has not
been preflighted, and #178 refuses to submit on that basis rather than re-running any of
these checks.

| Target | Store or catalog | Gated by | Verdict | Undecided, tracked by | Needs the real platform |
|---|---|---|---|---|---|
| UGOS Pro | UGREEN App Center | EPIC D (reported here, gated there) | **NOT_YET_APPLICABLE** | none | nothing |
| Synology DSM | Synology Package Center | EPIC B (Phase 5) | **READY_PENDING_OPERATOR** | none | 2 step(s) |
| TrueNAS | TrueNAS Apps catalog | EPIC B (Phase 5) | **READY_PENDING_OPERATOR** | none | 2 step(s) |
| Unraid | Unraid Community Applications | EPIC B (Phase 5) | **READY_PENDING_OPERATOR** | none | 2 step(s) |
| Generic Docker | no store (documented workflow) | EPIC B (Phase 5) | **READY_PENDING_OPERATOR** | none | 1 step(s) |
| OpenMediaVault | no store (documented workflow) | EPIC B (Phase 5) | **READY_PENDING_OPERATOR** | none | 1 step(s) |
| Proxmox VE | no store (documented workflow) | EPIC B (Phase 5) | **READY_PENDING_OPERATOR** | none | 1 step(s) |

Why each target reads the way it does:

- **UGOS Pro** — UGOS Pro declares no package this repository can inspect and ships no store artifact, so there is nothing to preflight yet; its shared listing materials are recorded on their own merits above
- **Synology DSM** — every rule this repository can decide held; 2 step(s) need the real platform: materials-screenshots, proactive-alert-delivery
- **TrueNAS** — every rule this repository can decide held; 2 step(s) need the real platform: materials-screenshots, proactive-alert-delivery
- **Unraid** — every rule this repository can decide held; 2 step(s) need the real platform: materials-screenshots, proactive-alert-delivery
- **Generic Docker** — every rule this repository can decide held; 1 step(s) need the real platform: proactive-alert-delivery
- **OpenMediaVault** — every rule this repository can decide held; 1 step(s) need the real platform: proactive-alert-delivery
- **Proxmox VE** — every rule this repository can decide held; 1 step(s) need the real platform: proactive-alert-delivery

### Per-rule results

| Rule | UGOS Pro (EPIC D) | Synology DSM | TrueNAS | Unraid | Generic Docker | OpenMediaVault | Proxmox VE |
|---|---|---|---|---|---|---|---|
| No self-update mechanism in the shipped package | N/A | PASS | PASS | PASS | PASS | PASS | PASS |
| No floating image tag | N/A | PASS | PASS | PASS | PASS | PASS | PASS |
| No privileged mode requested by the package | N/A | PASS | PASS | PASS | PASS | PASS | PASS |
| No mandatory telemetry endpoint | N/A | PASS | PASS | PASS | PASS | PASS | PASS |
| Drift gate: image reference | N/A | N/A | PASS | PASS | N/A | PASS | PASS |
| Drift gate: required mounts | N/A | N/A | PASS | PASS | PASS | PASS | PASS |
| Drift gate: expected ports | N/A | PASS | PASS | PASS | PASS | PASS | PASS |
| Drift gate: health check | N/A | N/A | PASS | PASS | PASS | PASS | PASS |
| Drift gate: runtime profile | N/A | N/A | PASS | PASS | PASS | PASS | PASS |
| Drift gate: declared architecture support | N/A | PASS | N/A | N/A | N/A | N/A | N/A |
| Drift gate: forbidden-privilege set | N/A | N/A | PASS | PASS | PASS | PASS | PASS |
| Drift gate: /api/v1 compatibility | N/A | N/A | PASS | PASS | PASS | PASS | PASS |
| Store description | PASS | PASS | PASS | PASS | N/A | N/A | N/A |
| Store icon | PASS | PASS | PASS | PASS | N/A | N/A | N/A |
| Store screenshots | N/A | OPERATOR | OPERATOR | OPERATOR | N/A | N/A | N/A |
| Release notes | PASS | PASS | PASS | PASS | N/A | N/A | N/A |
| Privacy disclosure | PASS | PASS | PASS | PASS | N/A | N/A | N/A |
| Permission rationale | PASS | PASS | PASS | PASS | N/A | N/A | N/A |
| Support, source and licence materials | PASS | PASS | PASS | PASS | N/A | N/A | N/A |
| Submission checklist complete against the tree | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| Proactive alerts reach the administrator | N/A | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR |
| Recovery documented without a terminal | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| Submitted bytes traceable to a recorded build | N/A | PASS | PASS | PASS | PASS | PASS | PASS |

### Totals

| Outcome | Cells |
|---|---|
| PASS | 104 |
| PENDING_OPERATOR | 9 |
| UNSUPPORTED | 0 |
| NOT_APPLICABLE | 48 |
| BLOCKED | 0 |
| FAIL | 0 |

### Phase 5 submission gate

Computed over the 6 targets EPIC B ships, and over nothing else: Synology DSM, TrueNAS, Unraid, Generic Docker, OpenMediaVault, Proxmox VE.

**Met.** Every rule that applies to every one of those targets was decided here and held.
External store approval stays outside this repository's control (§75).

**UGOS Pro is EPIC D's column** (work package 4.2), and it is recorded **NOT_YET_APPLICABLE**.
It is decided by these same checks, on the same terms as every other target, and it is in
nobody's Phase 5 verdict: a rule EPIC D owns cannot hold EPIC B's Phase 5 open, and an
EPIC D column that goes green cannot close it. UGOS Pro declares no package this repository can inspect and ships no store artifact, so there is nothing to preflight yet; its shared listing materials are recorded on their own merits above

### Every cell that is not a plain PASS

A rule that is not run reads as a rule that passed, so every cell this run did not pass is
below, with why.

#### UGOS Pro (Tier A, UGREEN App Center, reported here, gated by EPIC D)

| Rule | Outcome | Why |
|---|---|---|
| No self-update mechanism in the shipped package | N/A | EPIC D's #83 has not produced the .UPK, so there is no UGREEN artifact to preflight. Recorded as not yet applicable rather than as a failing check, per this work package's own rule that a UGREEN row can never block Phase 5; the same mechanism decides this row the day #83 lands, with no edit here. |
| No floating image tag | N/A | EPIC D's #83 has not produced the .UPK, so there is no UGREEN artifact to preflight. Recorded as not yet applicable rather than as a failing check, per this work package's own rule that a UGREEN row can never block Phase 5; the same mechanism decides this row the day #83 lands, with no edit here. |
| No privileged mode requested by the package | N/A | EPIC D's #83 has not produced the .UPK, so there is no UGREEN artifact to preflight. Recorded as not yet applicable rather than as a failing check, per this work package's own rule that a UGREEN row can never block Phase 5; the same mechanism decides this row the day #83 lands, with no edit here. |
| No mandatory telemetry endpoint | N/A | EPIC D's #83 has not produced the .UPK, so there is no UGREEN artifact to preflight. Recorded as not yet applicable rather than as a failing check, per this work package's own rule that a UGREEN row can never block Phase 5; the same mechanism decides this row the day #83 lands, with no edit here. |
| Drift gate: image reference | N/A | EPIC D's #83 has not produced the .UPK, so there is no UGREEN artifact to preflight. Recorded as not yet applicable rather than as a failing check, per this work package's own rule that a UGREEN row can never block Phase 5; the same mechanism decides this row the day #83 lands, with no edit here. |
| Drift gate: required mounts | N/A | EPIC D's #83 has not produced the .UPK, so there is no UGREEN artifact to preflight. Recorded as not yet applicable rather than as a failing check, per this work package's own rule that a UGREEN row can never block Phase 5; the same mechanism decides this row the day #83 lands, with no edit here. |
| Drift gate: expected ports | N/A | EPIC D's #83 has not produced the .UPK, so there is no UGREEN artifact to preflight. Recorded as not yet applicable rather than as a failing check, per this work package's own rule that a UGREEN row can never block Phase 5; the same mechanism decides this row the day #83 lands, with no edit here. |
| Drift gate: health check | N/A | EPIC D's #83 has not produced the .UPK, so there is no UGREEN artifact to preflight. Recorded as not yet applicable rather than as a failing check, per this work package's own rule that a UGREEN row can never block Phase 5; the same mechanism decides this row the day #83 lands, with no edit here. |
| Drift gate: runtime profile | N/A | EPIC D's #83 has not produced the .UPK, so there is no UGREEN artifact to preflight. Recorded as not yet applicable rather than as a failing check, per this work package's own rule that a UGREEN row can never block Phase 5; the same mechanism decides this row the day #83 lands, with no edit here. |
| Drift gate: declared architecture support | N/A | EPIC D's #83 has not produced the .UPK, so there is no UGREEN artifact to preflight. Recorded as not yet applicable rather than as a failing check, per this work package's own rule that a UGREEN row can never block Phase 5; the same mechanism decides this row the day #83 lands, with no edit here. |
| Drift gate: forbidden-privilege set | N/A | EPIC D's #83 has not produced the .UPK, so there is no UGREEN artifact to preflight. Recorded as not yet applicable rather than as a failing check, per this work package's own rule that a UGREEN row can never block Phase 5; the same mechanism decides this row the day #83 lands, with no edit here. |
| Drift gate: /api/v1 compatibility | N/A | EPIC D's #83 has not produced the .UPK, so there is no UGREEN artifact to preflight. Recorded as not yet applicable rather than as a failing check, per this work package's own rule that a UGREEN row can never block Phase 5; the same mechanism decides this row the day #83 lands, with no edit here. |
| Store screenshots | N/A | The shared bundle covers UGREEN App Center's listing like every other target's, and a screenshot is of the app running on the hardware. EPIC D's #83 has produced no .UPK, so there is nothing to install and nothing to photograph. The listing copy, the icon, the release notes, the privacy disclosure and the permission rationale are all recorded ready above, which is the bundle #178 consumes rather than assembling a second copy of. |
| Proactive alerts reach the administrator | N/A | Same reason as the screenshots: docs/acceptance/store-submission-preflight.md carries UGOS Pro's section so EPIC D's #178 has the procedure waiting, and no operator can run it against a package that does not exist yet. |
| Submitted bytes traceable to a recorded build | N/A | EPIC D's #83 has not produced the .UPK, so there is no UGREEN artifact to preflight. Recorded as not yet applicable rather than as a failing check, per this work package's own rule that a UGREEN row can never block Phase 5; the same mechanism decides this row the day #83 lands, with no edit here. |

#### Synology DSM (Tier B, Synology Package Center, gated by EPIC B's Phase 5)

| Rule | Outcome | Why |
|---|---|---|
| Drift gate: image reference | N/A | Decided by consuming the cross-provider conformance matrix's own verdict for this column rather than by a second check, and that verdict is NOT_APPLICABLE for canonical-image-parity: Synology is the one target that cannot consume the OCI image; its package embeds the canonical binaries instead, which is why it is the one target with a real hash-parity check of its own. See apps/common/packaging/conformance.json. |
| Drift gate: required mounts | N/A | Synology's package is not container-based: DSM runs the two canonical binaries directly under its own package lifecycle, so there is no container service for this element to drift from. §81's own wording is that every CONTAINER-BASED adapter registers with the drift gate; the elements that do apply to a package (its image reference parity, its architecture claim, its port isolation) are decided above. |
| Drift gate: health check | N/A | Synology's package is not container-based: DSM runs the two canonical binaries directly under its own package lifecycle, so there is no container service for this element to drift from. §81's own wording is that every CONTAINER-BASED adapter registers with the drift gate; the elements that do apply to a package (its image reference parity, its architecture claim, its port isolation) are decided above. |
| Drift gate: runtime profile | N/A | Synology's package is not container-based: DSM runs the two canonical binaries directly under its own package lifecycle, so there is no container service for this element to drift from. §81's own wording is that every CONTAINER-BASED adapter registers with the drift gate; the elements that do apply to a package (its image reference parity, its architecture claim, its port isolation) are decided above. |
| Drift gate: forbidden-privilege set | N/A | Synology's package is not container-based: DSM runs the two canonical binaries directly under its own package lifecycle, so there is no container service for this element to drift from. §81's own wording is that every CONTAINER-BASED adapter registers with the drift gate; the elements that do apply to a package (its image reference parity, its architecture claim, its port isolation) are decided above. |
| Drift gate: /api/v1 compatibility | N/A | Synology's package is not container-based: DSM runs the two canonical binaries directly under its own package lifecycle, so there is no container service for this element to drift from. §81's own wording is that every CONTAINER-BASED adapter registers with the drift gate; the elements that do apply to a package (its image reference parity, its architecture claim, its port isolation) are decided above. |
| Store screenshots | OPERATOR | docs/acceptance/store-submission-preflight.md's Synology DSM section covers "screenshot"; the hardware run has not happened |
| Proactive alerts reach the administrator | OPERATOR | the dashboard renders the conditions and docs/acceptance/store-submission-preflight.md's Synology DSM section exercises all four; the hardware run has not happened |

#### TrueNAS (Tier B, TrueNAS Apps catalog, gated by EPIC B's Phase 5)

| Rule | Outcome | Why |
|---|---|---|
| Drift gate: declared architecture support | N/A | Decided by consuming the cross-provider conformance matrix's own verdict for this column rather than by a second check, and that verdict is NOT_APPLICABLE for architecture-parity: this target makes no architecture claim of its own; it names the canonical image, whose architectures the release manifest records. See apps/common/packaging/conformance.json. |
| Store screenshots | OPERATOR | docs/acceptance/store-submission-preflight.md's TrueNAS section covers "screenshot"; the hardware run has not happened |
| Proactive alerts reach the administrator | OPERATOR | the dashboard renders the conditions and docs/acceptance/store-submission-preflight.md's TrueNAS section exercises all four; the hardware run has not happened |

#### Unraid (Tier B, Unraid Community Applications, gated by EPIC B's Phase 5)

| Rule | Outcome | Why |
|---|---|---|
| Drift gate: declared architecture support | N/A | Decided by consuming the cross-provider conformance matrix's own verdict for this column rather than by a second check, and that verdict is NOT_APPLICABLE for architecture-parity: this target makes no architecture claim of its own; it names the canonical image, whose architectures the release manifest records. See apps/common/packaging/conformance.json. |
| Store screenshots | OPERATOR | docs/acceptance/store-submission-preflight.md's Unraid section covers "screenshot"; the hardware run has not happened |
| Proactive alerts reach the administrator | OPERATOR | the dashboard renders the conditions and docs/acceptance/store-submission-preflight.md's Unraid section exercises all four; the hardware run has not happened |

#### Generic Docker (Tier C, no store (documented workflow), gated by EPIC B's Phase 5)

| Rule | Outcome | Why |
|---|---|---|
| Drift gate: image reference | N/A | Decided by consuming the cross-provider conformance matrix's own verdict for this column rather than by a second check, and that verdict is NOT_APPLICABLE for canonical-image-parity: container/compose.yaml BUILDS the canonical image from container/Dockerfile rather than consuming it by reference, so there is no reference to compare. See apps/common/packaging/conformance.json. |
| Drift gate: declared architecture support | N/A | Decided by consuming the cross-provider conformance matrix's own verdict for this column rather than by a second check, and that verdict is NOT_APPLICABLE for architecture-parity: this target makes no architecture claim of its own; it names the canonical image, whose architectures the release manifest records. See apps/common/packaging/conformance.json. |
| Store description | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/generic.md is it. |
| Store icon | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/generic.md is it. |
| Store screenshots | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/generic.md is it. |
| Release notes | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/generic.md is it. |
| Privacy disclosure | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/generic.md is it. |
| Permission rationale | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/generic.md is it. |
| Support, source and licence materials | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/generic.md is it. |
| Proactive alerts reach the administrator | OPERATOR | the dashboard renders the conditions and docs/acceptance/store-submission-preflight.md's Generic Docker section exercises all four; the hardware run has not happened |

#### OpenMediaVault (Tier C, no store (documented workflow), gated by EPIC B's Phase 5)

| Rule | Outcome | Why |
|---|---|---|
| Drift gate: declared architecture support | N/A | Decided by consuming the cross-provider conformance matrix's own verdict for this column rather than by a second check, and that verdict is NOT_APPLICABLE for architecture-parity: this target makes no architecture claim of its own; it names the canonical image, whose architectures the release manifest records. See apps/common/packaging/conformance.json. |
| Store description | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/openmediavault.md is it. |
| Store icon | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/openmediavault.md is it. |
| Store screenshots | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/openmediavault.md is it. |
| Release notes | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/openmediavault.md is it. |
| Privacy disclosure | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/openmediavault.md is it. |
| Permission rationale | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/openmediavault.md is it. |
| Support, source and licence materials | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/openmediavault.md is it. |
| Proactive alerts reach the administrator | OPERATOR | the dashboard renders the conditions and docs/acceptance/store-submission-preflight.md's OpenMediaVault section exercises all four; the hardware run has not happened |

#### Proxmox VE (Tier C, no store (documented workflow), gated by EPIC B's Phase 5)

| Rule | Outcome | Why |
|---|---|---|
| Drift gate: declared architecture support | N/A | Decided by consuming the cross-provider conformance matrix's own verdict for this column rather than by a second check, and that verdict is NOT_APPLICABLE for architecture-parity: this target makes no architecture claim of its own; it names the canonical image, whose architectures the release manifest records. See apps/common/packaging/conformance.json. |
| Store description | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/proxmox.md is it. |
| Store icon | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/proxmox.md is it. |
| Store screenshots | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/proxmox.md is it. |
| Release notes | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/proxmox.md is it. |
| Privacy disclosure | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/proxmox.md is it. |
| Permission rationale | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/proxmox.md is it. |
| Support, source and licence materials | N/A | This target has no store or catalog to submit to, so there is no listing for this asset to appear on. §73's own treatment of Dockge is the shape: a distribution target supported by Compose compatibility rather than by packaging gets a documented workflow instead of a submission bundle, and docs/submission/proxmox.md is it. |
| Proactive alerts reach the administrator | OPERATOR | the dashboard renders the conditions and docs/acceptance/store-submission-preflight.md's Proxmox VE section exercises all four; the hardware run has not happened |

<!-- END GENERATED PREFLIGHT -->
