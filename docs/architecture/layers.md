# The three layers, what each owns, and what enforces it

Phase 6 (EPIC B, #81) claims the product is **one Go application, one shared
TypeScript UI, one versioned multi-arch OCI image and one authoritative Compose
runtime**, with every NAS OS, container manager and app store reduced to a thin
adapter around it. Issue #165 exists to make the boundaries that claim depends
on explicit and mechanically enforceable, because a boundary that is only
documented is not a boundary.

Before this, the repository had two boundaries: `core/`, and "a provider app".
Two of the three layers below were conflated inside `apps/<platform>/`, and the
dependency rule named no distribution layer at all.

## The layers

### 1. core — provider-neutral core, application services, the API host, the shared UI

Owns lifecycle state, retention policy, validation rules, catalog truth and
backup policy, **exclusively**. Nothing outside this layer may declare any of
them.

| path | what it is |
|---|---|
| `core/` | the provider-neutral engine: lifecycle, retention, catalog, validation, state, transport |
| `apps/common/webhost/` | the `/api/v1` host |
| `apps/common/auth/` | the reusable local authentication service |
| `apps/common/csrf/` | the shared double-submit CSRF primitive |
| `apps/common/platform/` | the `PlatformCapabilities` / `PlatformAdapter` **contract**, and the generic sink that consumes it |
| `ui/shared/` | the one React/TypeScript UI |

`apps/common/platform/` sits in this layer and not in the platform one on
purpose. #81's dependency rule reads `platform ───► app/core contracts only`,
which means the contract itself belongs to core. A runtime profile *implements*
it; it does not own it.

### 2. runtime platform — a profile, for behaviour that legitimately depends on the host

A **runtime profile** changes behaviour that genuinely depends on the host: a
trusted native authentication gateway, a provider notification bridge, a launch
or navigation bridge, platform capability reporting. Selected explicitly, for
example `backup-manager serve --profile=generic`.

**A profile must never alter backup lifecycle semantics.** That is #81's wording
and it is the line that separates a profile from a fork.

| path | what it is |
|---|---|
| `apps/generic/` | the vendor-neutral profile and the binary that hosts it |
| `apps/ugos/` | the UGOS bridge (EPIC C and EPIC D own its runtime behaviour) |
| `apps/<platform>/frontend/` | one capability declaration, one bootstrap, at most an auth bridge, per platform |
| `apps/common/tests/` | the cross-provider bridge conformance suite: the one place that imports every provider's `platform.ts` |

### 3. distribution — packaging, metadata, templates, store presentation

A **distribution adapter** changes only how the same runtime is installed and
presented: a Portainer app template, TrueNAS catalog metadata, an Unraid Docker
template XML, CasaOS and ZimaOS `x-casaos` metadata, an OMV or Proxmox
deployment profile, a Synology `.spk`.

The rule that makes an adapter an adapter, quoted from #81 and binding here:

> Adapters may change installation metadata, host paths, the authentication
> bridge, notifications, launch behavior and store presentation. They must not
> fork backup behavior, API semantics, web application logic, retention logic,
> validation rules, or database truth.

The layer has two **kinds**, and the difference decides what gets deleted in the
proof below:

| kind | meaning | paths |
|---|---|---|
| `canonical` | the canonical runtime and the shared metadata every adapter derives from | `container/`, `distribution/` |
| `adapter` | one target's own packaging | `apps/truenas/{catalog,compose,README.md}`, `apps/unraid/{template,README.md,frontend/webui.json}`, `apps/openmediavault/{compose,README.md}`, `apps/proxmox/{compose,README.md,frontend/deployment.md}`, `apps/synology/{spk,cmd,go.mod,README.md}`, `tools/ugcli-install` |

### infrastructure — everything that is not product code

`docs/`, `scripts/`, `.github/`, `.husky/` and the repository-root config files.
Listed explicitly rather than silently excluded, so the completeness check has no
blind spot to hide a new product directory in.

## The dependency direction

```text
core/app  ─X─► distribution/*
core/app  ─X─► NAS SDKs
httpapi   ───► app/core
web       ───► versioned API contract
platform  ───► app/core contracts only
adapters  ───► canonical image/Compose/runtime metadata
```

Read as rules over the layers above:

- a **core**-layer Go module may import neither platform nor distribution;
- a **platform**-layer Go module may not import distribution;
- no core- or platform-layer module may import a NAS, container-manager or app-store SDK;
- the **shared UI** may not import a provider directory or a provider SDK, for any of Phase 6's ten platforms;
- **nothing outside core** may declare lifecycle state, retention, validation rules, catalog truth or backup policy;
- **core and the generic application** must build and pass their tests with the whole distribution adapter tree deleted.

## What enforces it

The manifest is `scripts/architecture/layers.conf`. It classifies every tracked
file into exactly one layer (longest matching path prefix wins), and every check
below reads it, so adding a platform or moving a packaging artifact is one edit
in one file rather than four scripts drifting apart.

| check | what it proves | how |
|---|---|---|
| `check-layer-manifest.sh` | every tracked file is classified, every entry exists, no layer is empty | static, over `git ls-files` |
| `check-core-dependency-rule.sh` | the dependency direction, for every Go module in the repository | `go list -deps`, per module, `GOWORK=off` |
| `check-layer-ownership.sh` | nothing outside core declares a core-owned concept | `ownership.go` parses Go and walks top-level declarations |
| `check-ui-shared-provider-imports.sh` | the shared UI imports no provider directory or SDK, across all ten platforms | module specifiers only, never whole lines |
| `verify-core-without-apps.sh` | `core/` stands with all of `apps/` deleted | actual deletion in a throwaway worktree |
| `verify-core-without-distribution.sh` | core, `apps/common` and `apps/generic` stand with the distribution **adapter** tree deleted | actual deletion in a throwaway worktree |
| `verify-ui-shared-without-provider-sdks.sh` | `ui/shared` installs and builds with every provider directory deleted | actual deletion, then `npm ci && npm run build` |
| `verify-ugos-removable.sh` | removing one provider breaks neither core nor the shared UI | actual deletion |
| `selftest.sh` | **every rule above can actually fail** | plants one real violation per rule in a copy of the tree and requires the named failure |

All of them run in CI (`.github/workflows/ci.yml`, job
`repository-structure dependency rules`) and locally (`scripts/ci-local.sh`).
The static ones run even under `CI_LOCAL_FAST=1`, because together they cost
seconds and they are what a mid-refactor edit is most likely to break.

### Why `ownership.go` parses instead of grepping

The thing being detected is a *declaration*, and grep cannot tell one from a
comment or a string. Every Go file here is heavily commented, and those comments
name retention, lifecycle and the catalog constantly and legitimately. A regex
over file text would either fire on all of them or be watered down until it
fired on nothing. Parsing and walking only top-level declarations gives a rule
that means what it says.

The identifier matching is deliberately **substring, case-insensitive, not word
boundary**. Go identifiers are camel-cased, so `\b` never fires between `Apply`
and `Retention`, and a word-boundary rule would pass `ApplyRetentionPlan` while
claiming to forbid it. That is not hypothetical in this repository: a scanner
here once missed `ADMIN_PASSWORD` for exactly that reason, because `\b` never
matches between `_` and `p`.

### Why every rule has a positive control

A negative assertion that has never been seen to fail is indistinguishable from
one that cannot fail. `scripts/architecture/selftest.sh` plants a real violation
per rule in a copy of the real tree, and asserts both that the check fails **and
that it fails with the message for that rule** rather than for some unrelated
reason. Writing the message assertion is what caught a version of the self-test
that "passed" every mutation because the check script was missing from its copy.

The same discipline covers the performance gate: `scripts/perf/selftest.sh`.

## The map from the old layout, and who moves what next

Rebasing an in-flight branch, or looking for something that used to be
somewhere else:

| was | is | who |
|---|---|---|
| `apps/common/packaging/` | `distribution/packaging/` | #165, done |
| `github.com/spdrman/rclone-manager/apps/common/packaging` | `github.com/spdrman/rclone-manager/distribution/packaging` (new module) | #165, done |
| `cd apps/common && go test ./packaging/` | `cd distribution && go test ./packaging/` | #165, done |
| `container/{Dockerfile,compose.yaml,.env.example,release-manifest.json}` | `distribution/compose/` | **#167**, not yet |
| `apps/truenas/{catalog,compose}` | `distribution/truenas/` | **#169**, not yet |
| `apps/unraid/{template,frontend/webui.json}` | `distribution/unraid/` | **#169**, not yet |
| `apps/openmediavault/compose` | `distribution/openmediavault/` | **#169**, not yet |
| `apps/proxmox/{compose,frontend/deployment.md}` | `distribution/proxmox/` | **#169**, not yet |
| `apps/synology/{spk,cmd/spkctl}` | `distribution/synology/` | **#169**, not yet |
| `tools/ugcli-install` | `distribution/ugos/` | **EPIC D**, not yet |

Those artifacts are **already classified** as distribution-layer, so every check
above covers them today and `verify-core-without-distribution.sh` deletes exactly
them. They are not physically moved yet because #81 says plainly that converting
the shipped Phase 4 packaging is #169's work and that nobody should reconcile
Phase 4 into this refactor on their own initiative, and the canonical runtime is
#167's. Moving them here and converting them there would churn the same files
twice. When those issues move them, the change to this repository's enforcement
is one edit in `scripts/architecture/layers.conf`.

## A note on the source specification's paths

The refactor specification behind #81's standing constraint roots its structure
diagram at `tools/backup-manager/`, a path that does not exist in this
repository. The binding requirement is the **dependency direction**, not the
literal paths: #81 says so, and #165 restates it. The layer a file is in is what
`scripts/architecture/layers.conf` says it is, not what its directory happens to
be called.

## Adding something new

- **A new platform**: add `apps/<platform>/frontend/` (a capability declaration
  and a bootstrap), classify it `platform` in the manifest, and classify its
  packaging artifacts `distribution adapter`. If you forget,
  `check-layer-manifest.sh` fails and names the file.
- **A new adapter artifact**: classify it `distribution adapter`. It then
  automatically joins the deletion proof, so the claim that core stands without
  it stays true rather than becoming stale.
- **A new Go module**: classify its `go.mod`. `check-core-dependency-rule.sh`
  finds every `go.mod` in the repository and refuses one the manifest does not
  classify, so a module cannot quietly escape the dependency rule.
