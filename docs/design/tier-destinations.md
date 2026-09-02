# Tier destinations: the seam, and the decisions it encodes

Issue #334 asks for retention tiers that can send artifacts somewhere
other than the local backup root, so twelve monthly copies can live on
object storage instead of NAS disk.

This document records the decisions that shape how that gets built. The
seam in `core/internal/artifactstore` is in place; nothing moves yet, and
retention still has exactly one verb, delete. A future adapter author
should read this before adding a backend, because several of the choices
below are load-bearing and are easier to keep than to rediscover.

## What exists now

`core/internal/artifactstore` defines `Store`, and `Local` implements it
over the backup root this product has always written to. The one formula
for where a committed artifact sits now lives there, and both
`lifecycle.finalPath` and `retention.pruneFinalPath` delegate to it.

Those two previously each carried a comment naming the other as the only
other place in the project allowed to compute that join. Two guarded
copies was the right answer while there was nowhere better to put it;
a store is that better place, because knowing where its own bytes go is
what a store is for.

No behaviour changed. The lifecycle and retention suites pass unmodified,
which is the point: an existing deployment computes exactly the same
paths it did before.

## Decision: the unit is an artifact, not a file

Every method is addressed by (backup set, artifact), never by a path the
caller composed. A path is a local-filesystem detail. An object store has
keys, no parent directories and no symlinks; handing the seam a path
would mean every caller had already assumed a filesystem, which is the
assumption #334 exists to remove.

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

`TestStoreHasNoMoveMethod` exists to make that deliberate rather than
incidental.

## Decision: safety proofs belong to the implementation, not the interface

FR-20's six checks before a local delete (canonicalize, prove containment
under the configured root, refuse a symlink, refuse a `.partial`, confirm
no tier selects it, confirm it is not last-known-good) are local
filesystem semantics. Containment and symlinks mean nothing to an object
store, and a path-traversal check against a key namespace is a different
question wearing the same words.

Hoisting them into the interface would produce methods a non-local
adapter could only stub, and a stubbed safety check is worse than an
absent one because it reads as satisfied.

So the interface states the obligation, `Remove` deletes exactly the
artifact named or refuses and says which check refused it, and leaves the
proof to the implementation. FR-20's checks stay in
`internal/retention`, re-derived from the artifact's own record
immediately before the delete, exactly as before.

A future adapter owes an equivalent proof in its own terms. It does not
owe these six.

## Decision: no `destination:` config key yet

The obvious next step looks like adding an optional `destination` to
`RetentionTier`. This deliberately does not.

Such a key could currently only ever say "local", and selecting the sole
existing behaviour is not a choice. It would appear in the schema, need
documenting, and do nothing. This repository has already had that
problem and named it: #299 removed several Settings and wizard fields
that were decorative, drawn by the UI and read by nothing. Adding a
config knob with one legal value would be the same mistake in the same
file.

The key arrives with the first backend that gives it a second value, in
the same change, so it is never shipped inert.

When it does arrive, it should follow `Retention.EffectiveTiers`: leave
the zero value zero in the parsed config so "not configured" stays
distinguishable from "configured to the default", and resolve through an
`Effective...` accessor. Absent means the local backup root.

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

## What a future adapter still needs

Beyond implementing `Store`:

- **Credentials handled like the SSH key.** Never in the repository,
  never in a log, never in the config file's tracked form. See how
  `Key`/`KeyEncryption` are resolved for the pattern to copy.
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
