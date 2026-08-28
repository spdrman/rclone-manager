# Phase 1 gate verdict

Issue #2. This is the go/no-go call docs/EPIC.md's Delivery Plan Phase 1 asks for: prove
rclone can be embedded behind the manager-owned transport interface without leaning on
unstable internals, or reassess toward a subprocess architecture (never a fork).

By the time I picked this up, every other Phase 1 sub-issue had already merged (#3 the
pinned module and upgrade procedure, #4 the Transport interface, #5 backend registration,
#6 SSH security, plus the standalone testing and upgrade-gate issues). So this isn't a
warm-up gate anymore, it's a synthesis: pull all of that together, fill in the pieces
nobody had proven yet with a real SFTP server, and give the actual verdict.

## Verdict: proceed

Every required capability is proven against real rclone code, most of it against a real
disposable SFTP server in Docker, not by reading the API and reasoning about what it
probably does. Nothing needed an unstable internal. The one real integration bug this work
turned up (below) had a fix that only touches the adapter's own config-building code and
stable, exported rclone APIs. The subprocess fallback is not needed.

## The checklist

| # | Requirement | Result | Evidence |
|---|---|---|---|
| 1 | A Go application embeds rclone successfully | PASS | `go build ./...` on this module; `cmd/backup-manager` links against the pinned rclone v1.75.0 with no CGO. |
| 2 | Only the local and SFTP backends are registered | PASS, with a documented exception | `local` and `sftp` are the only direct imports. `fs.Registry` also has `crypt`, registered transitively through `fs/operations` (needed for `operations.Copy`). Traced, measured (~2% of binary size) and accepted in `internal/transport/rclone/backends.go`, enforced by `TestRegisteredBackendsExactSet` so it can never widen silently. See "What did not pass outright" below for why this doesn't sink the gate. |
| 3 | Remote listing works | PASS | `TestPhase1Gate/Listing` and `/Connects`, real `Adapter.List` against `tests/sftpfixture`'s Docker SFTP server. |
| 4 | Single-file copy works | PASS | `TestPhase1Gate/CopyAndTransferStatistics` copies a 256KiB file over SFTP and compares it byte-for-byte against the source. |
| 5 | Context cancellation works | PASS, with a caveat | A context cancelled before a chunked transfer starts, or mid-transfer, reliably aborts `CopyToLocal` (`TestPhase1Gate/ContextCancellation/*`). A context cancelled before a single quick `List` round trip does not reliably abort it, see below. |
| 6 | Transfer statistics are accessible | PASS | `TestPhase1Gate/CopyAndTransferStatistics` reads `accounting.StatsGroup(ctx, group).GetBytes()` / `GetTransfers()` after a real SFTP copy and gets the real numbers back, not just the adapter's own return value. |
| 7 | Explicit remote delete works | PASS | `TestPhase1Gate/ExplicitDelete`: `DeleteRemote` over SFTP, checked two ways, the file is gone from the server's filesystem and gone from a follow-up `List`. |
| 8 | Host-key verification works | PASS | `internal/transport/rclone/ssh_test.go`'s `TestSFTPHostKeyVerification`, merged with #6: a positive control, an unknown host key refused, and a changed host key (MITM) refused, all against a real Docker sshd. I didn't duplicate that here; `TestPhase1Gate/Connects` just confirms my own fixture and Source are wired correctly. |
| 9 | The target UGREEN architecture builds and runs | PASS | `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build` produces a 21MiB static binary (matches the measurement in #5's PR). I went further than "builds" for this gate: I ran that exact binary, and the amd64 equivalent, inside `docker run --platform linux/arm64|amd64 alpine:3.20`, and both printed `backup-manager version` correctly. Building was already established before I started; running it is new. |

**Exit gate: proceed.** The required rclone APIs (`fs.Find`, `fs.NewFs`/`info.NewFs`,
`fs.Object`, `operations.Copy`, `fs/accounting`, `fs/hash`) are all stable, documented,
non-experimental parts of rclone's Go surface, and every one of them is reachable from
inside `internal/transport/rclone` without reflection tricks, unsafe casts, or copy-pasted
private code.

## What did not pass outright, and why it doesn't change the verdict

**crypt is registered even though nothing asks for it.** This is #5's finding, not mine,
carried over here because the exit-gate checklist explicitly asks for it. It's a real,
accepted gap against "only the required backends," not a fabrication or an oversight: it's
traced to one specific import (`fs/operations/lsjson.go`), measured, and pinned by a test
that fails the build the moment the registered set changes again. The exit-gate question is
whether the *required* rclone APIs can be isolated behind the interface without unstable
internals, not whether the registered backend count is exactly two. It can, crypt riding
along is a configuration-surface cost, not evidence of an unstable or unisolatable API.

**A single already-cancelled context doesn't reliably abort a plain `List`.** rclone's
accounting layer checks `ctx.Err()` before every chunked read
(`fs/accounting.Account.checkReadBefore`), which is exactly why `CopyToLocal` cancellation
is rock solid, a multi-GB backup artifact is many chunks, cancellation lands on one of them
almost immediately. A directory listing is one round trip with no chunk boundary for that
check to land on, so it can complete before the cancelled context is ever consulted. For
this project's actual shape, that's the right capability to have reliably: the operation
that runs for minutes and needs to be interruptible is the transfer, not the listing. I
still recorded the `List` behavior honestly (`TestPhase1Gate/ContextCancellation/AlreadyCancelledContext_List`)
instead of only testing the case that happens to pass.

**Remote hash verification is not available at all against a properly hardened SFTP
account.** `TestPhase1Gate/RemoteHashCapability` tried it: the sftp backend's hash support
works by running a remote shell command (`sha256sum`, `md5sum`, etc.) over the same SSH
connection, and the restricted, shell-less, `ForceCommand internal-sftp` account FR-6 asks
for has no shell to run one in. `RemoteHash` fails with an explicit capability error rather
than hanging or silently reporting no hash, which is exactly what FR-13 requires of that
path, but it means Phase 2's verification story for a real, hardened remote account cannot
lean on remote hashing at all. FR-13 already names the fallback: verify a checksum the
producer supplies alongside the artifact. Worth flagging now, before retention or
verification code gets written assuming a capability that won't be there in production.

**The reusable transport contract suite (#30) isn't wired up against SFTP yet.** It runs
against the local backend today (`internal/transport/transport_test.go`), and its own doc
comment names this issue as the one that should add an SFTP fixture. I looked into it and
didn't do it here: `contract.Fixtures.SupportedHash()` has to name an algorithm the backend
can actually compute, and, per the finding above, a properly hardened SFTP account can't
compute any of them. Wiring the generic suite against SFTP means either giving the fixture
shell access it wouldn't have in production (weakening what the fixture proves) or changing
the contract interface to let a backend legitimately support zero hash algorithms (a change
to a shared file outside this issue's scope). Recording the tradeoff here instead of picking
one silently.

**`List` doesn't see subdirectories, for either backend.** Already documented by #30's
`TestRcloneAdapter_List_DoesNotRecurseIntoSubdirectories`, not something I re-found. Noted
here only because a future backup-set config that nests artifacts in subdirectories would
hit it.

## What I actually built for this issue

`tests/sftpfixture` stands up a disposable `atmoz/sftp` container per test run: two host
keys (ed25519 and RSA, both mounted so `known_hosts` matches whichever golang.org/x/crypto/ssh
negotiates, since it prefers RSA-family algorithms by default regardless of which type is
pinned), a throwaway client key authorized for one user, and a bind-mounted upload directory
so the test can seed and inspect remote files directly from the host side without going
through the adapter for setup. All of it lives under `tests/.run` while a test is running and
is removed by `t.Cleanup`, gitignored as a backstop.

`internal/transport/rclone/gate_test.go` drives the real `Adapter` (`New()`, no test double)
against that fixture for everything the checklist above marks PASS with fresh evidence:
listing, stat, copy with content verification, transfer statistics, explicit delete, both
cancellation shapes, and the remote-hash capability check. Mid-transfer cancellation uses
rclone's own bandwidth limiter (`accounting.TokenBucket`, set to 64KiB/s) rather than a huge
payload to reliably outrun loopback Docker networking. That keeps the test fast and light on
disk while still proving the cancellation checkpoint is real: the 1MiB payload is throttled to
an estimated ~16 seconds if uncancelled, and cancelling 150ms in reliably aborts it in
practice around 2 to 3 seconds (see the test log), with the local backend's own
cleanup-on-error path removing the partial file.

## A dead end worth recording

Early on, `TestPhase1Gate/HostKeyVerification/CorrectKnownHosts` failed with `ssh: subsystem
request failed` even against a correctly pinned host key. I chased it down to `fsFor`
building the sftp backend's config from a bare `configmap.Simple{}` instead of going through
`fs.ConfigMap`, so options with real declared defaults (`subsystem` defaults to `"sftp"`,
`chunk_size` and `concurrency` default to values `pkg/sftp` requires to be at least 1) came
out as Go zero values instead, breaking every sftp operation, not just transfers. By the time
I'd built a standalone reproduction to confirm the fix, #6 had already merged the same fix
(`sftpConfig` in `ssh.go` now sets all three explicitly, with the reasoning written down). I
mention it here because it's good evidence for the verdict either way: the failure mode was a
config-wiring bug reachable through entirely stable, documented rclone APIs
(`fs.ConfigMap`, or just setting the three keys by hand), not a sign that rclone's Go surface
is too unstable to build on.
