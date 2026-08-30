# EPIC: Embedded-rclone NAS Backup Lifecycle Manager --- UI-Ready Architecture

## Summary

Implement a purpose-built backup lifecycle manager in its own
repository:

``` text
spdrman/rclone-manager
```

The manager will run on a UGREEN NAS and ingest backup artifacts that
are created by remote application/database backup jobs.

The implementation SHALL be a standalone **Go application that embeds
selected rclone Go packages** for filesystem/transport operations. It
SHALL NOT fork rclone, invoke the `rclone` CLI as its normal transport
mechanism, or implement SSH/SFTP itself.

The application owns the backup-specific control plane:

-   backup-set configuration;
-   completed-artifact discovery;
-   durable lifecycle state;
-   copy/verification/commit/delete sequencing;
-   GFS retention;
-   last-known-good protection;
-   validation/quarantine;
-   backup freshness;
-   health and observability;
-   reconciliation after failure.

rclone owns the generic data plane:

-   SFTP and local filesystem backends;
-   remote filesystem abstractions;
-   object listing;
-   copying;
-   hashing where supported;
-   deletion primitives;
-   transfer accounting;
-   retry/error primitives where appropriate.

The initial use case is:

``` text
Remote Server
    │
    │ SSH/SFTP
    ▼
embedded rclone SFTP backend
    │
    ▼
backup-manager
    │
    ├── lifecycle journal (SQLite)
    ├── verification/validation
    ├── GFS retention
    └── health/status
    │
    ▼
UGREEN NAS filesystem
```

Default retention:

  Tier                   Default
  --------- --------------------
  Daily                   7 days
  Weekly       3 calendar months
  Monthly     12 calendar months

The overriding safety rule is:

> **A remote backup artifact MUST NOT be deleted until a verified and
> durably committed NAS copy exists. If state is uncertain, preserve the
> remote copy.**

------------------------------------------------------------------------

# Architecture Decision

## Decision: embed rclone; do not fork it

rclone is intentionally modular and its own contributor documentation
supports keeping specialized commands/backends out of tree rather than
maintaining a fork. Its Go source also exposes filesystem and operation
packages used by its own commands.

The manager SHALL therefore consume a **pinned rclone Go module
version**.

Conceptually:

``` text
backup-manager
│
├── cmd/
├── config/
├── discovery/
├── lifecycle/
├── retention/
├── validation/
├── state/
├── health/
│
└── transport/rclone/
      ├── fs abstraction
      ├── local backend
      ├── SFTP backend
      ├── operations
      └── accounting/errors
```

Only required rclone backends SHOULD be registered in the initial
binary:

``` go
import (
    _ "github.com/rclone/rclone/backend/local"
    _ "github.com/rclone/rclone/backend/sftp"
)
```

Additional backends MAY be added later deliberately.

## Why not fork rclone?

A fork would make this project responsible for continuously integrating
upstream:

-   security fixes;
-   SSH/SFTP changes;
-   Go/runtime changes;
-   dependency updates;
-   backend fixes;
-   core filesystem behavior;
-   transfer/accounting changes.

The backup-specific functionality is small relative to rclone's full
codebase. Maintaining a fork would therefore create disproportionate
maintenance cost and security exposure.

## Why not shell out to the rclone CLI?

Invoking the CLI remains a possible diagnostic/fallback strategy but
SHALL NOT be the primary architecture.

Embedding provides:

-   one deployable binary;
-   typed Go errors instead of parsing process output;
-   direct `context.Context` cancellation;
-   direct object/filesystem APIs;
-   direct transfer statistics;
-   no subprocess lifecycle;
-   no CLI-output compatibility contract;
-   tighter testing;
-   explicit control over destructive operations.

## Important boundary: do not use rclone `move` as the backup transaction

rclone's move operations can copy and then delete the source when a
server-side move is unavailable. That behavior is useful for general
file movement but is **too coarse for this backup safety contract**.

The manager SHALL explicitly orchestrate:

``` text
COPY
  ↓
VERIFY
  ↓
DURABLY COMMIT LOCAL COPY
  ↓
PERSIST COMMITTED
  ↓
DELETE REMOTE
```

The source deletion SHALL therefore be a separate manager-controlled
operation after durable commit.

------------------------------------------------------------------------

# Goals

1.  Produce a small standalone Go binary suitable for UGREEN NAS
    deployment.
2.  Embed rclone rather than fork it.
3.  Pin the rclone dependency to an explicitly tested version.
4.  Initially compile only the local and SFTP backends required by this
    use case.
5.  Pull completed backup artifacts from remote servers.
6.  Maintain a durable SQLite lifecycle journal.
7.  Guarantee manager-controlled copy → verify → commit → delete
    ordering.
8.  Recover deterministically after interruption at any step.
9.  Enforce deterministic GFS retention.
10. Protect the last known-good restore point.
11. Support backup-specific validation and quarantine.
12. Expose operational health independently from process liveness.
13. Keep the rclone integration behind a narrow internal adapter.
14. Make future rclone upgrades testable and reversible.
15. Avoid coupling backup lifecycle semantics to unstable rclone
    internals wherever practical.

------------------------------------------------------------------------

# Non-Goals

-   Forking rclone.
-   Modifying upstream rclone unless a generally useful upstream
    contribution is warranted.
-   Building SSH/SFTP.
-   Calling the rclone CLI for normal data movement.
-   Creating the application/database backups.
-   Becoming a general-purpose synchronization application.
-   Replacing Borg/restic-style backup repositories.
-   Re-encoding canonical backup artifacts into a proprietary format.
-   Continuous replication.
-   Initial NAS-to-cloud replication.
-   A web UI in the initial release.

------------------------------------------------------------------------

# UI / Presentation Architecture

The initial release is CLI/daemon-first, but the architecture SHALL
support a future browser-based UGREEN NAS UI without rewriting backup
lifecycle logic.

The application SHALL use a presentation-agnostic application-service
layer:

``` text
                       BackupService
                            │
              ┌─────────────┼─────────────┐
              ▼             ▼             ▼
             CLI          daemon      future HTTP API
                                            │
                                            ▼
                                      future Web UI
```

Business rules MUST NOT live exclusively in CLI commands, daemon
scheduling code, HTTP handlers, or frontend code.

The application-service layer SHALL orchestrate lifecycle, retention,
validation, state, health, and transport. CLI and daemon interfaces
SHALL invoke this common layer. A future HTTP API/UI SHALL invoke the
same use cases.

A future UI SHOULD be able to expose:

-   overall `HEALTHY`, `DEGRADED`, `STALE`, or `FAILING` state;
-   configured sources and backup sets;
-   newest known-good restore point;
-   backup and validation history;
-   active transfer progress;
-   daily/weekly/monthly retention classification;
-   quarantined artifacts;
-   pending remote deletions;
-   disk utilization;
-   manual backup execution;
-   validation;
-   retention preview and execution;
-   diagnostics.

The full web UI is OUT OF SCOPE for this EPIC.

The architecture SHOULD permit a future static frontend to be embedded
into the Go executable, allowing a single-container deployment. It
SHOULD also permit future UGOS/UPK packaging or launcher integration
without changing the core backup engine.

Frontend code MUST NOT directly manipulate SQLite, rclone, or backup
files.

# Repository Layout

The repository root is the Go module root:

``` text
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
    transport.go
    rclone/
      adapter.go
      config.go
      listing.go
      copy.go
      hash.go
      delete.go
      errors.go

docs/
  EPIC.md

migrations/
tests/

container/
  Dockerfile
  compose.yaml
```

This was originally scoped as `tools/backup-manager/` inside `iasbuilt/iac`.
The project now lives in its own repository, so the module root is the
repository root. Nothing else in this specification depends on the
location.

Application packages outside `internal/transport/rclone` SHOULD NOT
directly import rclone packages.

This boundary is mandatory to contain upstream API churn.

------------------------------------------------------------------------

# Functional Requirements

## FR-1 --- Execution Modes

The application SHALL support:

``` bash
backup-manager run
backup-manager daemon
```

`run` performs one processing cycle and exits.

`daemon` repeatedly invokes the same processing cycle at a configured
interval.

Business logic SHALL be shared between both modes.

The daemon SHALL:

-   handle `SIGTERM`/`SIGINT`;
-   use Go context cancellation;
-   prevent overlapping processing for the same backup set;
-   continue processing unrelated sources after a source failure;
-   recover from transient network errors;
-   shut down without initiating unsafe source deletion.

------------------------------------------------------------------------

## FR-2 --- rclone Dependency Management

The project SHALL pin an explicit rclone module version in `go.mod`.

Upgrades SHALL NOT use an unconstrained/latest dependency.

Each rclone upgrade SHALL require:

1.  dependency update;
2.  compilation;
3.  unit tests;
4.  transport contract tests;
5.  SFTP integration tests;
6.  crash/reconciliation tests involving transfer operations;
7.  destructive-safety tests;
8.  release notes/changelog review.

The project SHOULD document the currently certified rclone version in
the README.

Renovate/Dependabot-style automation MAY open rclone upgrade changes,
but automatic merge SHALL NOT be enabled for the rclone dependency.

------------------------------------------------------------------------

## FR-3 --- rclone Integration Boundary

Define a manager-owned interface similar to:

``` go
type Transport interface {
    List(ctx context.Context, source Source) ([]RemoteArtifact, error)

    Stat(
        ctx context.Context,
        source Source,
        remotePath string,
    ) (RemoteArtifact, error)

    CopyToLocal(
        ctx context.Context,
        source Source,
        remotePath string,
        localPartialPath string,
    ) (TransferResult, error)

    RemoteHash(
        ctx context.Context,
        source Source,
        remotePath string,
        algorithm HashAlgorithm,
    ) (string, error)

    DeleteRemote(
        ctx context.Context,
        source Source,
        remotePath string,
    ) error
}
```

Exact signatures may change.

Requirements:

-   lifecycle code SHALL depend on the manager-owned interface;
-   rclone-specific types SHALL NOT leak into lifecycle/retention/state
    packages;
-   destructive operations SHALL be explicit;
-   no generic `Move()` method SHALL be exposed to lifecycle code;
-   test doubles SHALL be available.

------------------------------------------------------------------------

## FR-4 --- Required rclone Backends

Initial production build SHALL include:

-   local filesystem;
-   SFTP.

Do not import all rclone backends merely for convenience.

This reduces:

-   binary size;
-   dependency surface;
-   initialization complexity;
-   accidental configuration exposure.

Future backends require an explicit feature/architecture decision.

------------------------------------------------------------------------

## FR-5 --- Configuration

Configuration SHALL support at minimum:

``` yaml
poll_interval: 15m

state:
  database: /var/lib/backup-manager/state.db

sources:
  - id: production
    backup_sets:

      - id: postgres-primary

        remote:
          type: sftp
          host: production.example.internal
          port: 22
          user: backup
          key_file: /run/secrets/backup_ssh_key
          known_hosts: /etc/backup-manager/known_hosts

        remote_path: /backups/postgres
        local_path: /backups/production/postgres

        include:
          - "*.dump.zst"

        completion:
          strategy: stable
          stable_for: 10m
          # Only read when strategy is "stable". FR-15's remote-delete gate
          # additionally waits this long after an artifact last reached a
          # confirmed-good state before it treats a size/mtime heuristic as
          # equivalent to a producer rename/marker signal. Omit it and it
          # defaults to 1h; a negative value is refused.
          delete_safety_delay: 60m

        stale_after: 30h

        validation:
          hash: sha256
          command: null

retention:
  timezone: America/Vancouver
  week_starts_on: monday
  # The classic three-tier chain, written as scalars. These are sugar for
  # the equivalent `tiers:` list below and cannot be combined with it.
  daily_days: 7
  weekly_months: 3
  monthly_months: 12
  protect_last_known_good: true

# Or, spelled as an explicit chain of any length (FR-18). This example is
# byte-for-byte equivalent to the three scalars above for its first three
# entries, and then chains on past them.
#
# retention:
#   timezone: America/Vancouver
#   week_starts_on: monday
#   protect_last_known_good: true
#   tiers:
#     - name: daily
#       granularity: day
#       keep: 7
#     - name: weekly
#       granularity: week
#       keep: 3
#       window_unit: month
#     - name: monthly
#       granularity: month
#       keep: 12
#     - name: semi_annual
#       granularity: half_year
#       keep: 6
#     - name: annual
#       granularity: year
#       keep: 10
#     - name: fortnightly
#       granularity: days
#       period_days: 14
#       keep: 26
```

Configuration MUST NOT require recompilation.

Configuration SHALL be validated before destructive processing begins.

------------------------------------------------------------------------

## FR-6 --- SSH/SFTP Security

The embedded rclone SFTP backend SHALL use:

-   SSH key authentication, mandatory rather than merely preferred;
-   host-key verification;
-   explicit known-host configuration;
-   no automatic acceptance of changed/unknown production host keys.

"Mandatory" is stronger than "by default" on purpose. The adapter SHALL refuse
a source with none of `key_file`, `key.env` or `key.command` set (#74) rather
than letting the backend fall back to a running ssh-agent, and SHALL NOT set
the backend's password, prompt or use-agent options, so password and agent
authentication have no path into this program at all. `transport.Source`
carries no password field, so adding one would be a visible change to a
shared type rather than a quiet configuration choice. An operator whose agent
happens to hold a usable key MUST NOT be able to authenticate by accident,
because a login that works only because of ambient agent state is not
reproducible on the NAS at 03:40.

The backend's inline-key option (`key_pem`) is the one exception, and it is
narrow on purpose: it is set only when a source resolves its key through
`key.env` or `key.command`, and only ever with what that resolver returned
after it was confirmed to parse as an unencrypted SSH private key. There is
no configuration field an operator can put a value into `key_pem` with
directly; see "Key source" below.

The remote account SHOULD:

-   be dedicated to backups;
-   be SFTP-only where practical;
-   have no general interactive shell;
-   be confined to backup directories;
-   be able to list/read eligible artifacts;
-   be able to delete eligible artifacts;
-   be unable to modify/replace completed artifacts where practical;
-   have no unrelated server privileges.

Credentials MUST NOT be stored in Git.

### Key custody: the manager references a key, it does not hold one

The SSH private key is REQUIRED. Artifacts move over SFTP, and that connection
is authenticated by the key, so without one no copy happens at all. There is no
mode in which the manager fetches a backup without authenticating.

The manager SHALL NOT store, generate, copy or otherwise manage that key.
Configuration names WHERE the key lives, never carries it directly: a
`key_file` PATH is the default and documented case, and the manager reads the
file at connection time and does nothing else with it. It never writes the
key anywhere, never puts it in the journal, never logs it, and never has a
copy of its own that could drift from the operator's.

That boundary is deliberate, and it decides who owns what:

-   the key's generation, its filesystem permissions, its rotation, and any
    backup of the key itself are the OPERATOR's, outside this program;
-   the key file SHOULD be mounted read-only into the runtime, so the manager
    cannot modify it even if compromised;
-   because the manager holds no copy, rotating the key means replacing one
    file on the host and restarting, with nothing to migrate and no stale
    duplicate left behind;
-   a key that the manager cannot read is a startup failure, not a warning to
    be worked around, since the alternative is a backup run that appears
    configured and silently transfers nothing.

The same applies to `known_hosts`: a path the manager reads, never a store it
maintains.

The corollary is worth stating because it is easy to miss when planning a
deployment: the NAS running this manager needs its own key pair, and the remote
server needs that key in the backup account's `authorized_keys`. This program
does not distribute it, and a first run against a host that has never seen the
public key will fail authentication rather than falling back to anything.

`docs/ssh-setup.md` is the operational procedure, and `docs/deployment.md`
covers mounting the key into the container.

### Key source: file, environment, or command (#74)

A source's private key is named exactly one of three ways:

```yaml
remote:
  type: sftp
  host: cicd-pipeline.example
  user: backup
  known_hosts: /etc/backup-manager/known_hosts
  key:
    file: /etc/backup-manager/id_ed25519
    # env: BACKUP_SSH_KEY
    # command: ["op", "read", "op://infra/backup-manager/private-key"]
```

`key_file` (a bare path, no `key:` block) keeps working unchanged as a
deprecated alias for `key.file`, since this section's custody model above,
`docs/ssh-setup.md` and `docs/deployment.md` all document it, and an
operator's existing config MUST NOT break. Exactly one of `key_file`,
`key.file`, `key.env` or `key.command` MUST be set; two is a config error to
fix, not a precedence order the manager resolves silently.

`key.file` SHOULD be preferred, and is the default this section's custody
model describes above, because it is the only one of the three that never
puts key material into this program's own memory at all: rclone opens the
file itself. `key.env` and `key.command` exist for one reason, stated
plainly rather than hedged: they are the door this program opens for a
secrets manager (OpenBao, Vault, SOPS, 1Password, AWS Secrets Manager, or
anything else with a CLI) to be adopted later without a second change to this
config shape, and specifically without this program taking a dependency on
any vendor's SDK or picking a winner among them. `key.command` covers that
case generically: it is an argv array, never a shell string, run with a
timeout and a minimal environment, on the same reasoning already applied to
FR-13's external validator.

Whatever `key.env` or `key.command` produce is held only in memory, wrapped
so it cannot render through a log line, and validated as an unencrypted SSH
private key before this program will hand it to the embedded SFTP backend: a
secrets manager answering with an error string, an HTML login page, an empty
body, or a passphrase-protected key MUST fail loudly at that point, never
surface later as a confusing connection failure or, worse, hang waiting on a
passphrase prompt nobody is there to answer.

------------------------------------------------------------------------

## FR-7 --- Backup-Set Isolation

Every stream of logically interchangeable restore points SHALL have a
stable `backup_set_id`.

Retention, health, lifecycle and last-known-good calculations SHALL
operate independently per backup set.

Examples:

``` text
production/postgres-primary
production/uploads
staging/postgres-primary
```

------------------------------------------------------------------------

## FR-8 --- Completed-Artifact Discovery

The manager SHALL discover remote artifacts through the transport
adapter.

A candidate MUST be proven complete before ingestion.

Supported strategies SHOULD include:

1.  producer atomic rename;
2.  producer completion/manifest marker;
3.  stable size/modification metadata for a configured period.

Producer-controlled atomic completion SHOULD be preferred.

Remote filenames and metadata SHALL be treated as untrusted input.

------------------------------------------------------------------------

## FR-9 --- SQLite Lifecycle Journal

SQLite SHALL be mandatory.

It SHALL persist:

-   artifact identity;
-   backup set;
-   remote path;
-   local path;
-   remote metadata;
-   lifecycle state;
-   timestamps;
-   transfer results;
-   hashes;
-   validation results;
-   retry information;
-   remote deletion status;
-   retention classification.

Schema migrations SHALL be version-controlled.

The filesystem MUST NOT be the sole transactional journal.

------------------------------------------------------------------------

## FR-10 --- Lifecycle State Machine

Artifacts SHALL use explicit states:

``` text
DISCOVERED
    ↓
TRANSFERRING
    ↓
TRANSFERRED
    ↓
VERIFYING
    ↓
VERIFIED
    ↓
COMMITTING
    ↓
COMMITTED
    ↓
REMOTE_DELETE_PENDING
    ↓
COMPLETE
```

Exceptional states:

``` text
FAILED
QUARANTINED
```

Transitions SHALL be durable and idempotent.

------------------------------------------------------------------------

## FR-11 --- Transfer Semantics

The manager SHALL use rclone copy primitives, not rclone move semantics.

Nominal sequence:

``` text
discover
   ↓
record DISCOVERED
   ↓
copy remote → local .partial
   ↓
record TRANSFERRED
   ↓
verify
   ↓
record VERIFIED
   ↓
durably commit local file
   ↓
record COMMITTED
   ↓
record REMOTE_DELETE_PENDING
   ↓
explicit remote delete
   ↓
record COMPLETE
```

Source deletion MUST be independently invoked by the lifecycle manager.

------------------------------------------------------------------------

## FR-12 --- Temporary Destination

All incoming files SHALL initially use a non-restorable temporary
filename, e.g.:

``` text
backup-2026-08-27.dump.zst.partial
```

A `.partial` artifact SHALL:

-   never participate in retention;
-   never satisfy last-known-good protection;
-   never be presented as a valid restore point.

Final-name collisions SHALL fail safely rather than overwrite a
known-good backup.

------------------------------------------------------------------------

## FR-13 --- Verification

### Required transfer verification

At minimum:

-   rclone copy returned success;
-   destination exists;
-   expected size matches;
-   local file can be opened/read.

### Hash verification

Where the producer supplies a trustworthy checksum, verify it.

Otherwise the manager SHOULD support comparing remote and local hashes
when the backend and configured policy permit.

SHA-256 SHOULD be supported.

Unsupported remote hashes SHALL produce an explicit capability result
rather than silently weakening configured verification.

### Application validation

Support optional validators:

``` yaml
validation:
  command:
    executable: /usr/local/bin/validate-postgres-backup
    timeout: 10m
```

Required validator failure MUST prevent source deletion.

Invalid artifacts SHOULD enter `QUARANTINED`.

------------------------------------------------------------------------

## FR-14 --- Durable NAS Commit

Before `COMMITTED`, the manager SHALL:

1.  complete transfer to `.partial`;
2.  complete required verification;
3.  flush/synchronize the file to durable storage using appropriate OS
    primitives;
4.  atomically rename it to the final path;
5.  synchronize the containing directory where applicable;
6.  persist `COMMITTED` in SQLite.

The implementation SHALL document behavior and limitations of the target
NAS filesystem.

No remote deletion is permitted before `COMMITTED`.

------------------------------------------------------------------------

## FR-15 --- Remote Delete Safety

`DeleteRemote()` is security-sensitive and SHALL only be called by the
lifecycle transition responsible for:

``` text
COMMITTED → REMOTE_DELETE_PENDING → COMPLETE
```

Before deletion, the manager SHALL revalidate that:

-   the database artifact is `COMMITTED` or `REMOTE_DELETE_PENDING`;
-   the expected local final file exists;
-   local identity/size is consistent;
-   the remote object still corresponds to the artifact originally
    discovered.

If the remote artifact appears to have changed since discovery, deletion
SHALL be refused and the artifact SHALL require
reconciliation/intervention.

This protects against deleting a newly replaced remote file that reused
an old pathname.

------------------------------------------------------------------------

## FR-16 --- Remote Object Identity / TOCTOU Protection

The system SHALL defend against time-of-check/time-of-use changes.

Persist available remote identity metadata at discovery, such as:

``` text
path
size
mtime
hash
backend-specific stable identifier where available
```

Immediately before deletion, compare the current remote object against
the stored identity using the strongest practical available attributes.

If identity cannot be established with sufficient confidence:

> preserve the remote object.

------------------------------------------------------------------------

## FR-17 --- Reconciliation

On startup and before normal processing, reconcile SQLite, local files
and remote state.

Required scenarios include:

  -----------------------------------------------------------------------
  Remote          Local           Journal                 Required
                                                          behavior
  --------------- --------------- ----------------------- ---------------
  exists          absent          DISCOVERED              transfer

  exists          partial         TRANSFERRING            safe
                                                          retry/restart

  exists          final           COMMITTED               verify and
                                                          proceed toward
                                                          delete

  absent          final           REMOTE_DELETE_PENDING   reconcile
                                                          COMPLETE

  absent          final           COMPLETE                no-op

  exists          invalid final   any                     preserve
                                                          remote;
                                                          quarantine
                                                          local

  absent          invalid final   any                     quarantine,
                                                          unrecoverable

  changed         final           delete pending          refuse delete;
  identity                                                investigate
  -----------------------------------------------------------------------

I added the "absent / invalid final" row above: the original table had no
row for it at all, and it is not the same problem as "exists / invalid
final". When the remote copy is gone too, there is no source left to
re-fetch from, so preserve-and-quarantine (which assumes a fresh attempt
could still recover the artifact) is the wrong answer. The state machine
carries a twelfth state, QUARANTINED_LOST, reachable only from COMPLETE and
terminal by design, for exactly this case; see internal/lifecycle/state.go
and machine.go. Reconciliation reaches it either directly from COMPLETE, or
by first reconciling REMOTE_DELETE_PENDING to COMPLETE (the row above) and
then on to QUARANTINED_LOST in the same pass, since that is the only legal
path the state machine admits.

Reconciliation SHALL be idempotent.

------------------------------------------------------------------------

## FR-18 --- GFS Retention

Retention is an **ordered chain of named tiers**. An administrator picks
how many tiers there are and what each one keeps: the classic
daily/weekly/monthly grandfather-father-son chain is the default, but a
chain may be as long as the operator wants and may reach out to
semi-annual, annual, or an arbitrary custom period.

Each tier has:

  Field              Meaning
  ------------------ -----------------------------------------------------
  `name`             lower_snake_case identifier, unique within the chain
  `granularity`      the calendar bucket the tier groups artifacts into
  `keep`             how many of that tier's own buckets to look back over
  `window_unit`      optional: measure the look-back in this unit instead
  `period_days`      only for `granularity: days`, the custom period length

`granularity` is one of `day`, `week`, `month`, `quarter`, `half_year`,
`year`, or `days` (with `period_days: N`, the escape hatch for any period
the named list does not cover: fortnightly, every 10 days, and so on).

`window_unit` accepts the same named granularities (never `days`) and
exists because a tier's look-back is not always measured in its own
bucket. The default `weekly` tier is exactly that case: it buckets by
week but looks back over 3 calendar *months*. Omit `window_unit` and the
look-back is measured in the tier's own granularity.

A tier's window runs from the start of the bucket `keep - 1` units back
from today, through today inclusive, where "start of the bucket" means
the same calendar anchor the granularity itself names: the day itself for
`day`, the configured week-start weekday for `week`, the 1st for `month`,
the 1st of January/April/July/October for `quarter`, the 1st of
January/July for `half_year`, the 1st of January for `year`, and a fixed
epoch-aligned boundary for a custom `days` period (so custom buckets never
drift with the day the calculation runs).

Within its window, each tier independently selects the **newest valid
backup in each of its own buckets**.

Default chain:

  Tier      Granularity   Look-back
  --------- ------------- -------------------
  daily     day           7 days
  weekly    week          3 calendar months
  monthly   month         12 calendar months

For each backup set, with `tiers` the configured chain:

``` text
KEEP =
    ⋃ over every configured tier t of  selections(t)
  ∪ protected

DELETE =
    managed_complete_backups - KEEP
```

That formula is unchanged from the fixed three-tier version: it is still a
union of tier selections plus FR-19's protected term, and DELETE is still
everything managed and complete that the union did not claim. What is
generalized is only *how many* tiers may contribute to the union and *what
granularities* they may use.

Two consequences follow, and are stated here rather than left to be
inferred:

-   The chain does not have to be contiguous. `daily` plus `annual` with
    nothing in between is a legal policy, and every artifact falling in
    the gap between the two windows is a DELETE candidate.
-   An artifact older than the longest configured window is a DELETE
    candidate, regardless of how many tiers there are.

Tier order is the order the administrator writes the chain in. It is
presentation and processing order (it fixes the order tier names appear
against a KEEP verdict) and never changes which artifacts are kept, since
KEEP is a union.

Retention SHALL be deterministic.

Default semantics:

``` text
timezone: America/Vancouver
week starts: Monday
bucket representative: newest valid backup in bucket
```

Calendar semantics, DST, leap years and year boundaries SHALL be tested,
for every granularity the chain admits, including the calendar half-year
and calendar-year boundaries semi-annual and annual tiers depend on.

### Backward compatibility

`daily_days`, `weekly_months` and `monthly_months` remain valid
configuration and are **sugar for the default three-tier chain**. A
config file that sets only those three (or omits the retention block
entirely, and takes 7/3/12) SHALL produce exactly the decisions it
produced before this generalization existed.

Setting both the three scalar keys and an explicit `tiers:` list is a
configuration error, not a silent precedence rule: an operator who writes
both is asking two different questions and deserves to be told so rather
than have one answer quietly discarded.

------------------------------------------------------------------------

## FR-19 --- Last-Known-Good Protection

The newest known-good restore point SHALL NOT be deleted solely because
it exceeds normal retention age.

A backup is eligible as known-good only if it is a valid
committed/complete restore point satisfying required verification.

`FAILED`, `QUARANTINED` and `.partial` artifacts cannot satisfy this
protection.

------------------------------------------------------------------------

## FR-20 --- Local Deletion Safety

Retention deletion SHALL operate only on positively identified
database-managed files.

Before deletion:

-   canonicalize the path;
-   prove it is beneath the configured backup-set root;
-   ensure it is a final managed artifact;
-   ensure no retention tier selects it;
-   ensure it is not last-known-good;
-   reject symlink/path traversal escape.

A dry-run is mandatory:

``` bash
backup-manager retention --dry-run
```

It SHALL explain every KEEP/DELETE decision.

------------------------------------------------------------------------

## FR-21 --- Disk Capacity

Monitor destination filesystem capacity.

Support:

-   warning threshold;
-   critical threshold;
-   incoming artifact size;
-   configurable safety margin.

Do not begin a transfer known not to fit safely.

Do not silently violate retention to free space.

------------------------------------------------------------------------

## FR-22 --- Retry and Error Classification

The rclone adapter SHALL translate rclone-specific errors into
manager-owned categories such as:

``` text
Transient
Authentication
HostVerification
NotFound
PermissionDenied
IntegrityFailure
Conflict
UnsupportedCapability
Permanent
Cancelled
```

Lifecycle code SHALL NOT make policy decisions by inspecting rclone
error strings.

Transient errors SHALL use bounded exponential backoff with jitter.

Cancellation SHALL propagate through Go contexts.

------------------------------------------------------------------------

## FR-23 --- Observability

Structured logs SHALL cover:

-   startup/version;
-   configured rclone version;
-   cycle start/end;
-   discovery;
-   lifecycle transitions;
-   transfer statistics;
-   hashes;
-   validation;
-   durable commit;
-   remote deletion;
-   reconciliation;
-   retention;
-   retries;
-   stale backups;
-   disk pressure;
-   errors.

Secrets MUST never be logged.

------------------------------------------------------------------------

## FR-24 --- Health

The system SHALL distinguish:

``` text
PROCESS HEALTH
BACKUP HEALTH
```

Suggested backup-set states:

``` text
HEALTHY
DEGRADED
STALE
FAILING
```

Expose at minimum:

-   last successful poll;
-   last completed backup;
-   age of newest known-good backup;
-   stale threshold;
-   current transfer;
-   pending deletes;
-   failures;
-   quarantined count;
-   last retention;
-   free space;
-   binary version;
-   embedded rclone version.

CLI:

``` bash
backup-manager status
```

Container health support is mandatory.

Optional HTTP:

``` text
/health/live
/health/ready
/status
```

------------------------------------------------------------------------

# rclone Upgrade Compatibility Contract

Because rclone is embedded rather than treated as a stable external CLI,
upstream API evolution is an explicit project risk.

The project SHALL minimize that risk through:

1.  a narrow `internal/transport/rclone` adapter;
2.  pinned module versions;
3.  no rclone types outside the adapter;
4.  transport contract tests;
5.  destructive-operation integration tests;
6.  explicit dependency upgrade PRs;
7.  rollback capability.

The project SHALL prefer stable/high-level rclone APIs over reaching
into implementation details.

If an rclone API required by the manager is unstable, the adapter SHALL
isolate that dependency.

Where a generally useful API improvement is needed, contributing it
upstream SHOULD be considered before maintaining a local patch.

A permanent local patch to rclone SHOULD require an Architecture
Decision Record because it creates fork-like maintenance obligations.

------------------------------------------------------------------------

# Security Requirements

1.  Dedicated SSH key.
2.  Host-key verification mandatory.
3.  Restricted SFTP account preferred.
4.  No credentials in Git.
5.  Credentials mounted read-only where practical.
6.  Remote metadata treated as hostile.
7.  No shell interpolation of remote filenames.
8.  Path traversal rejected.
9.  Unsafe symlinks rejected.
10. Destructive remote operation available only through explicit adapter
    API.
11. Remote object identity rechecked before deletion.
12. Local retention constrained to database-managed artifacts.
13. Container runs non-root where practical.
14. Architecture should permit future immutable/off-site copies.
15. rclone dependency security updates SHALL be tracked.

------------------------------------------------------------------------

# Failure-Safety Invariants

1.  **Never delete the remote source before a verified, durably
    committed local copy exists.**
2.  **Never use rclone move as a shortcut around manager-controlled
    commit sequencing.**
3.  **Never treat `.partial` as a restore point.**
4.  **Never overwrite a known-good backup with an unverified transfer.**
5.  **Never delete a remote pathname if the object appears to have
    changed since discovery.**
6.  **Never prune outside the managed local backup root.**
7.  **Never prune the last known-good backup solely because of age.**
8.  **Every lifecycle operation must be restart-safe.**
9.  **Retries must be idempotent.**
10. **Network uncertainty preserves data.**
11. **Required validation failure preserves the remote source.**
12. **Lifecycle policy must not depend on parsing rclone log/error
    strings.**
13. **rclone API details must not leak outside the transport adapter.**
14. **Process liveness is not evidence of backup freshness.**

------------------------------------------------------------------------

# UGREEN Deployment

Preferred deployment:

``` text
UGREEN NAS
┌─────────────────────────────────────────────────────┐
│ backup-manager container                            │
│                                                     │
│ Single Go executable                               │
│  ├── manager logic                                 │
│  └── embedded rclone packages                      │
│                                                     │
│ SQLite state ─────► persistent state volume         │
│ backups ──────────► NAS backup volume               │
│ SSH key ──────────► read-only secret                │
│ known_hosts ──────► read-only config                │
└─────────────────────────────────────────────────────┘
                  │
                  │ SSH/SFTP
                  ▼
            Remote Server
```

The production container SHALL NOT require a separately installed rclone
executable.

Container requirements:

-   pinned Go build;
-   pinned rclone module;
-   reproducible build;
-   minimal runtime image;
-   non-root where practical;
-   no privileged mode;
-   read-only application filesystem;
-   persistent SQLite state;
-   mounted backup storage;
-   read-only credentials/configuration;
-   restart policy;
-   health check.

Builds SHOULD target the architecture used by the UGREEN NAS, with
`linux/amd64` and/or `linux/arm64` supported as required.

------------------------------------------------------------------------

# CLI

``` bash
backup-manager run
backup-manager daemon

backup-manager check
backup-manager status

backup-manager sources
backup-manager artifacts

backup-manager fetch --source production --backup-set postgres-primary --dry-run

backup-manager retention --dry-run
backup-manager retention

backup-manager reconcile
backup-manager validate <artifact-id>

backup-manager version
```

`version` SHALL report both:

``` text
backup-manager version
embedded rclone version
Go version
build commit
```

------------------------------------------------------------------------

# Testing Requirements

## Unit

Test:

-   state machine;
-   artifact identity;
-   duplicate detection;
-   TOCTOU identity comparison;
-   rclone error translation;
-   retention buckets;
-   overlapping retention;
-   last-known-good;
-   timezones;
-   DST;
-   leap years;
-   path safety;
-   configuration.

## Transport Contract Tests

Every transport implementation SHALL pass a common contract suite:

``` text
list
stat
copy-to-local
hash/capability
delete
cancel
not-found
permission-denied
changed-object detection
```

This allows the embedded-rclone adapter to be replaced without rewriting
lifecycle tests.

## SFTP Integration Tests

Use a disposable SFTP server.

Cover:

-   authentication;
-   host-key verification;
-   listing;
-   copy;
-   interruption;
-   cancellation;
-   hashing where supported;
-   explicit delete;
-   permission denial;
-   remote object replacement;
-   multiple sources.

## Crash Matrix

Terminate after:

``` text
DISCOVERED
TRANSFERRING
TRANSFERRED
VERIFYING
VERIFIED
local fsync
rename
directory sync
COMMITTED
REMOTE_DELETE_PENDING
remote deletion
before COMPLETE
```

Restart/reconcile and prove safe convergence.

## rclone Upgrade Tests

A dependency upgrade SHALL execute:

-   full unit suite;
-   transport contract suite;
-   SFTP integration suite;
-   destructive safety suite;
-   crash/reconciliation suite.

## Destructive Safety

Prove that:

-   malicious paths;
-   symlinks;
-   replaced remote objects;
-   malformed configuration;
-   stale journal state;
-   adapter errors;

cannot cause unauthorized local or remote deletion.

------------------------------------------------------------------------

# Documentation Requirements

`README.md` SHALL document:

-   why rclone is embedded;
-   why rclone is not forked;
-   why the CLI is not normally invoked;
-   pinned rclone version;
-   dependency upgrade procedure;
-   adapter architecture;
-   SSH/SFTP setup;
-   restricted remote account;
-   lifecycle state machine;
-   verification;
-   durable commit;
-   TOCTOU protection;
-   GFS retention;
-   last-known-good protection;
-   reconciliation;
-   quarantine;
-   UGREEN deployment;
-   status/health;
-   recovery;
-   restore procedure.

An ADR SHOULD record:

``` text
ADR: Embed rclone behind transport adapter rather than fork or subprocess
```

------------------------------------------------------------------------

# Delivery Plan

## Phase 1 --- Embedded rclone proof of concept

Before implementing the full manager, prove:

-   Go application embeds rclone successfully;
-   only local + SFTP backends are registered;
-   remote listing works;
-   single-file copy works;
-   context cancellation works;
-   transfer statistics are accessible;
-   explicit remote delete works;
-   host-key verification works;
-   target UGREEN architecture builds/runs.

**Exit gate:** proceed only if the required rclone APIs can be isolated
behind the manager-owned transport interface without extensive use of
unstable internals.

If this gate fails, reassess subprocess integration before considering
any fork.

## Phase 2 --- Lifecycle foundation

Implement:

-   configuration;
-   SQLite/migrations;
-   artifact identity;
-   discovery;
-   completion detection;
-   state machine;
-   transfer;
-   verification;
-   durable commit;
-   explicit remote delete;
-   reconciliation.

## Phase 3 --- Retention and operations

Implement:

-   GFS;
-   last-known-good;
-   dry-run;
-   disk capacity;
-   health;
-   status;
-   daemon;
-   container deployment.

## Phase 4 --- Validation hardening

Implement:

-   checksums;
-   validators;
-   quarantine;
-   scheduled validation/restore-test hooks.

## Phase 5 --- Security/resilience extensions

Potential:

-   immutable NAS snapshots;
-   off-site copy;
-   separate ingestion/retention privileges;
-   alerts;
-   metrics;
-   WORM/immutable storage.

------------------------------------------------------------------------

# Acceptance Criteria

-   [ ] Tool lives in its own repository, `spdrman/rclone-manager`.
-   [ ] Implementation is Go.
-   [ ] rclone is embedded as Go modules.
-   [ ] rclone is not forked.
-   [ ] Normal operation does not require the rclone executable.
-   [ ] Only required rclone backends are registered initially.
-   [ ] rclone dependency is explicitly pinned.
-   [ ] rclone is isolated behind a manager-owned transport interface.
-   [ ] No rclone-specific types leak into lifecycle/state/retention.
-   [ ] No generic transport `Move` operation is exposed to lifecycle
    logic.
-   [ ] Phase-1 embedding feasibility gate passes.
-   [ ] SFTP key authentication works.
-   [ ] Host-key verification works.
-   [ ] Completed backup discovery works.
-   [ ] SQLite lifecycle journal works.
-   [ ] Explicit lifecycle state machine works.
-   [ ] Transfer uses a `.partial` destination.
-   [ ] Transfer verification works.
-   [ ] Required validation can block deletion.
-   [ ] Local durable commit occurs before remote deletion.
-   [ ] Remote delete is separately invoked after `COMMITTED`.
-   [ ] Remote object identity is rechecked before deletion.
-   [ ] Changed/replaced remote objects are not deleted.
-   [ ] Restart/reconciliation is safe at every lifecycle stage.
-   [ ] GFS retention works.
-   [ ] Last-known-good protection works.
-   [ ] Retention cannot escape the managed root.
-   [ ] Dry-run explains retention decisions.
-   [ ] Disk-pressure handling works.
-   [ ] Backup freshness is reported independently of daemon liveness.
-   [ ] UGREEN container deployment works.
-   [ ] Transport contract tests pass.
-   [ ] SFTP integration tests pass.
-   [ ] Crash matrix passes.
-   [ ] Destructive-safety tests pass.
-   [ ] rclone upgrade procedure is documented.
-   [ ] No credentials are committed.

------------------------------------------------------------------------

# Adversarial Review and Consensus

This design was subjected to an adversarial review from five engineering
perspectives before finalization.

## Expert 1 --- Backup / Disaster-Recovery Architect

### Initial challenge

Embedding rclone must not blur the distinction between **successful
transfer** and **valid restore point**. Generic file-copy success is not
backup validity.

The design must retain:

-   producer completion semantics;
-   explicit verification levels;
-   application-specific validation;
-   quarantine;
-   GFS retention;
-   last-known-good protection.

### Required change

The lifecycle manager, not rclone, remains authoritative for backup
validity and source deletion.

### Final position

**APPROVE.**

------------------------------------------------------------------------

## Expert 2 --- Security Engineer

### Initial challenge

Direct library access to rclone destructive operations can make
accidental source deletion easier than a constrained subprocess
interface if the abstraction is too broad.

A remote pathname may also be replaced between discovery and deletion.

### Required changes

-   expose no generic `Move` method;
-   isolate `DeleteRemote`;
-   restrict deletion to lifecycle commit transitions;
-   re-stat/re-identify the remote object before deletion;
-   fail closed on identity uncertainty;
-   use restricted SFTP credentials;
-   track rclone dependency security updates.

### Final position

**APPROVE.**

------------------------------------------------------------------------

## Expert 3 --- Go / Dependency Architecture Engineer

### Initial challenge

Embedding rclone couples the project to Go APIs that may evolve more
freely than the CLI contract. A fork would be worse, but indiscriminate
direct imports could still create significant maintenance cost.

### Required changes

-   one narrow adapter package;
-   no rclone types outside it;
-   pinned versions;
-   contract tests;
-   explicit upgrade workflow;
-   avoid unstable internals;
-   prefer upstream contributions over local patches.

### Final position

**APPROVE.**

------------------------------------------------------------------------

## Expert 4 --- Distributed Systems / Reliability Engineer

### Initial challenge

Using rclone `move` would combine transfer and deletion inside a generic
operation and undermine the explicit distributed transaction required by
the backup manager.

There is also a TOCTOU risk if a producer replaces a file under the same
remote pathname after discovery.

### Required changes

-   copy only;
-   manager-controlled verification;
-   durable local commit;
-   SQLite `COMMITTED`;
-   separate delete;
-   remote identity persisted at discovery;
-   remote identity checked again before deletion;
-   crash matrix around every boundary.

### Final position

**APPROVE.**

------------------------------------------------------------------------

## Expert 5 --- SRE / Maintainability Engineer

### Initial challenge

Embedding a large dependency creates upgrade and operational risk. The
project must prove that the integration is practical on the UGREEN NAS
before building the rest of the system.

The design also needs an escape hatch if rclone's Go APIs prove too
unstable.

### Required changes

Add a Phase-1 feasibility gate proving:

-   build for target NAS;
-   SFTP listing/copy/delete;
-   cancellation;
-   host-key verification;
-   transfer statistics;
-   narrow adapter feasibility.

If the gate fails, prefer reverting to a subprocess adapter rather than
forking rclone.

### Final position

**APPROVE.**

------------------------------------------------------------------------

# Consensus Decision

All five reviewers converge on the following architecture:

> **Build a standalone Go backup lifecycle manager that embeds a pinned
> version of rclone behind a narrow manager-owned transport interface.
> Do not fork rclone. Do not use rclone `move` for the backup
> transaction. Copy through rclone, verify and durably commit under
> manager control, persist the commit in SQLite, revalidate remote
> object identity, and only then explicitly delete the remote source.**

The reviewers further agree that:

1.  rclone is an implementation dependency, not the owner of backup
    lifecycle policy;
2.  rclone APIs must be quarantined behind the transport adapter;
3.  SQLite is the authoritative lifecycle journal;
4.  remote deletion must remain an explicit manager-controlled
    transaction step;
5.  remote object replacement/TOCTOU must be detected before deletion;
6.  rclone upgrades require full transport and destructive-safety
    regression testing;
7.  a Phase-1 embedding feasibility gate is mandatory;
8.  failure of that gate should lead to a subprocess architecture, **not
    a fork**;
9.  backup validation, GFS retention, last-known-good protection and
    freshness monitoring remain first-class manager responsibilities;
10. the architecture should make generic improvements upstreamable to
    rclone rather than accumulating local rclone patches.

------------------------------------------------------------------------

# Final Engineering Principle

The project should own only what is specific to trustworthy backup
lifecycle management.

``` text
rclone:
    move bytes reliably

backup-manager:
    decide what those bytes mean,
    when they are safe,
    when the source may be destroyed,
    and which restore points must survive
```

That boundary is the central architectural constraint of this EPIC.
