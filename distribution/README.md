# `distribution/`, the distribution layer

This tree is one of the three layers issue #165 made explicit. It holds
**packaging, metadata, templates and store presentation**: everything that
changes how the same runtime is installed and presented, and nothing that
changes what the runtime does.

The rule that makes an adapter an adapter, quoted from EPIC B #81's standing
constraint, and binding here:

> Adapters may change installation metadata, host paths, the authentication
> bridge, notifications, launch behavior and store presentation. They must not
> fork backup behavior, API semantics, web application logic, retention logic,
> validation rules, or database truth.

## What is in here today

| path | what it is |
|---|---|
| `packaging/canonical.json` | the single source of truth every provider package agrees with: image reference, architectures, port, auth mode, commands, container paths, and each platform's own host paths |
| `packaging/` (the rest) | the cross-provider conformance suite that holds those packages to `canonical.json`, plus Phase 4's per-capability conformance matrix and its completeness and staleness guards |

## Where the rest of the layer lives, for now

The per-platform packaging artifacts are still under `apps/<platform>/`
(`apps/truenas/catalog`, `apps/unraid/template`, `apps/openmediavault/compose`,
`apps/proxmox/compose`, `apps/synology/spk`, and the canonical runtime in
`container/`). They are already **classified** as distribution-layer in
`scripts/architecture/layers.conf`, so every layer check covers them today,
and `verify-core-without-distribution.sh` deletes exactly them to prove the
core and the generic application stand without the adapter tree.

They are not physically moved here yet, and that is deliberate rather than
unfinished. EPIC B #81 says plainly that converting the already-shipped Phase 4
platform packaging is **#169's** work, and that nobody should reconcile Phase 4
into this refactor on their own initiative; the canonical image and Compose
runtime under `container/` are **#167's**. Moving those files here now and
converting them there would churn the same artifacts twice and put this issue
inside two others' scope. `docs/architecture/layers.md` records the mapping so
those issues have a destination to move to rather than a decision to re-make.

## Why this is its own Go module

Before Phase 6 this code was `apps/common/packaging`, inside the module that
also holds the `/api/v1` host and the shared authentication service. That put
the distribution layer and the core application layer behind one `go.mod`, so
"the core does not depend on distribution" was a claim no tool could check.
Now the module graph carries it:
`scripts/architecture/check-core-dependency-rule.sh` reads each module's layer
from `scripts/architecture/layers.conf` and refuses a core-layer module that
imports this one.

Nothing here is compiled into any binary. It exists so the packaging rules are
executable rather than prose.

## Running its checks

```sh
cd distribution && GOWORK=off go test ./... -count=1

# Regenerate the conformance matrix report after a real run
cd distribution && CONFORMANCE_UPDATE=1 GOWORK=off go test ./packaging/ -count=1 -run TestCrossProviderConformanceMatrix
```

`-count=1` matters: these tests read files from outside their own module, and
Go's test cache does not notice those changing.
