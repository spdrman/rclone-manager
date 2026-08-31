# The canonical runtime contract

Issue #167 (B6.3), EPIC B #81 Phase 6.

`container/compose.yaml` is the authoritative definition of how this product
runs. Every other deployment artifact in this repository derives from it, and
`distribution/compose` fails the build when one of them stops agreeing.

"Authoritative" here means a check, not a path. Before this issue the file was
correct, carefully reasoned and thoroughly commented, and none of that was
mechanically true: its security posture was asserted in a header comment, its
field set was whatever had accumulated, and an adapter agreeing with it was a
matter of review. `distribution/compose/runtime-contract.json` is the
contract-shaped version, and the suite next to it is what makes the file
authoritative.

## The standardised field set

Every field below must be declared. Deleting one fails
`TestCanonicalDefinitionDeclaresEveryRequiredField` by name, with the reason
the contract gives for requiring it, and
`TestRequiredFieldCheckFailsWhenAFieldIsRemoved` removes each one in turn to
prove the check can actually see it go.

| field | where | what it is |
|---|---|---|
| `image-reference` | both services | one canonical image, named identically by both, so `command` is the only difference between them |
| `command-and-runtime-profile` | both services | the command **and** `--profile=<name>`, standardised as one field |
| `listen-port` | web UI | the one published port; the engine deliberately publishes none |
| `health-check` | both services | declared here, not inherited from the image and described in a comment |
| `start-gate-liveness` | engine | the engine's healthcheck asks `/health/live`, never backup freshness: `web-ui` waits on it |
| `graceful-shutdown-period` | both services | `stop_grace_period`: 30s for the engine, 15s for the UI host |
| `restart-policy` | both services | `unless-stopped` |
| `ownership` | both services | explicit `user: PUID:PGID` |
| `explicit-writable-paths` | both services | `read_only: true`, which is what makes the writable paths explicit |
| `timezone` | engine | `TZ`, because retention is evaluated against calendar boundaries |
| `private-state-mount` | engine | `/data/state` |
| `backup-data-mount` | engine | `/data/backups` |
| `configuration-mount` | engine | `/etc/backup-manager/config`, a writable directory holding `config.yaml` (issue #196) |
| `secret-file-mount` | engine | `/etc/backup-manager/id_ed25519`, read-only |
| `resource-expectations` | document | `x-canonical-runtime.resources` |
| `supported-architectures` | document | `x-canonical-runtime.architectures` |
| `digest-policy` | document | `x-canonical-runtime.digest_policy` |
| `contract-version` | document | `x-canonical-runtime.contract` |
| `runtime-profiles` | document | `x-canonical-runtime.profiles` |

### The two mounts that must never contain one another

`/data/state` holds the lifecycle journal, the local-authentication
administrator record and nothing an operator would ever hand to somebody else.
`/data/backups` is a share people are given access to. Putting either inside
the other puts the state database and the Argon2id password hash into a
directory whose whole purpose is to be shared, so
`TestPrivateStateAndBackupDataAreSeparateMounts` checks containment both ways,
and checks that its own containment helper is not vacuous.

## The prohibition list

The canonical definition and every derived artifact are checked against all of
these. None may be required:

```text
privileged: true
network_mode: host
host PID namespace
host IPC namespace
/var/run/docker.sock (or /run/docker.sock)
unbounded host filesystem access (/, /etc, /usr, /var, /boot, /proc, /sys, /root, /home)
cap_add
seccomp / AppArmor unconfined
```

An absent setting and an unnecessary setting are different claims, so the
prohibition list is checked two ways. Statically,
`TestProhibitionCheckFiresOnEveryProhibitedSetting` injects each prohibited
setting into a copy of the real definition and requires the matching rule to
fire and to name the service and the value: a rule nobody has watched fail is a
comment. And `TestProhibitionScanSeesKeysTheParserHasNoFieldFor` covers the
specific way a check like this fails open, which is parsing compose into a
struct that has no field for `privileged` on a service nobody modelled.

At runtime, the existing `apps/generic/tests/dockercli` suite runs the real
built image with none of them and completes real work.

Any future exception needs its own security review. A thin distribution adapter
cannot introduce one: the prohibition rules run against every registered
artifact, and `TestEveryComposeArtifactInTheTreeIsRegistered` fails when a
compose file exists in the tree that nothing registered.

## Runtime profiles

A runtime profile is how one executable changes host-dependent behaviour
without becoming a second build.

```
backup-manager-web serve    --profile=generic
backup-manager-web serve    --profile=ugos --trusted-gateway=172.19.0.2/32
backup-manager-web serve-ui --profile=ugos --trusted-gateway=10.1.2.3/32 --ui-root=/usr/share/backup-manager/ui
```

Seven profiles exist: `generic`, `ugos`, and the five issue #169 added when it
converted the shipped platform packaging (`truenas`, `unraid`,
`openmediavault`, `proxmox`, `synology`). Those five declare no capability and
no gateway, and that is the finding rather than an omission: every one of them
uses section 13A local authentication, none has a server-side notification
channel, and the capabilities their frontend bridges declare are browser-host
capabilities this Go process cannot deliver. What a profile changes for them is
exactly what legitimately differs, which is the platform the runtime reports
itself as, how the deployment is described, and which UI bridge the Web UI host
serves. That is not nothing: before it, a user who installed through the
TrueNAS catalog was told by the running application that this was a generic
Docker Compose deployment.

A profile may change exactly four things:

- a trusted native authentication gateway,
- a provider notification bridge,
- a platform launch or navigation bridge (which UI bundle is served),
- platform capability reporting.

It may never change backup lifecycle, retention or validation semantics, and it
may never change authorization semantics either. A profile can supply an
identity; it cannot decide what that identity may do.

That is enforced two ways. Structurally, `profile.Profile` may declare no field
outside an allow-list, and the checker is proved non-vacuous against a
deliberately forked profile carrying `RetentionTiers` and `LifecycleStates`.
Behaviourally, `apps/common/webhost/serve/parity_test.go` stands up one engine
per profile over **one shared backend** and compares whole response bodies on
the lifecycle, retention, validation and storage reads. Its own positive
control is `/api/v1/system/capabilities`, which must differ, because reporting
what the host can do is one of the four things a profile is allowed to change.

Profile dispatch is startup-time configuration. There is no per-request
profile indirection to pay for, and the parity suite would show one if there
were: it drives every route through a real listener rather than calling a
handler.

Selecting an unknown profile is refused with exit code 2, before anything opens
a file or a listener. There is no fallback to `generic`: a deployment that
asked for `ugos` and silently got the generic authentication story is the
outcome that refusal exists to prevent.

### The trusted-gateway boundary

`--profile=ugos` declares that an identity header set by the platform gateway
may be believed. It is believed **only** from a network source the deployment
declared:

- a request from outside `--trusted-gateway` is refused with
  `ErrUntrustedPeer`, its identity header is never read, and the refused
  `AuthContext` never carries the forged username;
- a request from inside it with no identity is refused with
  `ErrNoGatewayIdentity`, which is a different error on purpose: one is an
  attack and the other is a misconfigured gateway;
- a remote address that does not parse is untrusted, never trusted by accident;
- `--profile=ugos` with **no** `--trusted-gateway` refuses to start at all,
  because without a declared peer there is no gateway, only a header anyone on
  the LAN can set.

Write the range as narrowly as the deployment allows, and never as a `/8`.
The values above are single addresses on purpose: the range's job is to name
the gateway, and a range wide enough to hold the whole LAN says every host on
the LAN is the gateway.

`CompiledGateway.Sanitize` strips the identity header from an untrusted
request, and it runs on the request path in both processes, so "stripped or
ignored" is literally true rather than a description of the authenticator's
behaviour. Its positive control checks that the same header from the *trusted*
peer survives, so the test is about trust and not about a function that
deletes everything it sees.

**Which hop strips.** Both, and they answer for different peers.

The engine publishes no port, so its only peer is `web-ui` over the internal
network. Its trust test can therefore only ever say "this came from `web-ui`",
which is equally true of a header the gateway set and one a client on the LAN
set against the published port and `web-ui` forwarded. `serve-ui` is the hop
that can tell them apart, because it holds the only published port and its
`RemoteAddr` is the real client's, which is why `serve-ui` takes
`--trusted-gateway` too and refuses to start a gateway profile without one.

Stripping unconditionally in the proxy would be the wrong fix, and quietly so.
On UGOS the platform gateway sits *upstream* of `serve-ui`, so a blanket delete
removes the legitimate identity and native authentication stops working
entirely. The proxy carries the same declared trust boundary the engine does
rather than a bare header name, and the engine strips as well so nothing
downstream of it, a handler or a log line or a middleware added later, can read
a value that was never trusted.

EPIC C's #92 proves the same boundary against the real UGOS gateway on real
hardware. This side needs no UGREEN device: the synthetic trusted peer is
loopback and the synthetic untrusted peer is everything else.

## Runtime-selected UI bundles

Issue #180. `serve-ui` used to embed one bundle with `go:embed` and offer no
alternative, and `ui/shared/vite.config.ts` picks the provider shell at build
time from `VITE_PLATFORM`. Together those meant shipping Synology's bridge
required compiling a Synology-specific binary, and section 3.7 requires every
provider package to carry the exact same core binary. The choice was between
the wrong bridge and a forbidden build.

The bundle is now resolved at run time, in this order:

1. `--ui-dir PATH` (`$UI_DIR`), an explicit directory. Wins outright.
2. `--ui-root PATH` (`$UI_ROOT`) plus the selected profile: the bundle served
   is `PATH/<profile>`.
3. the bundle compiled into the binary.

A configured `--ui-dir` or `--ui-root` that turns out to be unusable is a hard
start failure, never a silent fall back to the embedded bundle. "Unusable"
means missing, not a directory, or without an `index.html`. An empty directory
is exactly what a bind mount that did not mount produces, and serving it would
answer every route with 404 instead of saying what went wrong.

`ui/shared/scripts/build-bundles.mjs` (`npm run build:bundles`) builds one
bundle per provider into `dist-bundles/<provider>/`, which is what a package
ships beside the binary.

`apps/generic/tests/uibundle` proves the property that matters against a real
built artifact rather than against a function: one binary serves the embedded
bundle, two different profile-selected bundles and one package-supplied
bundle, and its sha256 is unchanged afterwards. A second test builds the same
source twice with a provider named in the environment and requires the two
digests to be identical, with a control proving the comparison can see a real
change.

### Which carrier each adapter uses

Issue #169 packaged this, and there are exactly three carriers because there
are exactly three kinds of thing that can hold a bundle.

| Carrier | Who uses it | How the bundle is selected |
|---|---|---|
| the binary | generic | nothing configured; the compiled-in bundle |
| the canonical image, at `/ui/bundles` | TrueNAS, Unraid, OpenMediaVault, Proxmox, Synology's Container Manager project | `UI_ROOT=/ui/bundles` plus the adapter's own `--profile=` |
| the package's own payload | Synology's `.spk` | `--ui-dir <target>/ui-bundle` |

An adapter that is metadata and nothing else, which is what a catalog entry, a
Docker template or a compose profile is, has no payload to put a bundle in, so
the image is its only carrier. A `.spk` installs native binaries and never
pulls the image at all, so the image is no carrier for it and it carries its
own; `spk.Build` refuses a package built without one, or with one built for
another provider, because a package that installs cleanly and shows the wrong
interface is the failure mode this whole issue is about.

**Five bundles in the image, and not seven.** `generic` is already compiled
into the binary and duplicating it buys nothing. `ugos` is EPIC D's, and its
UPK carries its own. The rest is arithmetic against a gated budget: measured
on `darwin-arm64-mac17-2`, `linux/arm64`, the image goes from 43,008,762 bytes
to **44,811,244** bytes, which is +1,802,482 (+4.19%) against a ceiling of
45,159,200 (1.05x). That leaves 347,956 bytes of headroom, which is less than
one more bundle: this image can carry these five and not a sixth. Shipping all
seven, as #167 estimated, would have been roughly 2.4 MB and outside the gate.

The end-to-end evidence, against the built image rather than against a
function:

```
serve-ui --profile=truenas --ui-root /ui/bundles  ->  deployment: "TrueNAS app (container)"
serve-ui --profile=generic                        ->  deployment: "Docker Compose"
serve-ui --profile=ugos    --ui-root /ui/bundles  ->  refuses to start:
    no usable UI bundle: --ui-root /ui/bundles has no usable bundle for
    profile "ugos" at /ui/bundles/ugos
```

The third line is the one worth reading twice. A missing bundle is a hard
start failure, never a silent fall back to the generic bridge, which is why
carrying too FEW bundles is a loud failure and not a quiet reappearance of
#180.

## Deriving an adapter instead of authoring one

Issue #169. Phase 4 shipped five platforms that agree with the canonical
runtime by review: each states its own image reference, its own mounts, its
own port and its own health check, and nothing compared those statements to a
single source. Five independently authored copies of one runtime definition is
the definition of drift.

`distribution/packaging/derive.go` makes the agreement mechanical. Seven
fields, one authoritative value each, and a mismatch that names the field:

| Field | Authority |
|---|---|
| `contract-version` | each platform's `derivesFrom.contract` against the contract's own version |
| `image-reference` | `canonical.json`'s `image.reference` |
| `runtime-profile` | `x-canonical-runtime.profiles`, and the platform's own declared profile |
| `storage-mounts` | `canonical.json`'s `containerPaths`, all of them, and none besides |
| `published-port` | `canonical.json`'s `listenPort`, on the Web UI role only |
| `health-check` | `canonical.json`'s per-role tests |
| `supported-architectures` | the release, once; an adapter states none of its own |

`contract-version` is the one that keeps the other six honest over time. A
derivation check that only compares values keeps passing after the contract
grows a field nobody applied, because there is no value to disagree with yet.
Repeating the version in each adapter makes a contract change fail every one
of them until somebody has re-derived it and said so.

Every field has a positive control that breaks it deliberately, run against
every adapter rather than against one, because the five are read out of four
different metadata formats and a rule that fires on a compose file and not on
an Unraid template is a rule Unraid does not have.

## Migrating a Phase 4 installation

The conversion changes declarations, not data. On every platform the state
directory, the backup root, the SSH key and `known_hosts` keep the host paths
they already had, so an existing installation keeps its catalog, its retained
artifacts and its enrolled administrator across the change.

One mount is redeclared, and the host side of it does not move either:

| | Phase 4 | Converted adapter |
|---|---|---|
| host path | `<appdata>/config/config.yaml` | `<appdata>/config` |
| container path | `/etc/backup-manager/config.yaml` | `/etc/backup-manager/config` |
| mode | `ro` | writable |

The file an operator already has stays exactly where it is; what the adapter
mounts is its parent directory, writable, which is issue #196. The directory
has to be writable by the container's uid/gid before the first start, because
a bind mount does not chown its source, and each platform's acceptance
procedure step 0 now says so.

What enforces the change is not the same on every platform, so it is worth
saying per platform rather than once:

| platform | what carries the old answer | what stops it |
|---|---|---|
| generic, OpenMediaVault, Proxmox | an env file the operator edits | `CONFIG_FILE` became `CONFIG_DIR`, a fail-closed `${VAR:?}` reference, so an unconverted file stops the deployment with a message |
| Synology (Container Manager) | `backup-manager.env` | the same, `${APPDATA:?...}/config` |
| TrueNAS (catalog) | the platform, not a file | the question was renamed `config` to `configDir`, so an upgrade has no stored answer to carry forward and the wizard asks again |
| Unraid | the operator's own template copy | nothing automatic: a changed `<Config>` Target does not retire a mapping already in the user template, so the old read-only file mapping has to be deleted by hand |
| Synology (`.spk`) | the package's own layout | the package installs the directory itself |

The `${VAR:?}` claim only ever covered the first three rows. There is no
`${VAR:?}` anywhere in a TrueNAS catalog answer or an Unraid template, and
saying otherwise made a fail-closed guarantee out of a property those two
platforms do not have. On TrueNAS the failure it hid was concrete: an upgrade
that kept a Phase 4 answer of `<pool>/backup-manager/config/config.yaml` bind
mounts that FILE at `/etc/backup-manager/config`, `--config` resolves to
`/etc/backup-manager/config/config.yaml` inside it, and the engine crash-loops
on ENOTDIR with a message naming neither the mount nor the migration.

Two things close that. The TrueNAS question carries a new identifier, so there
is no answer to carry forward. And the engine now recognises the shape: when
the configuration path's parent is a file rather than a directory it says so,
names issue #196 and points here, instead of reporting "not a directory".
Unraid gets the same message, which is the only thing that can help there,
because retiring an operator's existing mapping is not something a template
can do.

## Digest policy

`x-canonical-runtime.digest_policy` names `container/release-manifest.json`,
which records the image digest and the binary SHA-256 per architecture. Deploy
by digest, not by tag: a tag can be moved, a digest cannot.

`TestArchitecturesAgreeAcrossTheThreePlacesTheyAreWrittenDown` holds the
architectures declared here, in `distribution/packaging/canonical.json` and in
the release manifest to one value, so "the same source revision produces
linux/amd64 and linux/arm64" is checkable rather than asserted.

## The engine plus web-ui deviation, and what it costs

`container/compose.yaml` runs two services from one image. The engine has no
published port; `web-ui` serves the static UI and reverse-proxies `/api/v1` and
`/health` to the engine over a private bridge network. It is the only service
with a LAN-facing port.

That is a project-owner requirement, adopted for network isolation, and it is
in tension with the refactor's "one production application process wherever
practical". The tension was resolved in favour of keeping it: the split is
already shipped, it is not a new proxy, it runs the same binary from the same
image with no second runtime, and it satisfies both absolute rules (one
canonical Go executable per architecture, no production Node server). The
performance contract prohibits **adding** a data-path hop, and "one process
wherever practical" is explicitly qualified.

What #167 owed was the measurement, because an already-shipped hop whose cost
nobody has measured is indistinguishable from one that is fine.

`apps/generic/tests/perfbaseline/proxycost_test.go` (`PERF_PROXY_COST=1`) runs
the same read the Phase 6 baseline times, twice against the same engine
process: once directly, once through a real `serve-ui` process proxying to it
exactly as compose wires them.

Measured on `darwin-arm64-mac17-2`, workload `phase6-baseline-v1`,
`GET /api/v1/backup-sets` (6,438-byte response, 400 timed samples after 40
warmups), five runs:

| | direct p50 | proxied p50 | direct p95 | proxied p95 | hop p50 | hop p95 |
|---|---|---|---|---|---|---|
| median of 5 | 0.062 ms | 0.087 ms | 0.082 ms | 0.128 ms | **0.025 ms** | **0.047 ms** |
| range | 0.061-0.063 | 0.085-0.088 | 0.080-0.082 | 0.128-0.135 | 0.023-0.026 | 0.046-0.055 |

So the hop costs about **25 microseconds at p50 and 47 microseconds at p95** on
this host, on a read that itself takes 62 to 82 microseconds. In relative terms
that is a real cost, roughly 40% of a very fast loopback read, and in
absolute terms it is well under a tenth of a millisecond on a request whose
end-to-end budget is dominated by the browser and the network in front of it.

Two things worth noting rather than burying:

- The number lines up with `docs/perf/gate.json`'s own reasoning. #165 set the
  API-read noise floor at 0.05 ms and justified it as "below the cost of the
  cheapest structural regression this gate exists to catch, an added loopback
  proxy hop." The measured hop is 0.047 ms, which is just under that floor. The
  floor was set from measurement and it turns out to be tight against the
  thing it was sized for, which is worth knowing before anyone adds a second
  hop.
- The harness has one assertion, and it is there because the most misleading
  result it could produce is a hop cost of roughly zero from a misconfigured
  upstream answering out of the UI host's own static handler. It fails unless
  both paths return the same number of bytes.

**The line this holds.** No further hop, no sidecar, no second runtime. A
second proxy would double a cost that is already about half the read.

## Performance evidence for this change

All seven metrics EPIC B #81's performance contract names, measured with
`scripts/perf/capture-baseline.sh --repeat 5` on the designated benchmark host
`darwin-arm64-mac17-2`, workload `phase6-baseline-v1`, and compared against
#165's committed baseline with `scripts/perf/check-baseline.sh --compare`.
Nothing was re-baselined.

| metric | gated | baseline | this change | threshold | result |
|---|---|---|---|---|---|
| `api_read_p95_ms` | yes | 0.130 ms | 0.149 ms | ratio <= 1.10 **and** more than 0.05 ms above baseline | **pass**, delta 0.019 ms |
| `transfer_mb_per_second` | yes | 537.7 MB/s | 673.0 MB/s | >= 483.9 MB/s | **pass**, 1.25x |
| `idle_rss_bytes` | yes | 98,861,056 | 99,221,504 | <= 1.10x | **pass**, 1.004x |
| `config_write_p95_ms` | yes | 11.357 ms | 11.448 ms | <= 1.10x | **pass**, 1.008x |
| `image_size_bytes` | yes | 43,008,762 | 43,074,298 | <= 1.05x | **pass**, 1.0015x (+64 KiB) |
| `startup_to_healthy_ms` | no | 19.652 ms | 18.403 ms | recorded, not gated | 0.94x |
| `idle_cpu_seconds_total` | no | 0.12 s | 0.11 s | recorded, not gated | below the measurement floor on both sides |

Two of those deserve more than a tick.

**`api_read_p95_ms` moved 14.6% in ratio terms, and passed on the noise floor
rather than on the ratio.** That is the gate working as #165 designed it, not a
gate being lenient: the absolute movement is 0.019 ms, and the within-run
capture-to-capture spread of that same median was 62% of the median in this
very run. At 0.13 ms a 10% budget is smaller than the measurement's own
scatter, which is exactly why `gate.json` requires both conditions. Worth
saying out loud because the number will look like a near-miss to anyone reading
the ratio alone: it is not a near-miss, it is a metric whose ratio is not
meaningful at this magnitude, and the floor is what makes the gate real.

Note also that this is measured against the engine **directly**, so it does not
contain the reverse-proxy hop measured above. The hop is 0.047 ms, more than
twice this delta, which is a useful sanity check that the movement here is not
the two-service topology leaking into the direct path.

**`image_size_bytes` grew by 65,536 bytes.** That is the profile table, the
gateway authenticator and the bundle resolver compiled into
`/backup-manager-web`. It is 0.15% of the image against a 5% budget, and it is
real growth rather than noise: #165 recorded that two independent builds of the
same commit produced byte-identical image sizes, so there is no noise here to
hide in and no reason to describe 64 KiB as anything but 64 KiB.

`transfer_mb_per_second` improved by 25% and `startup_to_healthy_ms` by 6%.
Neither is attributable to this change: nothing here touches the transport or
the startup sequence, and both metrics have wide recorded spreads (27% and 15%
within this run). They are recorded because the contract names them, not
claimed as an improvement.

## Migration from the previous definition

Nothing was renamed and nothing was removed, so an existing deployment keeps
working with no change. What is new is additive:

| addition | effect on an existing deployment |
|---|---|
| `--profile=${RUNTIME_PROFILE:-generic}` on both commands | none; `generic` is what the previous build did |
| `TZ: ${TZ:-UTC}` | none; UTC is what the image defaulted to |
| `stop_grace_period` | the engine now gets 30s instead of Docker's 10s default, so a shutdown during a journal write is less likely to be killed mid-write |
| explicit `healthcheck` on the engine | the engine's compose healthcheck is now `/health/live` rather than the image's `backup-manager status`, so a DEGRADED or unconfigured instance no longer keeps `web-ui` from starting. Backup freshness stays the image's own HEALTHCHECK, the alerts block, and `docker compose exec rclone-manager /backup-manager status` |
| `UI_DIR` / `UI_ROOT` on `web-ui` | none when unset, which is the default |
| `x-canonical-runtime` | none at runtime; compose ignores unknown `x-` keys |

No mount, environment variable or host path changed, so there is no migration
path to test and nothing to roll back.

The configuration mount is the exception, and it has its own migration table
under "One mount is redeclared" above, including what enforces the change on
each platform and what does not.
