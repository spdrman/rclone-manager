# EPIC E: Alternative Storage Mediums per Retention Tier, S3 First

## Status

**Type:** EPIC / Detailed implementation specification
**Repository:** `spdrman/rclone-manager`
**Parent / predecessor EPICs:** EPIC A (#1, backup engine), EPIC B (#81, provider-neutral core and multi-NAS apps)
**Primary implementation root:** `core/`
**Tracker issue:** #232 (sub-issues #233 through #242)
**FR numbering:** this specification continues the product's FR series at **FR-27**. FR-1 through FR-24 are defined in `docs/EPIC.md`; FR-26 is claimed by the `version` command in `core/cmd/backup-manager` and `core/internal/app`. Nothing here renumbers an existing FR.

---

# Adversarial Review, Five-Expert Panel

Same discipline as `docs/EPIC-B-multi-nas.md`: each reviewer was instructed to reject this EPIC if the design could create data loss, a credential leak, misleading backup UX, an untestable guard, or a compatibility break for an existing deployment.

## Expert 1, Storage and Retention Architect

### Initial verdict: REJECT

Critical findings:

1. The draft never said which medium an artifact selected by TWO tiers with different mediums lives on. KEEP is a union (FR-18, settled by #215), so daily-selects-it and monthly-selects-it is the common case, not a corner.
2. Chain order was documented as cosmetic ("order never changes which artifacts are kept"), and the draft silently gave it a second, load-bearing meaning without saying so.
3. The draft allowed a move for any managed-complete artifact, which lets a migration race FR-15's pre-delete local-file checks in `COMMITTED` and `REMOTE_DELETE_PENDING`.
4. "Move" was underspecified as one operation when it is three (copy, verify, delete) with three failure modes.

Required corrections, all adopted:

- define the home-medium rule exactly: the first tier in chain order that currently selects the artifact names its home (FR-27);
- state outright that chain order now has a placement meaning, in the config field's documentation and in this spec;
- restrict moves to artifacts in `COMPLETE` (FR-30), so FR-15's checks never observe a half-moved local file;
- specify the move as a journaled three-phase operation with named crash points (FR-30).

### Consensus position: APPROVE AFTER REVISION

## Expert 2, Application Security / Threat Modeling

### Initial verdict: REJECT

Critical findings:

1. S3 credentials are a bigger prize than an SSH key scoped to one hardened SFTP account: they unlock every retained artifact on the medium at once. The draft did not say where they live or how they are kept out of every observable output.
2. A compromised or hostile S3 endpoint must never be able to cause LOCAL data loss. The draft's upload verification accepted the endpoint's own checksum attestation before deleting the local copy, and a malicious endpoint can echo the checksum it was handed at upload without storing a byte.
3. S3 object metadata, ETags and LastModified are exactly the untrusted input class FR-8 already names for SFTP, and the draft used LastModified in a retention-relevant place.
4. Catalog rebuild from a bucket hands an attacker who owns the bucket a write path into the catalog.

Required corrections, all adopted:

- credentials reuse the SSH key custody model byte for byte: file, env or command sources, `obs.Secret` wrapping, shape validation before use, no inline spelling in the schema at all (FR-33);
- the move's pre-delete verification defaults to read-back (download and re-hash against the journal's recorded hash); provider checksum attestation is a per-medium opt-in that names its trust assumption (FR-31);
- no value read from a medium may ever place an artifact on the retention calendar, widen DELETE, or displace anything from KEEP; the #215 union direction is the law here too (FR-32);
- catalog rebuild from a medium proposes and never deletes, same as the existing rebuild contract (FR-32).

### Consensus position: APPROVE AFTER REVISION

## Expert 3, Distributed Systems / Backup Reliability

### Initial verdict: REJECT

Critical findings:

1. Copy-verify-delete across two storage systems is a distributed transaction this product has exactly once already (ingestion), and the draft did not reuse that discipline: no durable intent record before side effects, no restart story.
2. "At least one good copy always exists" was implied, never stated as an invariant a test can hold.
3. The draft put move states on the artifact lifecycle machine, roughly doubling its edge count and making `QUARANTINED` ambiguous (is the local copy bad, or the S3 copy?).
4. Validation of a bucket-resident artifact was described as "supported" without saying what each level costs or which level each caller gets.

Required corrections, all adopted:

- the move engine mirrors ingestion's journal discipline exactly: durable intent before every side effect, the same `COMMITTED -> REMOTE_DELETE_PENDING -> COMPLETE` shape transposed to `COPIED -> VERIFIED -> SOURCE_DELETE_PENDING -> DONE` (FR-30);
- the invariant is normative and machine-checked: every managed-complete artifact has, at every instant, at least one ACTIVE verified placement, and uncertainty always preserves the source copy (FR-30);
- placement is a separate table and a separate small state machine, not new artifact lifecycle states; the artifact machine is untouched except for reusing its existing `COMPLETE -> QUARANTINED_LOST` edge (FR-29, FR-30);
- verification is a named three-class ladder with the cost of each class stated, and every surface reports the class actually achieved, never a stronger one (FR-31).

### Consensus position: APPROVE AFTER REVISION

## Expert 4, Backup Product / UX / Operations

### Initial verdict: REJECT

Critical findings:

1. A Glacier restore presented like a local read is a lie with an hours-long punchline. The draft had one "available" boolean.
2. The draft floated a cost estimate column. The backend has no price list, no negotiated rates and no way to compute one honestly; #211 removed nine fields for exactly this sin.
3. Deleting the NAS copy after an upload is the most consequential behavior in this EPIC and was buried in a config reference.
4. Automatic revalidation that silently downloads from S3 turns a safety feature into a surprise bill.

Required corrections, all adopted:

- placement access is a closed vocabulary (`immediate`, `requires_restore`, `restoring`, `unreachable`), derived only from facts the backend holds, and restore is an explicit durable operation, never a side effect of a read (FR-34);
- no cost figures anywhere; the UI and CLI state the bytes, the storage class, and the fact that the provider bills for retrieval, and stop there (FR-34);
- the tier-to-medium mapping is written through the existing settings flow with an explicit disclosure naming the deletion consequence, the same pattern as remote-source deletion disclosure (FR-27);
- automatic revalidation of S3 placements is existence-checked by default; anything that costs egress is operator-initiated (FR-31).

### Consensus position: APPROVE AFTER REVISION

## Expert 5, Release Engineering / Compatibility / Supply Chain

### Initial verdict: REJECT

Critical findings:

1. Adding an S3 path must not add an AWS SDK. The repository already contains the only S3 implementation it needs, inside the embedded rclone, and a second one would violate the FR-3 containment and the ui/shared provider-SDK rule in spirit and eventually in letter.
2. FR-4 says a new rclone backend is an architecture decision, not an import line, and the draft imported first.
3. A schema migration adding location truth to every artifact record had no backfill story and no downgrade story.
4. "Existing configs keep working" was an intention, not a gate.

Required corrections, all adopted:

- the S3 medium is rclone's own `s3` backend registered inside `core/internal/transport/rclone`, the one package allowed to import rclone; no AWS SDK anywhere in the tree, enforced by the existing backend-set test and the ui/shared provider-import check (FR-28);
- this specification IS the FR-4 architecture decision, recorded with the same measurement obligations the crypt precedent set (binary size delta measured and recorded in the landing PR) (FR-28);
- migration 0004 backfills a `local` placement for every existing artifact inside the same migration transaction, and an older binary meeting the new schema version fails closed exactly as today (FR-29, FR-35);
- backwards compatibility is a phase exit gate written as a checkable claim with a planted violation (FR-35, Phase 2 exit gate).

### Consensus position: APPROVE AFTER REVISION

## Five-Expert Consensus

> **The S3 medium is the embedded rclone's own backend behind the existing FR-3 transport boundary, never a second storage stack. Placement is journal truth in its own table, moves are a three-phase journaled operation with ingestion's crash discipline, the source copy survives every uncertainty, nothing read from a medium can make retention less safe, credentials follow the SSH key custody model, and a config that names no medium behaves byte for byte as it does today.**

---

# 1. Purpose

Today an artifact's durable copy lives in exactly one place: the backup set's `local_path` on the NAS (`core/internal/config.BackupSet.LocalPath`). FR-18's generalized retention chain (B3.8, #156) lets an operator chain any number of tiers, daily then weekly then monthly then semi-annual then annual, but every tier's artifacts sit on the same local disk.

This EPIC makes the storage medium selectable **per retention tier**, with S3 as the first non-local medium:

- daily on local disk, so recent restores are a filesystem read;
- monthly on S3 standard, off the NAS;
- annual on S3 with a colder storage class, cheap and slow.

An artifact ageing from one chain item into the next may therefore have to MOVE between mediums. That turns tier ageing from bookkeeping into data movement, which is why most of this specification is about crash safety, verification honesty and destructive gates rather than about S3.

What this EPIC deliberately is not:

- It is not a sync product. The manager remains the only retention authority; S3 bucket lifecycle rules, object lock and versioning are explicitly unmanaged and their use on a managed bucket is the operator's own affair, documented as unsupported interference.
- It is not multi-cloud. One non-local medium type ships: `s3`, which includes any endpoint speaking the S3 API (MinIO, Wasabi, and similar come along for free because it is the same rclone backend), but S3 proper plus a MinIO fixture are the only conformance targets.
- It is not a restore browser. Restore surfaces exactly what verification and honest status need; a full restore workflow remains future work.

# 2. Where every piece lives

The layering rules in `scripts/architecture/layers.conf` and the `scripts/architecture/*.sh` checks hold unchanged. Nothing outside `core/` declares lifecycle, retention, validation, catalog or backup policy, and every piece of this EPIC is policy, so every piece of this EPIC is core-layer:

| Piece | Location | Layer |
|---|---|---|
| Medium config schema and validation | `core/internal/config` | core |
| S3 backend registration and the `MediumStore` implementation | `core/internal/transport/rclone` (the only rclone importer, unchanged) | core |
| The `MediumStore` interface | `core/internal/transport` | core |
| Placement records and the moves journal | `core/internal/state` + `core/migrations/0004_*.sql` | core |
| The move engine | `core/internal/placement` (new package) | core |
| Tier-to-medium planning, preview and apply | `core/internal/retention`, `core/internal/app`, `core/service` | core |
| Verification ladder and revalidation policy | `core/internal/placement`, `core/internal/revalidate` | core |
| API additions | `api/v1/openapi.json` plus generated bindings | core |
| UI surface | `ui/shared` | core |

No platform profile and no distribution adapter changes behavior. Packaging inherits the feature by shipping the same binary. `ui/shared` imports no provider SDK, which costs nothing to honor because the Go side imports no provider SDK either: rclone's `s3` backend is the entire S3 implementation.

# 3. Functional Requirements

## FR-27, Storage Mediums and Tier Placement

A **storage medium** is a named, configured destination on which an artifact's durable copy can live.

`local` is the implicit medium every deployment already has: the backup set's `local_path`, with exactly today's semantics. It is reserved: a configured medium SHALL NOT claim the id `local`.

Additional mediums are declared at the top level of the configuration:

```yaml
storage_mediums:
  - id: offsite_s3
    type: s3
    region: us-east-1
    endpoint: ""                  # empty means the AWS endpoint for region
    bucket: nas-backups
    prefix: rclone-manager        # key namespace inside the bucket
    storage_class: STANDARD
    upload_verification: readback # readback (default) or attested; see FR-31
    credentials:
      file: /var/lib/backup-manager/s3/offsite_s3.creds
      # env: BACKUP_S3_OFFSITE
      # command: ["op", "read", "op://infra/backup-manager/s3-offsite"]
```

A retention tier names the medium its artifacts live on:

```yaml
retention:
  tiers:
    - name: daily
      granularity: day
      keep: 7                    # no medium key: local, exactly as today
    - name: monthly
      granularity: month
      keep: 12
      medium: offsite_s3
    - name: annual
      granularity: year
      keep: 7
      medium: offsite_cold
```

Validation (in `core/internal/config`, the one place that owns config truth):

- medium ids SHALL be lower_snake_case, unique, and not `local`;
- `type` SHALL be `s3` (the closed set grows only by a future FR);
- `storage_class` SHALL be one of a closed, validated set (`STANDARD`, `STANDARD_IA`, `ONEZONE_IA`, `INTELLIGENT_TIERING`, `GLACIER_IR`, `GLACIER`, `DEEP_ARCHIVE`);
- `RetentionTier.Medium` SHALL be empty (meaning `local`) or name a declared medium; a dangling reference is a validation error;
- a declared medium no tier references is legal (an operator staging config), not an error;
- `medium` is only expressible in the `tiers` spelling; the three legacy scalars cannot name one, and do not need to, because adopting mediums means adopting the tiers spelling;
- every new field carries `omitempty`, for the same wizard round-trip reason `tiers` itself does: a config that never heard of mediums must come back from a settings save without a `storage_mediums: []` or `medium: ""` injected into it.

**The home-medium rule.** An artifact's REQUIRED medium is decided by the retention chain: the **first tier in chain order that currently selects the artifact** names its home. When no tier selects it (an artifact kept only by FR-19's protection, or an artifact awaiting deletion), it stays where it is; absence of a selecting tier never triggers a move. This gives chain order a second meaning beyond presentation, and this spec says so plainly rather than letting it be discovered: order still never changes WHICH artifacts are kept (KEEP is a union), but it now decides WHERE a multiply-selected artifact lives. Operators write chains fine-to-coarse, so the first selecting tier is the warmest, which is the behavior the daily-local-monthly-S3 story needs.

**Consent.** Writing a tier-to-medium mapping is a configuration change through the existing settings flow (optimistic concurrency, config revision), and the UI and CLI SHALL present an explicit disclosure before the first save that maps any tier of a backup-affecting chain to a non-local medium: artifacts selected only by that tier will live only on that medium, and the NAS copy will be deleted after verified upload. This is the remote-source-deletion disclosure pattern applied to the other end of the pipeline. After that consent, moves execute automatically as declared policy, exactly as FR-15's remote delete does.

## FR-28, The S3 Medium Is the Embedded rclone Behind the FR-3 Boundary

This FR is the FR-4 "explicit feature/architecture decision" for a third rclone backend.

- `s3` joins `RequiredBackends` in `core/internal/transport/rclone/backends.go`. The registered-backend-set test is updated in the same change, so the set stays enforced, and the landing PR SHALL record the measured binary-size delta, the same obligation the crypt precedent established.
- No AWS SDK, no Azure SDK, no GCS SDK enters the tree, in Go or in TypeScript. rclone's `s3` backend is the entire S3 implementation. The existing ui/shared provider-import check and the backend-set test are the enforcement.
- A new manager-owned interface, `transport.MediumStore`, is defined beside `transport.Transport` and implemented only by the rclone adapter package:

```go
type MediumStore interface {
    StatObject(ctx, medium, key) (ObjectInfo, error)
    UploadFromLocal(ctx, medium, localPath, key, opts) (UploadResult, error)
    OpenObject(ctx, medium, key) (io.ReadCloser, error)   // for read-back verification and restore-to-local
    ObjectChecksum(ctx, medium, key, alg) (ChecksumAttestation, error)
    DeleteObject(ctx, medium, key) error
    RestoreStatus(ctx, medium, key) (RestoreState, error)
    InitiateRestore(ctx, medium, key, days) error
    ListObjects(ctx, medium, prefix) ([]ObjectInfo, error) // for catalog rebuild and reconciliation
}
```

  Exact signatures may change. The FR-3 rules carry over verbatim: lifecycle and retention code depend on this interface, rclone types never leak past the adapter, destructive operations are explicit, and there is **no generic `Move()`**: a migration is `UploadFromLocal` plus verification plus a separate local delete, composed by the move engine (FR-30), never a transport primitive.
- Error classification extends `transport.Category` mapping for the S3 backend: throttling and 5xx are Transient, `NoSuchBucket` and endpoint resolution failures are Configuration, `AccessDenied` and signature failures are Auth, `NoSuchKey` is NotFound. The existing contract-test shape (`core/internal/transport/contract`) gains a MediumStore suite, run against the local backend in-tree and against a MinIO fixture in integration.
- The key layout inside a medium is deterministic and mirrors FR-7's backup-set isolation: `<prefix>/<source>/<set>/<artifact-name>`, plus `<prefix>/<source>/<set>/.manifest/<artifact-name>.json` for the recovery sidecar (FR-29). No timestamps, no random components, so re-running an interrupted upload targets the same key idempotently.

## FR-29, Placement Is Journal Truth

An artifact's location becomes part of its durable record, in a new table rather than new columns on the artifact row, because one artifact can have several copies during a move and zero local copies after one.

Migration `0004_placements.sql` SHALL create:

- `placements`: one row per durable copy. Artifact FK, medium id (`local` or a configured id), location (an absolute path for local, a key for s3), size, hash, hash algorithm, verification class achieved (FR-31), verified-at, status (`ACTIVE`, `DELETE_PENDING`, `GONE`), created/updated timestamps.
- `placement_moves`: one row per migration, the FR-30 journal. Artifact FK, source placement, destination medium, destination key, phase, bytes, error, timestamps.

The same migration SHALL backfill one `ACTIVE` `local` placement for every existing artifact row, derived from its `local_path`, `local_hash` and `local_hash_alg`, inside the migration transaction, so no code path ever observes an artifact with no placement. Existing behavior (schema-version fail-closed on downgrade, forward-migration tests, TDD invariant 6) applies unchanged.

`state.Record` gains its placements; `LocalPath` keeps meaning what it means today (the ingestion landing path) and stays valid while a local placement is ACTIVE. Code that asks "can I read this artifact locally" SHALL ask the placements, not assume `LocalPath` readable, and the compiler-assisted sweep of those call sites (lifecycle verify, revalidate, prune, recovery manifest, health) is part of this FR's scope.

Recovery metadata (EPIC B section 19.3) extends in both directions: the local sidecar manifest records the artifact's placements, and every uploaded artifact gets a sidecar object under `.manifest/` carrying the same non-secret recovery fields the local manifest carries today, so `catalog rebuild` can reconstruct from a medium when local state is lost. Sidecars carry no credentials, no endpoints and no secret material, same rule as today.

## FR-30, Tier Transitions Are Journaled Moves

A move is the three-phase operation `copy -> verify -> delete source`, executed only by the move engine in `core/internal/placement`, and journaled in `placement_moves` with a durable write **before every side effect**, the same discipline ingestion's `COMMITTED -> REMOTE_DELETE_PENDING -> COMPLETE` already carries.

Move phases:

```text
PLANNED -> COPYING -> COPIED -> VERIFYING -> VERIFIED -> SOURCE_DELETE_PENDING -> DONE
                                                   \-> ABANDONED (destination cleaned up, source untouched)
```

Rules:

- Only artifacts in `COMPLETE` are move-eligible. `COMMITTED` and `REMOTE_DELETE_PENDING` still owe FR-15 its pre-delete local-file checks, and a move racing those checks is a bug this rule makes unrepresentable. Retention tiers age in days; remote deletes settle in minutes; the restriction costs nothing real.
- The **standing invariant**: every managed-complete artifact has at least one ACTIVE placement whose recorded verification class is read-back or better, at every instant, including mid-move and mid-crash. Uncertainty preserves the SOURCE placement, always. The destination copy is the disposable one until `VERIFIED` is durably recorded.
- The source placement is deleted only after `VERIFIED` is durably recorded, only through the FR-20 discipline when the source is local (canonicalized path, proven beneath the backup-set root, journal-managed, symlink and traversal refusal), and only after re-checking that no tier whose medium is the source still selects the artifact.
- **Restart semantics** (FR-17 reconciliation extension): on startup the engine reads `placement_moves` for non-terminal rows. A move at or before `COPYING` restarts its upload to the same deterministic key (idempotent by construction). A move at `VERIFYING` re-verifies from scratch. A move at `SOURCE_DELETE_PENDING` re-verifies the destination and, on success, completes the source delete; on destination verification failure it returns to `COPYING` with the source still intact. A destination object that cannot be verified is deleted (destination, never source) and the move restarts or is abandoned.
- Moves are driven from the retention cycle: after a retention pass computes each artifact's home medium (FR-27), the engine plans moves for artifacts whose home differs from their ACTIVE placement's medium, bounded per cycle (a `max_moves_per_cycle` guard, same shape as revalidation's `max_per_cycle`), and executes them under FR-27's already-given consent. There is no per-move confirmation, and there is also no move that a config change did not declare.
- Artifact lifecycle states do not change. The artifact stays `COMPLETE` throughout a move. The one reuse: an artifact whose ONLY placement fails verification, with the remote source confirmed gone, takes the existing `COMPLETE -> QUARANTINED_LOST` edge, which is exactly what that edge already means.
- FR-20's prune gains medium awareness: deleting an expired artifact whose placement is on a medium deletes the object through `MediumStore.DeleteObject`, preceded by an FR-16-style identity re-check (stat the object; compare size and, where available, checksum against the placement record; refuse on mismatch and require reconciliation). The mandatory dry-run explains per-artifact WHERE the deletion would happen, not only whether.

## FR-31, Verification Off Local Disk: a Three-Class Ladder

Validation, FR-19 eligibility and revalidation currently assume a local file that can be re-read and hashed. On a medium, "verified" becomes a ladder, and every class has a cost the operator can see:

| Class | What it proves | What it costs |
|---|---|---|
| `content` (read-back) | The bytes on the medium hash to the journal's recorded SHA-256 | A full download: time plus egress; for archive classes, a restore first |
| `attested` | The provider's stored full-object checksum equals the recorded SHA-256 | One metadata call, no egress; trusts the endpoint to implement S3 checksum semantics honestly |
| `existence` | The object exists with the recorded size (and recorded sidecar metadata where present) | One HEAD request |

Rules:

- A move reaches `VERIFIED` only at `content` class by default: download and re-hash against the journal's recorded hash, at the last moment the local truth still exists. A medium may opt into `attested` via `upload_verification: attested`, and the config documentation SHALL name the trust assumption in plain words: an endpoint that lies about checksums can then cause the local copy to be deleted against a bad upload. `existence` is never sufficient to delete a source.
- Where the endpoint or the embedded rclone version cannot produce a full-object checksum attestation, `attested` SHALL fail with an explicit capability result, never silently degrade to something weaker: FR-13's "explicit capability result rather than silently weakening configured verification" applies verbatim.
- Periodic revalidation (`core/internal/revalidate`) becomes placement-aware. Local placements keep today's behavior. Medium placements are `existence`-checked by default on the revalidation interval; `attested` and `content` re-verification of a medium placement are operator-initiated operations, because anything that costs egress must never happen silently. A revalidation pass that could only achieve `existence` SHALL be recorded and reported as `existence`, never as the artifact having been "revalidated" in today's sense; the checked-vs-passed distinction `revalidate` already draws (a pass that verified nothing must not reset the due-ness clock as if it had) extends to classes.
- An artifact on an archive storage class (`GLACIER`, `DEEP_ARCHIVE`) is `existence`-checkable only, until an explicit restore (FR-34) makes stronger classes possible. The status surfaces say exactly that.
- FR-19 last-known-good eligibility is unchanged in form (managed-complete, validation passed), and the protection continues to refuse deletion regardless of medium. The health surface reports the protected artifact's verification class and its age, so "protected by a copy nobody has content-verified in a year" is visible instead of implied.
- Quarantine becomes placement-scoped: a medium placement failing verification marks that placement, and the ARTIFACT enters `QUARANTINED`/`QUARANTINED_LOST` only when no other ACTIVE verified placement remains (`QUARANTINED_LOST` when the remote source is also confirmed gone, the existing meaning of that state).

## FR-32, Medium Metadata Is Untrusted Input

FR-8's rule ("remote filenames and metadata SHALL be treated as untrusted input") applies to everything a medium reports: ETags, LastModified, user metadata, list results, sidecar objects. Specifically:

- An ETag is never a content hash. Multipart uploads and encrypted objects make it not one, so nothing in this product compares an ETag to a recorded hash, ever.
- S3 `LastModified` is upload time, not backup time, and is **never admissible as a producer timestamp**. FR-18's two placements (discovery timestamp, and the producer's own where admissible, per #215) are captured once at original discovery and live in the journal; a move copies journal truth and never re-derives it from the destination. An artifact's retention bucketing is therefore invariant under movement, by construction, and there is a test that pins it: move an artifact, recompute the verdicts, assert bit-identical placement.
- The #215 union direction is the law for medium-supplied data too: nothing read from a medium may move an artifact out of KEEP, widen DELETE, or displace a KEEP selection. Medium data may only ever add safety (for example, a sidecar found during rebuild proposing a catalog entry).
- Catalog rebuild from a medium (`catalog rebuild` extension) treats keys and sidecar contents as untrusted proposals: dry-run first, reconstruction never deletes anything local or remote, conflicts with existing journal rows are reported rather than resolved silently, and a sidecar can never overwrite an existing row's timestamps or hashes.

## FR-33, Credential Custody

S3 credentials follow the SSH key custody model in `core/internal/config.Key` and `core/internal/transport/rclone/keysource.go`, exactly:

- Three sources, exactly one set per medium: `credentials.file` (preferred: an AWS shared-credentials format file that rclone reads itself, so the secret never enters this process's memory), `credentials.env`, `credentials.command` (argv array, never a shell string, bounded timeout, minimal environment).
- There is **no schema field for a literal key**. `access_key_id:` or `secret_access_key:` inline in the config is an unknown field, refused by `Load`'s `KnownFields(true)` before validation even runs, and a test pins that refusal.
- Whatever `env` or `command` produce is validated by shape before use, wrapped in `obs.Secret`, and never echoed: a resolver failure is reported by the shape of the problem, never by the content that failed.
- Credential files live under private state (`/var/lib/backup-manager`, EPIC B section 19.1), never under the backup root, and the API import flow mirrors SSH key import: the secret goes into private state, and the config holds a path.
- The following SHALL never contain a credential, in whole or in part: `config.yaml` values, any log line at any level, any error message, any API response (the mediums surface returns id, type, bucket, region, class, never key material), the redacted config export, recovery manifests and sidecar objects, and bucket object metadata.
- The enforcement is a canary: an integration test resolves a known canary secret through each source and asserts its absence from every observable output above. The planted violation for this guard is a build that logs the resolved medium config verbatim; the canary gate fails it, and that failing run is recorded in the landing PR.

## FR-34, Honest Retrieval Status

A colder storage class can take hours to restore and costs money to read. The product tells the truth about that, and only the truth it can compute:

- Each placement carries an `access` state from a closed vocabulary, derived only from held facts: `immediate` (local, or a non-archive class), `requires_restore` (archive class, no restore in progress), `restoring` (restore initiated; S3 reports no percentage, so none is shown), `unreachable` (the medium cannot currently be reached; distinct from the artifact being gone).
- Restore is an explicit durable operation (`submitOperation` family): it names the artifact, the placement, and the restore window in days, records the operation before acting, and survives restarts like every other operation. Reads never initiate a restore as a side effect.
- When S3 reports a restore's expiry date, it is shown; until then the surface says a restore is in progress and that the provider reports no progress. No ETA is invented.
- No cost figures are served anywhere. The backend cannot compute egress or restore pricing honestly (no price list, no negotiated rates), so per the #211 rule it serves what it holds: bytes, storage class, and a plain statement that retrieval from this class is billed by the provider. If a future FR adds operator-entered price tables, that is its own decision; nothing here fakes it.
- The CLI mirrors the same vocabulary (`backup-manager artifacts`, artifact detail), so a terminal operator and a UI operator read the same truth.

## FR-35, Compatibility

- A configuration with no `storage_mediums` key and no `medium` key on any tier SHALL behave byte for byte as today: identical validation outcomes, identical retention verdicts (the existing golden tests run unmodified against the migrated schema and pass unmodified), identical API responses except for additive fields, identical CLI output except for additive columns that render only when a non-local placement exists.
- Migration 0004's backfill SHALL leave every existing deployment reading as "every artifact has one ACTIVE local placement", with no behavioral difference observable through any surface.
- A wizard or settings save SHALL NOT inject `storage_mediums: []` or `medium: ""` into a config that never configured them (the `omitempty` round-trip rule, same trap `tiers` already documented and avoided).
- An older binary opening a database at schema version 4 fails closed with the existing unsupported-downgrade behavior; nothing here weakens it.
- This FR is a Phase 2 exit gate line, not an aspiration, and its planted violation is defined there.

# 4. TDD Contract

EPIC B's section 4B contract applies to every child issue here unchanged: SPECIFY, RED, GREEN, REFACTOR, INTEGRATE, REGRESSION, ACCEPT, with the section 82 child-issue template mandatory, and production code alone never completing an issue. The invariants in EPIC B section 4C apply with particular force to invariants 3 (destructive behavior needs positive and negative safety tests), 6 (migrations need forward and failure tests), 8 (filesystem deletion needs containment tests) and 9 (remote deletion needs TOCTOU refusal tests before success tests), because this EPIC adds instances of all four.

**Every guard this EPIC adds must be shown to fire.** This repository has hit fifteen cases of a check passing for the wrong reason, so each gate below names its planted violation, and the landing PR for the issue that builds a gate records the planted violation actually failing:

| Guard | Planted violation that proves it fires |
|---|---|
| Source survives every move uncertainty | A mutation that issues the source delete before `VERIFIED` is durably recorded; the crash-injection suite must fail it |
| Medium data only ever adds (FR-32) | A mutation that admits S3 `LastModified` as a producer timestamp; the KEEP-superset invariant test from #215 must fail it |
| Bucketing invariant under movement | A mutation that rewrites the journal's discovery timestamp from the destination object during a move; the bit-identical-verdict test must fail it |
| Credential canary (FR-33) | A build that logs the resolved medium config verbatim; the canary gate must fail it |
| Inline secret refusal (FR-33) | A config with a literal `secret_access_key:`; `Load` must refuse it as an unknown field |
| Prune identity re-check on mediums (FR-30) | A fixture that swaps the object behind a key before prune; the delete must be refused |
| Compatibility (FR-35) | A migration variant that rewrites `retention_tier` during backfill; the golden retention suite must fail it |
| Verification honesty (FR-31) | A revalidation run forced to `existence` class; the surface must not report it as content verification, and a test asserts the class string |

# 5. Phases

Two phases, at most five sub-issues each. Numbering is `E<phase>.<n>`.

## Phase 1, the medium boundary (nothing destructive, no byte moves)

Phase 1 builds every load-bearing wall: schema, transport, state, verification. At the end of Phase 1 the product can describe, validate and verify placements, and still cannot move or delete anything it could not before.

- E1.1 The specification (this document), adversarially reviewed and landed
- E1.2 Config schema and validation for storage mediums, tier placement and credential references (FR-27, FR-33 schema half, FR-35 round-trip rule)
- E1.3 `MediumStore` boundary, rclone `s3` backend registration, credential resolution, error classification, MinIO contract fixture (FR-28, FR-33 runtime half)
- E1.4 Placement records: migration 0004 with backfill, `state.Record` surface, recovery manifest and sidecar extension (FR-29, FR-32 rebuild half)
- E1.5 The verification ladder and placement-aware revalidation (FR-31, FR-32 invariants)

### Phase 1 entry gate

- [ ] This specification is merged.
- [ ] The EPIC issue and its sub-issues exist with the section 82 template.

### Phase 1 exit gate

Checkable claims, not intentions:

- [ ] A config declaring an `s3` medium and a tier `medium` reference validates, round-trips through a settings save without injecting fields into a legacy config, and a config naming neither behaves identically to today (the existing config test suite passes unmodified).
- [ ] A config with an inline literal credential fails `Load` with an unknown-field error, proven by test.
- [ ] The rclone backend set test passes with exactly `local`, `sftp`, `s3` required and `crypt` accepted; the binary-size delta is measured and recorded in the landing PR.
- [ ] The MediumStore contract suite passes against the local backend in-tree and against a MinIO fixture in integration, including upload, stat, checksum attestation where supported, read-back, delete, and the explicit capability refusal where attestation is unsupported.
- [ ] The credential canary test passes for all three sources, and its planted violation (verbatim config logging) demonstrably fails it.
- [ ] Migration 0004 backfills a local placement for every pre-existing artifact row; the golden retention tests and the full existing suite pass unmodified against the migrated schema.
- [ ] Revalidation reports `existence` class for a medium placement and never a stronger class it did not achieve, proven by the class-string assertion test.
- [ ] Nothing in this phase can delete an artifact copy anywhere: the destructive-safety suite diff shows no new deletion path.

## Phase 2, movement, retention integration, and the operator surface

- E2.1 The move engine: journaled three-phase moves, crash-injection suite, restart reconciliation (FR-30)
- E2.2 Retention integration: home-medium planning, medium-aware preview/apply and prune with FR-16 identity re-checks on mediums (FR-27 home rule, FR-30 prune half)
- E2.3 API and UI: placements on the artifact surface, honest access states, tier-medium settings with disclosure, contract-first bindings (FR-34 read half, FR-27 consent)
- E2.4 Archive classes and the explicit restore operation (FR-34 restore half, FR-31 archive rules)
- E2.5 End-to-end conformance and the compatibility gate: full cycle against MinIO, crash-matrix run, FR-35 gate, operator docs

### Phase 2 entry gate

- [ ] Every Phase 1 exit line holds.
- [ ] The crash-injection harness design (which phases are interruptible, how injection is done) is written into E2.1 before its RED step, because a crash suite designed after the engine tests the engine's habits rather than its contract.

### Phase 2 exit gate

- [ ] A three-tier chain (daily local, monthly `s3`, annual `s3` cold) runs end to end against MinIO: ingest, age, move, verify, prune, with every move journaled and the standing invariant (at least one ACTIVE verified placement per managed-complete artifact) asserted continuously by the harness, not sampled.
- [ ] The crash matrix passes: a forced crash at every move phase boundary, followed by restart reconciliation, ends with the invariant intact and the move either completed or abandoned with the source intact; the planted violation (delete before durable `VERIFIED`) demonstrably fails the suite.
- [ ] Moving an artifact does not change its retention bucketing: verdicts before and after a move are bit-identical, and the planted timestamp-rewrite violation demonstrably fails the test.
- [ ] Prune against a medium refuses on identity mismatch (the swapped-object fixture) and the mandatory dry-run names the medium for every proposed deletion.
- [ ] A tier-to-medium settings save without the disclosure acknowledgment is refused by the API, with allow and deny tests (TDD invariant 4).
- [ ] An artifact on an archive class shows `requires_restore`, a restore is a durable operation surviving restart, and no surface anywhere renders a cost figure or an invented ETA (asserted by the contract tests on the response schemas: the fields do not exist).
- [ ] FR-35 holds: a deployment upgraded with a medium-free config shows zero behavioral diff through config validation, retention verdicts, API responses (minus additive fields) and CLI output; the planted backfill violation demonstrably fails the golden suite.
- [ ] `scripts/api/check-contract-drift.sh` and `check-client-paths.sh` pass with the new operations; the layer manifest classifies every new file; `verify-core-without-distribution.sh` still passes, since nothing here touches an adapter.

# 6. What I cut to fit two phases, and why

- **A second medium type** (Azure, GCS, SFTP-as-medium, a second NAS). The `MediumStore` boundary is the extension point; each future type is its own FR-4-style decision. Cutting it keeps Phase 1 honest about conformance: one type, actually proven.
- **Client-side encryption of medium artifacts.** rclone's `crypt` backend is already registered transitively and is the obvious lever, but key custody for an encryption key is its own custody problem with its own recovery story (lose the key, lose every offsite artifact), and bolting it on here would rush exactly the part that must not be rushed.
- **A restore browser and restore-to-NAS workflows** beyond the explicit restore operation verification needs. FR-34 ships the honest status and the durable restore operation; a full restore UX is future work.
- **Managed bucket lifecycle** (S3 lifecycle rules, object lock, versioning). The manager stays the only retention authority; delegating deletion to bucket rules would fork retention truth into a system FR-18 cannot see.
- **Cost accounting.** Nothing renders a number the backend cannot compute (the #211 rule). Operator-entered price tables could make estimates honest one day; that is a separate decision.
- **Per-backup-set medium overrides.** Retention policy is global by the #111 decision; mediums bind to tiers, and tiers are global. Per-set overrides are the same "separate, larger change" #111 already deferred, and they stay deferred together.
- **Automatic content re-verification of medium placements.** Silent egress is a surprise bill; `existence` checks are the automatic ceiling, and stronger re-verification is operator-initiated.

# 7. Compatibility and migration summary

An existing deployment upgrades in place: migration 0004 backfills local placements transactionally; a medium-free config keeps producing identical decisions and identical surfaces; the settings round-trip injects nothing; downgrade fails closed on schema version exactly as today. Adoption is opt-in per tier, gated by an explicit disclosure, and reversible in config (remapping a tier back to `local` plans moves back; the move engine is direction-agnostic, though the egress cost of coming home is the operator's, stated in the disclosure).
