<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="rclone-manager mark: a broken ring standing for a transfer cycle in progress, next to the rclone-manager wordmark" width="240">
  </picture>
</p>


A backup lifecycle manager for a UGREEN NAS. It pulls completed backup artifacts off a
remote server over SFTP, verifies them, commits them durably, and only then deletes the
remote copy.

It is a standalone Go binary that **embeds pinned rclone Go packages**. It does not fork
rclone, and it does not shell out to the `rclone` CLI for normal data movement.

**If you're here because a backup didn't arrive and it's 3am:** skip to
[Recovery](#recovery-when-a-backup-did-not-arrive) below, or go straight to
[`docs/recovery.md`](docs/recovery.md).

## The rule everything else serves

> A remote backup artifact MUST NOT be deleted until a verified and durably committed NAS
> copy exists. If state is uncertain, preserve the remote copy.

Every section below is either explaining how that rule is enforced or admitting where the
enforcement doesn't exist yet.

## Status: what actually runs today

Read this before the rest of the document, because it changes how to read everything else.

The lifecycle engine, the SQLite journal, discovery, verification, durable commit, remote
delete with TOCTOU protection, GFS retention, reconciliation, disk capacity guards, health
computation and structured logging are all real, implemented, unit- and integration-tested
Go packages under `core/internal/`. None of that is aspirational; it's covered by
`go test ./...` today.

What doesn't exist yet is anything that runs them. `core/cmd/backup-manager/main.go` is 25 lines
and understands exactly one subcommand:

```bash
backup-manager version
```

That's it. There is no `run`, no `daemon`, no `status`, no `retention`, no `reconcile`, no
`restore`. Nothing loads a config file and drives an artifact from `DISCOVERED` to
`COMPLETE` outside of a test. Building that orchestrator is issue #25 (execution modes) and
#26 (the CLI surface), both open. Because everything the orchestrator would call lives
under `core/internal/`, Go's own visibility rules mean no other module can import it either, so
until #25/#26 land, this project cannot be operated by anyone from outside this repository.

A few pieces further down this document are implemented but not yet wired to anything that
calls them:

- `core/internal/capacity`'s disk guard is real and tested, but nothing in the transfer path
  calls it yet.
- `core/internal/obs`'s structured logger is real and tested, but nothing else in the repository
  logs through it yet, or at all.
- `core/internal/health`'s four-state computation is real and tested, but nothing renders it as
  a `status` command or an HTTP endpoint.
- Last-known-good protection (FR-19, issue #20) and the mandatory dry-run for local
  deletion (FR-20, issue #21) are not implemented. `config.Retention.ProtectLastKnownGood`
  exists as a validated, defaulted-to-`true` config field, but nothing downstream reads it.
  Nothing in this repository deletes a local file at all yet.
- UGREEN container packaging (issue #27) is being built in parallel; once it lands,
  `docs/deployment.md` is the place for it.
- The SFTP integration / crash-matrix / destructive-safety test suite (issue #31) and the
  broader security/resilience pass (issue #29) are open.

None of that makes the packages that do exist any less real, and the guarantees they
enforce (never delete before commit, refuse a delete when identity is uncertain, never
treat `.partial` as a restore point) hold today in every test that exercises them. It just
means there's currently no program you can point at a real server and walk away from.

## Installing the current UGOS build from a `.UPK` package

This is issue #91's hardware-acceptance package, not the finished product: the backend
inside it answers `GET /health/live` and nothing else. What it proves is that the packaging
and installation path onto a real UGREEN NAS actually works, before any real functionality
gets wired behind it. Treat everything below as a developer/proof-of-concept install, not
a release.

**This has to happen through UGOS Web (the browser-based desktop at the NAS's own address),
not through any UGREEN mobile or desktop companion app.** Those apps don't expose App
Center's manual/developer install path at all.

1. Build the package (from a Debian 12 environment with a pinned `ugcli`, which is normally
   the NAS itself over SSH — see `apps/ugos/docs/upk-proof-procedure.md` for the full
   procedure and `tools/ugcli-install/` for getting `ugcli` onto the NAS safely):
   ```sh
   cd apps/ugos/upk-proof
   ugcli check
   ugcli pack --arch amd64 --build 1
   ```
   This produces a signed `.upk` file under `build_dir/pkgs/upk/`.
2. Get that file somewhere your browser can select it from. If you built it over SSH on the
   NAS itself, either copy it to a share your NAS's file browser can reach (e.g. a
   `Downloads` folder under your own account's home directory) and download it from there,
   or `scp`/`sftp` it straight to your own machine.
3. Open UGOS Web in a browser, sign in, and open **App Center**.
4. Look for the manual/developer install option (this is usually a small entry point rather
   than a prominent button — a settings/developer menu, or an icon near the top of the App
   Center screen — since UGOS doesn't want casual users side-loading packages). Select the
   `.upk` file from step 2.
5. If the install fails with a signature/security error, see
   [Troubleshooting](#troubleshooting-upk-install) below before assuming the package itself
   is broken.

Once installed, the app's icon should appear in the installed-app list and open as an inner
UGOS desktop window (`open_type: inner`), and its `/health/live` endpoint should answer
`{"status":"ok"}`. None of that means backups are actually running yet — see the Status
section above for what's real today.

### Troubleshooting UPK install

*(Steps 3-4's exact wording depends on your UGOS Pro version; this section will get more
precise as more of this actually gets tested against real hardware. If you hit something
not covered here, that's worth reporting back so this section can grow.)*

## Who owns what

rclone owns the data plane: SFTP and local backends, listing, copying, hashing, deletion
primitives, transfer accounting. This project owns the control plane: backup-set config,
artifact discovery, the durable lifecycle journal, copy/verify/commit/delete sequencing,
GFS retention, validation and quarantine, and reconciliation after a crash.

```text
rclone:
    move bytes reliably

backup-manager:
    decide what those bytes mean,
    when they are safe,
    when the source may be destroyed,
    and which restore points must survive
```

That boundary is the central architectural constraint. Application packages outside
`core/internal/transport/rclone` do not import rclone packages, so upstream API churn stays
contained in one adapter. `core/internal/transport/rclone/backends_test.go` fails the build if
that ever stops being true.

## Why rclone is embedded, not forked, not shelled out to

The short version: writing our own SFTP client is a bad use of time, and rclone already
does the data-plane part of this job well. That leaves three ways to consume it: fork it,
embed it as a library behind our own interface, or shell out to its CLI. We embed it.

We didn't fork it because a fork doesn't remove maintenance cost, it relocates it onto us
forever, for a large actively-developed project where we use maybe five percent of the
surface. We don't normally invoke the `rclone` CLI as a subprocess because every result
would come back as text we'd have to parse to recover typed errors and transfer statistics,
context cancellation would become process-signal management instead of a `context.Context`,
and the delete call, the most dangerous line in this codebase, would become a subprocess
invocation with no compile-time check that we passed the arguments we meant to.

The full reasoning, every alternative considered, and what embedding actually costs (not
just what it buys) is in
[`docs/adr/0001-embed-rclone-behind-transport-adapter.md`](docs/adr/0001-embed-rclone-behind-transport-adapter.md).
Read it if you're deciding whether this architecture fits a similar problem; this README
only summarizes it.

### The adapter

Every rclone import in this repository lives under `core/internal/transport/rclone`. Everything
else in the codebase depends only on the manager-owned interface in
`core/internal/transport/transport.go`:

```go
type Transport interface {
    List(ctx context.Context, source Source) ([]RemoteArtifact, error)
    Stat(ctx context.Context, source Source, remotePath string) (RemoteArtifact, error)
    CopyToLocal(ctx context.Context, source Source, remotePath, localPartialPath string) (TransferResult, error)
    RemoteHash(ctx context.Context, source Source, remotePath string, algorithm HashAlgorithm) (string, error)
    DeleteRemote(ctx context.Context, source Source, remotePath string) error
}
```

Notice what's missing: there is no `Move`. Copy, verify, commit and delete are four
separately owned steps on purpose. A `Move` would collapse them and take the delete
decision away from the lifecycle manager.

### The pinned version, and the backend count that surprised us

`core/go.mod` pins `github.com/rclone/rclone v1.75.0`. `core/internal/transport/rclone/adapter.go`
blank-imports exactly two backend packages, `backend/local` and `backend/sftp`. But the
adapter also needs `operations.Copy` from `fs/operations`, and that package itself imports
`backend/crypt` for an unrelated feature (decrypting filenames for `--show-encrypted`).
Backends self-register via `init()`, so importing `fs/operations` registers `crypt` too,
silently, as a side effect nothing in a casual read of the blank imports would reveal. So
importing two backends registers three. This is measured, traced to the exact import chain,
and pinned by `TestRegisteredBackendsExactSet` in
`core/internal/transport/rclone/backends_test.go`, so the registered set can't widen again
without the build failing.

If you need to confirm what's actually registered in a built binary rather than trust this
paragraph: `go mod why github.com/rclone/rclone/backend/crypt` shows the chain, and
`go version -m ./backup-manager | grep rclone/rclone` reads the exact linked rclone version
back out of a compiled binary, which is a faster sanity check than trusting whatever
`core/go.mod` said at build time actually got shipped.

### Upgrading the pin

No rclone dependency bump auto-merges, ever, not even a patch version with green CI. A
human reads the release notes. `docs/rclone-upgrade.md` is the actual checklist: what the
CI gate (`rclone-upgrade-gate.yml`) enforces today versus what's still manual, how to run
the regression set locally, and how to check what got registered instead of only what got
imported. Read it before touching the version in `core/go.mod`.

## Connecting to a remote: SSH/SFTP and the restricted account

`docs/ssh-setup.md` is the full walkthrough: generating a dedicated key, creating a
shell-less, chrooted SFTP-only account that can list/read/delete eligible artifacts but
can't overwrite a completed one, and verifying the server's host key out-of-band instead of
trusting whatever answers first. `core/internal/transport/rclone/ssh.go` refuses to build a
connection at all without both a real key file and a real `known_hosts` file; there's no
password fallback and no way to disable host-key checking.

That hardening has a direct consequence for verification and delete safety, and it's
important enough to state here instead of only in the setup doc: **rclone's SFTP hashing
works by running a hash command over the SSH session, and a shell-less
`ForceCommand internal-sftp` account has no shell to run one in.** So the account this
project's own setup guide recommends cannot supply a remote hash. See
[Verification](#verification) and [TOCTOU protection on delete](#toctou-protection-on-delete)
below for what that means in practice, but the short version is: against the recommended
deployment, remote deletes are usually refused, and that's not a bug.

## An artifact is identified by its basename, and that has a consequence

`model.ArtifactID` is a backup set plus a plain basename. It refuses anything containing a
path separator, which is deliberate: a remote filename is untrusted input, and the cheapest
place to stop a name like `../../etc/passwd` is the moment it first becomes an identity
rather than at whichever later call site forgets to check.

The cost shows up now that discovery recurses. Two remote paths that end in the same
filename collapse to one identity, so `gitea-runs/run-1/backup.dump` and
`gitea-runs/run-2/backup.dump` are the same artifact as far as the journal is concerned.
The journal's `UNIQUE (source, backup_set, artifact_name)` refuses the second one, and
discovery reports it as a conflict naming both paths rather than dropping it silently or
failing the whole batch.

Listing is sorted by remote path so that outcome is repeatable. Before that fix it was not:
`walk.GetAll` returns backend order, so whichever path the backend happened to yield first
won, and the pair swapped places between runs. One cycle ingested `run-1` and reported
`run-2` as a conflict, the next did the reverse, and neither was reliably backed up.

Sorting makes the conflict stable, not absent. If your producer writes one directory per
run with a fixed filename inside, you will get exactly one artifact ingested per backup set
and a conflict for every other run, which is almost certainly not what you want. Until
identity carries more than a basename, give the artifacts distinct names, for example by
putting the run stamp in the filename rather than only in the directory.

## The lifecycle

An artifact moves through twelve states, defined in `core/internal/lifecycle/state.go` and
`machine.go`, which are the single source of truth; the table below is a summary, not a
substitute.

```text
DISCOVERED -> TRANSFERRING -> TRANSFERRED -> VERIFYING -> VERIFIED
    -> COMMITTING -> COMMITTED -> REMOTE_DELETE_PENDING -> COMPLETE

FAILED         reachable from any state before COMMITTED; exits to
               DISCOVERED (retry) or QUARANTINED (retry budget spent)

QUARANTINED    reachable from VERIFYING, COMMITTED, REMOTE_DELETE_PENDING;
               exits to DISCOVERED only (a fresh attempt might recover it)

QUARANTINED_LOST   reachable only from COMPLETE; TERMINAL, no exit at all
```

`COMPLETE` and `QUARANTINED_LOST` are the two terminal states, and they mean opposite
things. `COMPLETE` is the only state that confirms the remote source is already gone, which
is exactly why it's the only predecessor of `QUARANTINED_LOST`: if the durably committed
local copy is later found corrupted and the remote copy is already deleted, there is no
copy of that artifact left anywhere, and no automatic path recovers it. `QUARANTINED`, by
contrast, means the content looked bad while a remote copy still exists or hasn't been
confirmed gone, so retrying from `DISCOVERED` has a real chance of fixing it.

This twelfth state isn't in the original FR-10 list; it was added because the eleven-state
version had no way to represent "the source is confirmed gone and the only copy we have is
bad," and sending that case back to `DISCOVERED` the way `QUARANTINED` does would just
livelock against a source that no longer exists. Whether an artifact is currently
`QUARANTINED_LOST` matters enough operationally that it's checked first, unconditionally,
in health computation (see [Status and health](#status-and-health)).

### Verification

`core/internal/lifecycle/verify.go` runs three layers, each with a different failure shape:

1. **Transfer verification**, always performed: the local file opens, reads without an I/O
   error, and its size matches what the transfer step recorded. Failing this means the copy
   didn't actually happen the way the journal claims; it's an operational failure, so it
   produces `FAILED`.
2. **Hash verification**, gated by `validation.hash` in config. If the operator hasn't
   asked for it, transfer verification is the whole guarantee for that backup set, and
   that's a legitimate choice. If they have asked for it, the manager either trusts an
   already-verified transfer-time checksum or asks the backend directly, and if the backend
   can't answer (see the shell-less SFTP account above), verification **fails explicitly**
   rather than silently falling back to a size check. A confirmed mismatch produces
   `QUARANTINED`, not `FAILED`, because that's a positive finding about the content, not
   about the copy mechanics.
3. **Application validation**, gated by `validation.command`, an optional external program
   (for example, something that opens a database dump and confirms it restores). A required
   validator's failure or timeout also produces `QUARANTINED`.

### Durable commit

`core/internal/lifecycle/commit.go` implements the FR-14 sequence between `VERIFIED` and
`COMMITTED`:

1. record `COMMITTING` in the journal, before touching any file;
2. `fsync` the `.partial` file's content;
3. atomically promote it to its final name without clobbering an unrelated collision
   (`linkWithoutClobbering`, a hard-link-then-remove, not a plain rename);
4. `fsync` the containing directory, because the directory entry that now points at the
   final name is a separate inode from the file's data, with its own separate write-back
   state, and skipping this step is the mistake that lets a crash leave content that was
   genuinely fsynced sitting under a name nothing in the directory points at yet;
5. record `COMMITTED`.

Every step is idempotent and safe to resume after a crash at any point in the sequence.

### TOCTOU protection on delete

`core/internal/lifecycle/remotedelete.go` is, by its own doc comment, "the most dangerous line
in the project," the only call site allowed to invoke `Transport.DeleteRemote`. Before
issuing a delete it revalidates, from scratch, every time:

1. the journal artifact is `COMMITTED` or `REMOTE_DELETE_PENDING`;
2. the expected local final file exists;
3. the local file's identity is consistent with what the journal recorded;
4. the remote object still matches what was captured at discovery, via
   `model.CompareIdentity`.

That fourth check is the TOCTOU defense: `RemoteIdentity` (path, size, mtime, hash where
available, a backend stable identifier where available) is captured once at discovery and
compared again immediately before delete. `CompareIdentity` can only reach
`ConfidenceStrong` through a hash match, a stable-identifier match, or an outright mismatch;
everything else, including "size and mtime both agree, nothing else was available," only
reaches `ConfidenceWeak`, and `IdentityComparison.Preserve()` is true for that outcome, same
as for a confirmed change. **Given the hardened SFTP account this project's own setup guide
recommends has no hash capability, that weak case is the normal, expected outcome, not an
edge case.** So in that deployment, `DeleteRemote` routinely refuses to delete, on purpose,
per the rule at the top of this document. That is not free: an archive that never prunes
its remote side will eventually fill the source disk. Every refusal is a typed
`*RemoteDeleteRefusalError` and gets written back into the journal's `remote_delete_error`
column, specifically so it's a queryable fact and not just a log line. See
[Recovery](#recovery-when-a-backup-did-not-arrive) for what to actually watch for this.

### Reconciliation

On startup, before normal processing touches anything, `core/internal/reconcile` compares what
the journal believes against what the local filesystem and the remote backend actually show
right now, for every scenario FR-17 names:

| Remote  | Local         | Journal               | Behavior                          |
|---------|---------------|------------------------|------------------------------------|
| exists  | absent        | `DISCOVERED`           | transfer (no-op here, next cycle's job) |
| exists  | partial       | `TRANSFERRING`         | safe retry/restart (no-op here)    |
| exists  | final         | `COMMITTED`            | verify and proceed toward delete   |
| absent  | final         | `REMOTE_DELETE_PENDING`| reconcile to `COMPLETE`            |
| absent  | final         | `COMPLETE`             | no-op                              |
| exists  | invalid final | any                    | preserve remote; quarantine local  |
| absent  | invalid final | any                    | quarantine, unrecoverable: `QUARANTINED_LOST` |
| changed identity | final | delete pending    | refuse delete; investigate         |

The last two rows are why `QUARANTINED_LOST` exists: the original FR-17 table had no row
for "remote already gone and the local copy is bad," and that case can't be treated the same
as "remote still there, local copy is bad," because there's nothing left to re-fetch from.
Every reconciliation transition is idempotency-keyed so a crash mid-reconciliation is safe
to retry. This runs per backup set today (`reconcile.Reconcile`); nothing in `cmd/` calls it
yet, for the same reason nothing calls the rest of the pipeline (see
[Status](#status-what-actually-runs-today)).

### Quarantine

Quarantine is a state, not a place. There is no quarantine directory and no file gets
moved; only the `artifacts.state` column changes, to `QUARANTINED` or `QUARANTINED_LOST`.
The file stays exactly where it was, its `.partial` path if quarantined before commit, or
its final committed path if quarantined afterward by reconciliation. See
[The lifecycle](#the-lifecycle) above for the states themselves.

## Retention

### GFS retention

`core/internal/retention` implements deterministic GFS (grandfather-father-son) classification
for every managed, completed backup in a set:

| Tier    | Default            |
|---------|---------------------|
| Daily   | 7 days              |
| Weekly  | 3 calendar months   |
| Monthly | 12 calendar months  |

`KEEP` is the union of whatever the daily, weekly and monthly tiers each retain (the newest
valid backup in every calendar bucket their look-back window covers). The calculation takes
"now" as a plain argument rather than calling `time.Now()`, specifically so the same journal
state always produces the same verdict regardless of when or where it runs. One thing worth
knowing if you're comparing this against `docs/EPIC.md`: the EPIC's example default
timezone is `America/Vancouver`, but `core/internal/config`'s actual validated default is `UTC`.
This package defers to whatever config supplies rather than hardcoding the EPIC's example,
so the honest current default is UTC; set `retention.timezone` explicitly if you want
something else.

**`GFSDecide` only classifies. It deletes nothing, and this repository has no code path
that deletes a local file yet.** A verdict of `Keep: false` is a delete *candidate*, not a
delete order.

### Last-known-good protection

**Not implemented.** FR-19 says the newest known-good restore point must never be deleted
solely for exceeding retention age. `config.Retention.ProtectLastKnownGood` exists as a
config field, defaults to `true` when the key is omitted, and is validated, but nothing
downstream reads it. `core/internal/retention`'s own package doc says plainly that it "does not
know about last-known-good protection." Tracked as issue #20. Until it lands, GFS's
`Keep: false` candidates are only candidates in principle, since nothing acts on them at
all, but don't rely on that as a substitute for the real protection once deletion is
actually wired up.

## Deployment on the UGREEN NAS

The target shape: one static binary linked against the pinned rclone module (a
`linux/arm64`, `CGO_ENABLED=0` build is 21MB), no separately installed `rclone` executable
on the NAS, no PATH dependency, no version skew between what's tested and what's deployed.
The SQLite journal needs a persistent volume; the SSH key and `known_hosts` file need to be
mounted read-only.

The full container build, compose/orchestration setup, and UGREEN-specific packaging is
issue #27's job and lands in `docs/deployment.md`. This README only covers the parts that
are true regardless of how the container ends up built.

## Status and health

`core/internal/health` computes two structurally separate things and enforces, by test, that
they never share a field: process health (is the binary alive, what version is it, what
rclone version is embedded) and backup-set health, one of four states:

- **HEALTHY** – a known-good backup exists within the freshness threshold, and nothing else
  is wrong.
- **DEGRADED** – either no history yet, or a known-good backup is still fresh enough but
  something less than ideal just happened (a quarantined newest arrival, a failure still
  being retried).
- **STALE** – no known-good backup inside the freshness threshold, and nothing suggests
  one is imminent.
- **FAILING** – checked first, unconditionally: any `QUARANTINED_LOST` artifact, or a
  `FAILED` artifact with no retry scheduled.

This computation is real and tested (`core/internal/health/compute.go`), including the
`QuarantinedCount`/`QuarantinedLostCount` aggregates FR-24 asks for. **Nothing renders it
yet.** There is no `backup-manager status` command and no `/health` HTTP endpoint; the
package's own doc comment says it's meant to back exactly those, both separate, open work.
Until one exists, computing health for real means querying the journal yourself, which is
exactly what [Recovery](#recovery-when-a-backup-did-not-arrive) below walks through.

## Recovery: when a backup did not arrive

This is the section to read under pressure. The fuller version, with more of the "what if"
branches, is [`docs/recovery.md`](docs/recovery.md); this is the part you shouldn't have to
click through to get.

Since there's no `status` or `restore` command (see [Status](#status-what-actually-runs-today)),
everything here comes down to one fact: **the SQLite journal at `state.database` is the
truth, and it's a plain SQLite file.** Query it directly:

```bash
sqlite3 /path/to/state.db "
  SELECT artifact_name, local_path, state, updated_at, remote_delete_error
  FROM artifacts
  WHERE source = 'production' AND backup_set = 'postgres-primary'
  ORDER BY updated_at DESC
  LIMIT 20;
"
```

Only three states are ever a valid restore point: **`COMMITTED`, `REMOTE_DELETE_PENDING`,
`COMPLETE`**. That's not a convention, it's the exact set `core/internal/health` calls
`knownGood`. Everything else, `DISCOVERED` through `COMMITTING`, `FAILED`, `QUARANTINED`,
`QUARANTINED_LOST`, or any `.partial` file you find sitting on disk regardless of what the
journal says, is not a restore point. Take the newest row in one of the three good states;
its `local_path` is the file, already fsynced and atomically committed (see
[Durable commit](#durable-commit)). Copy it wherever you're restoring to.

If the newest row for that backup set is `QUARANTINED_LOST`: that specific backup is gone
for good. The remote copy was already deleted before the local corruption was found, and no
automatic path recovers it (see [The lifecycle](#the-lifecycle)). Look at the next-newest
row in a known-good state and treat the loss as real when deciding what to tell whoever
needs the data, not as something to retry.

If it's `QUARANTINED` (not `_LOST`): the remote copy may still exist. The design intends
this to self-heal the next time discovery and reconciliation run against that backup set,
but there's no daemon running them automatically yet (see
[Status](#status-what-actually-runs-today)), so today that means either running them
yourself against these packages, or fetching the artifact by hand over SFTP with the key
and `known_hosts` from `docs/ssh-setup.md`, and re-running whatever validator the config
names.

If a row has been sitting at `REMOTE_DELETE_PENDING` for longer than you'd expect, look at
its `remote_delete_error` column before assuming something is stuck. Given the deployment
this project recommends, a persistent refusal there is the expected behavior described in
[TOCTOU protection on delete](#toctou-protection-on-delete), not necessarily a bug, and it
means the remote copy is very likely still sitting on the source server, still recoverable,
just not pruned. Left unattended, that also means the remote source disk isn't being
freed by this project on that backup set; monitor it directly rather than assuming pruning
is happening in the background.

## Toolchain

Go 1.27, and Docker for the disposable SFTP server the integration tests use.

The engine lives in its own `core/` Go module (`core/go.mod`), separate from the
repository root, so every command below runs from `core/`:

```bash
cd core
go build ./...
go vet ./...
go test ./...
```

CI (`.github/workflows/ci.yml`) runs the same three commands on every push and pull
request, with the Go module cache preserved between runs, and separately cross-compiles the
whole module (`go build ./...`, not just `core/cmd/backup-manager`) for both UGREEN targets
(`linux/amd64` and `linux/arm64`, `CGO_ENABLED=0`) as a compile check.
`.github/workflows/rclone-upgrade-gate.yml` runs whenever `core/go.mod` or `core/go.sum`
changes and reports the FR-2 checklist status.

## Layout

`core/` is its own Go module (`core/go.mod`), separate from the repository root, drawn
that way by #106/B1.1 so the engine has never heard of a provider or a UI (see
`docs/EPIC-B-multi-nas.md` §7 for why). `core/cmd/backup-manager/` is the entry point
(today, just `version`); `core/internal/` holds every application package, and every
rclone import stays inside `core/internal/transport/rclone/`:

```text
core/internal/
  config/       YAML config schema, loading, validation (Load takes any path)
  model/        shared identity types: ArtifactID, BackupSetID, RemoteIdentity, CompareIdentity
  discovery/    turns a raw remote listing into artifacts proven complete
  lifecycle/    the state machine plus every step: transfer, verify, commit, delete
  state/        the SQLite journal: durable, idempotent transition recording
  retention/    GFS classification
  reconcile/    startup reconciliation against the journal, filesystem and remote
  capacity/     disk-space admission checks (not yet wired into a transfer)
  health/       process and backup-set health computation (not yet exposed anywhere)
  obs/          structured event logging (not yet called by anything)
  transport/    the manager-owned Transport interface and the rclone adapter behind it
```

Config example: `core/internal/config/testdata/full.yaml` has a complete, valid config with
every field populated; that's a better reference than hand-writing one here, since it's
exercised by the config package's own tests and won't silently drift out of sync with the
schema the way a README example would.

`apps/common/` is a second, much smaller Go module (`apps/common/go.mod`): the
`PlatformCapabilities`/`PlatformAdapter` contract every provider app composes over
(`apps/common/platform/capabilities/`, `docs/EPIC-B-multi-nas.md` §3.4), plus two
reserved-but-empty packages (`apps/common/webhost/`, `apps/common/auth/local/`) that hold
the location the real `/api/v1` implementation and local-account auth land in (#94/B1.5) —
out of scope for #106/B1.1, which only draws the boundary. `apps/common/tests/` is a
separate small TS package: the one place in the repo that legitimately imports every
provider's frontend bridge at once (the provider-conformance matrix, §63A), kept outside
`ui/shared/` specifically so removing a provider never breaks `ui/shared`'s own build.

`ui/shared/` is the one shared frontend every provider app builds against
(`ui/shared/src/`), never providing its own product UI (see `docs/EPIC-B-multi-nas.md` §11):
pages, components, the `PlatformBridge` contract (`ui/shared/src/platform/`,
`ui/shared/src/types/platform.ts`), and the single causl-ts state graph
(`ui/shared/src/state/graph.ts`). A provider app under `apps/<provider>/frontend/`
supplies a `PlatformBridge` implementation and, for the seven that exist today
(`generic`, `ugos`, `synology`, `truenas`, `unraid`, `openmediavault`, `proxmox`), nothing
else — `ui/shared` never imports a provider, only the reverse.

This project was originally scoped as `tools/backup-manager/` inside `iasbuilt/iac`. It
lives here instead; nothing in the design depended on the location.

## Documentation index

- [`docs/adr/0001-embed-rclone-behind-transport-adapter.md`](docs/adr/0001-embed-rclone-behind-transport-adapter.md) – why embed, why not fork or shell out, what it costs
- [`docs/rclone-upgrade.md`](docs/rclone-upgrade.md) – the pinned-version upgrade procedure and its CI gate
- [`docs/ssh-setup.md`](docs/ssh-setup.md) – the dedicated key, the restricted SFTP account, host-key verification
- [`docs/recovery.md`](docs/recovery.md) – recovery and the restore procedure, in full
- [`docs/phase-1-gate.md`](docs/phase-1-gate.md) – the embedding proof-of-concept verdict and what it did and didn't prove
- [`docs/deployment.md`](docs/deployment.md) – UGREEN container packaging (issue #27; check it exists yet)
- [`docs/EPIC.md`](docs/EPIC.md) – the full specification this project is built against, including where it and the code have since diverged
