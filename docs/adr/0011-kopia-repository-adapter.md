# ADR 0011: Where an embedded-engine repository lives, how it is unlocked, and what it has to prove before it is used

## Status

Accepted as K2 (#781) of EPIC K Phase 1. Implemented in
`core/internal/backupengine` (`engine.go`'s repository location and reports,
`reserved.go`, `maintenanceowner.go`), its single adapter
(`core/internal/backupengine/kopia/repository.go`), and the new secret
resolver `core/internal/secretref`. Proved locally by
`core/internal/backupengine/kopia`'s own tests and against a real S3 API by
`core/tests/miniointegration/kopiarepository_test.go`.

It stands on ADR 0006 (the embedded engine lives behind
`core/internal/backupengine` and exactly one adapter package imports it) and
ADR 0010 (a Repository Domain is the security boundary; where its bytes live
is this ADR's business, not the model's).

## Context

The engine embedded by ADR 0006 was proved against one storage backend, a
local directory chosen by the caller, with a passphrase carried as a plain
string on the location struct. That is enough for a feasibility spike and
not enough for a product. Four things were missing, and each of them is a
decision rather than an implementation detail.

1. **A repository has to have a name that survives a restart.** A location
   identified by its directory is identified by a mount point, and mount
   points move: a NAS replaced, a volume restored, a share remounted
   elsewhere. Everything downstream — the catalog's snapshot ids,
   maintenance ownership, retention — refers to a repository that has to
   still be the same repository tomorrow.

2. **A repository in a backup root is a few thousand files that look like
   nothing.** A backup root is populated, exported over SMB or AFP, walked
   by discovery, recorded by a catalog and pruned by retention. Pack and
   index blobs sitting in it have no sidecar manifest and no catalog row,
   which makes them candidates for adoption, for quarantine, or for
   deletion. The likely outcome is not an error: it is a prune that removes
   pack files and a repository that cannot restore anything, found out
   about later.

3. **"S3-compatible" is a marketing claim.** The embedded engine's own
   storage contract requires read-after-write on GETs *and on listings*,
   atomic whole-blob writes, honest range reads, and timestamps that do not
   run backwards. A number of products answering S3 on a port provide some
   of those. The ones they most often miss are the ones whose absence
   cannot be recovered from: an index blob that is not yet visible in a
   listing is not an error, it is content the repository has forgotten.

4. **Two secrets now have to be inside this process.** A repository
   passphrase and, for a bucket, object-store credentials. The medium plane
   (`internal/transport/rclone`) avoids this by handing rclone the
   credentials *file* so the material never enters this program's memory.
   The repository plane cannot: the native providers take a passphrase and
   an access key as parameters, and there is no subprocess to hand a path
   to.

## Decision

### A repository is identified by its Repository Domain, and its storage is derived

`RepositoryLocation` carries `Domain` (ADR 0010's
`model.RepositoryDomainID`), a `Root` for local repositories or an
`S3Storage` for a bucket, a `StateDir` for this process's own local state,
and two secret references. It no longer carries a repository directory, a
config file path or a cache path: all three are derived from the domain id.

Deriving rather than carrying removes a whole failure class at its source.
The config file is named after the domain, so two repositories cannot share
connection state by accident; the storage directory is named after the
domain, so two domains cannot share a directory and overwrite each other's
format blob; and reopening after a restart needs nothing but the two things
an operator declared, which is what
`TestRepositoryReopensByStableIdAfterRestart` proves by deleting every byte
of local state before the second open.

The identity check survives this and is not redundant. The case it catches
is now the realistic one: the backup root moved, the domain id did not, so
both locations resolve to one config file, and the file's contents describe
the old root. `TestOpenRepositoryConnectsTheRequestedStorage` constructs
exactly that.

### Local repositories live in a reserved namespace, and reserved means a predicate

`<backup-root>/.backupd/repositories/<domain>/` holds blobs;
`<backup-root>/.backupd/state/` holds this manager's own local state about
them. Both are under one reserved directory, and the rule is not the dot
prefix — the dot prefix is politeness towards an operator browsing the
share. The rule is `backupengine.LocalPathIsReserved`, one predicate,
compared as paths and never as strings, that artifact management consults
and that is never true for an artifact.

The test that matters is not that the predicate returns the right answers
for a list of paths, though it does. It is
`TestReservedNamespaceIsInvisibleToArtifactManagement`, which stands up a
real repository with real pack and index blobs in a backup root that also
holds a real artifact and its sidecar, and then walks the entire root
asserting that *every* file is either that artifact or reserved. A future
change that writes a lock file, a log or a cache beside the artifacts fails
there, which is the only place it would fail before a prune deleted it.

### Native providers only, and the target is probed before it is trusted

Two backends are registered: filesystem and native S3. The vendor also ships
a provider that proxies to rclone, and registering it would have made every
backend rclone speaks available for one import line. It is refused. rclone
stays on the source side, where a wrong answer is a failed read; a
repository built on storage that does not meet the contract above does not
fail, it loses content.

Inside the S3 backend the same scepticism is applied to the endpoint rather
than to the client. `probeStorage` writes one blob to a reserved blob-id
prefix and checks, in order: that it can be read back whole, that a range
read at a non-zero offset returns *that range*, that a listing shows it
immediately, that a delete removes it, that the listing then agrees, and
that the storage reported a timestamp for it. It runs at create time,
before the repository format is written, so an unsupported target is left
as an empty bucket and an explicit `ErrStorageUnsupported` naming the
missing property — not as a half-usable repository discovered during a
restore.

It also runs on every `Health` call, deliberately. A bucket policy that
starts denying deletes, or a filesystem that fills up, is as interesting as
a target that never had the property.

The faults are injected in unit tests (`probe_internal_test.go`), one
property at a time, because the endpoints worth refusing are the ones
nobody has in a test rig. The MinIO run proves the other half: a conforming
endpoint passes, over the wire, with signed requests and multipart uploads.

### Clock skew is a warning, and time synchronisation is not this product's job

Almost everything a repository does about concurrency and reclamation is
expressed in timestamps: a maintenance lock is held until a time, recently
written content is too new to garbage-collect until a time, and a
snapshot's own start time is what retention later reasons about. A clock an
hour fast can make one process treat another's live lock as expired; an
hour slow can make fresh content look old enough to reclaim. Neither
produces an error when it happens.

So the probe's timestamp — which for S3 is the server's own
`Last-Modified`, a genuine comparison against another machine's clock — is
compared against this host's, and a disagreement over five minutes is a
`HealthWarningClockSkew` warning with the measurement in it.

It is a warning and never a refusal. A product that stops backing up
because an NTP server was unreachable has traded a risk for a certainty.
Fixing the clock is the operating system's job, and this ADR does not
propose implementing time synchronisation.

### Both secrets are references, resolved as late as possible

`RepositoryLocation.Passphrase` and `S3Storage.Credentials` are
`secretref.Ref`: a file, an environment variable, or a command whose stdout
is the material. There is no field a literal secret fits into, which is the
enforcement rather than a preference — a test that wanted to shortcut would
have to add one.

A `Ref` is therefore safe to log, safe to render into an error, and safe to
keep for the life of an open repository handle, which is what lets `Health`
reach the storage again without being handed the credentials a second time.
The material is resolved at the moment a provider is built and is not
retained by the adapter; buffers holding it are zeroed as far as Go allows.

`core/internal/secretref` is a new package, and the honest account of why
is this: the *declaration* is not new — `config.Passphrase`,
`config.MediumCredentials` and `transport.MediumCredentials` are the same
three fields, and `TestRefFieldSetMatchesTheConfiguredOnes` fails if any of
them grows a fourth, because a source an operator can write and this
resolver cannot read is what would make "one custody model" untrue. The
*mechanism* is duplicated: `internal/transport/rclone` resolves its own
copies privately, in a shape built around rclone's configmap, and unifying
the two means refactoring that package's most security-sensitive file.
That is tracked separately and is not something to attempt sideways from a
repository adapter.

### Maintenance ownership is recorded now and decided later

A repository has exactly one maintenance owner; two processes reclaiming
space concurrently is how a repository loses content it still references.
The adapter already overrides the vendor's own owner check (deferring to
whichever machine created the repository means a maintenance window that
silently never runs), so this product owes the same guarantee from its own
side, and that needs a durable record: repository, owner, last quick, last
full, next eligible, last result.

`backupengine.MaintenanceOwnership` and a file-backed store are that record
and nothing more. There is deliberately no method that decides whether
maintenance may run: `NextEligible` is a recorded fact, nothing here reads
the clock to compare against it, and a half-made scheduling decision in
this file would be a second scheduler for #786 to contend with rather than
a foundation to build on. `Load` distinguishes "no record" (nobody has
owned this yet; claim it) from "unreadable record" (something is wrong;
absolutely do not claim it), because reading the second as the first is how
two instances both decide they own maintenance.

## Consequences

- `Repository` gains `LookupSnapshot`, `Health` and `Stats`. Lookup exists
  because the catalog stores an id and later has to ask what became of it;
  answering that by listing a source's snapshots costs a manifest load
  each and cannot answer for a source whose identity has since changed.
  Health and Stats are separate because their costs differ by orders of
  magnitude: health is a cheap preflight, and stats walks the storage's
  blob listing.
- `RepositoryStats` reports PHYSICAL numbers, read from the storage's own
  listing rather than the repository index, because the question is what
  the repository costs and the answer includes blobs an interrupted
  maintenance left behind.
- There is no "do not verify TLS" option, and there will not be. A knob
  that disables authentication of the endpoint carrying every backup this
  product holds is not a convenience; `RootCA` is the answer to a private
  CA.
- A repository's content cache is always enabled, under `StateDir`. The
  spike's "empty cache path means no cache" spelling is gone: a persistent
  index cache is the correct production default, and the alternative was a
  knob whose only user was a test.
- Space reclamation is still not observable immediately after maintenance,
  and the S3 matrix says so rather than asserting otherwise. Maintenance
  runs at full safety, which keeps recently written content out of garbage
  collection so that it is safe to run while a snapshot is in progress;
  every blob in a test's repository is seconds old.
- Configuration does not yet name a repository's storage. An operator
  cannot point a Repository Domain at a bucket from `config.yaml` until the
  wiring issue lands; `S3Storage` is deliberately the same set of facts
  `config.StorageMedium` already collects, so that wiring is a mapping and
  not a second vocabulary.

## Alternatives considered

**Let the operator choose the repository directory.** Rejected on the
strength of consequence 2 in the context: the directory an operator would
choose is inside the backup root, which is the one place it must not be,
and a warning in a doc comment is not a mechanism. Deriving the path also
made the config-file collision impossible by construction rather than
merely checked.

**Use the vendor's rclone-backed repository provider.** Rejected. It would
have delivered every backend rclone speaks for one import line, and it
would have made the storage contract unenforceable: an rclone remote
provides whatever its own backend provides, and the properties a repository
needs are exactly the ones a file-copy tool has no reason to guarantee.

**Trust the endpoint and report failures as they arrive.** Rejected. The
failures that matter do not arrive as failures. A listing that is
eventually consistent returns a successful, short listing, and the
repository proceeds on it.

**Resolve secrets once at startup and hold them.** Rejected: it maximises
exactly the window this design is trying to minimise, and it would have put
material on a struct that is otherwise safe to log, which is the property
every leak assertion in this change depends on.

**Refuse to open a repository whose clock is skewed.** Rejected. See the
decision: the correct response to "NTP is broken" is not "stop backing up".
