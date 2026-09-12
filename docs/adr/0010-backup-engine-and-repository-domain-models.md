# ADR 0010: What a backup engine, a repository domain and a source identity are in this product's own vocabulary

## Status

Accepted as the first Phase 1 issue of EPIC K (K1, #780). The types are
implemented and tested in `core/internal/model` (`engine.go`,
`repository.go`, `sourceidentity.go`, `verificationlevel.go`) and the
configuration seam is in `core/internal/config`. Nothing in #781–#784 may
introduce a second vocabulary for any of them.

It stands on ADR 0006 (the embedded engine lives behind
`core/internal/backupengine` and its single adapter) and ADR 0009 (what a
source promises and what its metadata proves). This ADR is about the
domain's own nouns: which engine a backup set runs, which security boundary
its repository is, which source it is snapshotting, and how far a restore
point has been checked.

## Context

EPIC K adds a second engine to a product that has exactly one. Everything
in this ADR follows from two facts about that:

1. **Every backup set that exists today is an artifact set, and says
   nothing about an engine.** There are, and will remain, deployments whose
   config file has never heard of any of this. An upgrade that reinterpreted
   one of those sets would change what a run does to a source: the artifact
   engine's whole point is that a committed durable copy may release the
   source's copy (FR-15), and applying that to a live source tree would
   delete an operator's data.

2. **A content-addressed repository is a security and failure domain, not
   a directory.** One repository is one encryption key, one credential, one
   deduplication span, one maintenance owner, one corruption blast radius
   and one set of administrators. The operational pressure runs entirely one
   way: deduplication improves the more data shares a repository,
   maintenance is cheaper with one repository, credentials are simpler with
   one. The cost only becomes visible during the incident that makes it
   matter.

There is a third fact that is less obvious and caused more design work than
either of the above: **an incremental engine has to recognise a source it
last saw a week ago**, and the ways a deployment moves a source are
routine. A NAS replaces a disk and `/mnt/disk1` becomes `/mnt/disk2`. A
container's bind mount changes. A package moves from `/opt` to
`/usr/local`. An externally-provided snapshot (ADR 0009's
`external_snapshot` mode) is mounted under a fresh temporary directory
every single night, by design. If any of that is part of the source's
identity, the next run finds no predecessor snapshot, re-reads every byte,
stores a second full copy, and every retention, last-known-good and
verification decision downstream is being made about two streams that are
the same data. Nothing fails. The deployment silently doubles and loses its
history.

## Decision

### 1. `BackupEngine` is a domain enum, and omission resolves to `artifact` in one place

`model.BackupEngine` has two values, `artifact` and `kopia`, and
`model.ResolveBackupEngine` is the only thing that turns what
configuration said into an engine. An empty string resolves to
`EngineArtifact`; anything unrecognised is an **error**, not a fall-back.

Both halves are deliberate. Resolving silence in one named function means
no caller compares against a zero value, so a third engine cannot silently
inherit the default by sorting first. Refusing an unrecognised value in
both directions means a typo can neither discard an operator's request for
snapshots nor change what a run does to a source tree.

`BackupEngine.UsesRepository()` is what the configuration layer branches
on, and it reports false for an engine this build does not know — the
conservative direction, since the alternative invents a security boundary
for something nobody modelled.

The configured identifier of the incremental engine is the vendor's name,
because that is what an operator writes and what the API will carry. That
is a **value** in this product's namespace, not a borrowed type, and the
distinction is checked rather than asserted: see §5.

### 2. A Repository Domain is a boundary of six things, shared all or nothing

`model.RepositoryDomain` is an id, an operator's description, and a
declared `RepositoryIsolation` of `shared` or `isolated`.
`model.RepositoryRef` is one backup set's reference to a domain, carrying
both the domain id **and** the set's own identity, which is what makes a
reference checkable: co-tenancy is a statement about two sets.

`model.RepositoryBoundaries` names what co-tenancy shares — encryption,
credential, failure/corruption, maintenance, deduplication, administrative
trust — and there is deliberately no way to share some and not others,
because they are properties of one repository. A field offering partial
sharing would be a promise the storage layer cannot keep.

Three rules, all enforced by the model rather than by a validator:

- `Admits` refuses a reference that names another domain
  (`ErrForeignDomain`).
- `MayShare` permits two sets in a `shared` domain, and refuses two
  different sets in an `isolated` one (`ErrIsolationViolated`). The same
  set compared with itself is not co-tenancy and is not a violation, or an
  isolated domain could not hold the single set it exists for.
- Both refusals **name what would have been shared**. A refusal that says
  "not allowed" is an assertion somebody overrules the next time
  deduplication ratios come up; a refusal that lists the six is an
  argument.

Isolation is **required** in configuration, with no default. Defaulting to
`shared` would make an isolation boundary a belief rather than a rule;
defaulting to `isolated` would silently forgo the deduplication that is the
reason to run this engine at all. The topology EPIC K asks for —
production, security-sensitive, customer-A, customer-B, archive — is
expressible because nothing in the model funnels a deployment towards one
domain: there is no singleton and no "default" domain that others are
variations of.

What is deliberately **not** in a domain: where the repository's bytes
live, how it is unlocked, and what maintenance it needs. Those belong to
`core/internal/backupengine` and #781. A domain is the boundary; the
repository behind it is somebody else's type.

### 3. Source identity is a function of three things and nothing else

`model.NewSourceIdentity` is a SHA-256 over a versioned, length-prefixed,
labelled canonical form of exactly:

- the backup set's **durable identifier** (`uuid` in configuration),
- the source **endpoint** identity (kind, host, port, user),
- the source **root as the source sees it**.

Everything else is excluded, and each exclusion is one of the moves from
the Context above:

| Excluded | Because |
| --- | --- |
| the set's name and its source's name | both are editable in the UI (FR-7), and a rename must not orphan a lineage |
| `local_path` | the staging/durable path on this side is not the source |
| `source_mount_prefix` | the declared mount, install prefix or per-run temp directory is how this deployment reaches the source, not what the source is |
| the host's letter case | DNS is case-insensitive |
| an omitted port versus the default written out | one endpoint, two spellings; the wizard writes it, a hand-written file usually does not |

The mount prefix is **declared, not detected**. Nothing on this side can
tell which leading segments of `/srv/snap-47/data/pg` are the mount, and a
heuristic there would be a guess deciding whether a deployment keeps its
backup history. A prefix that is not a **segment** prefix of the path is
refused rather than ignored (`/mnt/user` is not a prefix of
`/mnt/userdata`), because ignoring it would put the whole mount path back
into the identity for the one deployment that tried hardest to keep it out.

The identity is a digest rather than a readable composite for two reasons:
the composite would contain a host and a path, and this product already has
deployments that treat both as sensitive (`sensitive_endpoint`, #295); and
a composite invites parsing, which would become a second, unvalidated way
to build one.

`identitySchema` (`backupd.source-identity.v1`) is mixed into every digest
and pinned by a test, so a future change to the canonical form has to be
taken as a **migration** — it re-identifies every source in every existing
deployment — rather than arriving as a refactor.

### 4. Verification is a ladder of four rungs, not a bool

`model.VerificationLevel` is `structural` < `content_sample` < `full` <
`restore_drill`, ordered, with `AtLeast`, `ReadsContent` and
`ProvesRestorable`. An unrecognised level ranks 0, so it satisfies nothing
and nothing satisfies it: a typo in a policy must never buy a claim.

This exists because structural verification is not restore verification.
An engine can prove that every content identifier resolves and every hash
matches, and none of that proves a restore produces the tree an operator
expects. A product that reports "verified" without saying which rung it
climbed is making a promise it has not tested.

It is **not** ADR 0009's `VerificationMode`, and the two live beside each
other on purpose: that one is a backup-time decision about how much source
content must be read rather than trusted to metadata; this one is an
after-the-fact question about a snapshot that already exists. A
conservative source policy says nothing about whether the repository has
rotted since, and a nightly restore drill says nothing about whether the
source was read honestly in the first place.

The default is the weakest rung, because every stronger one spends a
deployment's I/O budget on a cadence, which is not this product's decision
to make by default.

### 5. The configuration seam is additive, and the boundary is checked by name

A backup set gains `engine`, `uuid`, `repository_domain`,
`source_consistency`, `verification_level` and `source_mount_prefix`; the
file gains a top-level `repository_domains` list. Every one of them is
`omitempty`, which is FR-35's round-trip rule: `core/service` re-marshals
the whole `Config` on every settings save, and a key injected into a file
that never opted in is a file an older binary refuses outright under
`Load`'s `KnownFields(true)`.

`Validate` resolves the seam the same way it already resolves `ID`,
`ReadOnly` and `Retention`: the raw keys are never read outside `Validate`,
and the resolved `Engine`, `Repository`, `Consistency`,
`VerificationLevel` and `SourceIdentity` fields are what everything
downstream reads.

Keys an artifact set cannot act on are **refused**, not ignored. That is
this package's existing rule for sftp fields on a local remote and for an
`attested` upload verification no medium can achieve: a configuration the
build validates and can never execute is worse than one it refuses,
because the operator who wrote the key believes something about how their
backup runs and `backupd check` told them it was fine.

The quarantine claim itself is checked mechanically in
`core/internal/backupengine/boundary_test.go`:

- no import of the embedded engine anywhere in the **repository** (not just
  in `core/`, which is as far as the previous sweep reached), outside the
  one adapter directory;
- and, for the surfaces EPIC K names by name — `api/v1`, `ui/shared/src`,
  `core/apicontract`, `core/internal/state`, `core/internal/model`,
  `core/internal/config`, `core/service`, `core/cmd` — no **declaration**
  named after the vendor. Go surfaces are parsed and their type, field,
  method, function and import names are checked; a constant's value is not,
  because `engine: kopia` is this product's own configuration vocabulary.
  Non-Go surfaces are checked as text, where the same distinction is "the
  bare token, quoted" versus "glued to an identifier".

Both scanners are driven over sources that must fail and sources that must
pass, so a clean sweep is evidence about the surfaces rather than about a
scanner that looks for nothing.

## Consequences

- **Existing deployments are untouched.** A set with no `engine` key
  resolves to the artifact engine, acquires none of the incremental
  vocabulary, and re-marshals byte for byte. The compatibility corpus
  (`core/tests/compat`) sees no change to any medium-free surface.
- **An incremental set cannot be created accidentally.** It has to name an
  engine, a durable identifier and a declared repository domain, and the
  domain has to state its co-tenancy. There is no path that silently lands
  snapshots in a shared repository.
- **Renaming a set, moving a mount, changing an install prefix and
  re-mounting a nightly snapshot under a new temp directory all preserve
  the lineage**, and a genuinely different set, host, user, port or source
  root does not.
- **The identity's canonical form is now a compatibility surface.** It is
  pinned by a test whose failure message says what changing it costs.
- **Two vocabularies now exist for the word "verification"** — ADR 0009's
  mode and this ADR's level. They are named differently
  (`VerificationMode` / `VerificationLevel`, `Verify*` / `Level*`) and each
  one's doc comment names the other. This is the cost of keeping
  backup-time and after-the-fact verification distinguishable, and it is
  cheaper than a single number that means either thing depending on who is
  reading.
- **#781–#784 inherit the nouns.** A repository adapter that wanted its own
  notion of a domain, or a snapshot lifecycle that wanted its own source
  identity, is now a change to this ADR rather than a local decision.

## Alternatives considered

**A `BackupEngine` whose zero value is the artifact engine.** Identical
behaviour today, and it makes "what does an unset engine mean" a property
of Go's zero values rather than of a function with a test on it. The first
engine added after this one inherits the default by accident.

**Repository storage (local filesystem, native S3) as another rclone
backend, listed beside `local_volume` and `s3`.** This is the modelling
mistake EPIC K's architectural rules name explicitly. A repository is not a
destination for a copy; the engine that writes it is not a transport; and a
surface that offered `local_volume`, `s3` and `kopia` as one set of choices
would be teaching operators something false.

**A domain with per-boundary sharing flags** ("share deduplication, not
credentials"). Unimplementable: they are the same repository.

**Source identity as a readable composite** (`host:path` or
`setname@host:path`). Cheap to debug, and wrong on three counts: it carries
an endpoint some deployments treat as a credential into the catalog and the
logs, it invites parsing, and the `setname` form re-forks on every rename.

**Deriving the durable identifier from the set's name** instead of
requiring a uuid. It is the bug the identifier exists to prevent, with
extra steps.

**Detecting the mount prefix** by walking up to a mount point or by
recognising temp-looking paths. A heuristic deciding whether a deployment
keeps its backup history.

**Verification as a boolean plus a "deep" flag.** Two bits that cannot
express the one distinction EPIC K asked for, between reading what is
stored and proving what can be restored.
