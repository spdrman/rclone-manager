# Hardware acceptance: provider store and catalog submission preflight

The §82 procedure for the half of Work Package 5.4 that no checkout can decide. Everything
else in that work package is automated in `distribution/packaging` and reported in
`docs/conformance/submission-preflight.md`: the four hard rules, the eight-element adapter
drift gate, the submission materials, and the recorded readiness verdict per target. What
is left is what needs the real platform, and it is the same two things on every target.

## What this procedure decides

1. **That a proactive alert actually reaches the administrator.** §71 defines four
   conditions and the automated suite proves they fire and dispatch. What it cannot prove
   is that a person sitting in front of the platform's own interface finds out. On these
   targets the platform's own notification centre is unsupported at their §4A tier, so the
   administrator's path is this application's own dashboard, and "the dashboard renders
   it" is a claim about a browser, not about a source file.
2. **That the store screenshots exist and are of a real installation.** See
   `docs/submission/screenshots.md` for what to capture. A listing built from the shared
   UI's mock API shows data no administrator will ever see, which is why this is an
   acceptance step rather than a build step.

## What this procedure does not decide

Whether the store accepts the submission. External provider approval is outside this
repository's control (§75), and no procedure here can change that. This makes the
submission ready; a reviewer decides the rest.

## Preconditions, on every target

- The target installed from the package or profile this repository ships, at the version
  `distribution/packaging/canonical.json` pins, not a hand-assembled deployment.
- A real SFTP source with a real backup set, configured through the interface, with at
  least one successful run behind it. A set that has never succeeded cannot go stale.
- `alerts.enabled: true` in the configuration, and the engine restarted since.
- A second machine on the same network with a browser, so the interface is exercised the
  way an administrator reaches it rather than from the NAS's own console.
- Somewhere to record evidence that is not the NAS being tested.

## The four conditions, on every target

Each section below runs all four. They are `core/service/alerts.go`'s own names and the
evidence is recorded against them so two runs on two platforms are comparable.

- `STALE_BACKUP` — stop the source from producing, or move the remote path aside, and wait
  past the set's window. Expect the health summary to show the set stale.
- `REPEATED_FAILURE` — break the source's credentials or its path so runs fail, and let it
  fail enough times to trip the condition rather than once.
- `HOST_KEY_CHANGED` — regenerate the source's SSH host keys. Expect runs to stop and the
  condition to be raised as its own alert rather than as a generic failure. Re-pin
  afterwards, following `docs/recovery-without-a-terminal.md` as an administrator would.
- `CRITICAL_STORAGE_PRESSURE` — fill the volume holding the backup root to the threshold.
  Expect the condition, and expect the application to refuse rather than to half-write.

## Evidence to record, on every target

For each of the four conditions: wall-clock time, the engine's own `alert` log line, and
whether the administrator saw it in the interface without being told to look. For the
screenshots: which build they were taken from, and the file names. For the whole run: the
target's version, the architecture, and accept or reject.

## Accept or reject

Accept a target when all four conditions reached the administrator through the interface
and the screenshots that target's listing needs exist. Reject it otherwise, and record
which condition did not arrive: a partial pass recorded as a pass is how a product ships
with one of its four alerts silently broken.

---

## Generic Docker

**Hardware:** Docker CLI on any Linux host, or the NAS itself
**Deliverable:** `docs/submission/generic.md` (no store (documented workflow))

There is no store to submit to. What this section decides is that the documented workflow in `docs/submission/generic.md` is one an administrator can actually follow, which is the deliverable for a target distributed by Compose compatibility rather than by packaging.

### Generic Docker: install

1. Follow `docs/submission/generic.md` exactly as written, from a machine that is not the NAS, and record
   any step whose wording did not survive contact with the real platform. A workflow
   document that needs interpreting is a defect in the document.
2. Sign in through the enrollment link the engine prints, from the second machine.
3. Configure the real backup set and let it succeed at least once.

### Generic Docker: proactive alerts

Run all four conditions from the list above, in this order, recording the evidence the
preamble asks for:

- `STALE_BACKUP`
- `REPEATED_FAILURE`
- `HOST_KEY_CHANGED`
- `CRITICAL_STORAGE_PRESSURE`

Then answer the question this section exists for, in one sentence: **through which surface
did the administrator find out?** The expected answer on this target is the application's
own dashboard health summary, because the platform's native notification capability is
unsupported at its §4A tier. If the answer is "a log line", that is a reject, and it is the
finding the whole procedure was written to produce.

### Generic Docker: screenshots

No screenshot is needed: this target has no listing. Capture one anyway if the workflow document would be clearer with it.

### Generic Docker: removal

Remove the application through the platform's own mechanism and confirm the retained
artifacts in the backup root are untouched, byte for byte, against the baseline that
target's own acceptance procedure records. Removal that takes backups with it is a reject
regardless of everything above.

---

## Synology DSM

**Hardware:** A DSM 7 unit on amd64 or arm64
**Deliverable:** `docs/submission/synology.md` (Synology Package Center)

Package Center's own review is external. What this section decides is that the package installs, runs, alerts and can be removed without losing retained artifacts, and that the three screenshots the listing needs exist and are of a real installation.

### Synology DSM: install

1. Follow `docs/submission/synology.md` exactly as written, from a machine that is not the NAS, and record
   any step whose wording did not survive contact with the real platform. A workflow
   document that needs interpreting is a defect in the document.
2. Sign in through the enrollment link the engine prints, from the second machine.
3. Configure the real backup set and let it succeed at least once.

### Synology DSM: proactive alerts

Run all four conditions from the list above, in this order, recording the evidence the
preamble asks for:

- `STALE_BACKUP`
- `REPEATED_FAILURE`
- `HOST_KEY_CHANGED`
- `CRITICAL_STORAGE_PRESSURE`

Then answer the question this section exists for, in one sentence: **through which surface
did the administrator find out?** The expected answer on this target is the application's
own dashboard health summary, because the platform's native notification capability is
unsupported at its §4A tier. If the answer is "a log line", that is a reject, and it is the
finding the whole procedure was written to produce.

### Synology DSM: screenshots

Capture the 3 screenshots `docs/submission/screenshots.md` lists for Synology Package Center, from this installation, at this version.

### Synology DSM: removal

Remove the application through the platform's own mechanism and confirm the retained
artifacts in the backup root are untouched, byte for byte, against the baseline that
target's own acceptance procedure records. Removal that takes backups with it is a reject
regardless of everything above.

---

## TrueNAS

**Hardware:** A TrueNAS SCALE 24.10 or later system with a pool
**Deliverable:** `docs/submission/truenas.md` (TrueNAS Apps catalog)

The catalog's own validation and render tooling runs in the iX catalog repository and cannot run on a laptop. What this section decides is that the rendered deployment behaves, and that the three screenshots the catalog entry needs exist.

### TrueNAS: install

1. Follow `docs/submission/truenas.md` exactly as written, from a machine that is not the NAS, and record
   any step whose wording did not survive contact with the real platform. A workflow
   document that needs interpreting is a defect in the document.
2. Sign in through the enrollment link the engine prints, from the second machine.
3. Configure the real backup set and let it succeed at least once.

### TrueNAS: proactive alerts

Run all four conditions from the list above, in this order, recording the evidence the
preamble asks for:

- `STALE_BACKUP`
- `REPEATED_FAILURE`
- `HOST_KEY_CHANGED`
- `CRITICAL_STORAGE_PRESSURE`

Then answer the question this section exists for, in one sentence: **through which surface
did the administrator find out?** The expected answer on this target is the application's
own dashboard health summary, because the platform's native notification capability is
unsupported at its §4A tier. If the answer is "a log line", that is a reject, and it is the
finding the whole procedure was written to produce.

### TrueNAS: screenshots

Capture the 3 screenshots `docs/submission/screenshots.md` lists for TrueNAS Apps catalog, from this installation, at this version.

### TrueNAS: removal

Remove the application through the platform's own mechanism and confirm the retained
artifacts in the backup root are untouched, byte for byte, against the baseline that
target's own acceptance procedure records. Removal that takes backups with it is a reject
regardless of everything above.

---

## Unraid

**Hardware:** An Unraid 6.12 or later server with Docker enabled
**Deliverable:** `docs/submission/unraid.md` (Unraid Community Applications)

Community Applications review is external and is a human reading the template. What this section decides is that both templates install in the order the documentation gives, that the pair works, and that the two screenshots exist.

### Unraid: install

1. Follow `docs/submission/unraid.md` exactly as written, from a machine that is not the NAS, and record
   any step whose wording did not survive contact with the real platform. A workflow
   document that needs interpreting is a defect in the document.
2. Sign in through the enrollment link the engine prints, from the second machine.
3. Configure the real backup set and let it succeed at least once.

### Unraid: proactive alerts

Run all four conditions from the list above, in this order, recording the evidence the
preamble asks for:

- `STALE_BACKUP`
- `REPEATED_FAILURE`
- `HOST_KEY_CHANGED`
- `CRITICAL_STORAGE_PRESSURE`

Then answer the question this section exists for, in one sentence: **through which surface
did the administrator find out?** The expected answer on this target is the application's
own dashboard health summary, because the platform's native notification capability is
unsupported at its §4A tier. If the answer is "a log line", that is a reject, and it is the
finding the whole procedure was written to produce.

### Unraid: screenshots

Capture the 2 screenshots `docs/submission/screenshots.md` lists for Unraid Community Applications, from this installation, at this version.

### Unraid: removal

Remove the application through the platform's own mechanism and confirm the retained
artifacts in the backup root are untouched, byte for byte, against the baseline that
target's own acceptance procedure records. Removal that takes backups with it is a reject
regardless of everything above.

---

## OpenMediaVault

**Hardware:** An OMV 7 system with the Compose plugin
**Deliverable:** `docs/submission/openmediavault.md` (no store (documented workflow))

There is no omv-extras package to submit. What this section decides is that the documented workflow is followable and that the deployment behaves.

### OpenMediaVault: install

1. Follow `docs/submission/openmediavault.md` exactly as written, from a machine that is not the NAS, and record
   any step whose wording did not survive contact with the real platform. A workflow
   document that needs interpreting is a defect in the document.
2. Sign in through the enrollment link the engine prints, from the second machine.
3. Configure the real backup set and let it succeed at least once.

### OpenMediaVault: proactive alerts

Run all four conditions from the list above, in this order, recording the evidence the
preamble asks for:

- `STALE_BACKUP`
- `REPEATED_FAILURE`
- `HOST_KEY_CHANGED`
- `CRITICAL_STORAGE_PRESSURE`

Then answer the question this section exists for, in one sentence: **through which surface
did the administrator find out?** The expected answer on this target is the application's
own dashboard health summary, because the platform's native notification capability is
unsupported at its §4A tier. If the answer is "a log line", that is a reject, and it is the
finding the whole procedure was written to produce.

### OpenMediaVault: screenshots

No screenshot is needed: this target has no listing. Capture one anyway if the workflow document would be clearer with it.

### OpenMediaVault: removal

Remove the application through the platform's own mechanism and confirm the retained
artifacts in the backup root are untouched, byte for byte, against the baseline that
target's own acceptance procedure records. Removal that takes backups with it is a reject
regardless of everything above.

---

## Proxmox VE

**Hardware:** A PVE 8 host with a dedicated container-host guest
**Deliverable:** `docs/submission/proxmox.md` (no store (documented workflow))

Proxmox VE has no third-party application store. What this section decides is that the documented workflow for the dedicated guest is followable and that the deployment behaves.

### Proxmox VE: install

1. Follow `docs/submission/proxmox.md` exactly as written, from a machine that is not the NAS, and record
   any step whose wording did not survive contact with the real platform. A workflow
   document that needs interpreting is a defect in the document.
2. Sign in through the enrollment link the engine prints, from the second machine.
3. Configure the real backup set and let it succeed at least once.

### Proxmox VE: proactive alerts

Run all four conditions from the list above, in this order, recording the evidence the
preamble asks for:

- `STALE_BACKUP`
- `REPEATED_FAILURE`
- `HOST_KEY_CHANGED`
- `CRITICAL_STORAGE_PRESSURE`

Then answer the question this section exists for, in one sentence: **through which surface
did the administrator find out?** The expected answer on this target is the application's
own dashboard health summary, because the platform's native notification capability is
unsupported at its §4A tier. If the answer is "a log line", that is a reject, and it is the
finding the whole procedure was written to produce.

### Proxmox VE: screenshots

No screenshot is needed: this target has no listing. Capture one anyway if the workflow document would be clearer with it.

### Proxmox VE: removal

Remove the application through the platform's own mechanism and confirm the retained
artifacts in the backup root are untouched, byte for byte, against the baseline that
target's own acceptance procedure records. Removal that takes backups with it is a reject
regardless of everything above.

---

## Portainer CE

**Hardware:** A Linux host running a current Portainer CE release, with a second machine on the same LAN
**Deliverable:** `docs/submission/portainer.md` (no store (documented workflow))

Portainer has no vendor-run store to submit to. What this section decides is that the App Template and the stack it points at deploy on a real Portainer CE release, that the form Portainer renders asks for every value the stack needs, and that the deployment inherits none of Portainer's own Docker socket access.

### Portainer CE: install

1. Follow `docs/submission/portainer.md` exactly as written, from a machine that is not the
   Portainer host, and record any step whose wording did not survive contact with the real
   platform. A workflow document that needs interpreting is a defect in the document.
2. Add this repository's `apps/portainer/templates.json` as an App Template URL, deploy the
   entry, and record every field the form asked for. A stack variable the form never offered
   is the defect this step exists to find.
3. Sign in through the enrollment link the engine prints, from the second machine.
4. Configure the real backup set and let it succeed at least once.
5. Inspect the deployed containers and record that neither of them mounts
   `/var/run/docker.sock`, neither is privileged, and neither uses host networking.

### Portainer CE: proactive alerts

Run all four conditions from the list above, in this order, recording the evidence the
preamble asks for:

- `STALE_BACKUP`
- `REPEATED_FAILURE`
- `HOST_KEY_CHANGED`
- `CRITICAL_STORAGE_PRESSURE`

Then answer the question this section exists for, in one sentence: **through which surface
did the administrator find out?** The expected answer on this target is the application's
own dashboard health summary, because the platform's native notification capability is
unsupported at its §4A tier. If the answer is "a log line", that is a reject, and it is the
finding the whole procedure was written to produce.

### Portainer CE: screenshots

No screenshot is needed: this target has no listing. Capture the App Template form anyway if the workflow document would be clearer with one.

### Portainer CE: removal

Remove the application through the platform's own mechanism and confirm the retained
artifacts in the backup root are untouched, byte for byte, against the baseline that
target's own acceptance procedure records. Removal that takes backups with it is a reject
regardless of everything above.


---

## Dockge

**Hardware:** A Linux host running a current Dockge release, with a second machine on the same LAN
**Deliverable:** `docs/submission/dockge.md` (no store (documented workflow))

Dockge ships no stack of its own on purpose: it imports `container/compose.yaml`, the canonical definition, unmodified. What this section decides is that the import and deploy workflow is followable and that the canonical stack really does deploy through Dockge with no fork of it.

### Dockge: install

1. Follow `docs/submission/dockge.md` exactly as written, from a machine that is not the
   Dockge host, and record any step whose wording did not survive contact with the real
   platform. A workflow document that needs interpreting is a defect in the document.
2. Import `container/compose.yaml` into a Dockge stack directory, drop the `build:` block as
   `apps/dockge/README.md` describes, and deploy. Record any edit beyond that one: a second
   required edit is a real incompatibility, and it gets written down here before any code is
   written for it.
3. Sign in through the enrollment link the engine prints, from the second machine.
4. Configure the real backup set and let it succeed at least once.

### Dockge: proactive alerts

Run all four conditions from the list above, in this order, recording the evidence the
preamble asks for:

- `STALE_BACKUP`
- `REPEATED_FAILURE`
- `HOST_KEY_CHANGED`
- `CRITICAL_STORAGE_PRESSURE`

Then answer the question this section exists for, in one sentence: **through which surface
did the administrator find out?** The expected answer on this target is the application's
own dashboard health summary, because the platform's native notification capability is
unsupported at its §4A tier. If the answer is "a log line", that is a reject, and it is the
finding the whole procedure was written to produce.

### Dockge: screenshots

No screenshot is needed: this target has no listing. Capture the Dockge stack view anyway if the workflow document would be clearer with one.

### Dockge: removal

Remove the application through the platform's own mechanism and confirm the retained
artifacts in the backup root are untouched, byte for byte, against the baseline that
target's own acceptance procedure records. Removal that takes backups with it is a reject
regardless of everything above.


---

## CasaOS

**Hardware:** A machine running a current CasaOS release, with a second machine on the same LAN
**Deliverable:** `docs/submission/casaos.md` (CasaOS AppStore)

What this section decides is that the store compose file installs from the CasaOS AppStore, that the install dialogue presents every mount and port the runtime actually uses, and that the app tile opens the web UI rather than the engine.

### CasaOS: install

1. Create the four host directories `apps/casaos/README.md` names, owned by uid 1000 and
   gid 1000, before installing. The runtime image is distroless and cannot create or chown
   them.
2. Install from the store entry and record every field the install dialogue asked for and
   every mount it showed. A mount the dialogue hides is a mount the administrator was never
   told to create.
3. Sign in through the enrollment link the engine prints, from the second machine.
4. Configure the real backup set and let it succeed at least once.
5. Click the app tile and record which container answered. It has to be the web UI.

### CasaOS: proactive alerts

Run all four conditions from the list above, in this order, recording the evidence the
preamble asks for:

- `STALE_BACKUP`
- `REPEATED_FAILURE`
- `HOST_KEY_CHANGED`
- `CRITICAL_STORAGE_PRESSURE`

Then answer the question this section exists for, in one sentence: **through which surface
did the administrator find out?** The expected answer on this target is the application's
own dashboard health summary, because the platform's native notification capability is
unsupported at its §4A tier. If the answer is "a log line", that is a reject, and it is the
finding the whole procedure was written to produce.

### CasaOS: screenshots

Capture the 2 screenshots `docs/submission/screenshots.md` lists for CasaOS AppStore, from this installation, at this version.

### CasaOS: removal

Remove the application through the platform's own mechanism and confirm the retained
artifacts in the backup root are untouched, byte for byte, against the baseline that
target's own acceptance procedure records. Removal that takes backups with it is a reject
regardless of everything above.


---

## ZimaOS

**Hardware:** A machine running a current ZimaOS release, with a second machine on the same LAN
**Deliverable:** `docs/submission/zimaos.md` (ZimaOS app store)

One runtime, a second store registration. This section is the same procedure run again on the other platform, and it is a separate section because the two are submitted and certified separately: a run on the other one is not evidence for this listing.

### ZimaOS: install

1. Create the four host directories `apps/zimaos/README.md` names, owned by uid 1000 and
   gid 1000, before installing. The runtime image is distroless and cannot create or chown
   them.
2. Install from the store entry and record every field the install dialogue asked for and
   every mount it showed. A mount the dialogue hides is a mount the administrator was never
   told to create.
3. Sign in through the enrollment link the engine prints, from the second machine.
4. Configure the real backup set and let it succeed at least once.
5. Click the app tile and record which container answered. It has to be the web UI.

### ZimaOS: proactive alerts

Run all four conditions from the list above, in this order, recording the evidence the
preamble asks for:

- `STALE_BACKUP`
- `REPEATED_FAILURE`
- `HOST_KEY_CHANGED`
- `CRITICAL_STORAGE_PRESSURE`

Then answer the question this section exists for, in one sentence: **through which surface
did the administrator find out?** The expected answer on this target is the application's
own dashboard health summary, because the platform's native notification capability is
unsupported at its §4A tier. If the answer is "a log line", that is a reject, and it is the
finding the whole procedure was written to produce.

### ZimaOS: screenshots

Capture the 2 screenshots `docs/submission/screenshots.md` lists for ZimaOS app store, from this installation, at this version.

### ZimaOS: removal

Remove the application through the platform's own mechanism and confirm the retained
artifacts in the backup root are untouched, byte for byte, against the baseline that
target's own acceptance procedure records. Removal that takes backups with it is a reject
regardless of everything above.


---

## UGOS Pro

**Hardware:** A UGREEN NAS running UGOS Pro
**Deliverable:** `docs/submission/ugreen.md` (UGREEN App Center)

**This section is written and not yet runnable.** EPIC D's #83 has produced no `.UPK`, so there is nothing to install. It is here so that #178 has the procedure waiting rather than writing a second one, and the preflight records every UGREEN row as not yet applicable rather than as a failure. Nothing in this section is EPIC B's to run, and nothing in it can hold Phase 5 open.

### UGOS Pro: install

1. Follow `docs/submission/ugreen.md` exactly as written, from a machine that is not the NAS, and record
   any step whose wording did not survive contact with the real platform. A workflow
   document that needs interpreting is a defect in the document.
2. Sign in through the enrollment link the engine prints, from the second machine.
3. Configure the real backup set and let it succeed at least once.

### UGOS Pro: proactive alerts

Run all four conditions from the list above, in this order, recording the evidence the
preamble asks for:

- `STALE_BACKUP`
- `REPEATED_FAILURE`
- `HOST_KEY_CHANGED`
- `CRITICAL_STORAGE_PRESSURE`

Then answer the question this section exists for, in one sentence: **through which surface
did the administrator find out?** The expected answer on this target is the application's
own dashboard health summary, because the platform's native notification capability is
unsupported at its §4A tier. If the answer is "a log line", that is a reject, and it is the
finding the whole procedure was written to produce.

### UGOS Pro: screenshots

Capture the 4 screenshots `docs/submission/screenshots.md` lists for UGREEN App Center, from this installation, at this version.

### UGOS Pro: removal

Remove the application through the platform's own mechanism and confirm the retained
artifacts in the backup root are untouched, byte for byte, against the baseline that
target's own acceptance procedure records. Removal that takes backups with it is a reject
regardless of everything above.

---
