# ADR 0005: A destination backend is its own decision, independent of a source backend

## Status

Accepted and implemented (, /). Extends FR-4's
"each backend is an architecture decision, not an import line" rather
than superseding it. It is the ADR `core/internal/backend/doc.go` asked a
future reviewer for in its "What was genuinely weakened" section.

## Context

FR-4's toll on a new rclone backend is a blank import in
`core/internal/transport/rclone/adapter.go`, a name in
`RequiredBackends`, and a measured binary-size delta recorded beside it.
That toll was designed when a backend meant one thing: somewhere this
product READS backup artifacts from.

 split that in two. A storage destination is now declared by a
manifest under `core/internal/backend/bundled/`, and a manifest's
`rclone_backend` has to be in `backend.SupportedRcloneBackends`, which is
a deliberately narrower list than `RequiredBackends`. also wrote
down what that arrangement weakens, in so many words: sftp was already
linked, already registered and already dialed, because a backup SOURCE is
read over it. So offering SFTP as a DESTINATION costs a JSON file and one
map entry. No import, no dependency, no binary-size delta to measure,
because every one of those was already paid for a different reason.

 named the gap and left it open, with `SupportedRcloneBackends` at
`{local, s3}` and one test
(`TestSupportedRcloneBackendsIsNarrowerThanRequiredBackends`) refusing a
list that had become equal to `RequiredBackends`, as a proxy for
"somebody reused the source list instead of writing this one".

 is the diff that walks through the gap. It registers sftp as a
destination: `bundled/sftp.json`, `sftp` in `SupportedRcloneBackends`,
and a third `Role`.

## Decision

**A destination backend is its own architecture decision, taken
separately from the source backend that happens to share its rclone
implementation.** Concretely:

1. `backend.SupportedRcloneBackends` is that decision, written out as a
   map literal in `core/internal/backend/registry.go`, and it is pinned
   name for name by `TestSupportedRcloneBackendsIsItsOwnReviewedList` in
   `core/internal/transport/rclone`. A fourth entry fails a named test,
   whether or not the backend behind it was already linked. That test
   replaces 's equality proxy, which expired the moment a source
   backend was legitimately promoted and the two lists coincided.
2. The toll for a destination is NOT a binary-size measurement, because
   for an already-linked backend there is nothing to measure. It is this
   instead, and all of it is reviewable diff:
   - a bundled manifest declaring the fields an operator fills in and
     which of `mediumcheck`'s steps apply;
   - an entry in `SupportedRcloneBackends`;
   - a change to `TestTheBundledSetIsExactly`, which enumerates the
     shipped ids;
   - a `Role` this engine can dispatch on, or the honest refusal that
     comes of not having one yet (see below).
3. Registering a destination does NOT mean the engine can write to it
   yet. `Role` stays closed in Go, and `internal/app`'s `mediumType`
   dispatches on it; a role no `transport.MediumType` answers refuses at
   the moment a destination is about to be reached. adds
   `RoleRemoteFilesystem` and no dialer, so the layers say so in order:
   `config.expressibleBackendIDs` does not accept `type: sftp` until
   `StorageMedium` grows the fields the manifest requires,
   `service.storageMediumFromFields` refuses a field it cannot store
   BY NAME, and `mediumType` refuses a role it cannot dial. A registry
   that knows about a backend before the schema can express one is the
   state `expressibleBackendIDs`' own doc describes, not a bug.

## Consequences

### What we get

- An operator looking for SFTP finds it in the add-a-destination picker,
  with the fields an SSH destination actually needs and the probe steps
  that would prove one, instead of a "understood, not registered" row
  that names a protocol and nothing else. The row is disabled and says
  why (see the `configurable` flag below); what changed is that the
  answer on the screen is the real shape rather than a transport name.
- The next backend already linked for a source (there are none today, and
  `RequiredBackends` is the list to watch) is a reviewed decision with a
  named test in front of it rather than a judgement call in a comment.
- `host_key` verification is not optional on a destination: `known_hosts`
  is a REQUIRED field of `bundled/sftp.json`, which is FR-6 applied to
  the write path the same way `transport/rclone/ssh.go` already applies
  it to the read path.

### What it costs, honestly

- `BackendCatalog.Unregistered` is now empty in this build: it is
  `RequiredBackends` minus what a manifest claims, and sftp was the last
  name on it. The disclosure surface stays wired (it is a subtraction,
  not a list), and the test that covers it now passes vacuously until a
  fourth transport is linked. That is the right shape but it is worth
  knowing when reading that test.
- Role is a third member of a closed enum, so
  `api/v1/openapi.json`'s `role` enum, both generated bindings and
  `ui/shared`'s `BackendRole` union move with it. That is the
  compiler-enforced blast radius promised; it is a real cost and it
  is paid once per role, not once per backend.
- An `sftp` destination cannot be declared in `config.yaml` or saved
  through the wizard yet, because neither `config.StorageMedium` nor
  `core/service`'s field table carries `host`, `port`, `user` or
  `known_hosts`. Every layer refuses by name rather than silently
  dropping a field. Growing those two tables (and a `MediumStore` that
  dials it) is the follow-on work this ADR deliberately does not do,
  because doing it would make the FR-4 question above impossible to
  review on its own. It is tracked in
- Because of that, the manifest declares `"configurable": false`, which
  is served on `/api/v1/backends` and rendered as a disabled row. A
  registered backend a surface offers and no layer can save is a dead
  end an operator walks the whole length of — a name, eight values and
  an SSH key — before the schema refuses it. The flag is what makes the
  catalogue TRUTHFUL rather than merely complete, and it defaults to
  true, so it is a claim a preview backend makes and never one an
  ordinary manifest has to remember to make.
- The `host` pattern and the `path` / `known_hosts` kinds are therefore
  unreachable in this build, and they are left as they are rather than
  refined against a dialer that does not exist. Both carry a
  `TODO` where they are pinned (`core/internal/backend/
  sftp_test.go`): a host rule belongs next to the code that resolves
  one, and whether a directory on a far host is the same KIND as a file
  on this one is a question answers with a dialer in hand.

## Alternatives considered

### Leave `SupportedRcloneBackends` at `{local, s3}`

The status quo chose. It keeps the decision unmade, which is
defensible exactly once: the second time somebody asks for SFTP as a
destination, "we already ship the code, we have not decided to let you
use it" is not an answer, it is a deferral wearing one.

### Model sftp under `RoleLocalVolume`

Cheapest possible diff: no Go change, no contract change, no ADR. It is
also the one genuinely dangerous option in the set.
`RoleLocalVolume` resolves to `transport.MediumTypeLocalDir`, so every
"offsite" copy would be written to a directory on the NAS itself, and an
operator who declared a second machine would be told the connection test
passed. A role is what a backend IS to this engine; lying about it to
avoid a Go change buys a silent single point of failure for the exact
data this product exists to protect.

### Make FR-4's toll apply per (backend, direction) pair

The thorough version: `RequiredBackends` becomes a list of pairs, each
recording whether it is registered to read, to write, or both. It is
strictly more precise and it is more machinery than the fact justifies.
The set has three names in it. `SupportedRcloneBackends` IS the write
half of that pair list, expressed as the subset it already is, and the
subset check (`TestEveryBundledManifestNamesABackendThisBinaryRegisters`)
is what keeps the two halves in agreement.
