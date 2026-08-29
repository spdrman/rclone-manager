# EPIC: Multi-NAS Backup Manager Apps — Provider-Neutral Core, UGOS/Synology/TrueNAS/Unraid/OpenMediaVault/Proxmox Layers — Lean TDD Revision

## Status

**Type:** EPIC / Detailed implementation specification  
**Repository:** `iasbuilt/iac`  
**Parent / predecessor EPIC:** `Embedded-rclone NAS Backup Lifecycle Manager — UI-Ready Architecture`  
**Primary implementation root:** `tools/backup-manager/`  
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

Build a **provider-neutral backup-manager core** and a family of thin NAS-platform application layers.

The core SHALL remain independent of UGOS, Synology DSM, TrueNAS, Unraid, OpenMediaVault, Proxmox VE, or any other NAS/hypervisor UI.

The predecessor EPIC established the core backup guarantees:

- Go implementation;
- embedded rclone Go packages;
- SQLite lifecycle journal;
- explicit copy → verify → durable commit → delete sequencing;
- deterministic GFS retention;
- validation/quarantine;
- backup freshness/health;
- restart/reconciliation safety;
- strict TDD.

This EPIC adds:

1. a stable provider-neutral application/service boundary;
2. a shared provider-neutral web UI;
3. a provider-adapter contract;
4. separate provider application directories;
5. packaging/distribution layers for multiple NAS platforms.

The architectural objective is:

> **Add a new NAS OS by creating a new provider app under `apps/<provider>/` without modifying backup lifecycle logic.**

UGOS is the first fully integrated provider, not part of the core.

This EPIC SHALL support the following deployment/provider targets at the indicated depth:

| Target | In-scope support |
|---|---|
| Generic Docker/Linux | Full headless + generic Web UI |
| UGREEN UGOS Pro | Full provider integration + `.UPK` |
| TrueNAS | Native Apps catalog/custom-app packaging around canonical OCI image |
| Unraid | Community Applications/Docker-template packaging around canonical OCI image |
| OpenMediaVault | Supported container/Compose provider package; native OMV plugin deferred |
| Synology DSM | `.spk` provider package using the same core binary/shared UI; DSM-native SSO may follow later |
| Proxmox VE | Supported deployment/appliance profile with shared Web UI; native PVE GUI plugin is not required |

The provider layers MUST NOT fork lifecycle, retention, validation, transfer, or state-management behavior.

---

# 2. Core Product Principle

The dependency direction SHALL be one-way:

```text
                       PROVIDER APPS
                            │
           ┌────────────────┼────────────────┐
           │                │                │
           ▼                ▼                ▼
        UGOS App       Synology App      Generic Web App
           │                │                │
           │                │        ┌───────┼─────────────┐
           │                │        │       │             │
           │                │        ▼       ▼             ▼
           │                │     TrueNAS  Unraid         OMV
           │                │                          / Proxmox
           │                │
           └────────────┬───┴───────────────┐
                        ▼                   ▼
                 shared Web UI       platform adapters
                        │                   │
                        └─────────┬─────────┘
                                  ▼
                         PROVIDER-NEUTRAL CORE
                    BackupService / lifecycle engine
                                  │
                 ┌────────────────┼────────────────┐
                 ▼                ▼                ▼
               SQLite          rclone          retention
```

Mandatory dependency rule:

```text
apps/* ─────► core
ui/*   ─────► provider-neutral API/contracts

core ─X─► apps/*
core ─X─► UGOS SDK
core ─X─► Synology SDK
core ─X─► TrueNAS APIs
core ─X─► Unraid APIs
core ─X─► OMV APIs
core ─X─► Proxmox APIs
```

The core SHALL build and pass all tests with every provider-app directory removed.

Provider apps may add:

- authentication integration;
- provider navigation/window integration;
- storage-selection integration;
- notification integration;
- packaging metadata;
- provider-specific install/update hooks;
- provider branding/help links.

Provider apps MUST NOT implement:

- transfer policy;
- retention decisions;
- remote deletion decisions;
- validation semantics;
- lifecycle state transitions;
- backup catalog truth.

---

# 3. Architectural Decision: Provider Apps on Top of the Core

## 3.1 Decision

The repository SHALL separate:

```text
core
shared UI
provider apps
distribution packaging
```

UGOS SHALL live under its own provider directory and SHALL NOT own the shared UI or application-service API.

## 3.2 Provider app model

Each provider app is a thin composition layer:

```text
provider package / launcher
          │
          ├── provider auth adapter
          ├── provider capability adapter
          ├── provider bootstrap/UI bridge
          ├── provider packaging
          │
          ▼
      shared Web UI
          │
          ▼
      BackupService
          │
          ▼
 provider-neutral core
```

## 3.3 Core API boundary

The core SHALL expose a stable application API suitable for:

- CLI;
- generic HTTP/Web host;
- UGOS app;
- Synology app;
- future NAS providers.

Provider code SHALL interact through exported application contracts rather than reaching into lifecycle internals.

## 3.4 Platform capability contract

Define a capability model similar to:

```go
type PlatformCapabilities struct {
    NativeAuth          bool
    NativeNotifications bool
    StoragePicker       bool
    EmbeddedWindow      bool
    AppStorePackaging   bool
}

type PlatformAdapter interface {
    ID() string
    Capabilities() PlatformCapabilities
    Authenticator() Authenticator
    Notifier() Notifier
    PlatformInfo(ctx context.Context) (PlatformInfo, error)
}
```

Exact interfaces may differ.

Unsupported capabilities SHALL be explicit rather than emulated incorrectly.

## 3.5 Shared frontend platform bridge

The shared React UI SHALL use a provider bridge similar to:

```ts
export interface PlatformBridge {
  id: string;
  getAuthContext(): Promise<AuthContext>;
  capabilities(): PlatformCapabilities;
  openExternal?(url: string): Promise<void>;
}
```

Provider SDK imports MUST remain inside provider-specific UI/bootstrap packages.

## 3.6 Authentication modes

Support at least:

```text
platform-auth
local-auth
```

`platform-auth` is used only where a provider offers a sufficiently secure/documented integration, initially UGOS.

`local-auth` is the reusable fallback for generic Docker, TrueNAS, Unraid, OpenMediaVault, Proxmox VE, and Synology until/if a native provider-auth adapter is implemented.

Local authentication SHALL use:

- secure password hashing (Argon2id or equivalent);
- secure HTTP-only session cookies;
- CSRF protection;
- rate limiting;
- one-time bootstrap/enrollment flow;
- no plaintext password persistence.

Provider-native authentication may replace local auth only after its trust boundary passes provider-specific security tests.

## 3.7 Release artifact hierarchy

The canonical release primitive SHALL be the provider-neutral Go binary per architecture:

```text
backup-manager binary
       │
       ├── canonical OCI image
       │      ├── Generic Docker
       │      ├── TrueNAS
       │      ├── Unraid
       │      ├── OpenMediaVault
       │      ├── Proxmox deployment profile
       │      └── UGOS Docker App → UPK
       │
       └── Synology SPK
              └── exact same core binary digest
```

A release manifest SHALL record:

- semantic version;
- Git commit;
- core binary SHA-256 per architecture;
- OCI image digest per architecture;
- provider-package digest;
- provider package version.

Provider packages MUST NOT silently rebuild different core behavior.

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


# 4A. Provider Packaging Survey and Scope Decision

Current platform models justify supporting multiple providers in this EPIC, but not at identical integration depth.

## UGREEN UGOS Pro — Full provider integration

UGOS supports Docker applications packaged as `.UPK`, application metadata, `open_type: inner`, and provider authentication integration.

**This EPIC:** full provider app.

## Synology DSM — Native package adapter

Synology DSM supports `.spk` packages, Package Center installation, lifecycle scripts, application privileges, and DSM desktop launch integration.

**This EPIC:** build/test an `.spk` wrapper containing the exact provider-neutral core binary and shared Web UI.

Initial DSM package MAY use reusable `local-auth`.

Native DSM SSO/session integration is a follow-on capability unless a secure supported contract is proven during implementation.

## TrueNAS — Container-native Apps catalog

Current TrueNAS Apps are Docker/Compose based. Custom apps can deploy OCI containers and the Apps catalog is open to contributions.

**This EPIC:** provide:

- custom-app/Compose deployment;
- TrueNAS Apps catalog metadata/templates suitable for contribution;
- storage mappings;
- portal/Web UI metadata;
- generic `local-auth`.

No separate TrueNAS lifecycle engine.

## Unraid — Container-native Community Applications

Unraid's Apps/Community Applications ecosystem deploys Docker containers and stores Docker template configuration.

**This EPIC:** provide:

- Unraid Docker template/Community Applications metadata;
- appdata mapping;
- backup-root mapping;
- WebUI link;
- generic `local-auth`.

No Unraid plugin is required for v1.

## OpenMediaVault — Container support now, native plugin later

OpenMediaVault supports plugins and recommends containerized software where practical.

A native OMV plugin would require its declarative Workbench/plugin framework and is not necessary to run the product.

**This EPIC:** provide:

- supported OCI/Compose deployment;
- OMV-oriented storage/configuration documentation;
- generic `local-auth`.

**Deferred:** native OMV Workbench plugin/navigation integration.

## Proxmox VE — Deployment target, not a NAS app store

Proxmox VE provides VM/LXC appliance/container-template infrastructure but does not provide an equivalent third-party NAS application store/UI integration model for this use case.

**This EPIC:** provide a supported deployment profile:

- unprivileged LXC or dedicated VM/container-host guidance;
- storage bind/mount guidance;
- shared Web UI;
- generic `local-auth`.

**Deferred:** any unsupported/fragile Proxmox Web UI plugin.

## Support tiers

```text
Tier A — fully integrated provider app
  UGOS

Tier B — provider package/catalog wrapper
  Synology
  TrueNAS
  Unraid

Tier C — supported provider deployment profile
  OpenMediaVault
  Proxmox VE
  Generic Docker/Linux
```

The architecture SHALL make promotion from Tier C → B → A possible without changing core backup logic.

---

# 4B. Mandatory Test-Driven Development Contract

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

# 4C. TDD Engineering Invariants

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

# 4D. Test Taxonomy

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

# 4E. Coverage Policy

Coverage percentage is not a substitute for behavioral quality.

However:

- core pure-logic packages SHOULD target high branch coverage;
- safety-critical lifecycle/retention/path/auth code SHOULD have near-complete branch coverage where practical;
- destructive paths MUST have explicit scenario coverage;
- changed production code without relevant test coverage SHALL fail review.

CI SHOULD publish coverage reports and MAY enforce package-specific thresholds once a stable baseline is established.

---

# 4F. Required TDD Evidence in Pull Requests / Agent Work

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



# 4G. Lean Delivery Principle

This revision deliberately reduces implementation-management overhead without weakening TDD or safety.

The project SHALL prefer **vertical slices** over layer-by-layer micro-issues.

A vertical slice should, where practical, include all required pieces for one operator-visible capability:

```text
behavior/specification
      ↓
service/domain
      ↓
API
      ↓
frontend
      ↓
integration/E2E
      ↓
acceptance
```

Do NOT create separate child issues merely because code lives in different layers when those layers are part of the same coherent behavior.

Examples:

**Prefer:**

```text
FEATURE: Backup Set Management
  - service behavior
  - API
  - UI
  - tests
```

over:

```text
Create backup-set DTO
Create backup-set API route
Create backup-set service method
Create backup-set React list
Create backup-set tests
```

The target decomposition for this EPIC is approximately:

```text
5 delivery phases
15–20 substantive child issues
```

Each child issue remains subject to the mandatory RED → GREEN → REFACTOR → INTEGRATE → REGRESSION → ACCEPT contract.

---

# 5. Product Deliverables

## D-1 — Provider-Neutral Core

Produce:

- reusable Go core/application module;
- headless CLI/daemon executable;
- `BackupService`;
- state/lifecycle/retention/validation engine;
- rclone adapter;
- provider-neutral tests.

The core SHALL contain no provider SDK dependencies.

## D-2 — Shared Web UI

Produce a provider-neutral React/TypeScript application under `ui/shared/`.

It SHALL contain normal backup-manager product UI.

Provider-specific bootstrap code SHALL not live here.

## D-3 — Generic Web Host

Produce a reusable generic Web host that:

- imports/hosts the core;
- serves shared UI;
- provides local authentication;
- exposes versioned API;
- can run on ordinary Docker/Linux.

This becomes the default host for TrueNAS, Unraid, OpenMediaVault, and Proxmox deployments.

## D-4 — UGOS Provider App

Produce:

```text
apps/ugos/
```

including:

- UGOS platform/auth adapter;
- UGOS frontend bridge;
- `.UPK` packaging;
- UGREEN metadata;
- hardware tests.

## D-5 — Synology Provider App

Produce:

```text
apps/synology/
```

including:

- `.spk` package definition;
- DSM desktop launcher integration;
- lifecycle scripts;
- provider metadata/icons;
- shared UI/generic auth host integration;
- architecture packaging/tests.

## D-6 — TrueNAS Provider App

Produce:

```text
apps/truenas/
```

including:

- Docker Compose/custom-app deployment;
- TrueNAS Apps catalog contribution structure/templates;
- storage/network/portal configuration;
- tests.

## D-7 — Unraid Provider App

Produce:

```text
apps/unraid/
```

including:

- Docker template XML;
- Community Applications metadata;
- appdata/backup-root mappings;
- WebUI metadata;
- tests.

## D-8 — OpenMediaVault Provider Profile

Produce:

```text
apps/openmediavault/
```

including:

- supported Compose/deployment assets;
- storage-path guidance;
- Web UI launch documentation;
- tests.

Native OMV plugin is deferred.

## D-9 — Proxmox VE Provider Profile

Produce:

```text
apps/proxmox/
```

including:

- supported LXC/VM/container-host deployment profile;
- storage mapping guidance;
- Web UI exposure;
- lifecycle/update documentation;
- tests.

Do not install arbitrary application services directly into the Proxmox host OS unless officially supported.

## D-10 — Canonical OCI Distribution

Produce a multi-architecture OCI image for:

- Generic Docker/Linux;
- UGOS Docker App;
- TrueNAS;
- Unraid;
- OpenMediaVault;
- Proxmox deployment profiles.

## D-11 — Compliance and Release Artifacts

Produce:

- third-party/open-source notices;
- license inventory;
- privacy/support materials;
- SBOM;
- checksums;
- binary hashes;
- OCI digests;
- provider package digests;
- release manifest.

---

# 6. Product Naming

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

The provider separation is mandatory.

Target structure:

```text
tools/
  backup-manager/
    README.md
    go.work

    core/
      go.mod
      cmd/
        backup-manager/
      app/
        service.go
        operations.go
      domain/
      config/
      discovery/
      lifecycle/
      retention/
      validation/
      state/
      health/
      transport/
        rclone/
      migrations/
      tests/

    ui/
      shared/
        package.json
        pnpm-lock.yaml
        src/
          app/
          api/
          components/
          features/
          pages/
          hooks/
          platform/
            bridge.ts
            capabilities.ts
        tests/

    apps/
      common/
        webhost/
        auth/
          local/
        platform/
          capabilities/
        tests/

      ugos/
        backend/
        frontend/
          platform-bridge/
        packaging/
          upk/
            project.yaml
            rootfs_common/
            rootfs_amd64/
            rootfs_arm64/
        tests/

      synology/
        backend/
        frontend/
          platform-bridge/
        packaging/
          spk/
            INFO.sh
            SynoBuildConf/
            scripts/
            conf/
            ui/
        tests/

      truenas/
        packaging/
          compose/
          catalog/
            app.yaml
            questions.yaml
            templates/
        tests/

      unraid/
        packaging/
          templates/
        tests/

      openmediavault/
        packaging/
          compose/
        tests/

      proxmox/
        packaging/
          lxc/
          compose/
        tests/

      generic/
        packaging/
          docker/
          compose/
        tests/

    release/
      manifest/
      sbom/
      notices/

    docs/
      architecture.md
      provider-apps.md
      testing-tdd.md
      security.md
      providers/
        ugos.md
        synology.md
        truenas.md
        unraid.md
        openmediavault.md
        proxmox.md
        docker.md
```

Exact file names MAY vary, but the following top-level separation MUST remain:

```text
core/
ui/shared/
apps/<provider>/
```

## 7.1 Dependency enforcement

CI SHALL enforce:

- `core/` imports no code from `apps/`;
- `core/` imports no provider SDK;
- `ui/shared/` imports no provider SDK;
- provider SDK imports exist only under that provider's directory;
- each provider app can be removed without breaking core tests;
- each provider app passes a common provider conformance suite.

## 7.2 Core public boundary

Because provider apps live outside the core module, the core SHALL expose only the minimum stable application interfaces required by:

- CLI;
- API/web host;
- provider apps.

Do not expose lifecycle internals simply to make provider code convenient.

---

# 8. Canonical Core / Multiple Provider Distributions Rule

There SHALL be one authoritative core implementation.

A release SHALL produce:

```text
core binary per architecture
     │
     ├── canonical OCI image
     │
     │    ├── Generic Docker
     │    ├── UGOS UPK
     │    ├── TrueNAS
     │    ├── Unraid
     │    ├── OpenMediaVault
     │    └── Proxmox deployment
     │
     └── Synology SPK
```

Provider-specific packages MAY contain different:

- metadata;
- launchers;
- authentication adapters;
- provider bridge code;
- icons;
- install/update scripts;
- storage configuration.

Provider-specific packages MUST NOT contain different:

- lifecycle logic;
- retention algorithm;
- remote-delete policy;
- validation policy implementation;
- SQLite schema semantics;
- rclone transport behavior.

The release manifest SHALL prove core parity through binary hashes and image/package digests.

---

# 9. Runtime Modes

The provider-neutral core executable SHALL support at minimum:

```bash
backup-manager run
backup-manager daemon
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

## 9.2 Generic Web App host

The reusable Web host under `apps/common/webhost` SHALL compose:

```text
shared UI
local auth
HTTP API
BackupService/core
```

It SHALL be used by generic Docker and provider packages that do not yet implement secure native platform authentication.

## 9.3 UGOS Docker App default

The UPK Compose profile SHALL run the canonical image in a combined supervised mode such as:

```bash
backup-manager serve --with-daemon --auth-mode=ugos
```

Exact command naming may vary.

The HTTP server and background scheduler SHALL share a common application service and process shutdown context.

Failure of the HTTP listener MUST NOT bypass lifecycle safety.

---

# 10. Frontend Technology

The shared provider-neutral frontend SHALL use:

- React;
- TypeScript;
- Vite;
- the current supported `@ugreen-nas/core`;
- the current supported `@ugreen-nas/builder-open` where appropriate.

The shared UI SHALL remain provider-neutral. The UGOS provider bridge MAY begin from UGREEN's official React sample, but UGOS SDK imports MUST remain under `apps/ugos/`.

The UGOS provider app SHALL use `open_type: inner`.

Reason:

- native UGOS desktop-window experience;
- JSSDK support;
- UGOS login/session integration;
- no separate backup-manager password database.

The application SHOULD initially support the UGOS `pc` client target.

Mobile-specific UI support is a future enhancement unless it can be delivered with negligible incremental complexity.

---

# 11. Shared UI Build and Provider Composition

The shared frontend SHALL compile to static assets independently of any NAS provider SDK.

Conceptually:

```text
ui/shared
   │
   ├── generic build ─────► apps/common/webhost
   │
   ├── UGOS bridge ───────► apps/ugos
   │
   └── future provider bridges
```

Provider code SHALL supply platform bootstrap/auth/capability adapters.

The shared UI SHALL not contain imports such as:

```text
@ugreen-nas/*
Synology DSM SDK modules
TrueNAS-specific modules
Unraid-specific modules
OMV-specific modules
Proxmox-specific modules
```

Production provider hosts MAY embed the shared static assets into a Go executable using `go:embed`.

Node/pnpm remain build-time dependencies only.

The shared UI/API contract SHALL remain versioned and provider-neutral.

---

# 12. UGOS Provider Window Integration

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

# 13. UGOS Provider Authentication

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

# 13A. Reusable Local Authentication for Non-Integrated Providers

The reusable Web host SHALL provide local authentication for providers without a secure native-auth adapter.

Initial consumers:

```text
Generic Docker
TrueNAS
Unraid
OpenMediaVault
Proxmox VE
Synology DSM (until native DSM auth is implemented)
```

Requirements:

- first-run administrator enrollment;
- Argon2id or equivalent password hashing;
- HTTP-only secure session cookie;
- CSRF protection;
- brute-force/rate-limit protection;
- session invalidation;
- password rotation;
- no plaintext password persistence;
- no default/static password;
- provider packaging must not bake credentials into images.

A provider MAY later replace local auth with a native provider adapter without changing core application logic.

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

# 20. UGOS Provider Installation Parameters

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

# 21. UGOS Provider `project.yaml`

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

# 22. UGOS Provider Docker Compose

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
Administrator account creation   (local-auth only; skipped under platform-auth)
  ↓
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

## 49.1 Administrator account creation

Under `local-auth` (section 3.6), the very first thing an operator sees SHALL be
the one-time enrollment flow that creates the administrator account. Nothing
else in the product is reachable until it completes: not the dashboard, not a
read-only view, not the API beyond the enrollment route itself.

Under `platform-auth` this step SHALL be skipped entirely. On UGOS the operator
has already authenticated against the NAS, and prompting them to invent a second
credential would be both confusing and a worse security posture, since it
creates an account the platform does not know about and cannot revoke.

The enrollment flow is a security boundary, not a form, and it SHALL be designed
as one:

- **It is single-shot and irreversible.** Once an administrator exists,
  enrollment SHALL be permanently closed. It MUST NOT reopen because a
  configuration file was deleted, because the container restarted, or because
  the database was replaced with an empty one. Reaching a state where enrollment
  is available again SHALL require deliberate operator action that is
  indistinguishable from a fresh install.
- **Reaching the port SHALL NOT be sufficient to claim the account.** An
  unclaimed instance exposed on a network is the obvious attack: whoever
  connects first becomes the administrator. Enrollment SHALL therefore require a
  bootstrap secret that the operator can only obtain from somewhere they already
  control, for example printed to the container log on first start. That secret
  SHALL be single-use and SHALL expire.
- Password handling follows section 3.6: Argon2id or equivalent, no plaintext
  persistence, HTTP-only session cookies, CSRF protection, and rate limiting on
  both enrollment and login.
- Enrollment SHALL be rate-limited and SHALL log every attempt, successful or
  not, to the audit trail (section 58).

Backup setup remains skippable, per below. Account creation does not.

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

A single logical semantic version SHALL identify the core behavior.

Example:

```text
Core                         1.2.0
Shared UI                    1.2.0
Canonical OCI image          1.2.0
UGOS UPK                     1.2.0.<build>
Synology SPK                 1.2.0
TrueNAS app metadata         1.2.0
Unraid template              1.2.0
OMV deployment profile       1.2.0
Proxmox deployment profile   1.2.0
```

Provider package build numbers MAY differ, but every provider artifact SHALL declare:

- core semantic version;
- core binary hash;
- source Git commit.

A provider adapter MAY rev independently for packaging-only fixes only if the release manifest still proves the embedded core binary is unchanged.

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
- core binary SHA-256 per architecture;
- exact OCI image digest used by container-based providers;
- Synology SPK digest and embedded core-binary hash;
- provider package/manifest digests.

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
8. provider conformance;
9. real provider hardware/OS acceptance where applicable.

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

# 63A. Provider Conformance Test Suite

Every provider app/profile SHALL pass a common contract appropriate to its capabilities.

Test categories:

```text
provider identified correctly
core version reported
core binary parity proved
state path persists
backup root constrained
auth mode explicit
UI launches
API reachable only through intended path
provider removal does not alter core behavior
upgrade preserves state
uninstall/remove does not unexpectedly delete retained backups
```

Capability-specific tests apply conditionally:

```text
native auth
native notifications
embedded window
app-store packaging
storage picker
```

The conformance suite SHALL distinguish:

```text
SUPPORTED
UNSUPPORTED
NOT_APPLICABLE
```

rather than silently skipping missing provider features.

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

# 68. Provider Test Matrix

Provider acceptance procedures SHALL be written/version-controlled before manual execution.

Required certification targets:

| Provider | Required v1 evidence |
|---|---|
| UGOS | real authorized UGREEN NAS |
| Synology | representative DSM 7.x amd64 and/or arm64 model for each architecture claimed |
| TrueNAS | current supported TrueNAS release VM/hardware |
| Unraid | current supported Unraid release VM/hardware |
| OpenMediaVault | current OMV 8.x Debian-based test system |
| Proxmox VE | current PVE release test host/VM environment |
| Generic Docker | Linux amd64 + arm64 container tests |

Record:

- provider/OS version;
- hardware/model where relevant;
- architecture;
- package/image version;
- install result;
- auth result;
- storage result;
- update result;
- uninstall/removal result;
- retained-backup safety;
- evidence.

A provider/architecture SHALL be described as **build-supported but uncertified** until its required acceptance test is completed.

---

# 69. Phase 1 — Core/Provider Architecture Foundation + UGOS Proof

## Objective

First enforce the provider-neutral architecture, then prove the first provider (UGOS) end-to-end on real hardware.

The core and shared UI MUST be structurally independent of UGOS before the UGOS package is considered successful.

This phase combines the previous:

- UGOS feasibility work;
- authentication PoC;
- private state/storage experiments;
- minimal API foundation;
- canonical container proof;
- UPK proof;
- architecture proof.

The goal is one thin but complete vertical path:

```text
UGOS desktop
    ↓
.UPK
    ↓
canonical container
    ↓
React shell
    ↓
UGOS authentication
    ↓
Go HTTP API
    ↓
BackupService
    ↓
SQLite/private state
```

## Phase 1 TDD / Acceptance-First Gate

Hardware behavior that cannot be automated SHALL have prewritten acceptance tests before the PoC.

Automatable behavior SHALL follow strict TDD.

Tests/checks SHALL cover before implementation:

- `project.yaml` validation;
- canonical image architecture/version;
- no `latest`;
- `inner` window assumptions;
- authenticated vs unauthenticated API;
- trusted gateway vs spoofed direct request;
- direct-port bypass;
- durable state write/read;
- container restart;
- private state vs backup-root separation;
- canonical image headless mode;
- UGOS serve mode.

## Work Package 1.1 — Extract Core, Shared UI, and Provider Contract

Implement:

- `core/` module;
- `ui/shared/`;
- `apps/common/`;
- `apps/ugos/`;
- provider capability contract;
- provider-neutral `BackupService`;
- generic/local-auth host boundary;
- dependency-rule CI tests.

TDD architecture tests SHALL prove:

- core builds with `apps/` removed;
- shared UI builds with provider SDK directories removed;
- core has no provider imports;
- UGOS provider depends on core, never inverse.

### Acceptance

- [ ] provider-neutral core is independently buildable/testable.
- [ ] UGOS SDK imports exist only under `apps/ugos/`.
- [ ] shared UI contains no UGOS imports.
- [ ] adding/removing a provider app requires no lifecycle changes.

## Work Package 1.2 — Developer Environment + Minimal UPK

Implement:

- Debian 12 development environment;
- pinned `ugcli`;
- developer-authorized NAS;
- minimal Docker App;
- App Center manual install;
- desktop icon;
- `open_type: inner`;
- React/JSSDK bootstrap;
- `/health/live`.

### Acceptance

- [ ] Minimal `.UPK` installs on real UGOS hardware.
- [ ] App opens inside UGOS.
- [ ] React/JSSDK initializes.
- [ ] Hardware test evidence is recorded.

## Work Package 1.3 — UGOS Authentication + Trusted Proxy Boundary

Implement/prove:

- `getThirdToken` frontend flow;
- UGOS gateway auth;
- backend authenticated identity;
- trusted-proxy source validation;
- spoofed header rejection;
- direct-LAN bypass testing;
- admin-only enforcement.

### Acceptance

- [ ] UGOS-authenticated requests succeed.
- [ ] unauthenticated requests fail.
- [ ] spoofed identity headers from direct LAN fail.
- [ ] backend trusts only the proven gateway path.
- [ ] destructive APIs remain disabled until this gate passes.

## Work Package 1.4 — Private State + Storage + Canonical Image

Implement/prove:

- private writable state source for `/var/lib/backup-manager`;
- user-authorized backup root mounted at `/data/backups`;
- update persistence;
- disable/enable persistence;
- uninstall behavior;
- state-loss assumptions;
- canonical image on amd64/arm64 build matrix;
- same binary supporting headless and UGOS modes.

### Acceptance

- [ ] private state and backup data are separate.
- [ ] retained backups are not stored with SSH private keys.
- [ ] update/disable/enable behavior is documented.
- [ ] uninstall behavior is documented from hardware evidence.
- [ ] canonical image builds for required architectures.

## Work Package 1.5 — Minimal API + Durable Operations Skeleton

Implement:

- versioned `/api/v1`;
- auth abstraction;
- `BackupService` adapter;
- durable operation model;
- idempotency key support;
- configuration revision model;
- authenticated operation polling;
- version/capabilities endpoint.

### Acceptance

- [ ] API invokes `BackupService`.
- [ ] operation survives browser/request disconnect.
- [ ] duplicate idempotency key does not create duplicate work.
- [ ] stale config revision returns conflict.
- [ ] API exposes no rclone/SQLite implementation types.

### Phase 1 Exit Gate

Proceed only if the core/shared UI are provider-neutral **and** an authorized UGREEN NAS can install the thin UGOS app, open the UI, authenticate securely, call the shared API/core, and persist provider-private state.

---

# 70. Phase 2 — Functional Backup Manager UI MVP

## Objective

Deliver the complete **shared provider-neutral operator UI** as vertical feature slices. UGOS and the generic Web host consume this same UI.

Initial navigation:

```text
Dashboard
Backup Sets
Backups
Activity
Quarantine
Settings
```

## Phase 2 TDD Gate

Each feature slice SHALL begin with:

- service behavior tests;
- API contract/auth tests;
- frontend component tests;
- E2E scenario definition.

Then implement the minimum vertical behavior.

## Work Package 2.1 — Dashboard + Health + Operation Progress

Implement:

- HEALTHY / DEGRADED / STALE / FAILING;
- last successful cycle;
- newest known-good backup;
- storage status;
- active operations;
- authenticated operation polling;
- process health distinct from backup health.

### Acceptance

- [ ] operator can determine backup health without CLI.
- [ ] stale backup is visibly distinct from a healthy running process.
- [ ] browser reload resumes operation-status display.

## Work Package 2.2 — Backup Set Management

Implement:

- list;
- detail;
- enable/disable;
- edit;
- optimistic concurrency;
- safe configuration removal;
- retention summary;
- last/next run.

### Acceptance

- [ ] stale edits are rejected.
- [ ] deleting config does not delete retained backups.
- [ ] disabled sources do not schedule work.

## Work Package 2.3 — Six-Step Add/Edit Backup Wizard

Implement grouped steps:

1. source identity + remote connection;
2. authentication + host trust;
3. artifact discovery + completion method;
4. destination + retention + source-deletion disclosure;
5. validation + non-destructive test;
6. review + activate.

Include:

- generated managed SSH key;
- key import only if required for v1;
- explicit host fingerprint trust;
- stable-size warning;
- remote-source deletion acknowledgement.

### Acceptance

- [ ] source can be configured entirely from UI.
- [ ] private key is never returned after persistence.
- [ ] referenced key cannot be deleted.
- [ ] changed host key blocks operation.
- [ ] connection test never deletes remote data.
- [ ] remote-source deletion behavior is explicitly acknowledged.

## Work Package 2.4 — Backups + Artifact Detail

Implement:

- retained backup list;
- artifact detail;
- size/time/hash;
- validation;
- lifecycle timeline;
- retention classifications;
- original remote path;
- local path;
- remote deletion status.

### Acceptance

- [ ] `.partial` and quarantined artifacts are not presented as valid backups.
- [ ] lifecycle status comes from backend.
- [ ] monthly/weekly/protected classifications display correctly.

## Work Package 2.5 — Manual Run + Validation + Quarantine

Implement:

- run now;
- operation polling;
- validate/revalidate;
- quarantine list/detail;
- retry ingestion where safe;
- typed errors.

### Acceptance

- [ ] manual run uses normal lifecycle invariants.
- [ ] validation failure preserves remote source.
- [ ] quarantined artifact cannot be treated as valid backup.
- [ ] no "delete remote anyway" shortcut exists.

## Work Package 2.6 — Activity + Settings

Implement MVP:

- chronological operational activity;
- severity/type filtering;
- polling interval;
- default retention;
- storage thresholds;
- log level;
- version/build/rclone metadata;
- redacted config export.

Keep filtering and diagnostics intentionally simple for v1.

### Phase 2 Exit Gate

An administrator can install/open the app and perform normal backup-manager configuration and monitoring without using a terminal.

---

# 71. Phase 3 — Retention, Recovery, and Operational Safety

## Objective

Complete the functionality most capable of deleting data or leaving backups unusable.

This phase gets the strictest safety-first TDD.

## Phase 3 TDD Gate

Before destructive implementation, write specification tests for:

- exact retention deletion set;
- stale retention plan;
- inventory change;
- last-known-good;
- concurrent admin changes;
- state loss;
- schema migration failure;
- catalog rebuild;
- critical disk pressure;
- alert triggering.

## Work Package 3.1 — Immutable Retention Planning + Apply

Implement:

- retention editor;
- server-calculated preview;
- immutable `plan_id`;
- inventory revision;
- config revision;
- expiry;
- exact delete set;
- stale-plan rejection;
- confirmation;
- durable retention operation.

Required test example:

```text
GIVEN plan P selects A and B
AND inventory changes before apply
WHEN P is applied
THEN zero files are deleted
AND RETENTION_PLAN_STALE is returned
```

### Acceptance

- [ ] UI never decides deletability.
- [ ] exact confirmed plan is applied or nothing is deleted.
- [ ] last-known-good remains protected.
- [ ] retention cannot escape managed root.

## Work Package 3.2 — Validation Hardening + Completion Assurance

Implement:

- checksum behavior;
- packaged/registered validator integration;
- preferred atomic rename/manifest modes;
- stable-size heuristic safeguards;
- quarantine/revalidation.

Stable-size mode SHALL remain visibly weaker and MAY require an additional delay/safety condition before remote deletion.

### Acceptance

- [ ] required validation failure prevents remote deletion.
- [ ] changed/incomplete artifact is preserved remotely.
- [ ] stable-size mode cannot silently masquerade as producer-confirmed completion.

## Work Package 3.3 — Catalog Reconstruction + State-Loss Recovery

Implement:

- non-secret per-artifact recovery metadata;
- `catalog rebuild --dry-run`;
- `catalog rebuild`;
- recovery/adoption of existing backup root;
- UI recovery status where needed.

### Acceptance

- [ ] deleting/loss of SQLite does not make retained backup files unusable.
- [ ] rebuild is non-destructive.
- [ ] secrets are not stored in recovery manifests.
- [ ] rebuilt catalog preserves retention-relevant timestamps/identity.

## Work Package 3.4 — Upgrade/Migration Safety + Storage Pressure

Implement:

- pre-migration state snapshot;
- transactional migrations;
- schema compatibility metadata;
- newer-schema refusal;
- migration failure readiness behavior;
- storage warning/critical handling;
- transfer refusal when capacity is insufficient.

### Acceptance

- [ ] migration failure starts no destructive daemon work.
- [ ] unsupported downgrade fails closed.
- [ ] insufficient disk prevents unsafe transfer start.
- [ ] retention policy is not silently violated to free space.

## Work Package 3.5 — Proactive Alerting

Implement at least one proactive mechanism for:

- stale backup;
- repeated failure;
- changed SSH host key;
- critical storage pressure.

Preference:

1. official UGOS local notification capability if available/documented;
2. otherwise one explicit opt-in generic alert mechanism.

Do not add a broad notification framework in v1.

### Phase 3 Exit Gate

The UI is operationally trustworthy even when administrators are not actively watching it, and loss/change/error cases fail safely.

---

# 72. Phase 4 — Multi-Provider Packaging and Distribution

## Objective

Package the same provider-neutral product for all in-scope target platforms.

The release hierarchy is:

```text
core binary
   │
   ├── OCI image
   │    ├── Generic Docker
   │    ├── UGOS UPK
   │    ├── TrueNAS
   │    ├── Unraid
   │    ├── OpenMediaVault
   │    └── Proxmox profile
   │
   └── Synology SPK
```

## Phase 4 TDD Gate

Before provider packaging is complete, automated/provider conformance tests SHALL verify:

- core version/hash parity;
- provider package metadata;
- architecture;
- state persistence;
- backup-root containment;
- auth mode;
- install/update/remove semantics where automatable;
- no bundled secrets;
- no provider-specific lifecycle implementation.

## Work Package 4.1 — Canonical Binary + OCI Image + Generic Docker App

Implement:

- static/provider-neutral core binary artifacts;
- generic Web host;
- local authentication;
- multi-stage canonical OCI image;
- headless mode;
- generic Web UI mode;
- Docker Compose;
- amd64/arm64.

### Acceptance

- [ ] generic Docker works without any NAS provider.
- [ ] headless mode remains available.
- [ ] generic Web UI is authenticated.
- [ ] binary/image hashes are recorded.

## Work Package 4.2 — UGOS Provider App / UPK

Implement only under:

```text
apps/ugos/
```

Include:

- UGOS platform adapter;
- UGOS auth;
- frontend bridge;
- `project.yaml`;
- Compose;
- icon;
- architecture image tar;
- `ugcli pack`;
- hardware lifecycle testing.

### Acceptance

- [ ] core/shared UI contain no UGOS code.
- [ ] UPK embeds canonical OCI image.
- [ ] trusted-gateway auth passes.
- [ ] install/update/disable/uninstall/reinstall are safe.

## Work Package 4.3 — TrueNAS + Unraid + OpenMediaVault Container Provider Packages

### TrueNAS

Implement under `apps/truenas/`:

- custom-app Compose;
- Apps catalog `app.yaml`/`questions.yaml`/templates;
- storage configuration;
- Web portal link;
- local auth;
- contribution-ready metadata.

### Unraid

Implement under `apps/unraid/`:

- Docker template XML;
- Community Applications metadata;
- `/config`/appdata mapping;
- backup-root mapping;
- WebUI link;
- local auth.

### OpenMediaVault

Implement under `apps/openmediavault/`:

- supported Compose deployment;
- persistent state mapping;
- backup-root mapping;
- Web UI instructions;
- local auth.

Do NOT implement a native OMV plugin in v1.

### Acceptance

- [ ] all three use the exact canonical OCI image.
- [ ] no provider-specific lifecycle code exists.
- [ ] install/start/update/remove workflows are documented/tested.
- [ ] retained backup data survives app/container replacement.

## Work Package 4.4 — Synology DSM SPK Provider App

Implement under:

```text
apps/synology/
```

Use the Synology Package Toolkit/package framework to create `.spk` artifacts.

Package:

- exact provider-neutral core binary for the target architecture;
- shared Web UI/generic Web host;
- DSM package metadata;
- lifecycle scripts;
- privileges/resources;
- Package Center icons;
- DSM desktop launcher.

Initial authentication MAY use reusable local auth.

Native DSM SSO/session integration requires its own security gate and is not required for initial SPK support.

### Acceptance

- [ ] SPK contains the exact release core binary hash.
- [ ] package installs manually in DSM Package Center.
- [ ] DSM desktop launcher opens the shared Web UI.
- [ ] state persists across package update.
- [ ] uninstall behavior does not unexpectedly delete retained backup data.
- [ ] supported architecture/model matrix is documented.

## Work Package 4.5 — Proxmox VE Deployment Profile + Cross-Provider Conformance

Implement under:

```text
apps/proxmox/
```

Support one documented safe deployment model, preferably:

- unprivileged LXC containing the app directly; or
- dedicated VM/container host running the canonical OCI image.

Do NOT install Docker or application daemons directly into the Proxmox VE host OS as a default design.

Provide:

- storage mount/bind guidance;
- network/port guidance;
- local auth;
- update/recovery procedure;
- shared Web UI.

Then execute the common provider conformance suite across all in-scope providers.

### Acceptance

- [ ] supported PVE deployment is reproducible.
- [ ] PVE host management plane is not modified by unsupported UI/plugin hacks.
- [ ] common provider conformance suite passes for each claimed provider.
- [ ] unsupported capabilities are explicitly reported.

### Phase 4 Exit Gate

The same core behavior is deployable through:

```text
Generic Docker
UGOS
TrueNAS
Unraid
OpenMediaVault
Synology DSM
Proxmox VE
```

at the support tier defined by this EPIC.

---

# 73. Phase 5 — Cross-Provider Hardening and Store/Catalog Readiness

## Objective

Complete adversarial regression, compliance, supply-chain, provider certification, and store/catalog submission readiness.

Phase 5 is NOT where features receive their first tests.

Any issue found here follows:

```text
failing regression test
      ↓
fix
      ↓
full affected regression
```

## Work Package 5.1 — Security / Destructive-Safety Red Team

Test:

- trusted proxy bypass;
- spoofed headers;
- auth/authorization;
- CSRF assumptions;
- path traversal;
- symlink escape;
- duplicate POST/idempotency;
- stale retention plan;
- concurrent sessions;
- changed host key;
- operation restart;
- catalog rebuild;
- migration downgrade;
- secret redaction.

### Acceptance

- [ ] no unresolved critical/high security issue.
- [ ] every discovered defect has a permanent regression test.

## Work Package 5.2 — Supply Chain + Compliance

Produce:

- SBOM;
- checksums;
- OCI digests;
- third-party license inventory;
- copyright notices;
- privacy policy;
- license agreement/source-code/source-offer link;
- support link;
- release provenance.

### Acceptance

- [ ] App Center-required compliance metadata exists.
- [ ] source/privacy/license/support links are valid.
- [ ] UPK built from known canonical image digest.
- [ ] release can be reproduced from tagged source.

## Work Package 5.3 — Resource + Hardware Certification

Measure/test:

- idle CPU;
- idle RSS;
- transfer resource usage;
- API/UI overhead;
- amd64 UGREEN hardware;
- arm64 UGREEN hardware where claimed supported.

### Acceptance

- [ ] each architecture claimed certified has real hardware evidence.
- [ ] app does not materially interfere with ordinary NAS use while idle.

## Work Package 5.4 — Provider Store/Catalog Submission Preflight

Prepare provider-appropriate distribution materials for:

- UGREEN App Center;
- Synology Package Center;
- TrueNAS Apps catalog;
- Unraid Community Applications.

Prepare:

- descriptions;
- icons;
- screenshots;
- release notes;
- privacy disclosures;
- permission rationale;
- support/source/license materials;
- provider submission checklists.

Verify:

- no self-update;
- no `latest`;
- no privileged mode;
- no mandatory telemetry;
- proactive alerts work;
- support/recovery docs exist.

### Phase 5 Exit Gate

The release is ready for the provider stores/catalogs targeted by this EPIC. External provider approval remains outside repository control.

---

# 74. Cross-Phase Acceptance Criteria

In addition to functional completion, every applicable child issue SHALL demonstrate the RED → GREEN → REFACTOR → integration/regression cycle defined in this EPIC.

This EPIC is complete when:

- [ ] the predecessor backup-manager core remains the only lifecycle engine;
- [ ] one canonical provider-neutral core binary exists per release/architecture;
- [ ] container-based providers use the canonical OCI image built from that core binary;
- [ ] Synology SPK proves the embedded core binary hash;
- [ ] Generic Docker, UGOS, TrueNAS, Unraid, OMV, and container-based Proxmox profiles use the expected canonical image digest;
- [ ] UGOS authentication cannot be spoofed from direct LAN access;
- [ ] local-auth mode is secure for non-integrated providers;
- [ ] private app state and retained backup data are separate;
- [ ] UI covers normal configuration and monitoring without terminal use;
- [ ] remote-source deletion behavior is explicitly disclosed;
- [ ] retention applies only the exact confirmed immutable plan;
- [ ] stale retention plans delete nothing;
- [ ] catalog reconstruction works after state loss;
- [ ] migration/downgrade failure is safe;
- [ ] proactive stale/failure alerting exists;
- [ ] headless Docker operation is independently usable;
- [ ] provider-specific install/update/remove flows are verified at the claimed support tier;
- [ ] claimed providers/architectures have appropriate certification evidence;
- [ ] security/destructive-safety suites pass;
- [ ] OSS/privacy/source/support compliance artifacts exist;
- [ ] every applicable child issue followed mandatory TDD.

---

# 74A. Explicit Post-v1 Backlog

The following capabilities SHOULD NOT block the first App Center-ready release unless UGREEN review or real operational testing demonstrates they are required:

- advanced diagnostics dashboard;
- downloadable support bundle;
- sophisticated Activity filtering/search;
- arbitrary SSH private-key import if generated managed keys satisfy initial deployment needs;
- browser-configurable arbitrary validator commands;
- Prometheus metrics;
- broad webhook/provider notification framework beyond one required proactive alert path;
- full mobile UGOS UI;
- localization beyond centralized strings and required App Center metadata;
- elaborate multi-user audit browsing;
- arbitrary remote file browsing;
- restore execution;
- cloud replication;
- WORM/immutable-storage integration UI;
- advanced custom completion plugins;
- native OpenMediaVault Workbench/plugin integration;
- Proxmox VE Web UI plugin integration;
- native Synology DSM SSO/session integration unless proven secure during v1;
- general-purpose SFTP management.

These items belong in follow-on EPICs/issues.

Moving them out of v1 is an intentional schedule optimization and MUST NOT be interpreted as weakening backup-integrity or authentication requirements.

---

# 75. Non-Goals

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
- duplicating the core lifecycle engine per NAS provider;
- requiring native OS integration where a supported container/package wrapper is sufficient;
- unsupported modifications to the Proxmox VE host management UI.

The EPIC makes the app **submission-ready**; UGREEN's external review decision is outside repository control.

---

# 76. Failure-Safety Invariants Inherited from Parent EPIC

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

# 77. Additional UI Safety Invariants

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

# 78. Documentation Deliverables

Create/update:

```text
tools/backup-manager/README.md
tools/backup-manager/docs/architecture.md
tools/backup-manager/docs/provider-apps.md
tools/backup-manager/docs/testing-tdd.md
tools/backup-manager/docs/security.md
tools/backup-manager/docs/providers/ugos.md
tools/backup-manager/docs/providers/synology.md
tools/backup-manager/docs/providers/truenas.md
tools/backup-manager/docs/providers/unraid.md
tools/backup-manager/docs/providers/openmediavault.md
tools/backup-manager/docs/providers/proxmox.md
tools/backup-manager/docs/providers/docker.md
tools/backup-manager/docs/release.md
```

`architecture.md` SHALL document the dependency rule:

```text
provider apps → core
core -X→ provider apps
```

`provider-apps.md` SHALL define:

- provider contract;
- capability model;
- how to add a new provider;
- provider conformance tests;
- local vs native auth;
- package/version parity rules.

Each provider document SHALL cover:

- supported versions/architectures;
- install;
- state path;
- backup root;
- authentication;
- update;
- uninstall/remove;
- recovery;
- certification status.

---

# 79. Architecture Decision Records

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

ADR-PLATFORM-001
Keep backup-manager core provider-neutral; all NAS OS integrations live under apps/<provider>

ADR-PLATFORM-002
Use one shared provider-neutral React UI with thin provider bridges

ADR-PLATFORM-003
Use reusable local authentication for providers lacking secure native authentication

ADR-PLATFORM-004
Use canonical OCI image for container-native NAS providers

ADR-PLATFORM-005
Package the same core binary directly into Synology SPK rather than creating a Synology-specific engine

ADR-PLATFORM-006
Treat Proxmox VE as a supported deployment target, not a native NAS-app UI provider
```

The parent ADR for embedding rclone remains authoritative.

---

# 80. Open Implementation Questions / Provider Gates

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
16. Which Synology DSM 7 architectures/models should be certified initially: amd64 and arm64 only?
17. What is the minimum secure DSM package privilege set for the SPK?
18. Is native DSM session authentication sufficiently documented/stable to justify a v1 adapter, or should v1 remain local-auth?
19. What TrueNAS catalog train/submission target is appropriate for initial publication?
20. What exact Unraid Community Applications template/repository submission process should be targeted at release time?
21. Should OpenMediaVault remain Compose-only until demand justifies a Workbench plugin?
22. Which Proxmox deployment is simpler/safer to certify: direct binary in unprivileged LXC or OCI image inside a dedicated guest/container host?
23. Which provider-native notification systems should be implemented after the shared alert contract exists?

Do not paper over an unresolved security-sensitive question with an assumption.

---

# 81. Recommended Implementation Sequence

```text
Parent backup-manager core behavior
        ↓
Phase 1 — Extract provider-neutral core/shared UI + prove UGOS adapter
        ↓
Phase 2 — Complete shared functional UI MVP
        ↓
Phase 3 — Retention/recovery/safety
        ↓
Phase 4 — Package provider adapters
        │
        ├── Generic Docker
        ├── UGOS
        ├── TrueNAS
        ├── Unraid
        ├── OpenMediaVault
        ├── Synology
        └── Proxmox
        ↓
Phase 5 — Cross-provider hardening + store/catalog readiness
```

Provider packaging work MAY proceed in parallel once the shared UI/API/core contracts are stable.

No provider package may introduce lifecycle behavior to work around a missing core capability; missing generic behavior must be solved in the core first.

---

# 82. Mandatory Child-Issue TDD Template

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


# 83. Suggested Gitea Child Issues — Provider-Neutral Vertical Slices

Target: approximately **22 substantive issues**. Provider packaging increases scope, but lifecycle/UI logic remains shared.

Every issue SHALL use the mandatory TDD template.

## Phase 1 — Core / Provider Architecture + UGOS Proof

- [ ] **P1.1 — Extract Provider-Neutral Core + Public Application Contracts**
- [ ] **P1.2 — Extract Shared React UI + Platform Bridge Contract**
- [ ] **P1.3 — Generic Web Host + Secure Local Authentication**
- [ ] **P1.4 — UGOS Provider Adapter + Trusted-Proxy Authentication PoC**
- [ ] **P1.5 — UGOS Private State + Minimal UPK + Durable Operations/API PoC**

## Phase 2 — Shared Functional UI MVP

- [ ] **P2.1 — Dashboard, Health, and Durable Operation Progress**
- [ ] **P2.2 — Backup Set Management**
- [ ] **P2.3 — Six-Step Backup Setup Wizard + SSH/Host Trust**
- [ ] **P2.4 — Backups List + Artifact Detail**
- [ ] **P2.5 — Manual Run, Validation, and Quarantine**
- [ ] **P2.6 — Activity, Settings, and Redacted Configuration Export**

## Phase 3 — Safety + Recovery

- [ ] **P3.1 — Immutable Retention Preview/Plan/Apply**
- [ ] **P3.2 — Validation Hardening + Completion Assurance**
- [ ] **P3.3 — Catalog Rebuild + State-Loss Recovery**
- [ ] **P3.4 — Migration/Downgrade Safety + Disk Pressure**
- [ ] **P3.5 — Shared Alert Contract + Proactive Failure/Staleness Alerting**

## Phase 4 — Provider Packaging

- [ ] **P4.1 — Canonical Binary + Multi-Arch OCI + Generic Docker Distribution**
- [ ] **P4.2 — UGOS Provider Production UPK + Lifecycle Certification**
- [ ] **P4.3 — TrueNAS + Unraid + OpenMediaVault Container Provider Packages**
- [ ] **P4.4 — Synology DSM SPK Provider App**
- [ ] **P4.5 — Proxmox VE Deployment Profile + Provider Conformance Suite**

## Phase 5 — Hardening / Release

- [ ] **P5.1 — Cross-Provider Security + Destructive-Safety Red Team**
- [ ] **P5.2 — Supply Chain, Compliance, Provider Certification, and Store/Catalog Readiness**

Provider-specific follow-on integrations such as DSM SSO, native OMV plugin UI, or Proxmox host-UI plugins SHALL be separate issues/EPICs unless they become necessary to meet the support tier promised here.

---

# 84. Definition of Done

The Definition of Done requires that the product remains one core with provider layers.

A provider is considered supported only at the tier explicitly claimed in this EPIC; no statement shall imply native OS integration where only a deployment profile exists.



The Definition of Done also requires that each of the lean vertical-slice child issues was completed under the mandatory TDD contract. A green final test suite without evidence of test-first behavior does not satisfy this EPIC's engineering process requirement.

The EPIC is done when an administrator can choose either of these supported deployment paths.

## Path A — Generic / Container-Native Providers

The canonical OCI image can be deployed with the appropriate provider package/profile on:

```text
Generic Docker/Linux
TrueNAS
Unraid
OpenMediaVault
Proxmox VE deployment profile
```

and provides the shared authenticated Web UI.

## Path B — UGOS

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

## Path C — Synology DSM

```text
install .spk
   ↓
DSM Package Center lifecycle
   ↓
DSM desktop launcher
   ↓
shared Web UI
   ↓
same provider-neutral core binary
```

The SPK SHALL prove its embedded core-binary hash matches the release manifest.

## Path D — Docker / Terminal

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

# 85. Official UGREEN References

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



## Additional Provider References

- Synology DSM Package Developer Guide  
  https://help.synology.com/developer-guide/

- Synology `.spk` package structure  
  https://help.synology.com/developer-guide/synology_package/introduction.html

- Synology Package Toolkit  
  https://help.synology.com/developer-guide/toolkit/toolkit.html

- TrueNAS Custom Apps / Docker Compose  
  https://apps.truenas.com/managing-apps/installing-custom-apps/

- TrueNAS Apps contribution model  
  https://github.com/truenas/apps/blob/master/CONTRIBUTIONS.md

- Unraid Community Applications  
  https://docs.unraid.net/unraid-os/manual/applications/

- Unraid Docker overview  
  https://docs.unraid.net/unraid-os/using-unraid-to/run-docker-containers/overview/

- OpenMediaVault plugins  
  https://docs.openmediavault.org/en/8.x/plugins.html

- OpenMediaVault plugin development  
  https://docs.openmediavault.org/en/7.x/development/plugins.html

- Proxmox VE Administration Guide  
  https://pve.proxmox.com/pve-docs/pve-admin-guide.pdf

---

# 86. Consensus Revision Change Log

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
- collapsed the delivery model from 10 phases to 5 vertical phases;
- reduced the intended child-issue count to approximately 18–20 substantive vertical slices;
- moved nonessential diagnostics, localization, broad integrations, restore execution, and similar scope to an explicit post-v1 backlog;
- preserved strict TDD and all destructive-safety gates while removing layer-by-layer project-management overhead.
- moved UGOS out of the core into `apps/ugos/`;
- introduced `core/`, `ui/shared/`, and `apps/<provider>/` dependency boundaries;
- added provider capability/auth contracts and reusable local authentication;
- added TrueNAS, Unraid, OpenMediaVault, Synology DSM, and Proxmox VE provider targets at platform-appropriate support tiers;
- made the provider-neutral core binary the canonical release primitive;
- added provider conformance testing and per-provider certification evidence;
- explicitly deferred native OMV and Proxmox UI plugins and optional DSM SSO rather than contaminating the core.


# 87. Final Architecture

```text
                         REPOSITORY
                tools/backup-manager/
                        │
        ┌───────────────┼────────────────┐
        │               │                │
        ▼               ▼                ▼
      core/         ui/shared/         apps/
        │               │                │
        │               │      ┌─────────┼───────────────┐
        │               │      │         │               │
        │               │      ▼         ▼               ▼
        │               │    ugos     synology        generic
        │               │                            / truenas
        │               │                            / unraid
        │               │                            / omv
        │               │                            / proxmox
        │               │
        └───────────────┴──────────┐
                                    ▼
                              BackupService
                                    │
                       ┌────────────┼─────────────┐
                       ▼            ▼             ▼
                    lifecycle     rclone       retention
                       │
                       ▼
                    SQLite
```

Release artifacts:

```text
provider-neutral core binary
          │
          ├── canonical OCI image
          │      ├── Generic Docker
          │      ├── UGOS → UPK
          │      ├── TrueNAS
          │      ├── Unraid
          │      ├── OpenMediaVault
          │      └── Proxmox deployment profile
          │
          └── Synology → SPK
```

The governing architectural rule is:

> **NAS provider integrations are replaceable presentation/packaging layers. The backup lifecycle engine is not.**

Adding another NAS provider SHALL normally mean:

```text
mkdir apps/<new-provider>
implement provider adapter/package
run provider conformance suite
```

—not modifying retention, transfer, validation, remote deletion, or lifecycle state logic.

The test suite remains part of the safety boundary: provider layers must prove they preserve the exact core behavior rather than merely resembling it.
