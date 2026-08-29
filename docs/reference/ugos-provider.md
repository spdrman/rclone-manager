> **This is a reference document, not an active EPIC.**
>
> It was written as a UGOS-only EPIC before the project settled on a
> provider-neutral core with thin platform adapters. That decision lives in
> `docs/EPIC-B-multi-nas.md`, which is the active EPIC and which covers UGOS as
> one provider among six.
>
> I kept this rather than deleting it because it is still the deepest UGOS
> material we have. It carries substantially more detail on UPK packaging,
> `project.yaml`, `ugcli`, the JSSDK bootstrap and App Center readiness than the
> multi-provider spec has room for, and that detail is needed the moment anyone
> actually builds the UGOS app.
>
> Read it as backing material for these EPIC B issues, not as work in its own
> right: B1.2 (developer environment and the minimal UPK proof), B1.3 (UGOS
> authentication and the trusted-proxy boundary), and B4.2 (the UGOS provider
> app and its UPK). Where the two documents disagree, `docs/EPIC-B-multi-nas.md`
> wins, because it is the one the issues are filed against.
>
> Two things in the Status block below were already stale when this landed and I
> have corrected them in place: the repository and the implementation root. The
> rest of the document is unedited.

---

# EPIC: UGOS Pro UI, UPK Packaging, and Headless Docker Distribution for Backup Manager — Adversarial Consensus + Full TDD Revision

## Status

**Type:** Reference material for EPIC B's UGOS provider work  
**Repository:** `spdrman/rclone-manager`  
**Parent / predecessor EPIC:** `Embedded-rclone NAS Backup Lifecycle Manager` (EPIC A, complete)  
**Active EPIC:** `docs/EPIC-B-multi-nas.md`  
**Primary implementation root:** repository root (the `tools/backup-manager/` path this originally assumed was corrected when EPIC A moved into its own repository)  
**Target platform:** UGREEN NAS / UGOS Pro  
**Primary UI distribution:** UGOS Pro Docker Application packaged as `.UPK`  
**Secondary distribution:** headless Docker image/package for terminal operation  
**Initial architectures:** `linux/amd64`, `linux/arm64`

---

# Adversarial Review — Five-Expert Panel

This revision incorporates a deliberately hostile design review. Each reviewer was instructed to reject the EPIC if the design could create data loss, an authentication bypass, misleading backup UX, App Center rejection, or unnecessary maintenance burden.

## Expert 1 — UGOS Platform / App Center Engineer

### Initial verdict: REJECT

Critical findings:

1. The EPIC mixed general UGOS native-app runtime assumptions with Docker-App behavior that is not fully documented inside containers.
2. The proposed `APP_DATA_PATH` installation parameter unnecessarily places sensitive state/SSH credentials in a user-selected filesystem location.
3. `allow_add_access_path`, Docker bind-mount behavior, private app data, update persistence, and uninstall behavior were being specified before hardware proof.
4. The proposed App Center metadata omitted current open-source compliance fields even though the product embeds rclone and other open-source components.
5. Privacy/App Center requirements were treated as late release paperwork rather than part of the product architecture.

Required corrections:

- make private-state mounting a Phase-0 hardware gate;
- do not make `APP_DATA_PATH` a normal user-facing install parameter;
- keep user backup data and private app state separate;
- add mandatory privacy, license, source-code/source-offer, support, and third-party notice work;
- freeze `app_id` only after package PoC succeeds;
- treat minimum firmware/Docker versions as release-derived facts, not copied example values.

### Consensus position: APPROVE AFTER REVISION

---

## Expert 2 — Application Security / Threat Modeling Engineer

### Initial verdict: REJECT

Critical findings:

1. UGOS identity is delivered through the system gateway. Trusting identity headers on a directly reachable Docker port would create an authorization bypass.
2. The EPIC recognized this risk but did not define a fail-closed trusted-proxy model.
3. Native browser `EventSource` cannot set the required custom `Ugreen-Ttk` authentication header. The proposed SSE design therefore conflicts with the documented UGOS authentication flow.
4. SSH private keys had filesystem-permission requirements but no strong separation from user-accessible backup data.
5. Key deletion lacked referential-integrity rules.
6. The API did not explicitly prevent request disconnects from cancelling server operations at unsafe boundaries.

Required corrections:

- accept UGOS identity headers only from a verified trusted gateway source;
- strip/ignore forwarded identity headers from all untrusted peers;
- prohibit release if the Docker service cannot be isolated or the proxy source cannot be authenticated;
- use authenticated polling for v1 operation progress; optionally evaluate `fetch()` streaming later;
- never place authentication tokens in query strings;
- store SSH keys only in private application state;
- reject deletion of keys referenced by active backup sets;
- separate HTTP request lifetime from durable backup-operation lifetime.

### Consensus position: APPROVE AFTER REVISION

---

## Expert 3 — Distributed Systems / Backup Reliability Engineer

### Initial verdict: REJECT

Critical findings:

1. Long-running API operations were only described as "durable or queryable"; that is insufficient for a backup system.
2. Retention preview followed by a fresh recalculation at apply-time can delete a different set of files than the user actually confirmed.
3. Configuration can change while a transfer/retention operation is running unless an immutable configuration revision is captured.
4. Duplicate POST requests can start duplicate jobs unless idempotency is explicit.
5. Losing SQLite/app state after uninstall would destroy catalog, validation, and configuration knowledge even though backup artifacts survive.
6. Schema migration rollback rules were not strong enough for App Center update/rollback behavior.

Required corrections:

- persist all administrative/long-running operations in SQLite;
- use explicit idempotency keys;
- snapshot config revision at operation creation;
- make retention preview return an immutable `plan_id` bound to an inventory revision/hash;
- apply exactly the confirmed plan or reject it as stale;
- implement optimistic concurrency for configuration changes;
- define catalog-rebuild/disaster-recovery behavior independent of SQLite survival;
- persist non-secret per-artifact recovery metadata or provide an equivalent reconstructable catalog mechanism;
- require migration compatibility gates and a pre-migration state snapshot.

### Consensus position: APPROVE AFTER REVISION

---

## Expert 4 — Backup Product / UX / Operations Engineer

### Initial verdict: REJECT

Critical findings:

1. A twelve-step wizard is unnecessarily long and increases setup failure.
2. "Restore Points" suggests restore functionality, but restore execution is explicitly out of scope.
3. The most consequential behavior—deleting the remote source after safe ingestion—was not prominent enough in onboarding and configuration.
4. A backup manager that only shows failures when someone opens the UI is operationally weak.
5. Stable-size completion detection was presented alongside producer atomic rename/manifest as if they provided equivalent assurance.
6. The UI did not provide a clear reinstall/recovery path when application state is lost but backup files remain.

Required corrections:

- collapse onboarding into approximately six grouped steps;
- use **Backups** / **Retained Backups** in the UI while retaining "restore point" as an internal retention concept;
- explicitly disclose remote-source deletion behavior before first activation;
- label stable-size completion as a heuristic/advanced mode with stronger safeguards;
- require at least one proactive alert mechanism before App Center 1.0 release, using UGOS notification capability if officially available or a documented configurable alternative;
- add a catalog/recovery workflow for existing retained backup files.

### Consensus position: APPROVE AFTER REVISION

---

## Expert 5 — Release Engineering / Supply Chain / Maintainability Engineer

### Initial verdict: REJECT

Critical findings:

1. Separate `backup-manager-ugos` and `backup-manager-cli` images create needless artifact drift when the design already embeds the UI in the same Go binary.
2. Four image builds (UGOS/CLI × amd64/arm64) double the release surface without adding lifecycle isolation.
3. "Same core version" is weaker than using the exact same executable/image digest.
4. Upgrade rollback could fail if an older binary sees a newer schema.
5. Open-source notices, license inventory, reproducible-build metadata, and source-code/source-offer requirements were under-specified.

Required corrections:

- build one canonical multi-architecture container image;
- publish it as the headless Docker package and bundle the exact same image in the UPK;
- vary command/auth/presentation by packaging profile, not by compiling another product image;
- record and verify image digest in the UPK release manifest;
- make schema compatibility explicit and fail closed on unsupported downgrade;
- generate third-party notices and license inventory as release artifacts.

### Consensus position: APPROVE AFTER REVISION

---

# Five-Expert Consensus

All five reviewers agree on the following mandatory architecture:

> **Build one canonical Go binary and one canonical versioned container image per CPU architecture. Publish that image as the headless Docker distribution and bundle the exact same image in the UGOS `.UPK`. The UPK changes runtime command, UGOS authentication, and presentation—not the backup engine.**

They further agree that this EPIC is not implementation-ready unless all of the following are enforced:

1. UGOS gateway authentication is fail-closed and cannot be spoofed from the LAN.
2. Native `EventSource` is not used for authenticated v1 progress because the documented UGOS flow requires a custom request header.
3. Administrative jobs are durable and survive browser disconnects/restarts.
4. Retention applies the exact plan the administrator confirmed; stale plans are rejected.
5. Configuration uses revisions/optimistic concurrency and each operation snapshots its configuration.
6. Private state/SSH credentials are separated from the user backup root.
7. Backup catalog recovery is possible if SQLite/app state is lost.
8. Remote-source deletion is disclosed prominently in the product UX.
9. Stable-size completion is treated as a weaker heuristic, not equivalent to atomic producer completion.
10. App Center OSS/privacy obligations are implemented before submission, not added afterward.
11. App Center 1.0 includes a proactive failure/staleness alert path.
12. Schema migration/downgrade behavior is explicitly safe.
13. UPK and terminal Docker distributions use the same image digest for a given architecture/release.


# 1. Purpose

Build the UGOS Pro application experience for the backup lifecycle manager defined by the predecessor EPIC.

The predecessor EPIC established the core architectural boundary:

- Go application;
- embedded rclone Go packages;
- SQLite lifecycle journal;
- explicit copy → verify → durable commit → delete sequencing;
- deterministic GFS retention;
- validation/quarantine;
- backup freshness/health;
- a presentation-agnostic `BackupService` application layer;
- CLI/daemon operation independent of any future UI.

This EPIC adds the **presentation, API, UGOS integration, packaging, and distribution layers** while preserving the predecessor's safety model.

The completed project SHALL produce **one canonical runtime artifact with two distribution envelopes**:

1. **UGOS Pro App**
   - graphical management UI;
   - packaged as a UGREEN `.UPK`;
   - implemented as a UGOS **Docker Application**;
   - bundles the canonical versioned container image;
   - starts that image in UGOS-authenticated UI/API/daemon mode;
   - intended to become suitable for later submission to the official UGREEN App Center.

2. **Headless Docker / Terminal Package**
   - publishes the **same canonical container image** to an OCI registry;
   - no UGOS dependency when started in headless mode;
   - terminal-first;
   - deployable with `docker run` or Docker Compose;
   - exposes the existing CLI/daemon behavior.

For a given version and CPU architecture, the UPK and OCI distribution MUST use the **same container image digest**, not merely binaries that claim the same semantic version.

---

# 2. Core Product Principle

```text
                         SAME CORE
                            │
                    ┌───────┴────────┐
                    │ BackupService │
                    └───────┬────────┘
                            │
          ┌─────────────────┼──────────────────┐
          │                 │                  │
          ▼                 ▼                  ▼
        CLI/daemon       HTTP API          UGOS Web UI
          │                 │                  │
          │                 └────────┬─────────┘
          │                          │
          ▼                          ▼
 Canonical OCI Image          UGOS Docker App
                                  │
                                  ▼
                         same image → .UPK
```

The UGOS UI is a **presentation adapter** to the existing application service.

The UI MUST NOT directly:

- invoke rclone;
- read or mutate SQLite;
- delete backup files;
- execute retention;
- modify lifecycle state;
- manipulate remote SSH/SFTP data;
- infer backup safety from raw filesystem state.

All such operations MUST remain owned by `BackupService` and the predecessor EPIC's lifecycle engine.

---

# 3. Architectural Decision: UGOS Docker App UPK

## 3.1 Decision

The UGOS edition SHALL initially be implemented as a **UGOS Pro Docker Application packaged with UGREEN's `ugcli` tooling**.

Do not create a separate native UGOS backend unless a later architecture review demonstrates a concrete requirement that cannot be met by the Docker Application model.

## 3.2 Why Docker App UPK

This model provides the strongest alignment between:

- the existing containerized backup-manager deployment;
- the requested terminal Docker package;
- amd64/arm64 release builds;
- reproducible builds;
- controlled dependency versions;
- App Center packaging;
- independent development/testing outside UGOS.

It also avoids maintaining:

```text
native-UGOS backend
        +
Docker backend
```

as two divergent operational environments.

## 3.3 UPK is a packaging layer, not a fork

The UPK SHALL wrap an architecture-specific, version-pinned Docker image plus UGOS metadata and Compose configuration.

Conceptually:

```text
UGOS .UPK
│
├── project.yaml
├── icon.png
├── docker-compose.yaml
│
└── bundled image tar
      │
      └── backup-manager:<exact-version>
            │
            ├── Go backup-manager
            ├── embedded rclone
            ├── SQLite support
            ├── HTTP API
            └── embedded Web UI
```

The UPK edition MUST NOT download a newer application image at startup.

Updates to the UPK edition SHALL occur by building and installing a newer reviewed/versioned UPK.

---

# 4. Verified UGOS Pro Platform Constraints

The implementation SHALL follow the current UGREEN developer platform rather than relying on reverse-engineered packaging.

Current UGOS Pro developer constraints to design around include:

- UGOS Pro supports `amd64` and `arm64`.
- UGREEN recommends a Debian 12 development environment for backend/package work.
- UGREEN provides `ugcli` to create and package applications.
- `project.yaml` currently uses specification version `2.1`.
- Docker applications set `is_docker_app: true`.
- Docker applications declare a minimum Docker package dependency.
- Docker application images are bundled as architecture-specific Docker image tar files.
- Docker image tags in the UPK package MUST be explicit/versioned rather than `latest`.
- Docker application `rootfs_common` is limited to the package icon and `docker-compose.yaml`.
- `ugcli pack --build <n>` produces architecture-specific `.upk` artifacts.
- UGOS applications may open with `open_type: inner` inside the UGOS desktop.
- `inner` mode supports UGREEN's frontend JSSDK and UGOS user authentication integration.
- Docker application install parameters may be declared through `project.yaml` and injected into Compose.
- UGREEN discourages/flags privileged containers, `cap_add`, host PID/IPC, and host networking.
- Manual testing of developer UPKs requires a developer-authorized UGREEN NAS.
- App Center publication requires developer identity/review compliance.
- Code/function updates intended for App Center distribution must go through the reviewed app update path rather than an unreviewed self-updater.

Official documentation references are included at the end of this EPIC.

---


# 4A. Mandatory Test-Driven Development Contract

This EPIC SHALL be implemented using **strict test-driven development (TDD)** for all production behavior that can be tested before implementation.

Testing is not a later phase. Tests are part of every implementation step.

The required cycle is:

```text
SPECIFY
  ↓
RED
  ↓
GREEN
  ↓
REFACTOR
  ↓
INTEGRATE
  ↓
REGRESSION
  ↓
ACCEPT
```

## 4A.1 SPECIFY — behavioral contract first

Before production implementation, define:

- behavior;
- inputs;
- outputs;
- invariants;
- failure modes;
- security boundary;
- persistence/restart behavior where relevant;
- acceptance examples.

For destructive or security-sensitive behavior, use specification-by-example.

Example:

```text
GIVEN retention plan P selects artifacts A and B for deletion
AND inventory/configuration still match the plan revision
WHEN administrator applies plan P
THEN only A and B may be deleted
AND no newly discovered artifact may be substituted into the deletion set
```

## 4A.2 RED — failing test first

Before implementation:

1. add the smallest meaningful automated test that describes the desired behavior;
2. run it;
3. verify it fails for the expected reason;
4. record the failure in development output/PR notes where useful.

A test that passes before the intended implementation exists does not satisfy RED unless it is proving an already-existing prerequisite.

## 4A.3 GREEN — minimum implementation

Implement only enough production code to satisfy the new behavior while preserving existing tests.

Do not use GREEN as permission to add unrelated functionality.

## 4A.4 REFACTOR

After tests pass:

- simplify;
- improve naming;
- remove duplication;
- improve boundaries;
- preserve behavior.

All focused tests remain green throughout refactor.

## 4A.5 INTEGRATE

Where behavior crosses a boundary, add the appropriate higher-level test before the step is complete.

Boundary examples:

- HTTP ↔ `BackupService`;
- UGOS gateway ↔ backend auth;
- backend ↔ SQLite;
- backend ↔ filesystem;
- rclone ↔ SFTP server;
- UI ↔ API;
- container ↔ bind mount;
- UPK ↔ UGOS hardware.

## 4A.6 REGRESSION

Before completing the step:

- run focused tests;
- run the owning package/module suite;
- run affected integration tests;
- run security/destructive-safety tests when applicable;
- run the broader regression suite required by the change.

## 4A.7 ACCEPT

A step is complete only when:

- behavior is implemented;
- required tests were written first;
- tests fail before implementation where applicable;
- tests pass after implementation;
- integration coverage exists for crossed boundaries;
- acceptance criteria are satisfied;
- no regression suite failure remains.

---

# 4B. TDD Engineering Invariants

These are mandatory.

1. **No new production behavior without a failing behavioral test first where automation is feasible.**
2. **Every bug fix begins with a regression test reproducing the defect.**
3. **Every destructive behavior requires both positive and negative safety tests.**
4. **Every authorization rule requires allow and deny tests.**
5. **Every API endpoint requires request/response/error/auth tests before handler implementation.**
6. **Every database migration requires forward-migration and failure/compatibility tests before migration code.**
7. **Every state-machine transition requires valid-transition and invalid-transition tests.**
8. **Every filesystem deletion path requires path-containment/symlink/traversal tests before deletion implementation.**
9. **Every remote deletion rule requires identity-change/TOCTOU refusal tests before successful deletion tests.**
10. **Every retention rule requires deterministic examples before implementation.**
11. **Every UI administrative workflow requires component/behavior tests before the final interaction logic.**
12. **Every packaging rule that can be machine-verified SHALL have artifact validation tests.**
13. **Hardware-only UGOS behavior SHALL have an explicit prewritten acceptance test/checklist before manual execution.**
14. **Tests SHALL NOT be deleted or weakened merely to make implementation pass unless the specification itself changes and the change is reviewed.**
15. **A child issue is not complete until its TDD evidence and acceptance tests are complete.**

---

# 4C. Test Taxonomy

Use the smallest test level that proves behavior, then add boundary coverage where necessary.

## Unit

For:

- pure retention logic;
- state transitions;
- DTO validation;
- error mapping;
- path normalization;
- policy calculations;
- capability decisions.

## Component / frontend

For:

- form behavior;
- validation;
- disabled states;
- confirmation flows;
- error rendering;
- health state rendering;
- retention plan display;
- operation polling.

## Service/API contract

For:

- authorization;
- idempotency;
- optimistic concurrency;
- operation creation;
- typed errors;
- retention-plan handling;
- key management;
- catalog rebuild.

## Integration

For:

- SQLite;
- filesystem;
- SFTP;
- rclone adapter;
- HTTP middleware;
- migration;
- container mounts.

## Browser E2E

For:

- UGOS UI workflows;
- setup wizard;
- operation progress;
- configuration edits;
- retention preview/apply;
- quarantine;
- recovery workflows.

## Hardware acceptance

For:

- `ugcli`;
- UPK install/update/uninstall;
- UGOS gateway authentication;
- `open_type: inner`;
- private state path;
- App Center/desktop behavior;
- architecture certification.

---

# 4D. Coverage Policy

Coverage percentage is not a substitute for behavioral quality.

However:

- core pure-logic packages SHOULD target high branch coverage;
- safety-critical lifecycle/retention/path/auth code SHOULD have near-complete branch coverage where practical;
- destructive paths MUST have explicit scenario coverage;
- changed production code without relevant test coverage SHALL fail review.

CI SHOULD publish coverage reports and MAY enforce package-specific thresholds once a stable baseline is established.

---

# 4E. Required TDD Evidence in Pull Requests / Agent Work

Every implementation PR or coding-agent task SHALL report:

```text
Behavior implemented:
Test(s) written first:
Observed RED failure:
Implementation added:
Focused tests:
Integration tests:
Regression tests:
Manual UGOS test (if applicable):
Acceptance criteria satisfied:
```

If a RED step is impossible because behavior depends exclusively on unavailable UGOS hardware, the issue SHALL contain:

- prewritten hardware test procedure;
- expected result;
- failure criteria;
- evidence recorded after execution.


# 5. Product Deliverables

## D-1 — Canonical Application Image

Produce:

```text
backup-manager:<version>
```

or equivalent repository-qualified image.

Purpose:

- run the backup daemon;
- expose all CLI commands;
- optionally serve the backend HTTP API;
- contain/serve the compiled web UI when `serve` mode is enabled;
- support UGOS authentication mode only when explicitly started in that mode;
- expose health endpoints.

The image SHALL be version-pinned and identified by OCI digest. The exact architecture-specific image digest bundled into an UPK SHALL match the digest published for the corresponding OCI release.

## D-2 — UGOS UPK Packages

Produce architecture-specific packages:

```text
amd64_<app-id>_<version>.upk
arm64_<app-id>_<version>.upk
```

Exact file names are generated by `ugcli`.

## D-3 — Headless Docker Distribution Profile

Publish the canonical image as the headless Docker package:

```text
<registry>/iasbuilt/backup-manager:<version>
```

The headless distribution SHALL:

- contain the same backup engine version as the UGOS release;
- not require UGOS;
- not require a web browser;
- default to terminal/headless workflows;
- support `docker run` and Docker Compose;
- be suitable for UGREEN Docker, ordinary Linux Docker hosts, and other NAS/container environments.

## D-4 — Source-level UI

Produce a React/TypeScript frontend under the same tool source tree.

## D-5 — API

Produce a versioned local HTTP API that adapts `BackupService` for the UGOS UI.

## D-6 — Compliance and Recovery Artifacts

Produce:

- third-party/open-source notices;
- license inventory;
- privacy policy integration;
- App Center `source_code_link` / source-offer destination appropriate to the project's licensing model;
- technical support link;
- disaster-recovery/catalog-rebuild documentation;
- release provenance.

## D-7 — Packaging/Release Automation

Produce repeatable build steps for:

- frontend;
- Go application;
- amd64/arm64 images;
- canonical OCI image for amd64/arm64;
- Docker image tar export;
- `.UPK` generation;
- checksums;
- SBOMs;
- release metadata.

---

# 6. Proposed Product Naming

The implementation SHALL define these values centrally.

Proposed values:

```text
Display name: Backup Manager
App ID:       com.iasbuilt.backupmanager
Category:     backup
```

`com.iasbuilt.backupmanager` is a proposed identifier and MUST be confirmed before the first externally distributed or App Center-submitted package because the UGOS application ID is intended to remain stable.

Do not derive runtime filesystem paths from the human-readable display name.

---

# 7. Repository Structure

Extend the predecessor layout approximately as follows:

```text
tools/
  backup-manager/
    README.md
    go.mod
    go.sum

    cmd/
      backup-manager/

    internal/
      app/
      config/
      model/
      discovery/
      lifecycle/
      retention/
      validation/
      state/
      health/

      transport/
        rclone/

      httpapi/
        server.go
        routes.go
        middleware/
        dto/
        errors/
        events/

      auth/
        auth.go
        ugos/
        standalone/

    migrations/

    ui/
      package.json
      pnpm-lock.yaml
      vite.config.ts
      src/
        app/
        api/
        auth/
        components/
        features/
        pages/
        hooks/
        types/
      public/
      tests/

    packaging/
      docker/
        Dockerfile
        Dockerfile.cli
        compose.example.yaml

      ugos/
        project.yaml
        rootfs_common/
          icon.png
          docker-compose.yaml
        rootfs_amd64/
          images/
        rootfs_arm64/
          images/

    scripts/
      build-ui.sh
      build-images.sh
      stage-upk.sh
      pack-upk.sh
      verify-release.sh

    tests/
      integration/
      e2e/
      ugos/

    docs/
      api.md
      ugos-development.md
      docker.md
      app-center-release.md
```

Exact layout MAY vary, but the separation between:

```text
core
API
UI
UGOS packaging
Docker packaging
```

MUST remain explicit.

---

# 8. Single Runtime / Dual Distribution Rule

For each release and architecture there SHALL be exactly one production backup-manager executable and one canonical container image.

The UGOS and terminal distributions SHALL therefore share:

- the exact executable;
- lifecycle logic;
- SQLite schema;
- migrations;
- configuration model;
- rclone adapter;
- retention engine;
- validators;
- reconciliation logic;
- embedded frontend assets;
- semantic version;
- OCI image digest;
- safety invariants.

They differ only through runtime/profile configuration:

```text
UGOS profile
  command: serve --with-daemon --auth-mode=ugos

Headless profile
  command: daemon
  no externally enabled HTTP/UI unless operator explicitly enables it
```

A UI-only feature that belongs in `BackupService` SHALL be rejected in code review.

---

# 9. Runtime Modes

The Go executable SHALL support at minimum:

```bash
backup-manager run
backup-manager daemon
backup-manager serve
backup-manager status
backup-manager check
backup-manager retention --dry-run
backup-manager retention
backup-manager reconcile
backup-manager validate <artifact-id>
backup-manager version
```

## 9.1 Headless Docker default

The headless Docker distribution SHOULD default to:

```bash
backup-manager daemon
```

Users SHALL be able to override the command, for example:

```bash
docker run --rm ... backup-manager check
```

## 9.2 UGOS Docker App default

The UPK Compose profile SHALL run the canonical image in a combined supervised mode such as:

```bash
backup-manager serve --with-daemon --auth-mode=ugos
```

Exact command naming may vary.

The HTTP server and background scheduler SHALL share a common application service and process shutdown context.

Failure of the HTTP listener MUST NOT bypass lifecycle safety.

---

# 10. Frontend Technology

The UGOS frontend SHALL use:

- React;
- TypeScript;
- Vite;
- the current supported `@ugreen-nas/core`;
- the current supported `@ugreen-nas/builder-open` where appropriate.

The team SHOULD begin from UGREEN's official React sample rather than reverse engineering UGOS desktop behavior.

The UI SHALL use `open_type: inner`.

Reason:

- native UGOS desktop-window experience;
- JSSDK support;
- UGOS login/session integration;
- no separate backup-manager password database.

The application SHOULD initially support the UGOS `pc` client target.

Mobile-specific UI support is a future enhancement unless it can be delivered with negligible incremental complexity.

---

# 11. UI Build and Embedding

The frontend SHALL compile to static assets.

Preferred production packaging:

```text
React/Vite build
      │
      ▼
static assets
      │
      ▼
Go //go:embed
      │
      ▼
single backup-manager executable
```

The production UGOS container SHALL NOT require Node.js.

Node/pnpm are build-time dependencies only.

The backend SHALL serve:

```text
/                         SPA
/assets/...                versioned static assets
/api/v1/...                API
/health/live
/health/ready
```

The production SPA SHALL use hashed assets and immutable caching for hashed files.

`index.html` SHALL avoid long-lived caching so an UPK update does not leave the client on an obsolete application shell.

---

# 12. UGOS Window Integration

The application SHALL open inside the UGOS desktop with:

```yaml
open_type: inner
```

The frontend SHOULD use UGREEN window configuration tooling to define sensible defaults such as:

```text
default width:   ~1200 px
default height:  ~800 px
min width:       ~900 px
min height:      ~600 px
resizable:       true
```

Exact values SHALL be verified on actual UGOS hardware/client versions.

The UI SHALL remain usable if the user resizes the application window.

Do not design the UI as a fixed desktop-only canvas.

---

# 13. UGOS Authentication

## 13.1 Principle

The UPK edition SHALL use the authenticated UGOS user session.

It SHALL NOT create its own username/password database for normal UGOS use.

## 13.2 Frontend

The frontend SHALL:

1. initialize the UGREEN frontend SDK;
2. obtain the current third-party application authentication token using the documented UGOS capability;
3. send the required UGOS token header on backend API requests;
4. handle session expiration gracefully.

## 13.3 Backend / Trusted Proxy Boundary

The backend SHALL obtain authenticated user identity only through the documented UGOS gateway/authentication mechanism.

The backend MUST NOT trust UGOS identity headers merely because they are present.

It SHALL:

1. determine the verified network source used by the UGOS gateway on real hardware;
2. define a narrow trusted-proxy allowlist;
3. ignore/strip UGOS identity headers received from any other peer;
4. reject UGOS-authenticated API requests that did not traverse the trusted gateway;
5. log rejected proxy-spoof attempts without logging authentication tokens.

The trusted-proxy source MUST be derived experimentally/documented during Phase 0 and MUST NOT be guessed from a generic Docker bridge address.

## 13.4 Admin-only initial release

Backup management includes destructive operations.

The first UPK release SHOULD be admin-only.

Set the UGOS package configuration to administrator-only access where supported.

The backend SHALL independently enforce authorization for destructive methods; hiding buttons in the UI is not authorization.

## 13.5 Direct-port bypass security gate

UGOS authentication depends on the system gateway.

Therefore Phase 0 MUST determine whether the Docker application service can bind/publish its backend only to loopback or another host-only path while remaining accessible through the UGOS gateway.

Preferred:

```text
127.0.0.1:<UGOS-port> → container:<app-port>
```

If the UGOS Docker App cannot operate correctly with host-loopback-only publication, the implementation MAY expose the mapped port only if the backend can cryptographically or topologically distinguish the documented UGOS gateway request path from direct LAN traffic.

At minimum, direct-LAN requests MUST NOT be able to inject trusted UGOS user headers.

If no robust trusted-proxy boundary can be proven on real hardware, **the Docker-App/UGOS-auth design is rejected for destructive APIs and the architecture must be reconsidered**.

This is a release-blocking security gate.

---

# 14. HTTP API Architecture

The HTTP layer SHALL be a thin adapter over `BackupService`.

Use versioned JSON endpoints:

```text
/api/v1/
```

The API SHALL NOT expose rclone-native types or SQLite schema structures.

Long-running and destructive operations SHALL be persisted in SQLite before execution and SHALL return a durable operation/job identifier.

HTTP request lifetime SHALL NOT own operation lifetime. A browser disconnect, page close, or gateway timeout MUST NOT implicitly cancel a backup, retention, reconciliation, or validation job.

Mutating POST requests SHALL support an idempotency key so client retries cannot create duplicate work.

Each operation SHALL snapshot:

- authenticated actor;
- backup-set identifier;
- configuration revision;
- requested action;
- safety-relevant parameters;
- creation timestamp.

Example:

```json
{
  "operation_id": "op_...",
  "status": "queued"
}
```

The v1 frontend SHALL observe long-running operation state through authenticated polling.

Native `EventSource` SHALL NOT be used because the documented UGOS authentication flow requires the custom `Ugreen-Ttk` header and native `EventSource` cannot reliably attach arbitrary authentication headers.

A future streaming implementation MAY use authenticated `fetch()` response streaming only after UGOS gateway buffering/streaming behavior is validated.

Authentication tokens MUST NEVER be placed in URLs/query strings.

WebSockets are not required for v1.

---

# 15. Required API Surface

Exact route names may vary, but the following capability surface SHALL exist.

## 15.1 System

```text
GET  /api/v1/system/status
GET  /api/v1/system/version
GET  /api/v1/system/capabilities
GET  /health/live
GET  /health/ready
```

## 15.2 Sources / Backup Sets

```text
GET    /api/v1/backup-sets
POST   /api/v1/backup-sets
GET    /api/v1/backup-sets/{id}
PUT    /api/v1/backup-sets/{id}
DELETE /api/v1/backup-sets/{id}
```

Deleting a backup-set configuration MUST NOT delete retained backup files by default.

## 15.3 Connection setup

```text
POST /api/v1/connections/test
POST /api/v1/ssh/keys
GET  /api/v1/ssh/keys
DELETE /api/v1/ssh/keys/{id}

POST /api/v1/ssh/host-keys/probe
POST /api/v1/ssh/host-keys/trust
GET  /api/v1/ssh/host-keys
```

## 15.4 Backup execution

```text
POST /api/v1/backup-sets/{id}/run
POST /api/v1/backup-sets/{id}/reconcile
```

## 15.5 Artifacts

```text
GET  /api/v1/artifacts
GET  /api/v1/artifacts/{id}
POST /api/v1/artifacts/{id}/validate
```

## 15.6 Retention

```text
GET  /api/v1/backup-sets/{id}/retention/preview
PUT  /api/v1/backup-sets/{id}/retention/policy
POST /api/v1/backup-sets/{id}/retention/apply
```

Retention preview MUST be non-destructive.

A preview response SHALL include:

```json
{
  "plan_id": "retplan_...",
  "inventory_revision": "...",
  "config_revision": 42,
  "expires_at": "...",
  "keep_count": 31,
  "delete_count": 4,
  "reclaim_bytes": 123456789
}
```

`POST .../retention/apply` MUST require the `plan_id` the administrator actually reviewed.

The server MUST apply **exactly that plan** only if its inventory/config preconditions remain valid. If backups or policy changed, return a conflict such as `RETENTION_PLAN_STALE` and require a new preview/confirmation.

The server MUST NOT silently recalculate a different deletion set after the administrator has confirmed an older preview.

## 15.7 Operations / Activity

```text
GET /api/v1/operations
GET /api/v1/operations/{id}
```

Operation progress is obtained through authenticated polling in v1.

## 15.8 Quarantine

```text
GET  /api/v1/quarantine
GET  /api/v1/quarantine/{artifact-id}
POST /api/v1/quarantine/{artifact-id}/revalidate
```

The first release SHOULD NOT expose a "force delete remote source anyway" action for quarantined artifacts.

---

# 16. API Safety Requirements

All mutating API operations SHALL:

- authenticate the UGOS user;
- authorize the operation;
- validate request structure;
- call `BackupService`;
- create a correlation/operation ID;
- emit an audit/activity event;
- return typed errors;
- remain idempotent where practical;
- enforce configuration revision/precondition checks;
- persist the operation before returning success for queued work.

Destructive operations SHALL require explicit intent.

Examples:

- retention application must be preceded by a current preview or server-side recalculation;
- deleting a backup-set configuration must not delete stored backup data;
- trusting a new SSH host key must display the fingerprint to the user first;
- changed host keys must never be silently accepted;
- remote backup deletion remains controlled only by lifecycle state, never by a UI "delete source" shortcut.

The API MUST NOT expose a generic endpoint equivalent to:

```text
DELETE arbitrary remote path
```

---

# 17. Browser/API Security

The production UGOS UI/API SHALL:

- disable permissive CORS by default;
- restrict accepted Origins/Hosts to expected UGOS access paths where practical;
- require UGOS authentication on `/api/`;
- sanitize all user-supplied display strings;
- avoid injecting filenames into HTML;
- never return private SSH key contents after storage;
- never return secrets in diagnostic exports;
- enforce request-size limits;
- use safe security headers compatible with UGOS embedding;
- use Content Security Policy where compatible with UGREEN JSSDK requirements;
- rate-limit high-risk operations where appropriate.

CSRF assumptions SHALL be documented and tested for the UGOS token/gateway model.

---

# 18. SSH Credential UX

## 18.1 Key generation

The UI SHOULD allow an administrator to generate an SSH key pair for a backup source.

Preferred algorithm:

```text
Ed25519
```

unless the target remote environment requires another supported algorithm.

The UI SHALL prominently display/copy the **public key** for installation on the remote server.

The private key SHALL remain in private app-controlled state storage, separate from the user-selected backup root.

The browser SHALL receive only a key identifier, public key, fingerprint, algorithm, and non-secret metadata after generation.

## 18.2 Import

The UI MAY support importing an existing private key through:

- secure file upload; or
- paste/import workflow.

Imported private key content MUST:

- be handled only by the backend;
- be stored only in private application state;
- use restrictive permissions;
- never be echoed back after successful import;
- never appear in logs;
- never be exported through ordinary config export.

Deleting a managed key SHALL be rejected while any enabled backup set references it unless the administrator first reassigns/disables those backup sets.

## 18.3 Host-key trust

First connection flow SHALL NOT silently accept the server.

Workflow:

```text
probe host
   ↓
show host + algorithm + fingerprint
   ↓
administrator verifies fingerprint
   ↓
explicit Trust action
   ↓
persist known host
```

A changed host key SHALL block backup operations and place the source in a visible security/error state.

---

# 19. Private State, Secrets, and Catalog Recovery

Private application state and user backup data SHALL be separate security domains.

## 19.1 Private application state

Private state includes:

- SQLite;
- SSH private keys;
- trusted host keys;
- source configuration;
- audit/operation state;
- validation metadata.

Preferred container path:

```text
/var/lib/backup-manager/
```

The UPK SHALL mount this path from a **UGOS-owned private writable application location** proven in Phase 0.

Do NOT make a user-selected shared folder the default location for SSH keys/state merely to survive uninstall.

If the Docker-App model does not expose a suitable documented private writable location, Phase 0 SHALL select and document the least-risk supported fallback before implementation.

## 19.2 User backup root

Retained backup artifacts SHALL live in a separately authorized user storage location mounted at:

```text
/data/backups
```

The backup root MUST NOT contain SSH private keys or authentication state.

## 19.3 State-loss recovery

Backup artifacts MUST remain useful if the application is uninstalled or SQLite is lost.

Before App Center 1.0, the core SHALL support a catalog reconstruction mechanism using non-secret durable metadata stored with/in the backup tree, for example sidecar manifests or an equivalent design.

Recovery metadata SHOULD preserve enough information to reconstruct safely:

- artifact ID;
- backup-set stable ID/name;
- producer timestamp;
- received timestamp;
- size;
- checksum(s);
- validation result summary;
- retention-relevant timestamp;
- backup-manager format version.

Recovery metadata MUST NOT contain:

- SSH private keys;
- authentication tokens;
- remote passwords;
- secret environment values.

Provide a dry-run recovery command such as:

```bash
backup-manager catalog rebuild --dry-run
backup-manager catalog rebuild
```

Reconstruction MUST NOT delete remote or local backup files.

---

# 20. UGOS Installation Parameters

The Docker App package SHOULD expose only installation-time parameters necessary to give the container durable storage access.

Proposed user-visible parameters:

```text
BACKUP_ROOT
LOG_LEVEL
```

The mechanism used for `BACKUP_ROOT` (a `path` parameter vs `allow_add_access_path`) SHALL be selected only after Phase-0 hardware validation.

## 20.1 `BACKUP_ROOT`

The administrator SHALL explicitly authorize/select where retained backup artifacts are stored.

The path is mounted to:

```text
/data/backups
```

The application SHALL constrain all configured local backup-set destinations beneath this root.

## 20.2 Private application state

Private application state SHALL NOT be a normal user-facing install parameter.

Phase 0 SHALL prove the supported UGOS Docker-App mechanism for a private writable state mount and document update/uninstall semantics.

## 20.3 `LOG_LEVEL`

Optional:

```yaml
type: string
changeable: true
```

Default:

```text
info
```

Do not expose SSH credentials as environment variables.

---

# 21. Proposed `project.yaml`

The implementation SHALL generate the final file from release metadata and validate it against the current UGREEN schema.

Do not copy minimum firmware/Docker versions from examples. Determine them from the capabilities actually used and current official guidance.

Illustrative skeleton:

```yaml
spec_version: "2.1"
app_id: com.iasbuilt.backupmanager
version: 0.1.0

support_arch:
  - amd64
  - arm64

supports:
  - pc

is_docker_app: true
only_admin: true

port: 29090
proxy_path: backup-manager-api
open_type: inner

tag_types:
  - backup

depend_fw_version: <verified-minimum>
depend_docker_version: <verified-minimum>

permissions:
  - NETWORK.ACCESS_INTERNET

# BACKUP_ROOT declaration is conditional on the Phase-0
# decision between path parameter and access-path authorization.
parameters:
  - key: LOG_LEVEL
    type: string
    required: false
    changeable: true
    i18n:
      en-US:
        name: Log Level
        description: Application logging verbosity.

privacy_policy_link:
  - https://<publisher>/backup-manager/privacy

# Current UGREEN project.yaml rules require these when
# open-source code/components are used.
license_agreement_link:
  - https://<publisher>/backup-manager/licenses
source_code_link:
  - https://<publisher>/backup-manager/source
technical_support_link:
  - https://<publisher>/backup-manager/support

i18n:
  en-US:
    name: Backup Manager
    description: Pull, verify, retain, and monitor remote backup artifacts.
    author: <publisher>
    publisher: <publisher>
```

Because the product embeds rclone and uses open-source Go/frontend components, release engineering SHALL maintain:

- a complete third-party license inventory;
- required copyright/license notices;
- a public source-code/source-offer destination sufficient for applicable licenses and current UGREEN App Center rules.

`app_id` MUST be frozen before the first public App Center submission.

---

# 22. Proposed UGOS Docker Compose

Illustrative:

```yaml
services:
  backup-manager:
    image: <exact-versioned-canonical-image-tag>
    restart: always

    environment:
      TZ: ${TZ}
      BACKUP_MANAGER_LOG_LEVEL: ${LOG_LEVEL}
      BACKUP_MANAGER_AUTH_MODE: ugos
      BACKUP_MANAGER_DATA_DIR: /var/lib/backup-manager
      BACKUP_MANAGER_BACKUP_ROOT: /data/backups

    volumes:
      - <verified-private-state-source>:/var/lib/backup-manager
      - ${BACKUP_ROOT}:/data/backups

    ports:
      - "127.0.0.1:29090:8080"

    read_only: true

    tmpfs:
      - /tmp

    security_opt:
      - no-new-privileges:true
```

Notes:

- loopback publication is the preferred security model but MUST be validated against UGOS Docker App/gateway behavior;
- do not use `latest`;
- do not use privileged mode;
- do not use host networking;
- do not use `cap_add`;
- architecture-specific image tar files SHALL contain the exact tag referenced here.

If loopback publication is incompatible with UGOS, Phase 0 must produce a reviewed alternative before implementation proceeds.

---

# 23. Dashboard UX

The default application page SHALL be a concise operations dashboard.

## 23.1 Overall status header

Show:

```text
Backup Manager        HEALTHY
Last successful cycle  8 minutes ago
Storage                1.8 TB free
```

State precedence SHALL be server-defined.

The UI MUST NOT calculate aggregate health independently.

## 23.2 Backup-set cards/table

For each backup set display:

- name;
- source host;
- status;
- newest known-good backup;
- age;
- next/last poll;
- retained restore-point counts;
- current transfer state;
- last error if any.

Example:

```text
Production / PostgreSQL
HEALTHY

Newest restore point     42m ago
Daily                    7 / 7
Weekly                   12
Monthly                  12
Last validation          Passed
```

## 23.3 Active operations

Show:

- source;
- artifact;
- stage;
- bytes transferred;
- progress where available;
- elapsed time;
- transfer rate;
- cancellability.

A cancellation request MUST be routed through the application service/context cancellation.

---

# 24. Primary Navigation

Initial UI navigation SHOULD include:

```text
Dashboard
Backup Sets
Backups
Activity
Quarantine
Settings
```

Optional developer/admin-only item:

```text
Diagnostics
```

Do not overload the first release with unrelated file-management features.

---

# 25. Backup Sets Page

The Backup Sets page SHALL provide:

- searchable/sortable list;
- source;
- remote path;
- local destination;
- status;
- stale threshold;
- retention policy summary;
- last run;
- next poll;
- enabled/disabled state.

Actions:

```text
Open
Run now
Test connection
Edit
Disable
Retention preview
```

Destructive configuration removal SHALL be separated from deleting stored files.

---

# 26. Add/Edit Backup Set Wizard

The setup flow SHOULD use approximately six grouped steps rather than twelve micro-steps.

## Step 1 — Source identity and remote connection

Fields:

- display/source name;
- backup-set name;
- hostname/IP;
- SSH port;
- username.

## Step 2 — Authentication and host trust

- generate/select/import managed SSH key;
- probe host key;
- show algorithm + fingerprint;
- require explicit administrator trust.

The UI SHALL instruct the administrator to verify the host fingerprint through an independent trusted channel where possible.

## Step 3 — Backup artifact discovery and completion

Fields:

- remote directory;
- include patterns;
- optional exclude patterns;
- completion strategy.

Completion strategies SHALL be visibly ranked:

**Preferred / strong assurance**
- producer atomic rename;
- completion marker/manifest.

**Advanced / heuristic**
- stable size/mtime.

Stable-size mode SHALL require:

- configurable stability window;
- minimum artifact age;
- explicit warning that stability is not proof the producer completed successfully;
- an additional deletion-safety delay or other core-approved safeguard before remote source deletion.

## Step 4 — Destination, retention, and source handling

- choose destination beneath authorized `BACKUP_ROOT`;
- daily/weekly/monthly retention;
- timezone/week start;
- last-known-good protection.

The UI SHALL prominently disclose:

> After a backup has been transferred, verified, durably committed to the NAS, and recorded safe by Backup Manager, the original remote backup artifact is deleted from the source server.

The administrator SHALL acknowledge this behavior before enabling a new backup set.

## Step 5 — Validation and non-destructive test

- transfer verification;
- checksum policy;
- packaged/registered validator;
- SSH authentication;
- trusted host;
- remote path listing;
- local destination writability;
- destination capacity.

Do not permit arbitrary shell commands from the browser.

Connection testing MUST NOT delete remote data.

## Step 6 — Review and activate

Display:

- source;
- paths;
- credential/key identity;
- host-key fingerprint;
- completion assurance;
- retention;
- validation;
- source-deletion behavior.

Secrets are redacted.

Actions:

```text
Save disabled
Save and enable
Save, enable, and run first ingestion
```

Running the first ingestion uses the normal lifecycle and may delete the remote artifact only after all predecessor safety invariants succeed.

---

# 27. Backups / Retained Backups Page

The UI SHALL present retained valid artifacts as **Backups** / **Retained Backups**. Internally, the retention engine may continue to call selected backups restore points.

Columns:

- timestamp;
- backup set;
- artifact;
- size;
- status;
- retention classifications;
- validation state;
- local path;
- received time.

Retention classifications may include multiple labels:

```text
Daily
Weekly
Monthly
Protected
```

A monthly restore point that also satisfies weekly retention must display both classifications if useful.

`.partial`, failed, or quarantined artifacts SHALL NOT appear as ordinary valid restore points.

---

# 28. Artifact Detail

Artifact detail SHOULD display:

- artifact ID;
- source;
- remote original path;
- local path;
- producer timestamp;
- discovery time;
- transfer time;
- completion time;
- size;
- hash data;
- validation results;
- lifecycle history;
- retention classifications;
- whether remote source deletion completed.

Lifecycle timeline example:

```text
DISCOVERED              02:00:11
TRANSFERRED             02:00:53
VERIFIED                02:00:59
COMMITTED               02:01:00
REMOTE_DELETE_PENDING   02:01:00
COMPLETE                02:01:01
```

The UI SHALL use server-provided lifecycle state.

---

# 29. Retention UI

## 29.1 Editor

Allow administration of:

- daily retention;
- weekly retention;
- monthly retention;
- timezone;
- week start;
- last-known-good protection.

## 29.2 Preview before apply

The UI SHALL expose a retention preview.

Preview MUST return the server-calculated plan.

Example:

```text
KEEP    backup-A     Daily + Weekly
KEEP    backup-B     Monthly
KEEP    backup-C     Last known good
DELETE  backup-D     Not selected by active retention policy
```

The UI MUST NOT independently decide which files are deletable.

## 29.3 Apply

Applying retention SHALL:

1. obtain a server-generated immutable preview/`plan_id`;
2. present:
   - count to keep;
   - exact count to delete;
   - bytes expected to reclaim;
   - plan expiry;
3. require explicit confirmation of that plan;
4. submit the same `plan_id`;
5. apply only if the inventory/config revision is unchanged;
6. if stale, abort with no deletions and require re-preview;
7. display durable operation progress/result.

---

# 30. Quarantine UI

Show artifacts that failed required validation or have inconsistent state.

Display:

- source;
- artifact;
- failure reason;
- validation output summary;
- remote source present/absent;
- local copy path;
- date/time;
- retry count.

Actions MAY include:

```text
Revalidate
Retry ingestion
Open diagnostics
```

Do not offer a generic "delete remote anyway" button.

---

# 31. Activity / Operations UI

Provide a chronological event stream.

Event categories:

- discovery;
- transfer;
- verification;
- validation;
- durable commit;
- remote deletion;
- retention;
- reconciliation;
- configuration change;
- security/host-key event;
- storage pressure;
- error.

Filtering:

- backup set;
- severity;
- event type;
- date range.

The activity UI SHALL be driven by durable/server events where available rather than only browser-session logs.

---

# 32. Settings UI

Settings SHOULD cover:

- polling interval;
- default retention;
- storage thresholds;
- log level;
- application version;
- embedded rclone version;
- database schema version;
- UGOS package version;
- build commit.

The UI SHOULD expose config export.

Config export MUST redact:

- private keys;
- secret values;
- authentication tokens.

---

# 33. Diagnostics UI

Admin-only.

Show:

- process uptime;
- daemon scheduler status;
- SQLite status;
- current migration version;
- destination free space;
- last reconciliation;
- rclone embedded version;
- Go version;
- build architecture;
- API/frontend version;
- current UGOS auth mode;
- active operations;
- recent sanitized errors.

Provide a downloadable **sanitized support bundle** only if secret redaction is tested.

---

# 34. Error UX

Errors SHALL be:

- actionable;
- mapped from typed backend errors;
- free from leaked secrets;
- attributable to source/backup set;
- correlated with an operation ID.

Examples:

```text
Authentication failed
Host key changed
Remote folder not found
Permission denied
Backup has not remained stable long enough
Checksum mismatch
Destination storage is low
Remote backup changed before deletion
Retention blocked by safety rule
```

Do not show raw Go stack traces in ordinary UI.

---

# 35. Health Model

The UI SHALL consume backend-defined health.

At minimum:

```text
HEALTHY
DEGRADED
STALE
FAILING
```

The UI SHALL visually distinguish:

```text
Application is running
```

from:

```text
Backups are current and valid
```

A running container with no successful backups for longer than `stale_after` MUST be visibly stale/failing.

---

# 36. API / UI Version Compatibility

The frontend and backend SHALL be produced from the same release.

Expose:

```text
GET /api/v1/system/version
```

including:

```json
{
  "backup_manager": "...",
  "api_version": "v1",
  "ui_build": "...",
  "rclone": "...",
  "commit": "..."
}
```

If the UI detects an incompatible API version, it SHALL stop destructive operations and display a clear version mismatch message.

---

# 37. Headless Docker Distribution

## 37.1 Purpose

The headless package supports operators who want:

- no UGOS dependency;
- terminal management;
- Docker/Compose;
- scripted deployment;
- deployment outside UGREEN.

## 37.2 Canonical image

Publish:

```text
<registry>/iasbuilt/backup-manager:<version>
```

The image is the same architecture-specific image digest bundled into the matching UPK release.

Production examples SHALL pin a semantic version and SHOULD record the resolved digest.

A convenience `latest` tag MAY exist in the registry, but it SHALL NOT be used by the UPK and SHALL NOT be the recommended production deployment.

## 37.3 Example usage

```bash
docker run --rm \
  -v /path/to/config:/etc/backup-manager:ro \
  -v /path/to/state:/var/lib/backup-manager \
  -v /path/to/backups:/data/backups \
  <registry>/iasbuilt/backup-manager:1.0.0 \
  backup-manager check
```

Daemon:

```bash
docker run -d \
  --name backup-manager \
  --restart unless-stopped \
  -v /path/to/config:/etc/backup-manager:ro \
  -v /path/to/state:/var/lib/backup-manager \
  -v /path/to/backups:/data/backups \
  <registry>/iasbuilt/backup-manager:1.0.0 \
  backup-manager daemon
```

The HTTP/UI listener SHALL be disabled by default in headless mode unless explicitly enabled.

---

# 38. Docker Compose for Terminal Users

Provide a supported example:

```yaml
services:
  backup-manager:
    image: <registry>/iasbuilt/backup-manager:1.0.0
    restart: unless-stopped

    command:
      - backup-manager
      - daemon

    volumes:
      - ./config:/etc/backup-manager:ro
      - ./state:/var/lib/backup-manager
      - /mnt/backups:/data/backups

    read_only: true

    tmpfs:
      - /tmp

    security_opt:
      - no-new-privileges:true
```

SSH key/known-host mounts SHALL be documented for headless deployments.

---

# 39. Container Hardening

The canonical image SHOULD:

- use a minimal runtime base;
- contain no compiler/build chain;
- run without privileged mode;
- avoid host networking;
- avoid unnecessary Linux capabilities;
- use a read-only root filesystem where practical;
- use writable state/backup mounts only;
- use `no-new-privileges`;
- terminate cleanly;
- implement health checking;
- contain CA certificates and timezone data as needed;
- expose no shell tooling beyond operational necessity.

The implementation SHOULD run as non-root after any required initialization.

Actual UGREEN path/UID behavior SHALL be verified on real hardware before making non-root execution an acceptance gate.

---

# 40. Multi-Architecture Build

Support:

```text
linux/amd64
linux/arm64
```

Build matrix SHALL produce one canonical image per architecture:

```text
backup-manager:<version>  linux/amd64
backup-manager:<version>  linux/arm64
```

For registry publication this MAY be represented by a multi-architecture manifest.

For UPK creation, export the exact architecture-specific image referenced by the release manifest with `docker save`.

Every image tar placed in an UPK staging tree SHALL contain exactly one versioned tag matching the Compose file.

CI SHALL compare the exported image digest/content identity against the corresponding published release artifact before packing.

---

# 41. UPK Project Layout

Target staging tree:

```text
packaging/ugos/
├── project.yaml
├── rootfs_common/
│   ├── icon.png
│   └── docker-compose.yaml
├── rootfs_amd64/
│   └── images/
│       └── backup-manager-<version>-amd64.tar
└── rootfs_arm64/
    └── images/
        └── backup-manager-<version>-arm64.tar
```

Do not put additional arbitrary files into Docker App `rootfs_common`.

Frontend assets belong inside the canonical application binary/image.

---

# 42. UPK Icon

Create a release icon that complies with current UGREEN rules.

Current target:

```text
256 × 256 PNG
< 100 KB
UGREEN-compatible rounded-square treatment
light/white background requirements as documented
```

The final icon SHALL be tested in:

- UGOS desktop;
- App Center installed-app list;
- light/dark desktop context where applicable.

---

# 43. UPK Build

Packaging SHALL use official `ugcli`.

Example:

```bash
ugcli pack --build "$BUILD_NUMBER"
```

The source-controlled project version SHALL use semantic:

```text
x.y.z
```

UGOS build output SHALL use:

```text
x.y.z.bbbb
```

with a monotonically increasing build number within the same `x.y.z`.

CI SHALL fail if the requested build number would violate release rules.

---

# 44. Developer Authorization / Hardware Testing

Before manual developer UPK testing, obtain UGREEN developer authorization for at least one target NAS.

The project documentation SHALL record the current official process but MUST NOT commit:

- NAS serial number;
- MAC address;
- authorization signature;
- admin username;
- other device credentials.

For current UGOS versions using developer authorization, the authorization file is device-specific and handled outside Git.

---

# 45. App Center Readiness

The project SHALL be designed from the beginning to avoid a packaging rewrite when submitting to the official App Center.

## 45.1 Stable app identity

Freeze:

```text
app_id
display name
publisher identity
```

before first public submission.

## 45.2 Metadata

Prepare:

- English display name;
- description;
- version;
- category (`backup`);
- author/publisher;
- website/help URL when available;
- release notes;
- screenshots;
- icon;
- privacy policy;
- permission explanation.

## 45.3 Developer identity

Official publication is gated by UGREEN developer identity verification and review requirements.

Treat this as a release/business prerequisite, not an engineering blocker for local developer UPK testing.

## 45.4 No self-updating App Center code

The UPK edition MUST NOT bypass App Center review by:

- pulling `latest`;
- downloading replacement executable code;
- self-installing plugins that alter reviewed behavior;
- silently replacing the UI/backend.

New code/function versions SHALL be released as a new UPK.

## 45.5 No forced telemetry

Initial App Center edition SHALL have no required cloud telemetry.

The application SHOULD operate fully locally except for administrator-configured backup source connections.

---

# 46. Upgrade Behavior

An UPK update MUST preserve:

- SQLite state;
- backup configuration;
- SSH credentials;
- trusted host keys;
- retained backup artifacts.

State lives outside the container image.

## 46.1 Migration

Startup sequence after upgrade:

```text
load version
    ↓
lock service initialization
    ↓
validate state directory
    ↓
backup/prepare SQLite as required
    ↓
run transactional migrations
    ↓
start BackupService
    ↓
start daemon/API
```

If migration fails:

- do not start destructive background operations;
- preserve previous data;
- mark readiness false;
- log actionable error.

## 46.2 UI/backend mismatch

Because UI is bundled with the image, normal UPK upgrades should keep versions matched.

The API version check remains mandatory.

---

# 47. Disable / Enable Behavior

Disabling the UGOS app SHALL:

- stop new polling;
- cancel active transfers safely;
- allow lifecycle reconciliation on next start;
- leave remote files preserved if commit state is uncertain;
- preserve state and backup data.

Re-enabling SHALL:

- run migrations if needed;
- run reconciliation;
- then resume scheduling.

---

# 48. Uninstall Behavior

Uninstall semantics are potentially destructive and MUST be explicitly tested on UGOS.

The application SHOULD treat:

- application container/image;
- app state;
- retained backup files;

as distinct resources.

Preferred behavior:

- uninstalling the app MUST NOT delete the user-selected backup root;
- app-data removal behavior MUST be clearly disclosed by UGOS/install flow;
- export/config backup SHOULD be offered/documented before removal;
- the UI SHALL warn that uninstall may remove private app configuration/SSH keys if current UGOS behavior does so;
- retained backup artifacts remain independent of app-state removal;
- reinstall documentation SHALL include catalog rebuild/adoption of an existing backup root.

Phase 0/packaging tests SHALL determine exact UGOS Docker App volume behavior during uninstall/update.

Do not assume undocumented persistence behavior.

---

# 49. First-Run Experience

On first open:

```text
Welcome / product purpose
  ↓
Privacy + data-processing notice
  ↓
Storage readiness
  ↓
Create first backup set (grouped wizard)
  ↓
Review source-deletion behavior
  ↓
Optional first ingestion
```

If the target App Center region/current UGREEN rules require first-launch privacy consent for the user identity/audit data processed by the application, that notice/consent step is mandatory and MUST NOT be skippable.

Backup setup itself SHALL be skippable so experienced administrators can enter the main UI and configure manually.

The welcome screen SHALL make clear that Backup Manager:

- manages backup artifacts that another system creates;
- does not create the database/application backup itself;
- does not perform an application/database restore in v1;
- deletes the remote backup artifact only after the predecessor lifecycle has safely committed the NAS copy.

No remote data may be deleted during first-run connection tests.

---

# 50. Read-Only vs Destructive UI Actions

Classify actions.

## Read-only / low risk

- list sources;
- list artifacts;
- view status;
- test SSH;
- probe host key;
- preview retention;
- view logs;
- view configuration.

## State-changing but non-destructive to backup data

- create/edit backup set;
- generate SSH key;
- trust host key;
- run backup cycle;
- revalidate;
- disable source.

## Destructive

- retention apply;
- remove local retained backups according to policy;
- remove configuration;
- key deletion;
- actions that may eventually enable remote source deletion via the normal lifecycle.

Destructive actions SHALL use explicit confirmation and backend authorization.

Remote deletion is never directly user-invoked; it remains a lifecycle consequence after safe local commit.

---

# 51. Operation Concurrency

The API/UI MUST honor predecessor locking rules.

The UI SHALL disable or explain conflicting actions when:

- a backup set is transferring;
- retention is operating on the same set;
- reconciliation is active;
- migration is active;
- validation holds a required artifact lock.

The server is authoritative.

Every mutating configuration resource SHALL carry a revision/ETag-equivalent. Updates based on stale revisions SHALL fail with conflict rather than overwrite newer settings.

Each long-running operation SHALL capture an immutable configuration snapshot/revision at creation. Editing a backup set while an older operation is running MUST NOT change that operation's host/path/credential/retention semantics mid-flight.

Two browser sessions must not be able to bypass concurrency constraints.

---

# 52. Operation Progress

V1 SHALL use authenticated polling against durable operation state.

Example:

```text
GET /api/v1/operations/{id}
```

Poll responses SHOULD include:

- operation state;
- lifecycle stage;
- artifact;
- bytes transferred;
- total bytes where known;
- transfer rate;
- elapsed time;
- cancellability;
- last update sequence/revision.

The browser SHALL stop polling completed/failed/cancelled operations.

The server operation SHALL continue if:

- the browser closes;
- the network drops;
- the UGOS window reloads.

Explicit cancellation, if supported, SHALL create a server-side cancellation request and propagate through the operation's own context. It SHALL NOT be coupled to the HTTP request context that originally created the job.

A future authenticated `fetch()` streaming transport MAY be added after UGOS gateway validation.

Native `EventSource` and query-string authentication tokens are prohibited.

---

# 53. Accessibility and Interaction

The UI SHALL:

- support keyboard navigation for primary workflows;
- provide visible focus;
- not rely on color alone for health/error state;
- provide accessible labels for icons;
- use confirmation text that describes the actual effect;
- display timestamps with timezone;
- format sizes consistently;
- remain usable at minimum supported window size.

---

# 54. Localization

Initial UI language:

```text
English
```

Architecture SHALL permit localization.

UGOS App Center `i18n` metadata and application UI translation are separate concerns.

Frontend strings SHOULD be centralized rather than scattered through components.

Do not block first release on full localization unless App Center submission target requires it.

---

# 55. Date/Time Semantics

The UI SHALL display:

- user's/local UGOS timezone where appropriate;
- explicit timezone on retention policy;
- absolute time and useful relative age.

Examples:

```text
Aug 28, 2026 23:41 PDT
42 minutes ago
```

The UI SHALL NOT recalculate retention calendar buckets differently from the backend.

---

# 56. Storage UX

Display:

- backup root;
- total capacity;
- free capacity;
- configured warning threshold;
- configured critical threshold;
- estimated size of incoming artifact where known.

Storage warnings SHALL not imply that the UI can override retention safety.

The UI SHALL not provide an "auto delete anything until enough space" option in v1.

---

# 57. Logging

The backend remains the source of operational logs.

The UI MAY display sanitized recent logs/events.

Logs MUST never contain:

- SSH private keys;
- UGOS tokens;
- passwords;
- raw secret environment variables.

UPK/container logs SHALL remain useful through the standard UGOS application troubleshooting path.

---

# 58. Audit Trail

Configuration and destructive administrative events SHALL record:

- authenticated UGOS user identifier where available;
- timestamp;
- operation;
- target backup set/artifact;
- result;
- correlation ID.

Do not treat the audit trail as a security boundary.

It is operational evidence.

---

# 59. Observability

Expose metrics internally through typed service state.

Optional future endpoint:

```text
/metrics
```

Prometheus metrics are not required for UI v1.

The UI should get structured status from the API rather than scrape logs.

---

# 60. Release Versioning

A single logical release version SHALL identify:

- core Go application;
- canonical OCI image;
- UI;
- API;
- UPK semantic version.

Example:

```text
Backup Manager 1.2.0
OCI image       1.2.0
UI              1.2.0
UPK             1.2.0.0042
```

The UPK build suffix may differ while still deriving from the same `1.2.0` source release.

---

# 61. Supply-Chain / Build Integrity

Release automation SHOULD produce:

- SHA-256 checksums;
- OCI image digests;
- SBOM for each image;
- dependency manifests;
- build commit;
- rclone version;
- Go version;
- frontend lockfile state;
- third-party license inventory/notices;
- exact OCI image digest used in each UPK.

Container images SHOULD be signed where the project's registry/tooling supports it.

The UPK SHALL be built only from release images whose digest is known.

---

# 62. CI/CD Pipeline

A release pipeline SHOULD execute these stages.

## Stage A — Core tests

```text
go test
go vet
static analysis
transport tests
lifecycle tests
retention tests
```

## Stage B — UI tests

```text
pnpm install --frozen-lockfile
typecheck
lint
unit/component tests
build
```

## Stage C — API contract

- API schema/contract tests;
- frontend client compatibility;
- auth middleware tests;
- destructive authorization tests.

## Stage D — Build binaries/images

Build one canonical image for:

```text
linux/amd64
linux/arm64
```

## Stage E — Docker integration

Run the canonical image in both profiles:

- headless daemon/CLI profile tests;
- UGOS serve+daemon profile tests;
- startup/shutdown;
- health;
- mounted state;
- SFTP fixture;
- backup lifecycle smoke test.

## Stage F — Export UPK image tar

For each architecture:

```text
docker save ...
```

Verify exact tag.

## Stage G — Stage UPK tree

Copy:

- `project.yaml`;
- icon;
- Compose;
- architecture image tar.

## Stage H — `ugcli pack`

Produce `.upk`.

## Stage I — Verify artifacts

Verify:

- filenames;
- version;
- checksums;
- image architecture;
- no `latest`;
- no secrets;
- no unexpected files;
- size sanity.

## Stage J — Hardware test gate

For release candidates, install on an authorized UGREEN NAS and run the defined UGOS E2E smoke suite.

App Center submission SHALL remain a manual approval step.

---

# 63. Test Strategy and TDD Gate

Testing SHALL be continuous across all phases and child issues.

The repository SHALL maintain a layered suite:

1. unit;
2. service/API contract;
3. integration;
4. frontend component;
5. browser E2E;
6. container;
7. destructive-safety/security;
8. real UGOS hardware acceptance.

No phase may defer its core tests to Phase 8.

Phase 8 is hardening and adversarial regression, not the first time features are tested.

For each child issue:

```text
RED test(s)
    ↓
minimal implementation
    ↓
focused GREEN
    ↓
refactor
    ↓
integration/E2E if boundary crossed
    ↓
affected regression suite
    ↓
issue acceptance
```

CI SHALL reject merges when required suites fail.

---

# 64. Frontend Unit/Component Tests

These tests SHALL be written before the corresponding frontend production logic wherever feasible.

Cover:

- health rendering;
- stale/failing states;
- backup-set forms;
- wizard validation;
- retention preview;
- confirmation dialogs;
- host-key fingerprint workflow;
- secret redaction;
- API typed-error mapping;
- version mismatch;
- operation progress;
- quarantine.

---

# 65. API Tests

API contract/auth/error tests SHALL precede route-handler implementation.

Cover:

- UGOS auth middleware;
- unauthenticated rejection;
- non-admin rejection where applicable;
- CRUD validation;
- connection-test behavior;
- SSH host trust;
- operation creation;
- status;
- operation polling/reload recovery;
- retention preview;
- retention apply authorization;
- config deletion semantics;
- no arbitrary remote delete endpoint;
- secrets not returned.

---

# 66. Browser E2E Tests

Use Playwright or equivalent.

E2E scenarios SHOULD be specified before the feature is considered implementation-complete; for critical destructive workflows, write the scenario before production UI integration.

Flows:

1. open dashboard;
2. add backup set;
3. generate/import key;
4. trust host key;
5. test remote connection;
6. save source;
7. run backup;
8. observe transfer progress;
9. inspect restore point;
10. preview retention;
11. apply safe retention;
12. simulate stale source;
13. simulate checksum failure/quarantine;
14. revalidate.

A mock UGOS auth provider MAY be used in local CI.

Real UGOS auth MUST be tested on hardware separately.

---

# 67. Docker CLI Tests

Container/CLI behavior SHALL be specified as integration tests before the packaging/runtime implementation is finalized.

Test:

- `backup-manager check`;
- one-cycle `run`;
- daemon;
- clean `SIGTERM`;
- persistent state mount;
- backup mount;
- SSH key mount;
- recovery after container restart;
- version output;
- architecture.

---

# 68. UGOS Hardware Test Matrix

Hardware test cases SHALL be written and version-controlled before manual execution. Record firmware, Docker package version, device model/architecture, result, and evidence.

At minimum test one representative device per supported CPU architecture before claiming both architectures supported.

Matrix:

```text
amd64 UGREEN NAS
arm64 UGREEN NAS
```

If physical hardware for one architecture is not available, that architecture SHALL remain "build-supported / not certified" until real hardware validation is completed.

Test:

- developer authorization;
- manual UPK install;
- icon;
- desktop launch;
- `inner` window;
- UGOS authentication;
- backend gateway proxy;
- path selection/mount;
- daemon operation;
- SFTP connectivity;
- container restart;
- disable/enable;
- update;
- state persistence;
- uninstall behavior;
- logs;
- permissions.

---

# 69. Phase 0 — UGOS Feasibility and Security Gate

## Phase 0 TDD / Acceptance-First Rule

Because much of Phase 0 is hardware-dependent, write the acceptance tests/checklists **before** building the PoC.

For every experiment define:

- setup;
- expected safe result;
- explicit failure condition;
- evidence to capture;
- architecture decision triggered by failure.

Automatable package-schema/image checks SHALL be written before packaging scripts.

This phase MUST occur before large-scale UI implementation.

## 0.1 Obtain official tooling

- set up Debian 12 build environment;
- install/pin supported `ugcli`;
- record tool version;
- create developer UPK project;
- obtain developer authorization for test NAS.

## 0.2 Minimal Docker App UPK

Create a minimal image that serves:

```text
/
/health/live
```

Package and install as `.UPK`.

## 0.3 `inner` window test

Verify:

- App Center installation;
- desktop icon;
- `open_type: inner`;
- resizing;
- React sample/JSSDK initialization.

## 0.4 UGOS auth test

Verify end-to-end:

```text
frontend getThirdToken
        ↓
UGOS gateway
        ↓
backend authenticated user
```

## 0.5 Direct-port bypass test

Determine whether the service can be published to loopback only.

Attempt direct LAN access.

**Exit gate:** destructive APIs cannot proceed until direct access cannot spoof UGOS identity.

## 0.6 Private state and backup-root experiment

Verify separately:

- a UGOS-supported private writable state source for `/var/lib/backup-manager`;
- user-authorized backup root for `/data/backups`;
- whether `path` parameter or `allow_add_access_path` is the better supported backup-root mechanism;
- container read/write ownership;
- update persistence;
- disable/enable persistence;
- uninstall behavior;
- whether private state is deleted on uninstall;
- whether retained backup data survives uninstall as required.

Do not proceed with a user-selected `APP_DATA_PATH` merely because it is convenient.

## 0.7 Architecture and gateway-source test

Verify:

- target CPU and image packaging;
- actual peer/source address of UGOS-gateway-proxied backend requests;
- direct-LAN peer identity;
- backend trusted-proxy enforcement;
- current `ugcli` version and schema behavior.

## 0.8 Compliance preflight

Verify current requirements for:

- privacy policy;
- first-launch privacy notice/consent;
- open-source license agreement link;
- source-code/source-offer link;
- support link;
- third-party SDK/component disclosure;
- target App Center distribution regions.

### Phase 0 Acceptance

- [ ] Minimal Docker App `.UPK` installs.
- [ ] App opens inside UGOS desktop.
- [ ] UGREEN frontend SDK initializes.
- [ ] UGOS user authentication reaches backend.
- [ ] Direct-port auth bypass is resolved.
- [ ] Private state and user backup storage are separate and documented.
- [ ] Trusted UGOS gateway source is proven and enforced.
- [ ] Docker App Compose constraints are validated.
- [ ] App Center OSS/privacy metadata requirements are documented.
- [ ] At least one real UGREEN device passes the PoC.

---

# 70. Phase 1 — Application Service and API Foundation

## Phase 1 TDD Gate

Before implementing handlers/services, write tests for:

- authenticated vs unauthenticated requests;
- admin vs non-admin access;
- trusted vs untrusted proxy source;
- spoofed UGOS identity headers;
- idempotency-key duplicate submission;
- stale configuration revision conflicts;
- durable operation persistence;
- browser/request disconnect independence;
- operation restart reconciliation;
- typed error responses;
- retention plan creation and stale-plan refusal.

Handlers SHALL be implemented only after these tests fail for the expected reason.

## 1.1 Confirm predecessor `BackupService`

Ensure all required UI use cases are expressible without direct lifecycle package calls.

## 1.2 Add API adapter

Create:

```text
internal/httpapi/
```

## 1.3 Domain DTO layer

Prevent rclone/SQLite types leaking to API.

## 1.4 Auth abstraction

Create:

```text
auth.Authenticator
auth.Authorizer
```

Implement:

```text
auth/ugos
auth/standalone-test
```

## 1.5 Durable operations model

Persist operation records in SQLite before execution.

Implement:

- idempotency keys;
- actor identity;
- config revision snapshot;
- operation revision/sequence;
- explicit cancellation request;
- restart reconciliation.

## 1.6 Operation polling

Implement authenticated operation polling. Streaming is deferred until UGOS proxy behavior is proven.

## 1.7 Version/capabilities endpoint

### Phase 1 Acceptance

- [ ] API is versioned.
- [ ] API invokes `BackupService`.
- [ ] UGOS auth middleware exists.
- [ ] Destructive routes require authorization.
- [ ] API exposes no rclone types.
- [ ] Long-running work returns operation IDs.
- [ ] Durable operation polling works across browser reload/disconnect.
- [ ] API contract tests pass.

---

# 71. Phase 2 — Frontend Foundation

## Phase 2 TDD Gate

Write frontend tests first for:

- authenticated API client header injection;
- expired-session handling;
- navigation;
- loading/empty/error states;
- version mismatch safety;
- destructive-control disablement when API is incompatible;
- operation polling/recovery after reload.

Use mocked API contracts before wiring production components.

## 2.1 Scaffold from UGREEN React sample

Use supported Vite/UGREEN tooling.

## 2.2 SDK/auth integration

Implement UGOS token acquisition.

## 2.3 API client

Typed API client with:

- auth header injection;
- typed errors;
- request correlation;
- cancellation.

## 2.4 Shell/navigation

Implement:

```text
Dashboard
Backup Sets
Backups
Activity
Quarantine
Settings
```

## 2.5 UI state patterns

Create consistent:

- loading;
- empty;
- error;
- stale;
- disabled;
- confirmation;
- toast/notification.

## 2.6 Embedded production build

Integrate Vite build into Go `embed`.

### Phase 2 Acceptance

- [ ] UI opens inside UGOS.
- [ ] UI authenticates via UGOS.
- [ ] UI calls API.
- [ ] UI build is embedded in production Go binary.
- [ ] Production image contains no Node runtime.
- [ ] Version mismatch handling works.
- [ ] Basic accessibility checks pass.

---

# 72. Phase 3 — Read-Only Operational UI

## Phase 3 TDD Gate

Write component/E2E tests first for:

- HEALTHY/DEGRADED/STALE/FAILING rendering;
- newest known-good backup visibility;
- no `.partial`/quarantined artifact in valid backup list;
- retention classifications;
- operation progress;
- sanitized error display;
- process-health vs backup-health distinction.

## 3.1 Dashboard

Implement aggregate status.

## 3.2 Backup Sets list/detail

Read-only first.

## 3.3 Backups

Implement list and artifact detail.

## 3.4 Activity

Implement event/history view.

## 3.5 Quarantine read view

## 3.6 Diagnostics

### Phase 3 Acceptance

- [ ] Operators can determine whether backups are healthy without CLI.
- [ ] Operators can inspect newest known-good restore point.
- [ ] Retention classifications are visible.
- [ ] Transfer progress is visible.
- [ ] Quarantined artifacts are visible.
- [ ] Process health and backup freshness are distinct.

---

# 73. Phase 4 — Configuration and Administrative Workflows

## Phase 4 TDD Gate

Write tests first for:

- six-step wizard validation;
- required acknowledgement of remote-source deletion behavior;
- SSH key generation/import metadata handling;
- inability to retrieve stored private key material;
- refusal to delete referenced SSH keys;
- changed-host-key blocking;
- non-destructive connection test;
- stable-size heuristic warning;
- retention `plan_id`;
- stale retention plan → zero deletions;
- stale config revision conflict;
- manual run using normal lifecycle safety;
- config deletion preserving retained backups.

Every destructive workflow requires negative tests before positive implementation.

## 4.1 Backup Set wizard

Implement all wizard steps.

## 4.2 SSH key management

- generate;
- import;
- list metadata;
- delete with safety checks.

## 4.3 Host-key trust

Probe/display/trust.

## 4.4 Connection testing

Non-destructive.

## 4.5 Retention editor

## 4.6 Retention preview/apply with immutable `plan_id` and stale-plan rejection

## 4.7 Manual run

## 4.8 Validation/revalidation

## 4.9 Disable/edit/remove backup-set config

### Phase 4 Acceptance

- [ ] A new source can be configured entirely from UI.
- [ ] No SSH private key is exposed back to browser after persistence.
- [ ] Changed host key blocks operation.
- [ ] Connection testing is non-destructive.
- [ ] Retention preview is server-calculated and bound to inventory/config revision.
- [ ] Retention application uses the exact confirmed `plan_id`; stale plans delete nothing.
- [ ] Manual run uses normal lifecycle safety.
- [ ] Configuration deletion does not delete retained backups.
- [ ] Remote-source deletion behavior is explicitly acknowledged during setup.
- [ ] Referenced SSH keys cannot be deleted.

---

# 74. Phase 5 — Canonical Production Container Image

## Phase 5 TDD Gate

Write artifact/container tests before final Dockerfile changes for:

- exact version;
- correct architecture;
- non-root behavior where supported;
- read-only root filesystem;
- writable state/backup mounts;
- no privileged mode;
- no unexpected listener in headless mode;
- health endpoint in serve mode;
- clean SIGTERM;
- restart-safe state;
- same binary behavior in headless and UGOS profiles.

## 5.1 Multi-stage image

Build frontend + Go, produce minimal runtime.

## 5.2 Runtime hardening

- read-only root;
- state/data mounts;
- no-new-privileges;
- no host network;
- no privileged mode.

## 5.3 Health

Add container health endpoint/check.

## 5.4 Signal/restart behavior

## 5.5 amd64/arm64

### Phase 5 Acceptance

- [ ] Canonical image runs independently under Docker.
- [ ] Image has exact version.
- [ ] No `latest` dependency.
- [ ] The same image supports headless daemon mode and UGOS UI/API/daemon mode safely.
- [ ] Clean shutdown leaves state recoverable.
- [ ] Both architecture images build.

---

# 75. Phase 6 — UPK Packaging

## Phase 6 TDD Gate

Before release packaging scripts are considered complete, automated tests SHALL validate:

- `project.yaml` schema/required fields;
- no `latest` image reference;
- exact image tag;
- exact architecture;
- expected `rootfs_common` contents;
- icon constraints;
- no secrets in staged package;
- OCI digest parity with published canonical image;
- version/build-number rules.

Hardware acceptance procedures for install/update/disable/uninstall SHALL be written before execution.

## 6.1 Finalize app ID

Freeze before public distribution.

## 6.2 `project.yaml`

Validate against current UGREEN schema.

## 6.3 Install parameters

Validate path parameter behavior.

## 6.4 Compose

Validate on hardware.

## 6.5 Icon

Produce compliant icon.

## 6.6 Export image tar

Per architecture.

## 6.7 Pack

Use:

```bash
ugcli pack --build <n>
```

## 6.8 Manual install

Test on authorized NAS.

## 6.9 Update test

Install version N, create state, update to N+1.

## 6.10 Disable/enable

## 6.11 Uninstall safety

### Phase 6 Acceptance

- [ ] amd64 UPK is generated.
- [ ] arm64 UPK is generated or clearly marked uncertified pending hardware.
- [ ] UPK installs through App Center manual installation.
- [ ] Icon appears on desktop.
- [ ] App opens in `inner` window.
- [ ] Upgrade preserves state/backups.
- [ ] Disable/enable is safe.
- [ ] Uninstall does not unexpectedly delete user backup data.
- [ ] Logs are accessible through documented UGOS path/tooling.
- [ ] The image digest bundled in each UPK matches the published OCI release for that architecture.

---

# 76. Phase 7 — Headless Docker Package

## Phase 7 TDD Gate

Write Docker/CLI integration tests first for:

- `check`;
- `run`;
- `daemon`;
- `status`;
- `retention --dry-run`;
- `reconcile`;
- `version`;
- persistent state;
- mounted SSH credentials;
- container replacement;
- absence of UGOS dependency;
- canonical image digest parity.

## 7.1 Headless publication profile

Publish the canonical image to the selected OCI registry.

Do not rebuild a separate CLI image.

## 7.2 Docker Compose example

## 7.3 Volume/secret documentation

## 7.4 Multi-arch publication

## 7.5 Gitea/OCI registry publication

Use the project's selected registry.

## 7.6 Terminal documentation

### Phase 7 Acceptance

- [ ] Canonical image runs headlessly without UGOS.
- [ ] `check`, `run`, `daemon`, `status`, `retention`, `reconcile`, and `version` work.
- [ ] Docker Compose example works.
- [ ] Version-pinned images are published.
- [ ] State survives container replacement.
- [ ] Exact architecture-specific image digest matches the corresponding UPK-bundled image.

---

# 77. Phase 8 — Security and Release Hardening

## Phase 8 TDD Gate

Phase 8 SHALL add adversarial tests before security fixes.

Any defect found during hardening SHALL first receive a failing regression test.

Required adversarial suites include:

- proxy/header spoofing;
- CSRF assumptions;
- path traversal;
- symlink escape;
- stale retention plan;
- duplicate POST/idempotency;
- concurrent admin sessions;
- operation restart;
- schema downgrade;
- state loss/catalog rebuild;
- secret-redaction;
- changed SSH host key;
- critical storage pressure;
- dependency/update regression.

## 8.1 Threat review

Specifically review:

- UGOS auth spoofing;
- direct port bypass;
- SSH key storage;
- host-key trust;
- CSRF;
- path traversal;
- API authorization;
- retention confirmation;
- support bundle redaction.

## 8.2 Dependency review

- Go;
- rclone;
- frontend;
- base image.

## 8.3 SBOM/checksum

## 8.4 Upgrade/migration/downgrade safety

Require:

- pre-migration state snapshot;
- schema compatibility metadata;
- startup refusal if database schema is newer than the binary can safely read;
- no destructive daemon start after migration failure;
- documented recovery.

## 8.5 Catalog/state-loss recovery

Test loss of SQLite/private app state while retained backup files survive.

Prove catalog reconstruction is non-destructive.

## 8.6 Proactive alerting

Before App Center 1.0, provide at least one proactive channel for:

- stale backup set;
- repeated backup failure;
- changed SSH host key;
- critical storage pressure.

Prefer an official UGOS local notification capability if documented and available. Otherwise implement a configurable, explicitly opt-in alternative such as a generic webhook/email adapter without mandatory cloud telemetry.

## 8.7 Resource tests

Measure:

- idle CPU;
- idle RSS;
- UI/API overhead;
- transfer resource usage.

### Phase 8 Acceptance

- [ ] Security review has no unresolved critical/high issue.
- [ ] No auth bypass is known.
- [ ] Secrets are redacted.
- [ ] SBOM/checksums produced.
- [ ] Upgrade failure is safe.
- [ ] Unsupported downgrade fails closed.
- [ ] Catalog can be rebuilt non-destructively after state loss.
- [ ] At least one proactive failure/staleness alert path exists for App Center 1.0.
- [ ] App does not materially interfere with ordinary NAS operation while idle.

---

# 78. Phase 9 — UGREEN App Center Readiness

## Phase 9 Acceptance-Test Gate

Before App Center submission, maintain a versioned release checklist that is treated as an executable/manual acceptance suite.

Automate where possible:

- required `project.yaml` metadata;
- privacy/license/source/support links present;
- no self-update path;
- no `latest`;
- no privileged configuration;
- SBOM/notices present;
- checksums/digests present;
- release built from signed/tagged source.

Manual review evidence SHALL be attached to the release issue.

## 9.1 Developer identity

Complete UGREEN requirements when the project is ready for public submission.

## 9.2 Listing metadata

Prepare:

- app description;
- icon;
- screenshots;
- support/help URLs;
- HTTPS privacy policy;
- first-launch privacy notice/consent behavior as required by target region/rules;
- license agreement / open-source notices;
- `source_code_link` / source-offer destination;
- third-party SDK/component disclosure;
- release notes;
- permission rationale;
- target distribution-region compliance checklist.

## 9.3 Review preflight

Verify:

- no self-update;
- no `latest`;
- no privileged mode;
- stable function;
- no unnecessary external network calls;
- no misleading system-like security UI;
- no destructive uninstall behavior;
- resource use acceptable.

## 9.4 Submission artifact

Build release UPKs from signed/tagged source.

## 9.5 Review feedback

Track requested changes as separate issues.

### Phase 9 Acceptance

- [ ] App Center submission checklist is complete.
- [ ] Release UPKs reproduce from tagged source.
- [ ] Privacy/support metadata exists.
- [ ] Required developer identity/materials are available.
- [ ] No application code is downloaded outside the reviewed update mechanism.
- [ ] App is ready for UGREEN submission.

---

# 79. Cross-Phase Acceptance Criteria

In addition to functional completion, every applicable child issue SHALL demonstrate the RED → GREEN → REFACTOR → integration/regression cycle defined in this EPIC.

This EPIC is complete when:

- [ ] The predecessor backup-manager core remains the only lifecycle engine.
- [ ] The UI communicates only through the application service/API.
- [ ] The UGOS edition is packaged as a Docker Application `.UPK`.
- [ ] The headless Docker edition is separately consumable.
- [ ] Both distributions use the same architecture-specific canonical OCI image digest.
- [ ] UGOS authentication is used rather than a second local password database.
- [ ] The UPK release is admin-only initially.
- [ ] Direct access cannot bypass UGOS authentication.
- [ ] The UI supports source configuration, host trust, retention, status, operations, restore-point visibility, and quarantine.
- [ ] The UI cannot directly trigger arbitrary remote deletion.
- [ ] Remote source deletion remains governed by predecessor lifecycle invariants.
- [ ] UPK images are bundled and version-pinned.
- [ ] UPK runtime does not self-update application code.
- [ ] amd64 build works.
- [ ] arm64 build works.
- [ ] At least one target UGOS device has completed developer UPK testing.
- [ ] Each architecture claimed as certified has been tested on representative hardware.
- [ ] UPK updates preserve state and backups.
- [ ] App state loss does not make retained backup artifacts unusable; catalog reconstruction is supported.
- [ ] Headless Docker container survives update/replacement with mounted state.
- [ ] UI/API/daemon terminate safely.
- [ ] Security test suite passes.
- [ ] Documentation covers both distributions.
- [ ] App Center readiness documentation includes OSS/privacy/source-link requirements.
- [ ] App Center 1.0 has proactive stale/failure alerting.

---

# 80. Non-Goals

This EPIC does NOT require:

- implementing backup creation;
- replacing the predecessor lifecycle engine;
- forking rclone;
- creating a second SQLite schema for UI;
- direct browser access to SQLite;
- direct browser access to rclone;
- arbitrary remote file browser;
- general-purpose SFTP client UI;
- restore execution into production systems;
- cloud account/hosted SaaS;
- telemetry;
- automatic unreviewed UPK self-updates;
- full mobile UGOS UI;
- public App Center approval itself.

The EPIC makes the app **submission-ready**; UGREEN's external review decision is outside repository control.

---

# 81. Failure-Safety Invariants Inherited from Parent EPIC

The UI and packaging MUST preserve all predecessor invariants, especially:

1. Never delete the remote source before a verified, durably committed NAS copy exists.
2. Never use rclone `move` to bypass manager-controlled sequencing.
3. Never treat `.partial` as a restore point.
4. Never overwrite a known-good backup with an unverified transfer.
5. Never delete a remote object if its identity changed after discovery.
6. Never prune outside the configured backup root.
7. Never prune the last known-good backup solely because of age.
8. Every lifecycle operation must be restart-safe.
9. Network uncertainty preserves data.
10. Required validation failure preserves the remote source.
11. Process liveness is not evidence of backup freshness.
12. UI convenience MUST NOT weaken a backend safety invariant.

If a UI requirement conflicts with one of these rules, the safety invariant wins.

---

# 82. Additional UI Safety Invariants

TDD itself is part of the safety system. Any change to authentication, deletion, retention, persistence, recovery, or migration behavior requires a failing safety/regression test before implementation.

1. **The frontend never decides whether a backup may be deleted.**
2. **The frontend never receives or stores persisted SSH private key material after import/generation.**
3. **UGOS authorization is enforced by the backend, not only the UI.**
4. **The UPK container must not expose an unauthenticated destructive API to the LAN.**
5. **A changed SSH host key requires explicit administrator intervention.**
6. **Retention preview is non-destructive.**
7. **Deleting a backup-set configuration does not imply deleting stored backup artifacts.**
8. **UPK updates must preserve user data/state or fail safely.**
9. **The App Center edition does not silently replace reviewed code.**
10. **All package variants report enough version/digest information to prove runtime parity.**
11. **UGOS identity headers are trusted only from the proven system-gateway source.**
12. **A browser/request disconnect cannot implicitly cancel a durable server operation.**
13. **Retention never applies a different deletion plan than the administrator confirmed.**
14. **Private SSH credentials never reside in the user backup root.**
15. **Loss of app state must not make retained backup artifacts unusable.**

---

# 83. Documentation Deliverables

Create/update:

```text
tools/backup-manager/README.md
tools/backup-manager/docs/ugos-development.md
tools/backup-manager/docs/ugos-install.md
tools/backup-manager/docs/docker.md
tools/backup-manager/docs/api.md
tools/backup-manager/docs/security.md
tools/backup-manager/docs/app-center-release.md
tools/backup-manager/docs/testing-tdd.md
```

## `ugos-development.md`

Document:

- developer portal;
- Debian 12 environment;
- `ugcli`;
- developer NAS authorization;
- React/JSSDK;
- package structure;
- local/hardware test loop.

## `ugos-install.md`

Document:

- Docker dependency;
- manual UPK installation;
- storage-folder selection;
- first-run wizard;
- update;
- disable/enable;
- uninstall/data behavior.

## `docker.md`

Document:

- canonical OCI image;
- headless command profile;
- Compose;
- volumes;
- config;
- secrets;
- upgrade;
- architecture support.


## `testing-tdd.md`

Document:

- RED/GREEN/REFACTOR workflow;
- required test taxonomy;
- how to run focused and full suites;
- test fixtures;
- SFTP integration environment;
- UGOS hardware acceptance evidence;
- bug-fix regression-test rule;
- destructive-safety specification examples;
- PR/agent TDD evidence template.

## `app-center-release.md`

Document:

- app ID;
- version/build rules;
- UPK build;
- release checklist;
- submission metadata;
- no-self-update policy.

---

# 84. Architecture Decision Records

Create ADRs for at least:

```text
ADR-UGOS-001
Use UGOS Docker Application UPK instead of a separate native backend

ADR-UGOS-002
Use UGOS inner-window authentication and no separate local user database

ADR-UGOS-003
Bundle reviewed version-pinned image into UPK; no runtime self-update

ADR-UGOS-004
Use one canonical OCI image for both UPK and headless Docker distribution

ADR-UGOS-005
Trust UGOS identity only through a proven trusted-proxy boundary

ADR-UGOS-006
Use durable operation polling rather than native EventSource for v1

ADR-UGOS-007
Bind retention confirmation to immutable plan IDs and inventory revisions

ADR-UGOS-008
Separate private app state from retained backup storage and support catalog rebuild

ADR-UGOS-009
Require test-driven development and safety-specification tests for all implementation work
```

The parent ADR for embedding rclone remains authoritative.

---

# 85. Open Implementation Questions / Phase-0 Gates

These questions MUST be resolved experimentally against current UGOS before final production packaging:

1. Can a Docker Application published with `open_type: inner` use the UGOS authenticated gateway while the container port is host-loopback-only?
2. What exact backend user identity headers are provided by the current UGOS gateway, and what is the supported validation/trust boundary?
3. What is the best persistent-volume pattern for Docker Apps across App Center update?
4. What exactly happens to bind mounts/app-data on uninstall?
5. Which minimum `depend_fw_version` should be declared for the specific auth/permission capabilities used?
6. Which minimum `depend_docker_version` should be declared for public distribution?
7. Are additional UGOS permissions required for LAN-only SFTP access beyond `NETWORK.ACCESS_INTERNET`?
8. How does `only_admin` interact with Docker App `inner` windows on current firmware?
9. Does `allow_add_access_path` provide a better backup-root UX than a `path` installation parameter for this application?
10. Are there App Center package-size constraints relevant to bundled architecture image tarballs?
11. What is the exact source address/network identity of a gateway-proxied Docker-App request?
12. Can the private UGOS app data directory be bind-mounted into a Docker App through a documented/stable mechanism?
13. Does the App Center review require first-launch privacy consent for the UGOS user identity/audit processing performed by this app?
14. What source-code/source-offer URL format will satisfy current UGREEN open-source-component rules for a product whose repository may not itself be public?
15. Is there an official UGOS notification capability suitable for backup-failure/staleness alerts?

Do not paper over an unresolved security-sensitive question with an assumption.

---

# 86. Recommended Implementation Sequence

Dependency order:

```text
Parent backup-manager core complete/stable enough
        ↓
Phase 0 UGOS PoC/security gate
        ↓
Phase 1 API/auth
        ↓
Phase 2 frontend foundation
        ↓
Phase 3 read-only UI
        ↓
Phase 4 administrative UI
        ↓
Phase 5 canonical production image
        ↓
Phase 6 UPK packaging
        ├───────────────┐
        ↓               ↓
Phase 7 headless Docker   Phase 8 hardening
        └───────┬───────┘
                ↓
      Phase 9 App Center readiness
```

The headless Docker package can begin earlier if desired because it has fewer dependencies on the UI.

---


# 86A. Mandatory Child-Issue TDD Template

Every implementation child issue created from this EPIC SHALL include:

```markdown
## Behavioral Contract

### Given / When / Then
- GIVEN ...
- WHEN ...
- THEN ...

## TDD Plan

### RED
- [ ] Add failing unit/component/contract test(s)
- [ ] Run and confirm expected failure

### GREEN
- [ ] Implement minimum production behavior
- [ ] Focused tests pass

### REFACTOR
- [ ] Refactor without behavior change
- [ ] Focused tests remain green

### INTEGRATION
- [ ] Add/run boundary integration test(s) where applicable
- [ ] Add/run E2E or hardware acceptance test where applicable

### REGRESSION
- [ ] Owning package/module suite passes
- [ ] Affected integration suite passes
- [ ] Security/destructive-safety suite passes where applicable

## Acceptance Criteria
- [ ] ...
```

A coding agent SHALL NOT mark a child issue complete by supplying production code alone.

For a bug:

```text
reproduce bug in failing test
        ↓
fix
        ↓
prove regression test passes
        ↓
run affected regression suites
```

For a hardware-only UGOS issue:

```text
write acceptance procedure first
        ↓
build PoC
        ↓
execute on authorized NAS
        ↓
record evidence
        ↓
accept/reject architecture assumption
```


# 87. Suggested Gitea Child Issues

This EPIC SHOULD be decomposed into child issues approximately as follows:

### Phase 0

Each issue below MUST include a prewritten automated test or hardware acceptance procedure.

- [ ] UGOS developer environment and authorized test device
- [ ] Minimal Docker App UPK PoC
- [ ] UGOS `inner` React/JSSDK PoC
- [ ] UGOS gateway authentication PoC
- [ ] Direct-port authentication bypass test
- [ ] UGOS persistent-path/update/uninstall experiment
- [ ] amd64/arm64 packaging experiment

### API/Core Adapter

- [ ] Establish API contract-test harness and TDD fixtures
- [ ] Establish SQLite durable-operation test fixtures
- [ ] Establish trusted-proxy/auth spoof test fixtures
- [ ] Establish idempotency/config-revision test fixtures
- [ ] Establish retention-plan immutable-token test fixtures
- [ ] Finalize `BackupService` UI use-case surface
- [ ] HTTP API skeleton
- [ ] UGOS authenticator/authorizer
- [ ] Operations model
- [ ] Durable operations + idempotency
- [ ] Authenticated operation polling
- [ ] Retention plan-id / inventory-revision contract
- [ ] Configuration revision / optimistic concurrency
- [ ] API contract and security tests

### Frontend

- [ ] Establish frontend component-test harness and mock API
- [ ] Establish Playwright E2E harness
- [ ] React/UGOS frontend scaffold
- [ ] API client + UGOS token integration
- [ ] App shell/navigation
- [ ] Dashboard
- [ ] Backup Sets list/detail
- [ ] Add/Edit Backup Set wizard
- [ ] SSH key management UI
- [ ] Host-key trust UI
- [ ] Backups and artifact detail
- [ ] Retention editor/preview/apply
- [ ] Activity/operations
- [ ] Quarantine
- [ ] Settings/Diagnostics
- [ ] UI E2E tests

### Packaging

- [ ] Establish package/image verification test harness
- [ ] Canonical production OCI image
- [ ] Headless publication profile using canonical image
- [ ] Multi-arch build
- [ ] UGOS `project.yaml`
- [ ] UGOS Docker Compose
- [ ] UGOS icon
- [ ] UPK staging automation
- [ ] `ugcli pack` CI
- [ ] UPK update/disable/uninstall tests
- [ ] Headless Docker Compose example
- [ ] Registry publication

### Release/App Center

- [ ] Establish release acceptance checklist/test runner
- [ ] SBOM/checksum/release provenance
- [ ] Security review
- [ ] App Center metadata
- [ ] Privacy/help/release documentation
- [ ] OSS license/source-code-link compliance
- [ ] Catalog rebuild/state-loss recovery
- [ ] Proactive alerting
- [ ] UGREEN submission preflight

---

# 88. Definition of Done

The Definition of Done also requires that every applicable implementation child issue was completed under the mandatory TDD contract. A green final test suite without evidence of test-first behavior does not satisfy this EPIC's engineering process requirement.

The EPIC is done when an administrator can choose either of these supported deployment paths.

## Path A — UGOS

```text
download/install .UPK
        ↓
App Center installs Docker dependency/app
        ↓
Backup Manager icon appears
        ↓
open inside UGOS desktop
        ↓
UGOS session authenticates user
        ↓
configure source
        ↓
trust SSH host
        ↓
run backup
        ↓
observe transfer/verification
        ↓
see retained backup
        ↓
preview/apply retention
```

without needing a terminal for normal operation.

## Path B — Docker / Terminal

```text
docker pull canonical versioned image
        ↓
mount config/state/backups
        ↓
backup-manager check
        ↓
backup-manager daemon
        ↓
manage via CLI
```

without requiring UGOS.

Both paths MUST produce the same lifecycle decisions and safety behavior for the same configuration and backup inventory.

For the same version/architecture, both paths MUST execute the same canonical image digest.

---

# 89. Official UGREEN References

The implementation team SHALL re-check these documents at development and release time because UGOS developer interfaces can evolve.

- UGREEN NAS Developer Platform  
  https://developer.ugnas.com/

- Development preparation  
  https://developer.ugnas.com/en/doc/backend/quick-start/prepare

- Docker Application packaging  
  https://developer.ugnas.com/doc/backend/quick-start/develop-docker-app.html

- `project.yaml` configuration  
  https://developer.ugnas.com/doc/tools/project-yaml.html

- Application open mode  
  https://developer.ugnas.com/doc/backend/application/open-type.html

- UGOS login authentication integration  
  https://developer.ugnas.com/doc/backend/system-capabilities/login-auth.html

- Application runtime environment / permissions  
  https://developer.ugnas.com/doc/backend/application/runtime-environment.html

- UGREEN frontend samples  
  https://developer.ugnas.com/en/doc/backend/quick-start/my-apps.html

- `@ugreen-nas/builder-open`  
  https://developer.ugnas.com/en/doc/frontend/ugos-builder/

- UGOS Core frontend SDK  
  https://developer.ugnas.com/doc/frontend/ugos-core/install.html

- App testing  
  https://developer.ugnas.com/doc/backend/quick-start/testing.html

- UGOS App Center manual installation  
  https://support.ugnas.com/detail/article/en-US/116

- UGREEN Docker  
  https://support.ugnas.com/detail/article/en-US/236

- UGREEN container applications  
  https://support.ugnas.com/detail/article/en-US/539

- App review / developer rules  
  https://developer.ugnas.com/doc/review/app-review/audit-key-points.html

- Runtime environment / app data behavior  
  https://developer.ugnas.com/doc/backend/application/runtime-environment.html

- Open-source/compliance fields in `project.yaml`  
  https://developer.ugnas.com/doc/tools/project-yaml.html

- rclone license (MIT)  
  https://github.com/rclone/rclone/blob/master/docs/content/licence.md

---


# 89A. Consensus Revision Change Log

The adversarial panel required these substantive changes from the prior draft:

- replaced dual UGOS/CLI image builds with one canonical image and dual distribution profiles;
- removed normal user-selected `APP_DATA_PATH`;
- made private state vs backup data separation mandatory;
- added state-loss/catalog reconstruction requirement;
- changed native SSE to authenticated polling for v1;
- made long-running operations durable and independent of browser/request lifetime;
- added idempotency keys;
- added configuration revisions and immutable operation config snapshots;
- added immutable retention `plan_id` and stale-plan rejection;
- strengthened trusted UGOS gateway/proxy enforcement;
- added SSH key referential-integrity requirements;
- reduced the setup wizard from twelve micro-steps to six grouped steps;
- made remote-source deletion disclosure/acknowledgement mandatory;
- demoted stable-size completion to an explicitly heuristic advanced mode;
- renamed user-facing "Restore Points" to "Backups/Retained Backups" because restore execution is out of scope;
- added proactive alerting as an App Center 1.0 gate;
- added pre-migration snapshot/schema downgrade safety;
- added mandatory OSS license/source/privacy/support metadata;
- added exact OCI digest parity between the headless and UPK distributions.
- made strict RED → GREEN → REFACTOR TDD mandatory for every implementation child issue;
- made regression-test-first mandatory for all bug fixes;
- added per-phase test-first gates and hardware acceptance-first procedures;
- added a mandatory Gitea child-issue TDD template and PR/agent evidence requirements.


# 90. Final Architecture

```text
                         SOURCE CODE
                  tools/backup-manager/
                           │
                           ▼
                   BackupService/Core
                           │
          ┌────────────────┼────────────────┐
          │                │                │
          ▼                ▼                ▼
         CLI             HTTP API        Scheduler
          │                │
          │                ▼
          │          React UGOS UI
          │                │
          ▼                ▼
   Canonical OCI image ──┬── headless Docker
                           └── same digest
                                  │
                                  ▼
                              image tar
                                  │
                                  ▼
                            ugcli package
                                  │
                                  ▼
                            .UPK / App Center
```

There SHALL be one backup lifecycle implementation.

UGOS provides the operator experience and package lifecycle.

Docker provides the portable runtime.

The UI provides visibility and safe administrative workflows.

The predecessor backup-manager core remains the authority for whether a remote backup may be deleted and which local restore points must survive.

The test suite is part of that authority: safety-critical behavior is specified in failing tests before implementation and remains permanently regression-protected.
