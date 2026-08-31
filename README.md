<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="rclone-manager mark: a broken ring standing for a transfer cycle in progress, next to the rclone-manager wordmark" width="240">
  </picture>
</p>


A backup lifecycle manager for a NAS. It pulls completed backup artifacts off a remote
server over SFTP, verifies them, commits them durably, and only then deletes the remote
copy.

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
I rewrote this section against the code at the commit this branch was cut from, not against
the specification and not against the previous draft, and I made the parts of it that a
machine can decide into tests rather than sentences (see
[How this document is kept honest](#how-this-document-is-kept-honest)).

### The engine and the CLI are real

`core/` is a working backup engine with a working command line. `backup-manager` registers
twelve commands, and the list below is checked against the dispatch table in
`core/cmd/backup-manager/main.go` on every run of the gate, so it cannot quietly go stale
the way its predecessor did.

<!-- BEGIN CLI-COMMANDS -->

| Command | What it does |
|---|---|
| `run` | perform one processing cycle and exit |
| `daemon` | repeat the processing cycle at `poll_interval` |
| `check` | validate config and the state database, then exit |
| `status` | report process and backup-set health (FR-24), exiting non-zero unless every set is HEALTHY |
| `sources` | list configured sources and backup sets |
| `artifacts` | list journal artifacts, optionally filtered by `--source` and `--backup-set` |
| `fetch` | run one backup set's cycle on demand |
| `retention` | preview GFS and last-known-good retention decisions, with per-run policy overrides |
| `reconcile` | run FR-17 reconciliation for every backup set |
| `validate` | re-check one artifact's durable local copy |
| `catalog` | `catalog rebuild` reconstructs a lost or corrupted state database from the sidecar recovery manifests |
| `version` | report the binary, Go and embedded rclone versions |

<!-- END CLI-COMMANDS -->

Every command except `version` takes `--config`, defaulting to
`/etc/backup-manager/config.yaml`. `backup-manager` with no arguments prints that same list
and exits 2.

The lifecycle engine, the SQLite journal, discovery, verification, durable commit, remote
delete with TOCTOU protection, GFS retention, last-known-good protection, local prune,
reconciliation, disk capacity guards, scheduled revalidation, sidecar recovery manifests,
quarantine reporting, proactive alerting, health computation and structured logging are all
real, implemented, unit- and integration-tested Go packages under `core/internal/` and
`core/service/`. Each of them is now reachable from the command line, from the web host, or
from both, which is the gap the previous version of this section described honestly and
which has since been filled.

### The API and the web UI meet in the middle

`apps/common/webhost` serves a versioned `/api/v1`, authenticated, CSRF-protected, with a
destructive-operation gate in front of anything that can destroy data.
`apps/generic/cmd/backup-manager-web` is the binary that hosts it: `serve` runs the engine,
the scheduler and the API in one process, and `serve-ui` serves the built static UI and
reverse-proxies the API to it. `ui/shared` is the React application both of them exist for.

Until #211 the browser client asked for fourteen `(method, path)` pairs that neither
`api/v1/openapi.json` nor `apps/common/webhost/router.go` had, so against a real backend
the dashboard, the backups list, the activity feed and the quarantine page all failed
outright with "The backup service returned an unexpected response." Every suite in the
repository stayed green while that was true, for the reason the next paragraph gives.

Four of those were the wrong path for an operation that existed, and are now the right one.
The other ten are real surfaces, added spec-first (contract, regenerate, then handlers) over
reads core has always computed and `backup-manager artifacts`, `status` and `catalog`
already print: the backups list and one backup, the activity feed over the append-only
lifecycle record, quarantine plus its revalidate and retry actions, the operations list,
enabling and disabling a backup set, the FR-24 health verdict, and catalog scan and rebuild.

**What keeps it that way is a check, not this paragraph.**
`scripts/api/check-client-paths.sh` reads `ui/shared/src/api/client.ts` statically, reduces
every request path it builds back to a `(method, path)` pattern, and requires each one to be
an operation the contract declares. It has no allowlist, it fails closed (a path it cannot
reduce, or a client method whose request it cannot find, is a failure rather than a skip),
and `scripts/ci-local.sh` runs it on every commit. Ten of `scripts/api/selftest.sh`'s
mutation controls exist to prove it can actually fail.

The same drift had been recorded before, exactly, in
`ui/shared/src/api/contract.conformance.test.ts`'s own list of unserved paths, described
there as "recorded debt, not an exemption mechanism". That description was accurate and the
suite was still green: an allowlist asserted exactly is a gate reporting the drift it was
built to catch as a pass. The list is empty now and says why it must stay so.

**The Playwright suite is still not evidence about the API, because it never talks to the
runtime.** `ui/shared/src/app/createApp.tsx` substitutes `createMockApi` whenever
`import.meta.env.DEV` is set, and `ui/shared/playwright.config.ts` runs the suite against
`npm run dev`. The mock implements whatever the client asks for, which is precisely why it
was green throughout. The suite is a real test of what the browser renders and of how the
pages behave, and it is no evidence at all about the API. Do not read a green e2e run as an
end-to-end proof.

### What is built but not exposed

- `core/internal/metrics` renders an already-computed health report as Prometheus text
  exposition format, and nothing imports it. There is no `/metrics` endpoint on any
  listener. `docs/adr/0002-phase-5-scope.md` is the reasoning for stopping there.
- Restore execution is out of scope by design, not by omission: there is no `restore`
  command and no restore endpoint. [Recovery](#recovery-when-a-backup-did-not-arrive) below
  is the manual procedure, and it is the whole of it.
- A release build does select a provider frontend, since #167 and #169. `serve-ui`
  resolves its bundle at run time (`--ui-dir`, then `--ui-root/<profile>`, then the
  compiled-in one) and fails to start rather than falling back to the generic bridge, and
  the canonical image carries the five provider bundles the shipped adapters name. What is
  still not exposed is a sixth: the image budget has room for these five and not another,
  and `ugos` carries its own in EPIC D's UPK, which does not exist here.
- `serve` refuses to start without a valid config file, because `core/service.Open` loads
  and validates one before anything else happens. An app-store install that has never been
  configured therefore cannot get as far as a setup screen. #176 implements the engine half
  of a first-run experience and is not merged yet.
- A packaged container can write its own configuration, since #169 carried #196's
  mount-role change: every adapter now bind-mounts the config DIRECTORY, writable, at
  `/etc/backup-manager/config` rather than the single `config.yaml` read-only. The three
  merged write paths that go through that file (creating a backup set, saving settings,
  first-run setup) reach a writable filesystem in a packaged container. What an operator
  still has to do by hand is make the host directory writable by the container's uid/gid
  before the first start: a bind mount does not chown its source, and the runtime image is
  distroless with no root step, so each acceptance procedure's step 0 says so.

### What has actually been exercised on real hardware

Nothing. Not one of the acceptance procedures in [`docs/acceptance/`](docs/acceptance/) has
been executed, because nobody working on this repository has a TrueNAS, Unraid,
OpenMediaVault, Synology or Proxmox VE machine to execute them on. The procedures are
written, reviewed and specific, and they are prose until somebody runs them.

[`docs/conformance/phase-4-matrix.md`](docs/conformance/phase-4-matrix.md) is the generated
record and it says the same thing from the other side: twenty cells across five providers
report `PENDING_OPERATOR`, which is that matrix's word for "the automated half held and the
hardware run has not happened". In section 68's own words, every one of those providers is
**build-supported and uncertified**. A green conformance matrix proves the packaging
metadata is well-formed and mutually consistent, and it proves nothing whatsoever about how
any of these platforms behaves.

The image itself has never been published either. `ghcr.io/spdrman/backup-manager` is the
settled target and `distribution/packaging/canonical.json` records `published: false`, so
that reference resolves to nothing today and every acceptance procedure opens with a step 0
covering how to make it resolvable in the meantime.

### There are no screenshots in this document

There should be, and issue #112 asks for them per provider. I did not add any, and I would
rather say so than ship something that looks like evidence and is not. `docs/assets/` holds
the two logo files and nothing else. The only screenshots I could produce from this tree
would be of the mock API in a dev server, which is exactly the kind of picture that makes a
reader believe a claim this document has just spent a section retracting. Real screenshots
need a running packaged deployment, and a running packaged deployment needs #196 and #166
first. Provider logos are a separate question and a trademark one, so they are the project
owner's call rather than mine.

## Installing it

### The canonical Compose runtime is the install path

There is one product here, not eleven. Every platform below wraps the same multi-architecture
OCI image and the same Compose topology, and the differences between them are host paths and
metadata formats. `container/compose.yaml` is that topology, and
[`docs/deployment.md`](docs/deployment.md) is the reasoning behind every setting in it.

Two services, one image. `rclone-manager` runs `/backup-manager-web serve`: the core service,
the scheduler, local authentication and `/api/v1`, in one process on one shutdown context,
with **no published port at all**. `web-ui` runs `/backup-manager-web serve-ui`: the static
UI plus a reverse proxy to the engine, and it is the only service with a LAN-facing port.
They meet on a private project-scoped bridge network, which is what makes the engine's
isolation a topology rather than a convention. `/backup-manager` (no `-web`) is the same
image's headless binary for a deployment that wants no web listener at all.

```bash
cd container
cp .env.example .env      # then edit: PUID/PGID, the host paths, LISTEN_PORT
docker compose up -d
```

Both containers run non-root as `PUID:PGID`, with a read-only root filesystem, all
capabilities dropped and `no-new-privileges`. The image has no shell and no init step, so
**the host paths have to exist and be owned by that uid/gid before the first start**;
nothing in the container will chown them for you.

### Which mount holds what, and why they are never the same directory

Every platform mounts three separate places for three different jobs, and conflating any two
of them is the mistake this section exists to prevent.

| Mount | Holds | Written by | `.env` key |
|---|---|---|---|
| Private application state | the SQLite journal and its `-wal`/`-shm` files | the app, constantly | `STATE_DIR` |
| Backup data | the retained artifacts and their sidecar recovery manifests | the app, on commit | `BACKUP_DIR` |
| Credentials and configuration | `config.yaml`, the SSH private key, the pinned `known_hosts` | you, out of band, read-only | `CONFIG_FILE`, `SSH_KEY_FILE`, `KNOWN_HOSTS_FILE` |

The SSH private key is the one that matters. It lives with the configuration, mounted
read-only, and it must **not** be inside the backup root: put it there and every backup of
that directory carries the key that can read and delete the source. The backup root on every
platform below is a dedicated child directory rather than a share you already use, for the
same reason.

`distribution/packaging/canonical.json` is the single source of truth for these paths, and
this repository's own test suite fails the build if any platform's metadata disagrees with
it.

### What "supported" means for each target

<!-- BEGIN SUPPORT-MODEL -->

| Target | Tier | What ships in this repository today | Where the paths are defined |
|---|---|---|---|
| Generic Docker and Linux | Tier C | the canonical image, `container/compose.yaml`, and `apps/generic`'s own Go module for the web host | `container/compose.yaml` |
| TrueNAS | Tier B | a custom-app Compose file plus a TrueNAS Apps catalog entry, metadata only | [`apps/truenas/README.md`](apps/truenas/README.md) |
| Unraid | Tier B | two Community Applications Docker templates, metadata only | [`apps/unraid/README.md`](apps/unraid/README.md) |
| Synology DSM | Tier B | a real `.spk` built by `apps/synology`, wrapping the release binaries unchanged and checking their digest against `container/release-manifest.json` | [`apps/synology/README.md`](apps/synology/README.md) |
| OpenMediaVault | Tier C | a Compose deployment profile, metadata only | [`apps/openmediavault/README.md`](apps/openmediavault/README.md) |
| Proxmox VE | Tier C | the same Compose profile for a dedicated container-host guest, metadata only | [`apps/proxmox/README.md`](apps/proxmox/README.md) |
| UGREEN UGOS Pro | Tier A | the frontend bridge and nothing else: no `.UPK`, no packaging | EPIC D, issue #83 |

<!-- END SUPPORT-MODEL -->

The tiers come from `docs/EPIC-B-multi-nas.md`'s support-tier list, from `canonical.json`
for the four container profiles it declares, and from `conformance.json`, which declares all
seven targets with their tiers. The gate checks every row of this table against those two
files rather than trusting the table, and it checks in both directions: a row here that
neither file declares is a failure, and so is a target they declare that this table has
dropped.

Two things about the Proxmox row are worth saying out loud. Its paths are inside the guest,
not on the PVE host: the supported model is a dedicated container-host guest with one host
directory or dataset shared into it, and running the app on the PVE host itself is ruled
out. And the Unraid row is the one profile where the engine's isolation is weaker than the
others, because both Unraid templates join a durable, host-wide, generically named bridge
the operator creates by hand; every container on such a bridge can reach every port of every
other one, so the engine does not trust forwarded headers there and rate-limits on the
proxy's own address instead. `apps/unraid/README.md` says so too.

### What is deliberately not being built

EPIC B commits to a support model, and the deferrals are part of it. New Synology `.spk`
work, native DSM SSO, a native OpenMediaVault Workbench plugin, a Proxmox Web UI plugin, a
Portainer plugin or API extension, a Dockge plugin, a second application server for any
provider, provider-specific backup engines and provider-specific copies of the React
application are all explicitly out of scope unless one of them is later proven necessary.

The Synology line reads like a contradiction and is not one. #85 shipped an `.spk` in Phase
4 because Phase 4 shipped as written, and the deferral is about **new** `.spk` work; #169
adds a Container Manager Compose path alongside the shipped package rather than replacing
it. Retiring shipped packaging would be a product decision and nobody has made one.

Portainer, CasaOS, ZimaOS and Dockge appear in EPIC B's Phase 6 support model as targets
that get a documented deployment profile. None of them exists in this tree yet, so this
document does not list them as installable.

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
project's own setup guide recommends cannot supply a remote hash. I re-checked this against
the code for this rewrite and it is unchanged:
`core/internal/transport/rclone/adapter.go` still treats an absent hash capability as a
correct outcome rather than a failure, and `errors_test.go` still fails if `RemoteHash` ever
starts succeeding against a shell-less account, so the capability cannot be silently
downgraded into a weaker check. See [Verification](#verification) and
[TOCTOU protection on delete](#toctou-protection-on-delete) below for what that means in
practice, but the short version is: against the recommended deployment, remote deletes are
usually refused, and that's not a bug.

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
putting the run stamp in the filename rather than only in the directory. I re-checked this
for the rewrite: the separator ban in `core/internal/model/ids.go` and the `UNIQUE`
constraint in `core/migrations/0002_quarantined_lost.sql` are both still exactly as
described.

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
livelock against a source that no longer exists. I re-checked the transition table for this
rewrite: `{From: Complete, To: QuarantinedLost}` is still the only edge into it, and it
still has no outgoing edges at all, pinned by `TestOnlyCompletePrecedesQuarantinedLost`.
Whether an artifact is currently `QUARANTINED_LOST` matters enough operationally that it's
checked first, unconditionally, in health computation (see
[Status and health](#status-and-health)).

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
   validator's failure or timeout also produces `QUARANTINED`. The validators an operator
   can pick from are a registered catalog served by `GET /api/v1/validators`, not an
   arbitrary command the browser can supply.

`core/internal/revalidate` re-runs this against artifacts that already passed, on a cadence
and at a scope `config.Revalidation` sets, because a backup that verified six months ago is
not a backup that is good today.

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

Every step is idempotent and safe to resume after a crash at any point in the sequence. A
non-secret sidecar recovery manifest lands next to the committed artifact as well
(`core/internal/recovery`), carrying exactly enough metadata to reconstruct that artifact's
journal row and nothing that could leak a credential. That is what `catalog rebuild` reads.

### TOCTOU protection on delete

`core/internal/lifecycle/remotedelete.go` is, by its own doc comment, "the most dangerous line
in the project on purpose," and it is the only call site allowed to invoke
`Transport.DeleteRemote`. Before issuing a delete it revalidates, from scratch, every time:

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
to retry. `backup-manager reconcile` runs it for every configured backup set, and `run` and
`daemon` run it for each backup set before touching that set (`core/internal/app/cycle.go`
is the ordering).

### Quarantine

Quarantine is a state, not a place. There is no quarantine directory and no file gets
moved; only the `artifacts.state` column changes, to `QUARANTINED` or `QUARANTINED_LOST`.
The file stays exactly where it was, its `.partial` path if quarantined before commit, or
its final committed path if quarantined afterward by reconciliation. See
[The lifecycle](#the-lifecycle) above for the states themselves. `core/internal/quarantine`
turns those rows into a countable, actionable picture, and `backup-manager validate` is how
you re-check one artifact's durable local copy by hand.

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

`GFSDecide` only classifies. A verdict of `Keep: false` is a delete *candidate*, and
`core/internal/retention/prune.go` is what turns candidates into deletions. That is a change
from the previous version of this document, which said no code path deleted a local file:
one does now, and it is the second file in this repository whose doc comment calls itself
the most dangerous line in the project.

### Last-known-good protection

**Implemented**, which is also a change from the previous version of this document. FR-19
says the newest known-good restore point must never be deleted solely for exceeding
retention age, and `core/internal/retention/lastknowngood.go` is that rule.
`config.Retention.ProtectLastKnownGood` defaults to `true` when the key is omitted, and
turning it explicitly off is reported by name as "a materially more dangerous
configuration" rather than accepted quietly.

Two ways to see what a policy would do before it does it:
`backup-manager retention --dry-run`, which also takes per-run overrides for the timezone,
the week start and each tier so you can compare policies without editing config; and
`GET /api/v1/backup-sets/{source}/{set}/retention/preview` in the web UI, whose apply
counterpart refuses a plan that has gone stale rather than silently recomputing a wider one.

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

`backup-manager status` renders all of it, including the `QuarantinedCount` and
`QuarantinedLostCount` aggregates FR-24 asks for, and exits non-zero unless every configured
set reports HEALTHY, which is what makes it usable as a container healthcheck rather than
only as something to read. The API side is `GET /health/live` and `GET /health/ready` on the
engine, deliberately outside `/api/v1` and outside authentication. What does not exist is a
`/metrics` endpoint, as [Status](#status-what-actually-runs-today) says above.

`core/internal/alert` turns those same computed signals into at most one operator-facing
notification per condition, and it delivers through a platform capability rather than
inventing one. The generic Docker adapter declares no notification capability at all, so on
that platform alerting refuses at wiring time and says so on startup, rather than being
discovered later as silence.

## Recovery: when a backup did not arrive

This is the section to read under pressure. The fuller version, with more of the "what if"
branches, is [`docs/recovery.md`](docs/recovery.md); this is the part you shouldn't have to
click through to get.

Start with `backup-manager status --config <path>` and `backup-manager artifacts --config
<path> --backup-set <set>`, which is faster than a query and does not need you to know the
schema. When you want the raw truth, or the binary is not to hand: **the SQLite journal at
`state.database` is the truth, and it's a plain SQLite file.** Query it directly:

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
[Durable commit](#durable-commit)). Copy it wherever you're restoring to. There is no
`restore` command to do that for you and there is not meant to be: restore execution is out
of scope, so the last step is yours.

If the newest row for that backup set is `QUARANTINED_LOST`: that specific backup is gone
for good. The remote copy was already deleted before the local corruption was found, and no
automatic path recovers it (see [The lifecycle](#the-lifecycle)). Look at the next-newest
row in a known-good state and treat the loss as real when deciding what to tell whoever
needs the data, not as something to retry.

If it's `QUARANTINED` (not `_LOST`): the remote copy may still exist, so this can self-heal.
`backup-manager reconcile` and the next `run` or `daemon` cycle against that backup set are
what try; `backup-manager validate <source/backup-set/artifact>` re-checks one artifact's
durable local copy without waiting for a cycle.

If a row has been sitting at `REMOTE_DELETE_PENDING` for longer than you'd expect, look at
its `remote_delete_error` column before assuming something is stuck. Given the deployment
this project recommends, a persistent refusal there is the expected behavior described in
[TOCTOU protection on delete](#toctou-protection-on-delete), not necessarily a bug, and it
means the remote copy is very likely still sitting on the source server, still recoverable,
just not pruned. Left unattended, that also means the remote source disk isn't being
freed by this project on that backup set; monitor it directly rather than assuming pruning
is happening in the background.

If the state database itself is gone or corrupt, `backup-manager catalog rebuild --dry-run`
reports what it could reconstruct from the sidecar recovery manifests sitting next to the
committed artifacts, and dropping `--dry-run` does it.

## Toolchain

Go 1.27, Node for the frontend workspaces, and Docker for the disposable SFTP server the
integration tests use.

This repository is four Go modules stitched together by `go.work`: `core/`, `apps/common/`,
`apps/generic/` and `apps/synology/`. The engine's own commands run from `core/`:

```bash
cd core
go build ./...
go vet ./...
go test ./...
```

### The local gate

`scripts/ci-local.sh` is the gate for this repository. `.github/workflows/ci.yml`,
`rclone-upgrade-gate.yml` and `nightly-e2e.yml` are all `workflow_dispatch`-only, so
**nothing runs on push or on a pull request**, and `.husky/pre-commit` runs this script on
every commit instead. It mirrors those workflows job for job, which makes it slow: the whole
`core/` suite including the Docker-backed crash matrix and the SFTP integration tests, both
cross-compiles, every Go module's build/vet/test/lint, the frontend
lint/typecheck/eslint/vitest/build set, the cross-provider conformance suite, and the
repository-structure dependency proofs.

Install the JS workspaces before the first full run in a new clone or `git worktree`:

```bash
(cd ui/shared && npm ci)
(cd apps/common/tests && npm ci)
```

`node_modules/` is gitignored, so a fresh checkout has none. A full run refuses to start
until every JS workspace present in the tree is installed, and prints the exact command
for each one that is not. It used to skip those checks and still print
`==> ci-local: ok`, which is what made the gate's own success line unreliable (#160).

The Docker daemon is the same rule with a bigger blast radius. With it stopped, the crash
matrix, the SFTP integration suite and the whole `apps/generic/tests/dockercli` package
call `t.Skip`, `go test` still exits 0, and nothing would reach the ledger. A full run
refuses to start without a reachable daemon.

Three environment variables change what runs:

| Variable | Effect |
|---|---|
| `CI_LOCAL_FAST=1` | Fast iteration loop: skips `core/`'s `./tests/...` (the crash matrix and the SFTP integration tests), both cross-compiles, the production builds, the conformance suite, the structure proofs and the gate's own self-test. It does not skip `apps/generic`, whose tests bring a compose stack up, so a FAST run is not a Docker-free run. Always ends INCOMPLETE. |
| `CI_LOCAL_SKIP_JS=1` | Proceeds past the preflight with uninstalled JS workspaces instead of failing, for a change that only touches Go. Ends INCOMPLETE whenever it actually left a workspace out; with everything installed it changes nothing and the run can still be `ok`. |
| `CI_LOCAL_SKIP_DOCKER=1` | Proceeds past the preflight with the daemon down instead of failing. Ends INCOMPLETE, because the Docker-backed suites will have reported `ok` without running. |

A run that skipped anything ends with `==> ci-local: INCOMPLETE`, lists what did not run,
and exits 3. A run that performed every check it invoked ends with `==> ci-local: ok` and
exits 0, and that pair is what makes the gate readable as merge evidence by a human and by
a script. A run that failed ends with `==> ci-local: FAILED` naming the step, and exits
with whatever failed. `.husky/pre-commit` allows 3 and says so out loud, so the fast
iteration loop still commits; nothing that merges on this gate's word may accept anything
but 0.

One qualification on `ok`: Playwright e2e is not in the gate at all (it matches
`nightly-e2e.yml`'s own reasoning, too slow and too flaky in front of every commit), so
`ok` means every check the gate invokes ran, not every test in the repository. Run
`cd ui/shared && npm run e2e` by hand before a release, and remember what the
[Status](#status-what-actually-runs-today) section says that run does and does not prove.

A component that is not in the tree at all is not a skip: its checks are inapplicable, and
the run can still be `ok`. Today `apps/ugos/backend` and `apps/ugos/frontend/upk-proof`
are the absent ones; `apps/generic` and `apps/synology` are present and are built, vetted,
tested and linted on every run.

## How this document is kept honest

The previous version of this README described a binary with one subcommand, eleven
subcommands after that stopped being true. It listed eleven packages under `core/internal`
when there were seventeen. Both survived because prose does not fail a build, so the claims in here
that a machine can decide are now decided on every run, by
`distribution/packaging/readme_claims_test.go`:

- every markdown link and every backticked repository path in this file resolves, with the
  handful of paths this document names *because* they are absent kept in an explicit list
  with a reason each, so admitting what is missing stays possible;
- the command table above matches the dispatch table in `core/cmd/backup-manager/main.go`,
  and that dispatch table matches the help text the binary prints, so all three move
  together or the build goes red;
- the `core/internal/` inventory in [Layout](#layout) matches the packages that are actually
  on disk;
- whether the browser client and the router still agree about the version route is
  re-derived from `client.ts` and `router.go` on every run, in both directions, so a claim
  about it in this document cannot outlive the drift it describes (and could not survive
  the repair either, which is how #211 found out this section needed rewriting);
- the "build-supported and uncertified" statement holds for exactly as long as the generated
  conformance matrix still reports an unexecuted operator cell, in both directions;
- the support tiers in the table above come from `distribution/packaging/canonical.json`.

Each of those carries its own positive control, because a check that cannot fail is
decoration. What is deliberately *not* checked there, and why, is written at the top of that
test file: anything needing real hardware, and the measured binary size. The client's
request paths used to be on that list as "not decidable by reading TypeScript string
concatenation"; #166 landed the contract that made the question answerable and #211 answered
it, in `scripts/api/check-client-paths.sh`.

## Layout

Since #165 (Phase 6) the repository has **three product layers**, declared once in
`scripts/architecture/layers.conf` and enforced rather than described: a
provider-neutral **core** (plus the application services, the `/api/v1` host and the
shared UI), a **runtime platform** layer of per-host profiles, and a **distribution**
layer of packaging, metadata, templates and store presentation.
[`docs/architecture/layers.md`](docs/architecture/layers.md) is the full account: what each
layer owns, the dependency direction, which check proves which claim, how each of those
checks was shown to be able to fail, and the map from the old layout for rebasing an
in-flight branch. The rest of this section describes the same tree from the inside.

`core/` is its own Go module (`core/go.mod`), separate from the repository root, drawn
that way by #106/B1.1 so the engine has never heard of a provider or a UI (see
`docs/EPIC-B-multi-nas.md` §7 for why). `core/cmd/backup-manager/` is the entry point,
`core/service/` is the process-lifetime service layer the web host and the CLI share, and
`core/internal/` holds every application package, with every rclone import staying inside
`core/internal/transport/rclone/`:

<!-- BEGIN CORE-INTERNAL -->

```text
core/internal/
  alert/        at-most-once operator notifications, delivered through a platform capability
  app/          the presentation-agnostic application service every command and handler calls
  capacity/     disk-space admission checks
  config/       YAML config schema, loading, validation (Load takes any path)
  discovery/    turns a raw remote listing into artifacts proven complete
  health/       process and backup-set health computation
  lifecycle/    the state machine plus every step: transfer, verify, commit, delete
  metrics/      a health report rendered as Prometheus text (built, exposed nowhere)
  model/        shared identity types: ArtifactID, BackupSetID, RemoteIdentity, CompareIdentity
  obs/          structured event logging
  quarantine/   the operator-facing view of what is quarantined and why
  recovery/     the non-secret sidecar manifest written beside every committed artifact
  reconcile/    startup reconciliation against the journal, filesystem and remote
  retention/    GFS classification, last-known-good protection, and the local prune
  revalidate/   scheduled re-verification of artifacts that already passed
  state/        the SQLite journal: durable, idempotent transition recording
  testenv/      the environment a test has to be in before it may conclude anything from file permissions
  transport/    the manager-owned Transport interface and the rclone adapter behind it
```

<!-- END CORE-INTERNAL -->

Config example: `core/internal/config/testdata/full.yaml` has a complete, valid config with
every field populated; that's a better reference than hand-writing one here, since it's
exercised by the config package's own tests and won't silently drift out of sync with the
schema the way a README example would.

`apps/common/` is a second Go module (`apps/common/go.mod`) and is no longer the mostly
empty boundary-drawing exercise the previous version of this document described. It holds
`platform/capabilities/` (the `PlatformCapabilities`/`PlatformAdapter` contract every
provider composes over, §3.4), `webhost/` (the whole `/api/v1` surface, its handlers, its
auth middleware and its destructive gate), `auth/local/` (local-account authentication,
enrollment and password rotation), `csrf/`, and `packaging/` (the canonical packaging
description plus the checkers that hold every provider to it). `apps/common/tests/` is a
separate small TS package: the one place in the repo that legitimately imports every
provider's frontend bridge at once (the provider-conformance matrix, §63A), kept outside
`ui/shared/` specifically so removing a provider never breaks `ui/shared`'s own build.

`ui/shared/` is the one shared frontend every provider app builds against
(`ui/shared/src/`), never providing its own product UI (see `docs/EPIC-B-multi-nas.md` §11):
pages, components, the `PlatformBridge` contract (`ui/shared/src/platform/`,
`ui/shared/src/types/platform.ts`), and the single causl-ts state graph
(`ui/shared/src/state/graph.ts`). A provider app under `apps/<provider>/frontend/`
supplies a `PlatformBridge` implementation and little else; `ui/shared` never imports a
provider, only the reverse. Two providers carry a Go module of their own: `apps/generic/`
is the generic Web host (#82/B4.1), and `apps/synology/` is the DSM `.spk` packaging and
conformance module (#85/B4.4), which ships no product binary of its own and instead wraps
the release binaries unchanged and checks their digest against
`container/release-manifest.json`.

Four providers carry packaging metadata next to their bridge: `apps/truenas/` (a custom-app
Compose file plus a TrueNAS Apps catalog entry), `apps/unraid/` (two Community Applications
Docker templates), `apps/openmediavault/` (a Compose deployment profile) and
`apps/proxmox/` (the same Compose profile again, for a dedicated container-host guest,
because Proxmox VE has no application store to package into at all). All four are metadata
and templates only, wrapping the exact canonical OCI image with no lifecycle code of their
own, and `distribution/packaging/` holds them to that on every commit: one shared source of
truth in `canonical.json`, plus scanners for the Phase 4 gate checks that are decidable from
the repository alone.

The same package runs the cross-provider conformance matrix (§63A) across all seven
providers at once, reporting an outcome per provider per capability rather than one
verdict per run, with `UNSUPPORTED`, `NOT_APPLICABLE` and `BLOCKED` as first-class
results a provider has to declare rather than reach by omission. The recorded run is
[`docs/conformance/phase-4-matrix.md`](docs/conformance/phase-4-matrix.md), generated and
then checked, so it cannot drift from what the suite actually finds. The half that is
not decidable here, installing and updating and removing on the real platform, lives in
[`docs/acceptance/`](docs/acceptance/) as prewritten operator procedures, and until one
is executed its provider is build-supported and uncertified.

EPIC B's Phase 6 reorganises this into explicit core, runtime-platform and distribution
layers and reduces every platform package to a thin adapter. That work is #184, #194, #199
and #169, none of them merged as this document is written, so the layout above is what is
here today rather than what it is becoming.

This project was originally scoped as `tools/backup-manager/` inside `iasbuilt/iac`. It
lives here instead; nothing in the design depended on the location.

## Documentation index

- [`docs/deployment.md`](docs/deployment.md) – the container build, the two-service Compose topology, the read-only rootfs and uid/gid rules, and release hashes
- [`docs/adr/0001-embed-rclone-behind-transport-adapter.md`](docs/adr/0001-embed-rclone-behind-transport-adapter.md) – why embed, why not fork or shell out, what it costs
- [`docs/adr/0002-phase-5-scope.md`](docs/adr/0002-phase-5-scope.md) – why observability stops where it does
- [`docs/adr/0003-pull-encrypted-runs-to-the-nas.md`](docs/adr/0003-pull-encrypted-runs-to-the-nas.md) – the pull model, and why the NAS is the initiator
- [`docs/rclone-upgrade.md`](docs/rclone-upgrade.md) – the pinned-version upgrade procedure and its CI gate
- [`docs/ssh-setup.md`](docs/ssh-setup.md) – the dedicated key, the restricted SFTP account, host-key verification
- [`docs/recovery.md`](docs/recovery.md) – recovery and the restore procedure, in full
- [`docs/phase-1-gate.md`](docs/phase-1-gate.md) – the embedding proof-of-concept verdict and what it did and didn't prove
- [`apps/synology/README.md`](apps/synology/README.md) – the Synology DSM `.spk`: supported architectures/models, how to build and verify one, and what is still uncertified
- [`apps/truenas/README.md`](apps/truenas/README.md) – the TrueNAS custom app and catalog entry
- [`apps/unraid/README.md`](apps/unraid/README.md) – the two Community Applications templates, and the one place this profile is weaker than the others
- [`apps/openmediavault/README.md`](apps/openmediavault/README.md) – the OMV Compose deployment profile
- [`apps/proxmox/README.md`](apps/proxmox/README.md) – the Proxmox VE deployment profile: the one supported model, what the PVE host contributes, and what is deliberately absent
- [`docs/conformance/phase-4-matrix.md`](docs/conformance/phase-4-matrix.md) – the cross-provider conformance matrix (§63A), per provider and per capability, including what is blocked and on what
- [`docs/acceptance/`](docs/acceptance/) – the provider acceptance procedures (§68), written and not yet executed
- [`docs/architecture/layers.md`](docs/architecture/layers.md) – the three layers (core, runtime platform, distribution), what each owns, the dependency direction, and the checks that enforce it
- [`docs/perf/README.md`](docs/perf/README.md) – the Phase 6 performance baselines, the benchmark host and workload, and the concrete regression thresholds
- [`distribution/README.md`](distribution/README.md) – the distribution layer: what makes an adapter an adapter, and where the rest of that layer still lives
- [`docs/compliance/release-provenance.md`](docs/compliance/release-provenance.md) – what a release records, how the SBOM and checksums are produced, and how an image is signed without this project ever holding a key
- [`docs/compliance/`](docs/compliance/) – the store-facing compliance materials: privacy policy, support, and the written offer of source
- [`docs/EPIC.md`](docs/EPIC.md) – the full specification this project is built against, including where it and the code have since diverged
- [`docs/EPIC-B-multi-nas.md`](docs/EPIC-B-multi-nas.md) – the multi-NAS provider architecture, the support tiers, and the Phase 6 refactor

## Licence

Apache License 2.0. The full text is in [`LICENSE`](LICENSE).

Every third-party component that reaches a shipped artifact is permissive (MIT,
BSD-2-Clause, BSD-3-Clause, Apache-2.0, CC0-1.0), which is what made that choice
available. It is checked rather than remembered: the inventory is re-derived
from the live module graph and the frontend lockfile on every gate run, and a
copyleft component fails the build.

- [`NOTICE`](NOTICE) – the attribution file Apache-2.0 §4(d) refers to, grouped by licence
- [`provenance/third-party-licenses.json`](provenance/third-party-licenses.json) – the machine-readable inventory, with the SHA-256 of each component's licence text
- [`provenance/sbom.spdx.json`](provenance/sbom.spdx.json) – an SPDX 2.3 SBOM of the same set
- [`provenance/checksums.txt`](provenance/checksums.txt) – `sha256sum -c` over the whole release

All four are generated, never hand-edited:

```
cd distribution && go run ./cmd/provenance -write
```
