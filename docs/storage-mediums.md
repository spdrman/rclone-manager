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

The EPIC lands in two phases and only part of it is in your build. This section
is the honest state of it, and it is here at the top rather than in a footnote
because a configuration reference for behavior that does not exist yet is worse
than no reference at all.

| What | State |
| --- | --- |
| `storage_mediums` and a tier's `medium` key are accepted, validated and round-tripped by a settings save | Landed |
| Credentials are resolved from a file, an environment variable or a command | Not landed (#235) |
| Artifacts are recorded as living somewhere, and the recovery manifest says where | Not landed (#236) |
| Verification classes, and revalidation that knows about mediums | Not landed (#237) |
| Artifacts actually MOVE between mediums when a tier says so | Not landed (#238) |
| Retention plans, previews and prune understand mediums | Not landed (#239) |
| The API and the UI show placements, access states and the disclosure | Not landed (#240) |
| Archive storage classes and the explicit restore operation | Not landed (#241) |

Until the move engine lands, a config that maps a tier to a medium is a
declaration the manager validates and does not act on. Nothing is uploaded and
nothing is deleted.

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
| `content` (read-back) | The bytes on the medium hash to the SHA-256 the journal recorded | A full download: time, plus egress. On an archive class, a restore first. |
| `attested` | The provider's stored full-object checksum equals the recorded SHA-256 | One metadata call, no egress. Trusts the endpoint to implement S3 checksum semantics honestly. |
| `existence` | The object exists with the recorded size | One HEAD request |

The rules that matter:

- A move reaches VERIFIED at `content` class by default. The local copy is
  downloaded back and re-hashed at the last moment the local truth still exists,
  and only then is the source deleted.
- `existence` is never sufficient to delete a source. Not ever, not with any
  setting.
- Periodic revalidation checks medium placements at `existence` class only.
  Stronger re-verification of a medium placement is something you ask for,
  because anything that costs egress must never happen silently. A revalidation
  pass that could only reach `existence` is reported as `existence`, and never as
  the artifact having been "revalidated" in the sense a local artifact is.
- An artifact on `GLACIER` or `DEEP_ARCHIVE` is `existence`-checkable only, until
  an explicit restore makes anything stronger possible.

### `upload_verification: attested` and rclone's s3 backend

`attested` trades a download for trust in the endpoint. The trust is real and
worth saying plainly: an endpoint that lies about checksums can cause your local
copy to be deleted against a bad upload.

It also does not work on `s3` in this build, and it will say so rather than
quietly do something weaker. rclone v1.75.0's s3 backend reports exactly one hash
capability, MD5 (`backend/s3.Fs.Hashes()` returns `hash.Set(hash.MD5)`), so it
cannot produce a full-object SHA-256 attestation at all. Configuring
`upload_verification: attested` on an `s3` medium is therefore an explicit
capability failure, not a silent fall back to `existence`, and not a silent
fall forward to a download you did not budget for.

An ETag is not a checksum and is never compared to one here. Multipart uploads
and server-side encryption both make an ETag something other than the object's
MD5, so the product does not treat it as content in any code path.

## Archive classes and restore time

`GLACIER` and `DEEP_ARCHIVE` are cheap because reading them is slow and billed.
The manager tells you what it knows and refuses to invent the rest.

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
actually fail. `docs/conformance/epic-e-matrix.md` is the ledger of which parts
of the EPIC are checked that way today and which are not.

A settings save will not write a `storage_mediums:` or `medium:` key into a
config that never had one. Downgrading to an older binary after the schema has
moved forward fails closed, exactly as it always has.
