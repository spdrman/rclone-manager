# 0002. Phase 5 scope: what to build now, out of the six security/resilience candidates

## Status

Accepted.

## Context

Issue #29 (A5.1) covers the whole of Phase 5. `docs/EPIC.md`'s Delivery Plan lists Phase 5
as "Potential" and names six candidates: immutable NAS snapshots, an off-site copy, separate
ingestion/retention privileges, alerts, metrics, and WORM/immutable storage. None of it is
committed scope. The one concrete architectural obligation Phase 5 creates ahead of time is
Security Requirement 14: "Architecture should permit future immutable/off-site copies." That
is a constraint on today's design, not a mandate to build the feature now.

Before weighing the six candidates, it matters what state the rest of the codebase is
actually in, because that changes which of them are even ripe. I read the code rather than
trusted the README's framing, and the two agree: as of this ADR, `go test ./...` covers real,
tested logic in `internal/lifecycle`, `internal/discovery`, `internal/retention`,
`internal/state`, `internal/health`, `internal/obs`, `internal/capacity`, `internal/reconcile`,
`internal/revalidate`, and `internal/quarantine`, and none of it is wired to anything that
runs continuously:

- `cmd/backup-manager/main.go` is 25 lines and understands exactly one subcommand,
  `version`. There is no `run`, `daemon`, `status`, `retention`, or `reconcile` subcommand.
  Issues #25 (execution modes) and #26 (the CLI surface) are both still open.
- `internal/obs`'s structured logger has zero callers anywhere in this repository, including
  its own tests' subjects. Nothing logs through it yet, so FR-23 is a library, not an
  observed system.
- `internal/health`'s four-state computation (`ComputeBackupSetHealth`, `Report`) is imported
  by nothing outside its own package. Nobody builds a `health.Report` from real data yet;
  its own package doc says it exists to back "a CLI command and an HTTP handler, both
  separate issues," and both are still open.
- `internal/capacity`'s disk guard is real and tested, but nothing in the transfer path
  calls it yet (its own package doc says the same).
- FR-20, local deletion safety, does not exist as code at all. `internal/retention.GFSDecide`
  and `ApplyLastKnownGood` classify artifacts into keep/not-kept; nothing deletes a local
  file anywhere in this repository (`grep` for `os.Remove` outside `internal/lifecycle` turns
  up nothing, and `docs/recovery.md` says as much directly). Issue #21 is open.
- The crash-matrix/destructive-safety test suite that would substantiate "the core is
  trustworthy" is issue #31, phase 2, and it is still open.
- `container/Dockerfile` and `container/compose.yaml` are explicit that they are "packaging
  ahead of" the daemon: the shipped command is `["version"]`, restart policy is documented
  as wrong until a real long-running command exists, and both files carry `TODO(daemon)`
  markers.

So four earlier-phase issues (#21, #25, #26, #31) sit strictly before #29 in the delivery
plan and are still open. That is the first-order fact this ADR has to respect: Phase 5 is
not next in line by the project's own plan, and several of its candidates are specifically
about hardening or extending pieces (a live process to alert from, a delete path to make
WORM-aware) that do not exist yet to hardened or extend. That does not make the candidates
worthless to think through now, security requirement 14 explicitly asks for exactly that,
but it does mean the honest output of this exercise is mostly "here is what the architecture
already permits, and here is what would have to be true before building it is a good trade,"
not a queue of features to land in this PR.

## The six candidates

### 1. Immutable NAS snapshots

This is a property of the storage the local backup directory lives on (a ZFS or btrfs
snapshot schedule, a UGOS-level snapshot feature), not of this program. Nothing in
`cmd/`, `internal/`, or `container/` runs on the NAS with the privilege or the API access to
take a filesystem snapshot, and nothing should: that capability belongs to the NAS's own
admin tooling, entirely outside this project's threat model and its container's
`cap_drop: [ALL]` posture (`container/compose.yaml`).

What this codebase *can* influence, and already does correctly, is whether its own write
pattern is safe to snapshot. `internal/lifecycle/commit.go`'s FR-14 sequence
(fsync `.partial` -> link-without-clobbering to the final name -> fsync the directory) means
that at any instant an external snapshot could observe the backup directory, every file in
it is either not yet visible under its final name or fully, durably committed under it.
There is no window where a snapshot could capture a torn or partially-written "final" file.
That guarantee was built for FR-14's own reasons, not for snapshotting, but it is exactly
the property a snapshot-based recovery story needs, and it required no new code to get.

**Recommendation: not now, and not really this codebase's to build.** The actionable
version of this candidate is operational guidance (point the configured local backup root
at a snapshot-capable volume, schedule snapshots independently of `backup-manager`), which
belongs in deployment documentation, not in `internal/`. Revisit only if a requirement
appears for `backup-manager` itself to trigger a snapshot (e.g. shell out to a NAS vendor
API right after a `COMPLETE` transition) — at which point the trigger, the specific
snapshot mechanism, and its failure modes all need to be named before it's a design, not
just a feature checkbox.

### 2. Off-site copy

This is the one candidate Security Requirement 14 names directly, so it's worth being
precise about how far "permits" reaches today.

The layering is right: `internal/lifecycle` owns every state transition and is the only
thing that decides when to move an artifact between stages, and `internal/transport` is the
only path to any remote (`internal/transport/rclone/backends_test.go` fails the build if
that stops being true). An off-site copy step would be, in shape, exactly one more lifecycle
step calling through transport, the same pattern `Commit` and `DeleteRemote` already use.
Nothing about the current design fights that.

The concrete gap is narrower than "needs a redesign": `transport.Transport` has no
local-to-remote method today (`List`, `Stat`, `CopyToLocal`, `RemoteHash`, `DeleteRemote`,
nothing symmetric to `CopyToLocal`). But the adapter's own implementation
(`internal/transport/rclone/adapter.go`) shows why that gap is cheap to close:
`CopyToLocal` is `operations.Copy(ctx, dstFs, nil, dstName, o)` against two `fs.Fs` values
built generically by `fsFor` from a `transport.Source`. Nothing about that call cares which
side is local and which is remote; a `CopyFromLocal` implementation is close to a mirror
image of the existing one, not new rclone plumbing.

What is real work, not a mirror image, is everything above the adapter: a new
`transport.Transport` method means a new obligation in `internal/transport/contract`'s
reusable contract suite; a genuine off-site copy needs its own lifecycle states and
`machine.go` transition rules (when is it attempted, does a failure block the existing
`COMMITTED -> REMOTE_DELETE_PENDING` path or run independent of it, what does reconciliation
do with an artifact that is `COMMITTED` locally but only half-copied off-site); a second
destination needs its own `internal/config` schema (a second `Remote`, its own credentials);
and the journal needs new columns via a migration. None of that is small, and layering a new
destructive-adjacent state machine surface on top of a lifecycle engine whose own
crash-matrix and destructive-safety suite (#31) has not landed yet is the wrong order to do
it in.

**Recommendation: not now.** The architecture genuinely does permit it (requirement 14's bar
is "should permit future," not "already includes," and that bar is met), and of the two
"potential storage feature" candidates (this one and WORM), this is the one closest to
buildable. Revisit once there's a concrete off-site target (a specific second remote and its
own credentials, not a hypothetical one) and #31 is green.

### 3. Separate ingestion and retention privileges

Worth separating two different claims hiding under one phrase.

**Package separation between "ingestion" and "retention" is real, not incidental.**
`internal/retention` imports `config`, `lifecycle`, `model`, and `state` — nothing from
`internal/transport`, at all. `GFSDecide` and `ApplyLastKnownGood` classify journal rows by
timestamp and state; they never touch a remote credential, never call `Transport`, and (per
`gfs.go`'s own package doc) never touch the filesystem either. That boundary isn't a
convention anyone has to remember to honor, it's structural: retention has no import through
which it could reach a remote even if it wanted to.

**Privilege separation on the remote side does not exist.** "Ingestion" (discovery + the
copy in `CopyToLocal`) and what actually retires the remote source
(`internal/lifecycle.DeleteRemote`, FR-15) go through the exact same `transport.Transport`
value and the exact same `transport.Source`, which carries exactly one `KeyFile` and
`KnownHosts`. `remotedelete.go`'s own package doc calls `DeleteRemote` "the one call site in
this whole codebase that is allowed to invoke `transport.Transport.DeleteRemote`", and that
is enforced by code review and there being literally one call site in the source tree, not
by any capability the credential itself lacks. The same SSH key that lists and reads a
remote is the same key a compromised or buggy build could use to delete with, because
nothing downstream of `config.Remote` ever had a second, more restricted credential to
reach for. `container/compose.yaml` matches this at the deployment layer too: one process,
one `uid:gid`, one mounted `SSH_KEY_FILE`, used for everything the binary does.

Retrofitting real separation means: a second credential in `config.Remote` (or a second
`Source` per backup set) for whichever operations should carry less privilege; either two
`Transport` instances or a capability-scoped wrapper that refuses `DeleteRemote` unless
constructed with the delete-capable credential; and, since a single OS process holds
whatever credentials it's given in memory regardless of how many Go values wrap them, real
protection against a *compromised running process* (as opposed to protection against a
leaked config file) needs two separate OS processes or containers, not just two structs.
That's a deployment redesign, not a library addition, and `config.Remote` is the one truly
load-bearing data contract in this project (every backup set's identity and every credential
flow through it) — the most expensive place in the codebase to make a speculative change.

**Recommendation: not now.** The half of this that was cheap (package-level separation
between ingest and retention logic) is already done, on its own merits, not because anyone
built it for Phase 5. The half that's still missing (remote-credential separation between
read/copy and delete) is a real, multi-layer change with no driving scenario yet: nobody has
asked for the manager to run with a read-only ingestion identity distrusting its own delete
path. Revisit only alongside a concrete threat model that names what a compromised ingestion
path is expected to be unable to do, since that's what would decide where the privilege
boundary actually needs to sit (config, process, container) rather than guessing.

### 4. Alerts

`internal/health.BackupSetHealth` already carries a `State` (`HEALTHY`/`DEGRADED`/
`STALE`/`FAILING`) and a human-readable `Reason` for every backup set, and `State.OK()`
already answers "does this need attention" with one call. `remotedelete.go`'s own doc
comment names the one genuine gap directly: turning "an artifact has been stuck at
`REMOTE_DELETE_PENDING` past some age" into a health signal is called out as intentional
future work, but that's a few fields' worth of computation belonging inside
`internal/health` itself (an age threshold alongside the count `BackupSetHealth.
PendingDeletes` already carries), not a new package, and this issue's file scope doesn't
let me touch `internal/health` to add it.

Beyond that gap, "alerts" as a Phase 5 line item is really asking for two different things:
a *signal* (which `internal/health` already computes) and *delivery* (deciding severity
tiers, a channel — webhook, email, Slack, a pager — and de-duplication policy). The signal
half is done. The delivery half is pure speculation right now: this project has no
configured notification channel, no on-call concept, and no stated preference between "log
loudly" and "page a human," and guessing at that shape produces exactly the kind of
infrastructure nobody asked for that this issue warns against building. A new package that
re-derives "is this backup set unhealthy" from `health.State != Healthy` would just be
`State.OK()` under a different name.

**Recommendation: not now**, and this is the clearest "no" of the six: building alert
*delivery* without a named channel is guessing, and the alert *signal* already exists in a
package I'm not allowed to touch here. Revisit once #25/#26 give this project a live process
worth alerting from, and once there's an actual answer to "alert whom, how."

### 5. Metrics

This is the strongest candidate, and the reasoning is narrower than "alerts, but cheaper."
`health.Report` (`internal/health/health.go`) is a complete, already-computed, already-tested
snapshot: `ProcessHealth` (binary/rclone version) plus one `BackupSetHealth` per configured
set (state, reason, every timestamp and count FR-24 asks for). Rendering that as text is a
mechanical transformation with no policy content: no threshold to pick, no severity to
invent, no channel to guess at. It's the same "pure library ahead of its own wiring" pattern
this codebase already uses for `internal/health` itself, whose own doc comment says it
exists so "a CLI command and an HTTP handler, both separate issues" can print it without
recomputing anything.

It also doesn't need a daemon to be useful the way alert delivery would: a Prometheus
exposition-format renderer is exactly the shape the "textfile collector" pattern wants
(`backup-manager status --prometheus > some.prom` on a cron, no HTTP server, no long-running
process), which fits a binary that doesn't have a daemon mode yet better than a `/metrics`
HTTP endpoint would.

I checked whether to reach for `github.com/prometheus/client_golang`, which is already an
*indirect* dependency in `go.mod` (pulled in transitively through rclone's own stats
plumbing) — using it directly wouldn't need a new module, only flipping its `go.mod`
annotation from indirect to direct, which is exactly the file I'm told not to edit. I hand
rolled the small, stable subset of the text exposition format instead, entirely in the
standard library. That's more conservative than it needed to be technically, but it means
this change touches nothing outside a brand new package.

**Recommendation: build this now.** It's genuinely cheap, it's a pure function with no
side effects and no wiring, it can't get the earlier-phase issues' sequencing wrong because
it doesn't depend on any of them landing first, and it's exactly the "well-chosen small
amount of code" this issue asks for rather than padding. See "Decision" below.

### 6. WORM / immutable storage

Like snapshots, this is fundamentally a storage-backend property (S3 Object Lock, a
NAS volume mounted with an immutable attribute, a WORM-certified appliance), not something
`internal/` code can create by itself. But unlike snapshots, there's a real design tension
worth naming rather than waving past: WORM as usually understood means a written object can
never be deleted, and this project's own retention policy (FR-18/FR-19) exists specifically
to delete artifacts once they age out of every GFS tier and aren't last-known-good. A literal
"never deletable" store is incompatible with that policy's whole purpose, not merely
unfinished by it. A workable design would need the storage-level lock's expiry tied to a
retention horizon decided at write time (e.g., set a fixed retain-until date at commit
covering the outermost tier `GFSDecide`'s config allows), which is a strictly more
conservative, less precise substitute for GFS's actual per-run, per-bucket recomputation
(`gfs.go`'s package doc is explicit that "protected" and tier membership are recomputed
every run against current state, not fixed at write time). That's a real, nontrivial design
problem, not a checkbox, and it can't be scoped sensibly before two things are true: FR-20
(actual local deletion, issue #21) exists in code at all — WORM is a constraint on a delete
capability that doesn't exist yet — and a specific storage backend is chosen, since the
mechanism (a filesystem attribute vs. an object-lock API call) is backend-specific and
would live in a place this project hasn't built yet.

**Recommendation: not now, by a wide margin**, and defer the design question (not just the
code) to its own future ADR once FR-20 lands and a concrete storage target is named.

## Decision

Build one new package, `internal/metrics`, and nothing else.

`internal/metrics.Render` takes an already-computed `health.Report` and returns Prometheus
text exposition format (content type `text/plain; version=0.0.4; charset=utf-8`) covering
process info and, per backup set: health state (as a one-hot label set, the standard
Prometheus enum pattern), newest-good-backup age, stale threshold, pending deletes,
failures, quarantine counts (recoverable and lost, broken out separately, matching
`BackupSetHealth`'s own separation), in-flight transfer count, free space, and the
last-successful-poll/last-completed-backup/last-retention-run timestamps, whichever of
those the caller actually populated (`BackupSetInputs`' optional fields stay optional here
too; an unset value omits that metric series rather than fabricating a zero).

It is deliberately a pure, dependency-free, standard-library-only transformation with no
new call site anywhere else in the repository. Wiring it into a `backup-manager status
--prometheus` flag or an HTTP handler is issue #25/#26's job, once a daemon or a CLI exists
to call it from; this package is written so that wiring, whenever it lands, is a few lines
calling `metrics.Render`, not a redesign.

## What is deliberately not built

- No alert delivery of any kind (no webhook, no email, no severity policy). The signal
  already exists in `internal/health.State`; delivery has no named channel to build toward.
- No change to `internal/health` to add the stuck-`REMOTE_DELETE_PENDING`-age signal
  `remotedelete.go` calls out, because that change belongs inside a package this issue's
  file scope does not let me touch. Recorded here so it isn't lost: whoever next touches
  `internal/health` should read that package's `remotedelete.go` comment first.
- No off-site copy, no new `Transport` method, no new lifecycle state, no config or journal
  schema change. Real, incremental, and premature ahead of #31.
- No privilege-separation change to `config.Remote`, `transport.Source`, or the container's
  credential mounts.
- No snapshot- or WORM-related code, and no attempt to guess at a storage backend for either.

## Consequences

`internal/metrics` adds one small, fully unit-tested package with zero production call
sites yet, the same position `internal/health`, `internal/obs`, and `internal/capacity` are
already in. That's a known, accepted shape in this codebase, not a new kind of risk.

Deferring the other five candidates means Phase 5 stays open after this PR, which is the
correct outcome given four earlier-phase issues (#21, #25, #26, #31) are still open ahead of
it. This ADR is the record of why, so the next time #29 or a related issue is picked up, the
reasoning doesn't have to be redone: check whether the trigger named under each candidate
above has actually occurred before building it.
