# ADR 0010: What a backup engine, a repository domain and a source identity are in this product's own vocabulary

## Status

Accepted as the first Phase 1 issue of EPIC K (K1, #780). The types are
implemented and tested in `core/internal/model` (`engine.go`,
`repository.go`, `sourceidentity.go`, `verificationlevel.go`), the
configuration seam is in `core/internal/config`, and the refusal that keeps
an engine this build cannot run away from a source tree is in
`core/internal/app` (`cycle.go`'s `ErrEngineNotImplemented`). Nothing in
#781–#784 may introduce a second vocabulary for any of them.

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
`kopia`, because that is what EPIC K's definition of done says an operator
writes and what the API will carry. That is a **value** in this product's
namespace, not a borrowed type, and the distinction is checked rather than
asserted: see §5.

The trade-off is real and is accepted with its eyes open. A vendor name in
configuration is a name a deployment's files are written in, so replacing
the embedded engine later means either keeping `kopia` as the spelling of
something that is no longer Kopia, or migrating every config file that set
it. The alternative — a neutral value such as `incremental` or `snapshot`
— was rejected because it makes the product LESS honest in the place
honesty is cheapest: an operator choosing an engine is choosing a
repository format they may have to open with the vendor's own tools during
a restore, and a name that hid that would be protecting the product's
future refactors at the expense of the person doing the recovery. The
quarantine rules in §5 are what keep the cost bounded to the value: no
type, field, method, function or import anywhere on the named surfaces
carries the name, so a replacement is a configuration migration and not a
redesign.

### 2. A Repository Domain is a boundary of six things, shared all or nothing

`model.RepositoryDomain` is an id, an operator's description, and a
declared `RepositoryIsolation` of `shared` or `isolated`.
`model.RepositoryRef` is one backup set's reference to a domain, carrying
both the domain id **and** the set's own identity, which is what makes a
reference checkable: co-tenancy is a statement about two sets.

`model.RepositoryBoundaries()` names what co-tenancy shares — encryption,
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

And one limit on what "enforced" means here, because it is easy to read as
more than it is: **isolation is enforced against the configuration, not
against the repository's contents.** `MayShare` compares two declared
references, so it catches the edit that dissolves a boundary — a second set
pointed at a domain declared `isolated` — which is how a boundary actually
dissolves. It says nothing about what is already inside the repository: a
domain declared `isolated` whose repository was populated by some other
tool, or by an earlier configuration, is still declared isolated and this
model cannot tell. Whatever checks a repository's own contents belongs to
#781, which is the first thing able to open one.

The vocabularies (`BackupEngines()`, `RepositoryIsolations()`,
`RepositoryBoundaries()`, `VerificationLevels()`) are accessors returning a
copy rather than exported slice variables, following
`backend.CapabilityKeys()`. A closed set a caller can assign through is not
closed, and the assignment would change what the `Parse*` functions accept
for every goroutine in the process.

### 3. Source identity is a function of three things and nothing else

`model.NewSourceIdentity` is a SHA-256 over a versioned canonical form
built from exactly:

- the backup set's **durable identifier** (`uuid` in configuration),
- the source **endpoint** identity, as four values: kind, user, host, port,
- the source **root as the source sees it**.

The canonical form is a fixed sequence of labelled, length-prefixed fields,
**one field per value**, appended into a single buffer and hashed once. The
length prefix is what makes the concatenation unambiguous; one field per
value is what stops a value reaching across a field boundary. The second
rule is not theoretical — the first implementation rendered the endpoint as
one `kind://user@host:port` string, which means `user="a@b", host="c"` and
`user="a", host="b@c"` hash identically, and an `@` in an sftp user is how
a tenant or a domain is ordinarily written. Two different sources on one
lineage is the failure this whole section exists to prevent, arriving from
the inside.

Everything else is excluded, and each exclusion is one of the moves from
the Context above:

| Excluded | Because |
| --- | --- |
| the set's name and its source's name | both are editable in the UI (FR-7), and a rename must not orphan a lineage |
| `local_path` | it is not the source — and an incremental set may not carry one at all (§5) |
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

The pin was re-taken once, inside this issue, when the endpoint was split
into four fields. The tag stays at `v1` deliberately: a schema tag exists
to make a migration visible to deployments that have identities STORED, and
nothing in this product writes one yet — #783 is the first thing that
will. There is no lineage in the field to fork, so bumping the tag would
announce a migration nobody has to perform. The next change to the
canonical form does not get that excuse.

### 4. Verification is a ladder of four rungs, not a bool

`model.VerificationLevel` is `structural` < `content_sample` <
`content_full` < `restore_drill`, ordered, with `AtLeast`, `ReadsContent`
and `ProvesRestorable`. An unrecognised level ranks 0, so it satisfies
nothing and nothing satisfies it: a typo in a policy must never buy a
claim.

This exists because structural verification is not restore verification.
An engine can prove that every content identifier resolves and every hash
matches, and none of that proves a restore produces the tree an operator
expects. A product that reports "verified" without saying which rung it
climbed is making a promise it has not tested.

The third rung is spelled `content_full`, not `full`. Beside
`restore_drill`, a bare `full` reads as "a full restore was done", which is
the one claim that rung does not make; the prefix ties it to
`content_sample`, which is what it actually differs from, and only by how
much is read. The value is persisted (configuration, and a catalog column
later), so it was renamed now, while nothing has written one.

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

#### 4a. How this ladder relates to FR-31's `verification_class`

There is already a verification ladder in this product:
`verification_class` on a **placement** (`core/internal/state`, served
through `api/v1`), whose rungs are `existence` < `attested` < `content`.
Two ladders with overlapping words is exactly the kind of thing that
becomes indistinguishable in a UI, so the relationship is stated here
rather than left to be inferred.

They are different ladders about different objects, and both are kept:

| | FR-31 `verification_class` | EPIC K `verification_level` |
| --- | --- | --- |
| Subject | one **placement**: a durable copy of one artifact on one medium | one **restore point**: a snapshot in a repository |
| Question | how hard did we look at THIS COPY | how hard did we look at THIS SNAPSHOT |
| Rungs | `existence`, `attested`, `content` | `structural`, `content_sample`, `content_full`, `restore_drill` |
| Written by | the move/verify engine, per copy, per check | #783's verification pass, per snapshot |
| Meaning of absent | nothing has verified this copy | — (a level is always configured; see below) |

The two ladders are not merged, for a reason that outlives both: their
rungs are answers to questions that do not compose. `attested` is "the
medium's own stored checksum matches ours" — a statement about a remote
object store's metadata, which a repository snapshot has no analogue of.
`restore_drill` is "we restored it and compared" — a statement no single
object placement can make. A merged enum would have to admit rungs that
are meaningless for half of its subjects, and the first surface to render
it would have to branch on the subject anyway.

What they DO share is the discipline, and it is stated once for both: **a
rung is something achieved, never something intended.** FR-31 spells that
as an absent class for an unverified copy rather than a weakest rung, and
`api/v1` refuses the empty string so there is no way to name a rung that
means nothing.

#### 4b. Configured versus achieved

`verification_level` on a backup set is a **configured** value: what the
operator asked for. It is always present, because an omitted key resolves
to `structural`.

The level a snapshot has actually **achieved** is a different fact, and
#783 needs both — they are what "the policy asks for `content_full` and
the last three nights only managed `structural`" is made of. This ADR
fixes three things about that, so the distinction cannot be lost in the
one place it matters:

1. **Nothing may write a configured level into a place that means
   achieved.** A snapshot row carrying `content_full` because the config
   said so is this product reporting an unperformed check as a result.
2. **An achieved level is absent until something achieves it**, following
   FR-31's rule for the class rather than inventing a second convention:
   there is no "achieved: structural" for a snapshot nobody has looked at.
3. **The comparison is `AtLeast`, in this direction only:**
   `achieved.AtLeast(configured)` answers "is the policy met". The reverse
   reads as met whenever the configuration is weak, which is how an
   unverified snapshot comes to be reported as compliant.

The field names #783 introduces are its own; what is fixed here is that
there are two of them and which one a report may call "verified".

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

**The engine is resolved before the keys whose legality depends on it.**
That ordering is the whole of the next rule: which keys a backup set may
carry is a question about its engine, so a validator that checked the keys
first could only check them for one engine.

Keys a set's engine cannot act on are **refused**, not ignored, in **both**
directions. That is this package's existing rule for sftp fields on a local
remote and for an `attested` upload verification no medium can achieve: a
configuration the build validates and can never execute is worse than one
it refuses, because the operator who wrote the key believes something about
how their backup runs and `backupd check` told them it was fine.

| Refused on an **artifact** set | Refused on an **incremental** set |
| --- | --- |
| `uuid`, `repository_domain`, `source_consistency`, `verification_level`, `source_mount_prefix` | `local_path`, `include`, `completion.*`, `validation.*`, `revalidation.*` |

The right-hand column is one sentence: every key in it describes a moment
in the ARTIFACT lifecycle. A finished file appears on a remote, is
recognised as complete, is copied to `local_path`, is validated there, and
is revalidated later. An incremental set has no such file — its engine
reads a source tree and writes content into a repository — so a completion
strategy names an event that never happens and a validation hash would be
taken over a local copy nothing makes.

`local_path` on an incremental set was the one real decision in that list,
because "engine scratch root" is an available reading. It is **refused**.
#783's lifecycle streams content from the source into the repository and
has no local staging step, so the key would name a directory nothing
writes; and a repository's cache location is a property of the repository,
not of one backup set that stores snapshots in it, so it would be the wrong
home for it even once there is something to configure. If a scratch root is
ever needed it arrives as its own key, which is a schema addition rather
than the silent re-pointing of a path an operator already wrote.

What refusing keys does NOT do is stop a set running. `Validate` accepts
`engine: kopia` — it has to, or the seam #781–#784 are built against could
not exist — so the guarantee that an incremental set is never run through
the artifact pipeline is held where a run actually happens:
`core/internal/app`'s cycle refuses any set whose resolved engine
`UsesRepository()` before reconcile, discovery, copy or delete
(`ErrEngineNotImplemented`). That placement is deliberate. A refusal in
`Validate` would be a daemon that will not boot for a config this build
says is valid; a refusal in the cycle is a set that is reported as
unrunnable while every artifact set beside it keeps working, and it is the
last point before anything reads or writes the set's remote.

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
- **A set this build cannot run is refused at run time, not at load
  time.** A config naming `engine: kopia` boots, `backupd check` passes,
  and the cycle reports that one set as unrunnable while every artifact set
  beside it keeps working. The source of an incremental set is never read,
  copied or deleted by this build.
- **An incremental set's configuration is smaller than an artifact set's,
  not larger.** It carries no `local_path`, `include`, `completion`,
  `validation` or `revalidation`. A deployment that had been writing those
  onto an incremental set — which only this issue's own fixture could have
  done, since the key never validated before it — has to remove them.
- **Three verification vocabularies now exist and none of them is a
  synonym.** ADR 0009's backup-time `VerificationMode`, FR-31's per-copy
  `verification_class`, and this ADR's per-snapshot `verification_level`.
  §4a and §4b state how the last two relate and what #783 may call
  "verified"; the cost of three names is accepted for the same reason as
  two, and the alternative is one word that means a different thing
  depending on which surface is rendering it.

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

**A merged verification ladder** spanning placements and snapshots, so
there is one enum and one column. Rejected in §4a: `attested` is a
statement about an object store's own metadata and `restore_drill` is a
statement about a restore, so a merged enum admits rungs that are
meaningless for half its subjects and every renderer branches on the
subject anyway.

**`local_path` on an incremental set, read as the engine's scratch root.**
Rejected in §5. There is no staging step to point it at, and a
repository's cache location is the repository's property rather than one
co-tenant set's.

**A neutral engine value** (`incremental`, `snapshot`) instead of `kopia`.
Rejected in §1: it would hide, in the cheapest possible place to be
honest, which repository format an operator may have to open with the
vendor's own tools during a recovery.
