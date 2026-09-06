# Storage mediums: configuring them, and what they cost you

An artifact's durable copy has always lived in exactly one place, the backup
set's `local_path` on the NAS. A **storage medium** is a named destination it can
live on instead, chosen per retention tier: daily on local disk so a recent
restore is a filesystem read, monthly on S3, annual on a colder S3 class.

`docs/EPIC-E-alternative-storage.md` is the specification. This is the operator's
half of it: what goes in the config file, what the manager will do with it, what
each verification class actually proves, and what an archive class means for the
day you need the file back.

## Read this first: what works today

The EPIC landed in two phases, and both are in this build. This section is the
honest state of it, and it is here at the top rather than in a footnote because
a configuration reference for behavior that does not exist yet is worse than no
reference at all. It is worth keeping for the one row that is still qualified:
an archive class is not a place a tier can deliver to.

| What | State |
| --- | --- |
| `storage_mediums` and a tier's `medium` key are accepted, validated and round-tripped by a settings save | Landed |
| Credentials are resolved from a file, an environment variable or a command | Landed (#235) |
| Artifacts are recorded as living somewhere, and the recovery manifest says where | Landed (#236): migration 0007 and the manifest's `placements` |
| Verification classes, and revalidation that knows about mediums | Landed (#237) |
| Artifacts actually MOVE between mediums when a tier says so | Landed, including a chain with two medium tiers |
| Retention plans, previews and prune understand mediums | Landed, including across the HTTP boundary: the preview carries every move with both mediums, and every deletion with the medium it happens on (#430) |
| The API and the UI show placements, access states and the disclosure | Landed |
| A medium can be proved to work before a cycle carries a real backup to it | Landed (#443): `backup-manager medium preflight`, and a button on the settings form |
| Archive storage classes and the explicit restore operation | Landed as far as the vocabulary and the operation go; a tier ON an archive class is refused when the config loads, see below |

One limit is worth knowing before you write a chain, and it is the manager
refusing to do something rather than doing it badly:

- **A tier whose medium names `GLACIER` or `DEEP_ARCHIVE` cannot take
  delivery of an artifact, and the config will not load** (#428, #442). The
  manager will not delete a copy against a destination it could not read
  back, an archived object cannot be read back, and there is no
  `upload_verification` mode that says "existence is enough". So the pairing
  is refused where you write it, with a message naming the tier, the medium,
  the class and what to write instead, rather than accepted at load and
  refused silently once per cycle for ever.

  The refusal is about the PAIRING, not about the medium. Declaring an
  archive-class medium that no tier delivers to is legal and is what you
  want if you already have objects on `DEEP_ARCHIVE`: the manager can see
  them and restore them, and nothing is going to be written there.

A chain with two medium tiers, which is the shape this document opens with
(daily local, monthly `s3`, annual on a colder readable class), works end to
end (#429). The second hop is a move from one medium to another, and the
manager does it by reading the artifact down to a `.moves` directory under
the backup set's own `local_path`, checking that what arrived hashes to what
it recorded at ingestion, uploading that, and removing it. So a chain like
this needs room on the NAS for the largest artifact that will ever hop
between two mediums, transiently, even though nothing is stored there
permanently. A hop that will not fit is refused before anything is
downloaded, and the copy it would have moved stays exactly where it is.

Every reason a move does not happen is visible without reading logs. A cycle in which artifacts were due to move and none
arrived says so on the `Last run cycle` panel, in the operation record the
activity feed reads, in the FR-23 event stream under `op=move`, and in
`backup-manager run`'s exit status, which becomes 1 with the engine's own reason
on stderr. A deployment that declares no storage medium attempts no moves, so
none of that can fire for it.

## The configuration

Mediums are declared once at the top level, and referenced by name from the
retention chain.

```yaml
storage_mediums:
  - id: offsite_s3
    type: s3
    region: us-east-1
    endpoint: ""                  # empty means the AWS endpoint for the region
    bucket: nas-backups
    prefix: rclone-manager        # key namespace inside the bucket
    storage_class: STANDARD
    upload_verification: readback # readback (default) or attested; see below
    credentials:
      file: /var/lib/backup-manager/s3/offsite_s3.creds

retention:
  timezone: UTC
  week_starts_on: monday
  tiers:
    - name: daily
      granularity: day
      keep: 7                    # no medium key: local disk, exactly as today
    - name: monthly
      granularity: month
      keep: 12
      medium: offsite_s3
```

### The fields

| Field | Rule |
| --- | --- |
| `id` | lower_snake_case, unique, and never `local`. `local` is the implicit medium every deployment already has, which is the backup set's own `local_path`. |
| `type` | `s3` is the only value. Any endpoint speaking the S3 API works, MinIO and Wasabi included, because it is the same rclone backend. |
| `region` / `endpoint` | An empty `endpoint` means the AWS endpoint for `region`. A non-AWS endpoint goes in `endpoint`. |
| `bucket` | Required. A medium with no bucket names no destination. |
| `prefix` | The key namespace inside the bucket. Keys are `<prefix>/<source>/<set>/<artifact-name>`, deterministic and with no timestamp or random part, so re-running an interrupted upload targets the same key. |
| `storage_class` | One of `STANDARD`, `STANDARD_IA`, `ONEZONE_IA`, `INTELLIGENT_TIERING`, `GLACIER_IR`, `GLACIER`, `DEEP_ARCHIVE`. |
| `upload_verification` | `readback` (the default) or `attested`. Read the verification ladder below before changing it. |
| `credentials` | Exactly one of `file`, `env` or `command`. There is no field for a literal key. |

### Which medium an artifact lives on

The **first tier in chain order that currently selects the artifact** names its
home. That gives chain order a second meaning it did not have before: order still
never changes WHICH artifacts are kept, because KEEP is the union of every tier's
selections, but it now decides WHERE a multiply-selected artifact lives.

Write chains fine to coarse, daily then weekly then monthly then annual, and the
first selecting tier is the warmest one, which is the behavior the
daily-local-monthly-offsite story wants.

When no tier selects an artifact at all, it stays where it is. An artifact kept
only by last-known-good protection, or one waiting to be deleted, is never moved
by the absence of a selecting tier.

### Credentials

Credentials follow the same custody model as SSH keys, and for the same reason:
the config file is read by people, copied into support tickets, and rewritten by
the settings API.

- `credentials.file` is preferred. It is an AWS shared-credentials format file
  that rclone reads itself, so the secret never enters the manager's memory.
- `credentials.env` names an environment variable.
- `credentials.command` is an argv array, never a shell string, run with a
  bounded timeout and a minimal environment.

Exactly one of the three per medium. There is **no schema field for a literal
key**: writing `access_key_id:` or `secret_access_key:` in the config is an
unknown field, and the config loader refuses the whole file before validation
even runs. That refusal is deliberate and is not going to be softened.

Credential files belong under private state (`/var/lib/backup-manager`), never
under the backup root. Nothing that leaves this process carries key material:
not a log line at any level, not an error message, not an API response, not the
redacted config export, not a recovery manifest, and not object metadata in your
bucket.

### Checking a medium before anything depends on it

Nothing in this product touched a bucket until a cycle carrying a real backup
did, which meant the first thing to discover a wrong region, a bucket that is
not there, a credentials file the daemon cannot read, or a policy that denies
`PutObject` was a move, in the middle of a cycle, after an artifact had already
been chosen to leave local disk. There is a preflight now:

```
backup-manager medium preflight offsite_s3
```

and the same check sits behind a button on the settings form, offered at the
moment you point a tier at a medium and before the save that starts sending
backups there. It exits non-zero when any check fails, so it composes into a
deployment script.

It writes. That is the point, and it is worth knowing before you run it against
production: a reachability ping is answered perfectly well by a wrong region, by
a policy that denies writes, and by an endpoint that accepts `storage_class` and
silently ignores it. So the preflight writes a small probe object with the
medium's own storage class, reads it back byte for byte, checks the class the
object actually landed in against the class the configuration claims, asks the
endpoint whether the medium's declared `upload_verification` can actually be
achieved there, and deletes the probe. The probe lives at a randomly named key
under a reserved `.rclone-manager-preflight/` segment inside the medium's own
prefix, which no configured artifact can produce.

Eight checks, and each one names which of them failed and whose problem it is.
The two worth calling out:

- **`credentials` and `reach` are separate answers.** A credential this manager
  could not obtain is a question for this host: a file it cannot read, a variable
  that is not set, a command that did not run. A credential the endpoint rejected
  is a question for your provider. They used to be indistinguishable.
- **`verification` asks the endpoint rather than a table.** A medium declared
  `upload_verification: attested` cannot be served by any s3 endpoint this build
  talks to (see the section on that below), so the preflight reports the refusal
  and tells you to declare `readback`. A preflight that reported attested green
  would be lying about the one thing it exists to establish.

A medium on an archive storage class is refused at the `deliverable` check and
nothing is written at all, because a probe object on `DEEP_ARCHIVE` is billed for
a 180-day minimum for an answer this product already holds. That refusal is about
DELIVERY: declaring an archive-class medium to restore objects that are already
there stays legal.

Nothing in the report ever carries a credential, a path on this host, or the name
of an environment variable. The classified cause goes to the manager's log, where
your diagnostics already are.

Two things it does not prove, which are worth knowing before you read a green
result as a guarantee. The probe lives at its own key under the medium's prefix,
so a bucket policy scoped to the whole prefix is covered and one scoped per
backup set is not. And a bucket **lifecycle rule** can transition objects to an
archive class days after they are written, whichever class you declared and
whichever class the endpoint reported at the moment of the write; nothing
observable at write time tells those buckets apart, so a medium that passes here
can still end up holding objects that need a restore. The manager catches that
when a read is attempted rather than assuming it away.

## The disclosure, and what you are agreeing to

Mapping a tier of a backup-affecting chain to a non-local medium is a
configuration change that the UI and the CLI put in front of you once, before the
first save that does it. What it says is the thing worth understanding:

> Artifacts selected only by that tier will live only on that medium, and the NAS
> copy will be deleted after the upload has been verified.

That is the whole point of the feature and it is also the part that can lose you
data if the medium is not what you think it is. After you acknowledge it, moves
run automatically as declared policy. There is no per-move confirmation, in the
same way there is no per-delete confirmation for the remote-source deletion you
already consented to.

Remapping a tier back to `local` is legal and plans moves in the other direction.
The move engine does not care which way it is going. Your provider will charge
you egress for coming home, and the disclosure says so.

## The verification ladder, and what each rung costs

"Verified" stops being one thing the moment the bytes are not on a disk you can
re-read for free.

| Class | What it proves | What it costs |
| --- | --- | --- |
| `content` (read-back) | The bytes on the medium hash to the SHA-256 the journal recorded | One full download per move: time, plus egress, plus two metadata calls. See below. A move a restart picks up pays for a second download. On an archive class, a restore first. |
| `attested` | The provider's stored full-object checksum equals the recorded SHA-256 | One metadata call, no egress. Trusts the endpoint to implement S3 checksum semantics honestly. |
| `existence` | The object exists with the recorded size | One HEAD request |

The rules that matter:

- A move reaches VERIFIED at `content` class by default. The copy that was just
  uploaded is downloaded back and re-hashed against the SHA-256 recorded when
  the artifact was ingested, while the source copy is still there, and the
  source is deleted only after that verdict is durably recorded.
- **A move at `content` class downloads the object once, and a move a restart
  picks up downloads it again.** The manager will not delete your source copy
  without a content-class verdict about the destination that is valid at that
  instant, and it asks for one unconditionally, immediately before the delete.
  Where that verdict comes from is the whole of the cost:

  - On a move that runs start to finish in one pass, it is the read that reached
    VERIFIED moments earlier, plus two HEAD requests: one taken immediately
    before that read, and one immediately before the delete. If the object's
    size, last-modified time or storage class has moved between the two, or the
    endpoint reports no last-modified time at all, or more than two minutes have
    passed, the manager downloads the object again rather than act on the older
    reading. **One artifact's worth of egress per move** is the number to budget.
  - On a move interrupted by a crash and picked up later, there is no such
    reading to stand on, and there deliberately cannot be one: `VERIFIED` may
    have been written weeks ago, and a bucket lifecycle rule, an expired restore
    window or an overwrite in the meantime is exactly what the check is for. That
    move downloads the object in full, every cycle, until it can finish. **Budget
    a second artifact's worth of egress for any move you see resume.**

  `attested` would avoid the download entirely and does not work on `s3` in this
  build; see below.
- `existence` is never sufficient to delete a source. Not ever, not with any
  setting.
- Periodic revalidation checks medium placements at `existence` class only.
  Stronger re-verification of a medium placement is something you ask for,
  because anything that costs egress must never happen silently. A revalidation
  pass that could only reach `existence` is reported as `existence`, and never as
  the artifact having been "revalidated" in the sense a local artifact is.
- An artifact on `GLACIER` or `DEEP_ARCHIVE` is `existence`-checkable only, until
  an explicit restore makes anything stronger possible.
- **A tier whose medium writes an archive class cannot take delivery of an
  artifact, and the move is refused before anything is uploaded.** Both settings
  `upload_verification` accepts need the object read back, a freshly written
  object on `GLACIER` or `DEEP_ARCHIVE` cannot be read until somebody asks for a
  restore, and nothing asks to restore an object that did not exist a second
  ago. So the answer is knowable from the configuration alone and the manager
  gives it for free, in the cycle report, every cycle, rather than uploading and
  discarding a copy to find out. That matters because `DEEP_ARCHIVE` has a
  180-day minimum billable duration: a copy deleted the second after it lands is
  still charged for six months. Issue #428 tracks what the eventual answer
  should be.

### `upload_verification: attested` and rclone's s3 backend

`attested` trades a download for trust in the endpoint. The trust is real and
worth saying plainly: an endpoint that lies about checksums can cause your local
copy to be deleted against a bad upload.

It also does not work on `s3` in this build, and **the config is now refused at
load rather than at the move**. rclone v1.75.0's s3 backend reports exactly one
hash capability, MD5 (`backend/s3.Fs.Hashes()` returns `hash.Set(hash.MD5)`), so
it cannot produce a full-object SHA-256 attestation at all. There is nothing an
s3 medium could ever do to satisfy the class.

Until recently `backup-manager check` accepted `upload_verification: attested`
and the refusal arrived from the move engine instead: after the upload, at the
verification step, on every cycle, forever, in a log line. `Validate` now names
the reason and names `readback` as the way out, and `check` fails. Nothing about
that is a fall back to `existence`, and nothing about it is a silent fall
forward to a download you did not budget for.

An ETag is not a checksum and is never compared to one here. Multipart uploads
and server-side encryption both make an ETag something other than the object's
MD5, so the product does not treat it as content in any code path.

## Archive classes and restore time

`GLACIER` and `DEEP_ARCHIVE` are cheap because reading them is slow and billed.
The manager tells you what it knows and refuses to invent the rest.

**Read #428 before configuring a tier on one of these.** The vocabulary below
is real and the restore operation is real, and they apply to a copy that is
already on an archive class. What does not work yet is getting a copy THERE
through a tier's `medium` key, because the move would have to delete a local
copy against a destination nothing can read back. The manager refuses that,
loudly, and leaves the artifact where it is.

Each placement carries an access state, and the vocabulary is closed:

| State | Means |
| --- | --- |
| `immediate` | Local disk, or a non-archive storage class. Read it now. |
| `requires_restore` | An archive class with no restore in progress. It is there, and you cannot read it until you ask for it back. |
| `restoring` | A restore has been initiated. S3 reports no percentage, so none is shown. |
| `unreachable` | The medium cannot currently be reached. This is not the same as the artifact being gone, and it is deliberately a different word. |

A restore is an explicit, durable operation. You name the artifact, the
placement, and how many days you want the restored copy to stay available. It is
recorded before anything happens and survives a restart like every other
operation. Reading an artifact never starts a restore as a side effect, so
nothing here can generate a retrieval bill you did not ask for.

When S3 reports the restore's expiry date, you see it. Until it does, the surface
says a restore is in progress and that the provider reports no progress, which is
the truth. No ETA is invented, because there is none to compute.

**No cost figures appear anywhere.** The manager has no price list and no
knowledge of your negotiated rates, so it serves what it holds: bytes, storage
class, and a plain statement that retrieval from this class is billed by your
provider. Multiplying a guess by a guess and printing it in an operator console
is how people make expensive decisions confidently, so this product does not do
it.

## Things the manager deliberately does not manage

- **Bucket lifecycle rules, object lock and versioning.** The manager is the only
  retention authority. Setting a lifecycle rule on a managed bucket forks
  retention truth into a system the manager cannot see, and it is unsupported
  interference rather than a feature.
- **Encryption of medium artifacts.** rclone's `crypt` backend is registered, and
  wiring it up is not in this EPIC, because losing an encryption key loses every
  offsite artifact and that custody problem deserves its own design rather than a
  bolt-on.
- **Per-backup-set medium overrides.** Mediums bind to tiers, and tiers are
  global. A per-set override is the same larger change a per-set retention policy
  already deferred, and the two stay deferred together.

## Upgrading into this

A deployment whose config names no medium behaves exactly as it did before this
existed: the same validation outcomes, the same retention verdicts, the same API
responses apart from fields that are simply new, and the same CLI output. That is
not a hope, it is a gate: `core/tests/compat` captures all of those surfaces from
a medium-free deployment and compares them against a checked-in baseline on every
build, and `scripts/compat/selftest.sh` proves each of those comparisons can
actually fail.

The other side of it, a deployment that DOES name a medium, has a gate of its
own: `core/tests/conformance` runs the three-tier chain against a real S3
endpoint, watches the "at least one verified copy at every instant" rule at
every event that could break it, and `scripts/conformance/selftest.sh` proves
that watch can fire. `docs/conformance/epic-e-matrix.md` is the ledger of
which parts of the EPIC are checked that way today and which are not, and
the two limits at the top of this page are in it as rows that are
deliberately not green.

A settings save will not write a `storage_mediums:` or `medium:` key into a
config that never had one. Downgrading to an older binary after the schema has
moved forward fails closed, exactly as it always has.
