# 0001. Embed rclone behind transport adapter rather than fork or subprocess

## Status

Accepted.

## Context

This project pulls completed backup artifacts off a remote server over SFTP,
verifies them, commits them durably on a UGREEN NAS, and only then deletes
the remote copy. The actual bytes-moving-reliably part of that job, SFTP
transport, local filesystem writes, listing, hashing, retries, is a problem
rclone already solves well and has solved for years across a much wider
range of backends than we need.

We don't want to write our own SFTP client, and we don't want the backup
lifecycle logic (retention, last-known-good protection, the copy/verify/
commit/delete sequencing that the whole safety story depends on) to live
inside someone else's CLI tool or fork. We need rclone's data-plane code
without taking on rclone's release cadence, its CLI surface, or its full
backend catalog as our problem.

There are three realistic ways to consume rclone from a Go program with this
shape of requirement: fork it, embed it as a library behind our own
interface, or shell out to its CLI as a subprocess. We picked embedding. This
records why, and what we gave up to get it.

## Decision

We embed a pinned version of rclone's Go packages directly in this binary,
and we quarantine every rclone import behind one package,
`internal/transport/rclone`, which implements a manager-owned `Transport`
interface. Nothing outside that package is allowed to import rclone types.
Lifecycle, retention, state, and validation code only ever see our own
`Source`, `RemoteArtifact`, `TransferResult`, and error categories, never
rclone's.

We register exactly the backends we need for the initial use case, local and
sftp, as blank imports in the adapter, not the full backend catalog. We
version-pin rclone in `go.mod` rather than tracking latest, and any bump to
that pin goes through the regression gate described in
`docs/rclone-upgrade.md`, with a human reading the upstream release notes as
a mandatory, non-automatable step.

## Alternatives considered

### Fork rclone

Forking would let us patch anything we wanted, including deep internals, and
never worry about an upstream API changing under us. We rejected it anyway,
because a fork doesn't remove maintenance cost, it just relocates it onto us
permanently. Rclone is a large, actively developed project. Forking it would
make this project responsible for continuously merging upstream security
fixes, SFTP protocol changes, Go runtime compatibility work, and backend bug
fixes into our copy, forever, for functionality that is maybe five percent of
what we actually use. The backup-specific logic that is genuinely ours (GFS
retention, the durable commit sequencing, TOCTOU protection on delete) is
small. A fork would mean maintaining something enormous to protect something
small, which is backwards.

### Subprocess: shell out to the `rclone` CLI

This is the safest option in one specific sense: the CLI is rclone's
actual stable public contract, and it's far less likely to break between
versions than the Go package internals we now depend on. We rejected it as
the *primary* architecture for a few concrete reasons:

- Every result comes back as text (stdout, stderr, exit codes, `--json`
  where available) that we'd have to parse to get typed errors, transfer
  statistics, and hash values. That parsing layer is itself a compatibility
  surface we'd have to maintain, arguably a worse one than an API surface,
  since CLI output format is explicitly documented as unstable in places the
  library API is not.
- Context cancellation becomes process-signal management instead of a
  `context.Context` we can thread through everything else in the codebase.
- Destructive operations (the remote delete) become a subprocess invocation
  we have to trust completed exactly as intended, with no compile-time check
  that we passed the arguments we think we passed.
- It adds a subprocess lifecycle (spawn, monitor, reap, handle partial
  output on crash) to a codebase whose entire reason to exist is being
  precise about partial failure.

We didn't rule subprocess out forever, though. It's still the named fallback
if embedding turns out not to work. See the Phase 1 gate note below.

### CLI invocation as a thin wrapper library

We considered a middle option: still shell out, but wrap it in a Go package
with a typed interface so the rest of the codebase never sees raw CLI
output. This is strictly better than ad hoc subprocess calls scattered
around the codebase, but it doesn't change the fundamental tradeoff, we'd
still be parsing text to recover typed information rclone's own Go structs
already hold in memory one layer down. It just moves the parsing into one
file instead of many. We rejected it as the primary path for the same
reasons as plain subprocess invocation, while noting that if we ever do fall
back to subprocess (see below), this is the shape that fallback should take,
not scattered `exec.Command` calls.

## Consequences

### What we get

- One deployable binary. No separately installed `rclone` executable on the
  UGREEN NAS, no PATH dependency, no version skew between a system rclone
  and what we tested against.
- Typed Go errors and typed transfer results instead of output parsing.
- Real `context.Context` cancellation all the way through a copy operation.
- Direct access to transfer statistics and backend capability queries (for
  example, "does this backend support SHA-256 hashing") as Go API calls
  instead of CLI flags and scraped output.
- A narrow, auditable boundary. Every place rclone touches this codebase is
  one package. That package is where the destructive operation (`DeleteRemote`)
  lives, deliberately not exposed as part of a general `Move`, so there's
  exactly one place to look when reasoning about "how could this ever delete
  the wrong thing."

### What actually hurts

This is the part a transcript of the review wouldn't tell you, because
"approve" was the final position of all five reviewers and that reads as
cleaner than it is.

- **We inherited rclone's module graph, not just its code.** Importing only
  `backend/local` and `backend/sftp` still causes `go mod tidy` to resolve
  rclone's entire module graph, roughly 1.7GB and 260 modules, including
  every cloud SDK rclone supports, none of which we use or want. That's
  dependency-graph weight, not binary weight (the linked `linux/arm64`,
  `CGO_ENABLED=0` binary is 21MB, since the linker only keeps what's
  reachable from `main`), but it's real cost: slow cold resolves (over six
  minutes), a large module cache, and a bigger transitive CVE surface to
  monitor than the two backends we actually asked for would suggest.
- **"Only two backends" was never quite true.** The adapter imports
  `fs/operations` for `operations.Copy`, and that package imports
  `backend/crypt`, which self-registers via `init()`. So three backends are
  registered at runtime, not two, and nothing in a casual read of the
  adapter's blank imports would tell you that. This is a category error
  that's easy to make and we made it: module graph size, linked binary size,
  and runtime-registered backend count are three different numbers, and
  "we only import two backends" is a claim about none of them precisely.
  `docs/rclone-upgrade.md` documents how to check this (`go mod why`)
  because we expect it to matter again on a future upgrade.
- **We are coupled to Go API stability, which rclone does not promise as
  strongly as its CLI contract.** The CLI is rclone's actual public
  interface. Its internal Go packages can and do change shape between
  versions in ways the CLI's behavior does not. Embedding trades a
  well-documented compatibility boundary (the CLI) for a less-documented one
  (whatever subset of `fs`, `fs/operations`, and backend packages we
  happen to call). We manage this by pinning versions and gating upgrades
  hard (`docs/rclone-upgrade.md`), not by assuming the coupling is free.
- **Every rclone upgrade is now a project event, not a background update.**
  FR-2 requires compilation, unit tests, transport contract tests, SFTP
  integration tests, crash/reconciliation tests, destructive-safety tests,
  and a human release-notes review before a version bump can merge, and
  auto-merge is explicitly and permanently disabled for this dependency.
  That is the correct tradeoff for a tool whose failure mode is deleting the
  wrong backup, but it is slower than `Dependabot` quietly bumping a patch
  version, and it will stay slower on purpose.

### The Phase 1 gate's failure mode is a subprocess, not a fork

The EPIC's Phase 1 delivery plan treats embedding as a hypothesis to prove,
not a foregone conclusion: build for the target NAS, register only local and
sftp, prove listing/copy/cancel/host-key-verification/delete all work
through the manager-owned interface without leaning on unstable rclone
internals. If that gate fails, meaning the required behavior can't be
reached through stable, reasonably isolated APIs, the fallback is a
subprocess architecture (the CLI-invocation alternative rejected above,
promoted from fallback to primary), not a fork. Forking stays off the table
regardless of how the embedding experiment goes, for the reasons in
"Alternatives considered" above. A failed embedding experiment is evidence
that the Go API surface is too unstable to build on directly, which is an
argument for a harder boundary (a subprocess and its stable CLI contract),
not for taking on a fork's permanent maintenance burden to route around the
instability.

### A permanent local patch to rclone needs its own ADR

This decision assumes we consume rclone unmodified. If we ever find an
rclone API gap that can't be worked around inside the adapter, the EPIC's
stated preference is to contribute the fix upstream. If that's not possible
and we have to carry a local patch indefinitely, that is a different
decision with its own cost, a permanent patch creates exactly the same kind
of continuous-integration-with-upstream obligation that "why not fork"
above rejects, just scoped to one patch instead of the whole tree. That
decision needs its own ADR when it happens, it should not be waved through
as an implementation detail of this one.
