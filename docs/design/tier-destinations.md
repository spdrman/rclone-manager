# Tier destinations: the seam, and the decisions it encodes

Issue #334 asks for retention tiers that can send artifacts somewhere
other than the local backup root, so twelve monthly copies can live on
object storage instead of NAS disk.

This document records the decisions that shape how that gets built. The
seam in `core/internal/artifactstore` is in place; nothing moves yet, and
retention still has exactly one verb, delete. A future adapter author
should read this before adding a backend, because several of the choices
below are load-bearing and are easier to keep than to rediscover.

## What exists now, and what is not wired up

`core/internal/artifactstore` defines `Store`, and `Local` implements it
over the backup root this product has always written to. The formula for
where a committed artifact sits now lives there, and both
`lifecycle.finalPath` and `retention.pruneFinalPath` delegate to it.

Those two previously each carried a comment naming the other as the only
other place in the project allowed to compute that join. Two guarded
copies was the right answer while there was nowhere better to put it;
a store is that better place, because knowing where its own bytes go is
what a store is for.

Those two, not every join of a root and an artifact name.
`retention.pruneVerifySafeToDelete` still finishes by joining the
artifact's name onto the root `filepath.EvalSymlinks` handed back. Same
shape, different question: that line's job is to produce the resolved,
symlink-free path the safety checks just proved, and routing it back
through the configured root would throw that resolution away. It stays
where it is and it fails closed.

When this document was written, the interface itself had **no production
caller at all**, and it said so bluntly because everything below reads
like a description of a live seam. The only production use was the
package-level function `artifactstore.LocalLocator`, from
`lifecycle/transfer.go` and `retention/prune.go`, and both of those held
a directory string rather than a `Store`, so both bypassed `Store`
entirely. The conversion was named as relocated rather than reduced:
those two call sites, resolving a `Store` for the backup set and asking
it instead of composing a path out of `LocalPath`.

**#235 did that conversion.** Both call sites now build a `Local` and ask
it, and `LocalLocator` is unexported, because an exported way to compute
an artifact's location without going through a store, once nothing needs
one, is a standing invitation to bypass the seam. `NewLocal`, `Locator`,
`Kind` and `KindLocal` have real callers now.

`Stat`, `Open`, `Put` and `Remove` still do not, and that is still
deliberate. FR-12's commit path writes its own `.partial` and hard-links
it because it carries crash-safety obligations `Put` does not reproduce,
and retention deletes through its own FR-20 discipline. The mover that
uses those four is #238.

The conversion did change behaviour, in the one direction #334 predicted:
`finalPath` and `pruneFinalPath` now return an error, because `NewLocal`
refuses an empty root and `Locator` refuses an artifact it cannot
address. Before, `finalPath("", artifact)` returned the artifact's bare
name, which is a path relative to whatever directory the daemon started
in. `config.Validate` refuses an empty `local_path`, so nothing that got
as far as running a cycle could reach it; that is why deferring the
change was safe and why leaving it was not. On the prune path the failure
becomes `PruneRefuse` rather than `PruneKeep`, because those are
different claims: KEEP asserts a tier selected the artifact, and a refusal
asserts nothing was decided at all.

## Decision: the unit is an artifact, not a file

Every method is addressed by the artifact, never by a path the caller
composed. A path is a local-filesystem detail. An object store has keys,
no parent directories and no symlinks; handing the seam a path would mean
every caller had already assumed a filesystem, which is the assumption
#334 exists to remove.

The backup set half of that pair arrives through the constructor instead
of through every call, because a store's configuration is a fact about
the store rather than about each operation. See the per-set decision
below.

`Locator` inverts it: ask the store where the artifact belongs, and get
back a string only that store interprets. `Kind` travels with it so a
locator is never handed to the wrong store.

## Decision: there is no Move, and that is the failure model

This is the decision everything else rests on.

A move is a put followed by a remove. Offering `Move` as one primitive
would push the ordering of those two steps into each implementation,
where review cannot see it and where every adapter author chooses it
again. One of those choices, remove-then-put, loses the artifact if the
process dies in between. A backup product that loses the backup has
failed at the only thing it does.

So the seam offers the pieces and no `Move`. A mover, when written,
composes them in one auditable place, in one order:

1. `Put` the bytes at the destination.
2. `Stat` the destination and confirm it holds them.
3. Only then `Remove` at the origin.

That answers #334's failure-model question directly:

- **The origin copy is the one guaranteed intact.** It is not removed
  until the destination copy is independently confirmed present.
- **An interrupted move leaves one copy or two, never zero,** and never
  only an unconfirmed one.
- **Two copies is wasteful, not dangerous, and self-correcting.** The
  next run finds the destination copy already there and completes the
  remove it did not reach.

Nothing enforces this ordering yet, because nothing moves yet. What the
seam does is make the dangerous ordering require *adding a method*
rather than merely calling two existing ones in the wrong sequence.

`TestSeamOffersNoMoveMethod` exists to make that deliberate rather than
incidental. It reflects over the `Store` interface and over `Local`, and
matches on the method *name*, because the decision is about the concept
and not about one signature: an earlier version pinned one exact
signature and an artifact-addressed `Move`, the shape this package's own
doc says operations should have, went straight past it.

`TestLifecycleUsesOnlyTheSharedFormulaFromThisPackage` guards the half a
missing method cannot. The FR-12 commit path must keep writing its own
`.partial` and hard-linking it, because `Put` does not reproduce that
path's crash-safety obligations; an absent `Move` takes someone writing
code to defeat, but a present `Put` takes someone calling it. So
`lifecycle` may reference exactly one symbol from this package, the
shared join, and the test parses `internal/lifecycle` and fails on
anything else.

## Decision: `Put` refuses an occupied locator, and fsyncs the directory

`Put` is the only operation in the package that can destroy an artifact's
bytes, and the no-`Move` argument above does not protect it. Adding
`Move` takes someone writing code; overwriting takes someone calling the
method that is already there, and a rename over a live artifact is not
recoverable.

So `Put` writes through a temp file in the same directory and **hard
links** it into place rather than renaming. `os.Rename` replaces whatever
is at the destination; `os.Link` fails with `EEXIST`, reported as
`ErrAlreadyPresent`. That is the same choice `lifecycle`'s
`linkWithoutClobbering` makes for FR-12's commit, for the same reason. A
destination that already holds something different is a case a person
decides about, and it is also the resumable case: the mover confirms by
`Stat` and finishes the remove the previous run did not reach.

`Put` then **fsyncs the containing directory**. The "never zero copies"
claim is about power loss, not just about a killed process, so it needs
the destination's *name* to be durable and not merely its content. A
directory is a separate inode with its own writeback state; skip the
fsync and a crash after the origin's remove reaches disk and before the
destination's directory entry does leaves zero copies, which is exactly
the outcome the failure model says is impossible. `commit.go`'s FR-14
treatment is the long version of the same argument, and this is the third
copy of `fsyncDir` in the repository. Consolidating it means moving
`commit.go`'s durability primitive, which belongs in a change about that
path rather than in a refactor claiming no behaviour change.

The interface says all three obligations out loud, because the obvious
streaming `Put` (open the destination, copy into it) satisfies "the bytes
end up there" and breaks the model: the mover `Stat`s a truncated object,
confirms it, and removes the origin.

## Decision: a store is built for one backup set, and never sees the config struct

`Store` methods do not take a `config.BackupSet`. An S3 adapter handed
one would receive the validation command, the SSH key path and the
schedule, while the only field it could act on, `LocalPath`, is
local-specific, and there is no field at all for where a non-local
store's bytes go.

Instead a store takes what it needs at construction: `NewLocal(root)`
takes the backup set's configured `local_path`, and a second backend
takes its bucket, prefix and credentials the same way. That is the
constructor convention to copy. It also settles whether an implementation
should hold per-set state: yes, that is what the value is for.

The backup set is not passed to the methods either, because
`model.ArtifactID` already carries its own `BackupSetID`, and a second
source of truth for the same fact is a mismatch someone has to guard.

There is a second reason to keep the struct out. `config.BackupSet`
carries fields with two different lifecycles and nothing distinguishes
them: `LocalPath` is meaningful straight out of YAML, while `ID` and
`ReadOnly` are the zero value until `Validate` has run. A store that
never sees the struct cannot read the wrong half of it. `NewLocal` takes
the raw path, and `Locator`'s doc says it reads no configuration at all.

## Decision: no enumeration

There is no `List`. The catalog, not a scan, is this product's answer to
"which artifacts exist and where": config describes intent, and a scan
can only ever see one backend at a time, so an enumerate method is how a
second, disagreeing inventory gets built by accident.

The obvious need is a reconciler hunting for an orphaned destination copy
after an interrupted move, and it does not need one: the catalog knows
which locator the move was writing, so the question is a `Stat` of one
locator. If a real need turns up later it should arrive with the argument
for why the catalog cannot answer it.

## Decision: safety proofs belong to the caller, not the interface

FR-20's six checks before a local delete (canonicalize, prove containment
under the configured root, refuse a symlink, refuse a `.partial`, confirm
no tier selects it, confirm it is not last-known-good) are local
filesystem semantics. Containment and symlinks mean nothing to an object
store, and a path-traversal check against a key namespace is a different
question wearing the same words.

Hoisting them into the interface would produce methods a non-local
adapter could only stub, and a stubbed safety check is worse than an
absent one because it reads as satisfied.

So the interface states the obligation and leaves the proof to the
caller. `Remove` deletes exactly the artifact named and nothing else,
follows no symlink to a different object, and never widens the target;
what it does *not* do is prove that this particular artifact is safe to
delete. That proof stays in `internal/retention`, re-derived from the
artifact's own record immediately before the delete, exactly as before,
and `Local.Remove` is a bare `os.Remove` for exactly that reason.

`Store.Remove`'s doc says that plainly now. The first version said it
"refuses and returns an error naming the check that refused it", which
the only implementation does not do and was never going to, and an
adapter author reading the method rather than the package doc a hundred
lines above would reasonably have put checks inside their own `Remove`.

A future adapter owes an equivalent proof in its own terms, on the caller
side. It does not owe these six.

## Decision: no `destination:` config key yet, SUPERSEDED by #234

The original decision, kept because the reasoning still binds: adding an
optional `destination` to `RetentionTier` was refused, because such a key
could only ever have said "local", and selecting the sole existing
behaviour is not a choice. It would have appeared in the schema, needed
documenting, and done nothing. #299 removed several Settings and wizard
fields that were exactly that, decorative and read by nothing. The key
was to arrive with something that gave it a second value.

EPIC E's #234 is that arrival, and the key is spelled `medium` rather
than `destination`. What gives it a second value is the new top-level
`storage_mediums` list: a tier's `medium` now names a declared S3 medium,
which validation resolves and refuses when it dangles. So the key is not
a knob with one legal value.

It IS still ahead of anything that acts on it, and that is worth stating
plainly rather than hiding behind the paragraph above. Phase 1 of EPIC E
builds every load-bearing wall (schema, transport, placements,
verification) and can still not move or delete anything it could not
before; the planner that reads a tier's medium is #239 and the mover it
feeds is #238. The difference from the #299 fields is that these have a
scheduled reader rather than none, and the phasing is the EPIC's own
decision, made so the schema is argued in review before a mover is
depending on it.

Two shapes this decision asked for, both kept:

- The zero value stays zero in the parsed config, so "not configured"
  stays distinguishable from "configured to the default", and resolution
  happens through accessors: `RetentionTier.EffectiveMedium`,
  `StorageMedium.EffectiveStorageClass`,
  `StorageMedium.EffectiveUploadVerification`. Absent means the local
  backup root. Nothing is written back into the struct, because a default
  Validate resolves in place is a default the next settings save freezes
  into the operator's own file (#294).
- Absent is the ONLY spelling of local. A tier writing `medium: local` is
  refused, and `local` is reserved as a medium id too. That is what makes
  FR-35's round-trip rule structural: with local unspellable, the only
  way a `medium:` key reaches a config file is an operator opting into a
  real destination, so no settings form can inject one into a file that
  never configured any. The same `omitempty` treatment `tiers` already
  carries covers the empty-submission case.

## Decision: the catalog will own location, and does not yet

#334 is right that the catalog, not the config and not a filesystem
scan, has to be the source of truth for where an artifact currently is.
Config describes intent, and intent is what an interrupted move differs
from; a scan can only see one backend at a time.

`state.Record` therefore needs to carry the store kind and locator for
each artifact. That is a schema change with a migration, and it is not
made here, because nothing yet writes a non-local location. A nullable
column no writer populates and no reader consults is a migration that
buys nothing and still has to be maintained.

It lands with the mover. Until then, an artifact's location is
`KindLocal` by construction, which is exactly what every existing
deployment means.

## What #235 built on top of this, and what it decided differently

E1.3 added the second place an artifact's bytes can live, and it did
**not** do it by writing a second `artifactstore.Store`. That is worth
recording, because it looks like the obvious move and it is the wrong
one.

The S3 medium is `transport.MediumStore`, a separate manager-owned
interface beside `transport.Transport`, implemented by the rclone
adapter. The two seams sit at different altitudes on purpose:
`artifactstore.Store` is per-backup-set, artifact-addressed, and its
configuration arrives at construction; `MediumStore` is key-addressed and
takes the medium on every call, exactly as `Transport` takes a `Source`,
which keeps the adapter stateless and keeps "which medium" visible at
every call site. That matters most for `DeleteObject`: a delete whose
destination is implicit in a value built somewhere else is a delete
nobody reviewing the call can check.

A `Store` implemented over a medium would additionally have to fake
`Put`'s `io.Reader` contract, because `MediumStore.UploadFromLocal` takes
a path (the medium needs the size before it starts, and a reader that
fails halfway is a class of partial upload a file does not have).
Satisfying it would mean spooling a whole artifact through a temporary
file to cross an interface, which is a real operational cost hidden
behind an abstraction nothing yet needs: the move engine (#238) holds a
local path by definition and uses both seams directly. When "resolve a
store from a placement record" becomes a real requirement, that is #236,
and it can add one with a caller attached.

Two findings from building it that a future adapter author should have:

- **An absent capability must be reported, not approximated.** rclone's
  s3 backend reports MD5 and nothing else, and that MD5 is the object's
  ETag, which FR-32 says is never a content hash. So FR-31's `attested`
  verification class is not reachable through the embedded rclone at
  v1.75, and `ObjectChecksum` returns an explicit capability error rather
  than an ETag wearing an attestation's name. Read-back is the only
  honest verification against S3 today.
- **Ask what the error translation throws away.** rclone turns a 404 into
  its own filesystem-shaped sentinels, which flattens "this key is
  absent" and "this bucket does not exist" into one answer. Measured
  against a real MinIO, that made a delete against a mistyped bucket
  report success and a listing report an empty medium. A backend author
  should assume the wrapper's error vocabulary is lossier than the
  protocol's and check.

## What a future adapter still needs

Beyond implementing `Store`:

- **Credentials handled like the SSH key.** Never in the repository,
  never in a log, never in the config file's tracked form. See how
  `Key`/`KeyEncryption` are resolved for the pattern to copy, and see
  `transport/rclone/credentials.go`, which #235 built by reusing that
  machinery outright rather than standing up a second copy: the command
  runner is literally the same function.
- **Identity on the far side.** The lifecycle already refuses a remote
  delete it cannot confirm, reporting `verdict=unconfirmed,
  confidence=weak` when size and mtime agree but no hash or backend
  stable id is available. An artifact that has moved must be held to the
  same standard when it is later restored, revalidated or deleted, not a
  weaker one because it is further away. `Stat` carries optional
  `Hash`/`HashAlg` for backends that can answer without streaming the
  object; a backend that cannot should leave them empty rather than
  reading the whole object behind the caller's back.
- **Restore, revalidate and delete reaching a non-local artifact.**
  These read through `Open` and `Remove` once the catalog can tell them
  which store to ask.
- **A reachability failure that refuses rather than silently retaining
  locally.** The operator configured a destination because local is not
  where those bytes should be; a quiet fallback to local is a policy
  the operator did not ask for, reported as success.
