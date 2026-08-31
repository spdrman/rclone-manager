# Recovery and the restore procedure

This is the page to read when a backup didn't arrive, an artifact looks wrong, or you're
trying to figure out whether you can still get a file back. It assumes you've already read
the README's [Status](../README.md#status-what-actually-runs-today) section; the short
version repeated here because it changes every answer below: there is no `backup-manager
status`, `restore`, `run` or `daemon` command yet (issues #25, #26). Everything in this
document works directly against the SQLite journal and the NAS filesystem, because that's
genuinely the only interface that exists today.

None of that makes this document a placeholder for later. Restore was never going to be its
own automated command: the design's answer to "how do I get a backup back" has always been
"the journal tells you which local file is trustworthy, go get it," whether that journal was
populated by a full daemon loop or, as today, by your own driver code or the test suite.
This procedure is the permanent shape, not a workaround.

> **No terminal?** Everything below assumes a shell on the NAS. If you have only the web
> interface, which is the normal case on a NAS appliance and the case every provider store
> assumes, read [recovery without a terminal](recovery-without-a-terminal.md) instead. It
> covers the same three failures this page starts with, through the interface, and it is
> the page the submission bundle's support materials point a reviewer at.

## The one fact everything else depends on

The journal is a plain SQLite file at whatever path `state.database` names in your config
(`core/internal/config/testdata/full.yaml` shows the shape). Query it with the `sqlite3` CLI,
no Go required:

```bash
sqlite3 /path/to/state.db ".schema artifacts"
```

The columns that matter for recovery: `source`, `backup_set`, `artifact_name`,
`remote_path`, `local_path`, `state`, `updated_at`, `remote_hash`, `local_hash`,
`retry_count`, `last_error`, `remote_deleted_at`, `remote_delete_error`. The full schema is
`core/migrations/0001_init.sql` plus `core/migrations/0002_quarantined_lost.sql`.

## Step 1: is this actually broken?

There's no `status` command to answer this yet, so answer it directly. For one backup set:

```bash
sqlite3 /path/to/state.db "
  SELECT state, count(*), max(updated_at)
  FROM artifacts
  WHERE source = 'production' AND backup_set = 'postgres-primary'
  GROUP BY state
  ORDER BY max(updated_at) DESC;
"
```

What `core/internal/health` would tell you if it were wired to anything (see the README's
[Status and health](../README.md#status-and-health)):

- If the newest row across the whole set is `COMMITTED`, `REMOTE_DELETE_PENDING` or
  `COMPLETE`, and it's recent enough for your `stale_after` window, that's a healthy set.
  Nothing to do.
- If there's no row in one of those three states inside the freshness window, treat it as
  stale: something has stopped landing new backups for this set, whether or not any single
  attempt has technically "failed" yet.
- If any row is `QUARANTINED_LOST`, that's an unconditional alarm regardless of anything
  else in the set, because it means an irrecoverable loss happened. See Step 3.
- If the newest row is `FAILED` with no `next_retry_at`, the retry budget for that attempt
  is exhausted and it needs a human, not another automatic pass.

## Step 2: finding a restore point

Only three states are ever a valid restore point. This isn't a convention this document is
inventing, it's the exact set `core/internal/health/compute.go` calls `knownGood`:

- `COMMITTED`
- `REMOTE_DELETE_PENDING`
- `COMPLETE`

Everything else is not a restore point, with no exceptions:

- `DISCOVERED`, `TRANSFERRING`, `TRANSFERRED`, `VERIFYING`, `VERIFIED`, `COMMITTING` all
  mean the durable commit (fsync, atomic rename, directory fsync) hasn't happened yet.
- `FAILED` and `QUARANTINED` mean something is actively wrong with this attempt.
- `QUARANTINED_LOST` means the loss already happened and is permanent (Step 3).
- Any file on disk with a `.partial` suffix, no matter what the journal says about a
  *different* row, is disposable by design (FR-12) and must never be treated as a restore
  point, even if it looks complete. If you're ever tempted to grab a `.partial` file because
  nothing else looks recent enough, stop and read Step 4 instead: that's a stale or stuck
  set, not a missing-but-actually-there backup.

To find the actual restore point:

```bash
sqlite3 /path/to/state.db "
  SELECT artifact_name, local_path, state, updated_at
  FROM artifacts
  WHERE source = 'production'
    AND backup_set = 'postgres-primary'
    AND state IN ('COMMITTED', 'REMOTE_DELETE_PENDING', 'COMPLETE')
  ORDER BY updated_at DESC
  LIMIT 1;
"
```

The `local_path` in that row is the file. It was fsynced and atomically promoted to that
name by `core/internal/lifecycle/commit.go` before `COMMITTED` was ever recorded (see the
README's [Durable commit](../README.md#durable-commit)), so treat it as trustworthy on the
strength of that alone; you don't need to re-verify it before copying it out, though
re-running whatever validator the backup set's config names is never wrong if the stakes
are high enough to justify the time.

Copy it wherever the restore actually needs to happen. There is no `backup-manager restore`
command to do this for you; a plain `cp`, `scp`, or whatever your restore target needs is
the entire remaining procedure once you have the right path.

## Step 3: the newest good row is `QUARANTINED_LOST`

`QUARANTINED_LOST` is reachable only from `COMPLETE`, the one state that confirms the remote
copy was already deleted, so by the time an artifact reaches it the manager believes there is
no copy anywhere: not on the remote (already deleted), not intact locally (that's why it's
here). Nothing automatic ever routes an artifact back out of it
(`core/internal/lifecycle/machine.go`).

**Check the obvious thing first, because the finding itself can be wrong.** The local check
that put the artifact here fails identically for "the bytes are corrupt" and "the volume was
not mounted when the check ran", and an unmounted volume takes every `COMPLETE` artifact in
the backup set down with it. If the file is there and reads correctly now, ask the manager to
re-check it and trust it again rather than treating this as a loss:

```
POST /api/v1/quarantine/<source>/<set>/<name>/reinstate
```

It re-runs the checks itself and only moves the artifact if they pass and the evidence is
conclusive (a recorded hash the copy still matches, or the backup set's validator running and
passing now). A reinstated artifact goes back to `COMPLETE` and counts as a restore point
again. See `docs/adr/0004-reinstating-a-quarantined-backup.md`.

If the local copy really is bad, the backup is gone. What to actually do then:

1. Query for the next-newest row in `COMMITTED`, `REMOTE_DELETE_PENDING` or `COMPLETE` for
   the same backup set (same query as Step 2, drop the `LIMIT 1` and look further down, or
   add `AND state != 'QUARANTINED_LOST'` if it's mixed in with rows you do want). Restore
   from that instead.
2. Treat the gap as a real, permanent loss of that specific restore point when you report
   this, not as "we're still working on retrieving it." Nothing is retrieving it.
3. If your retention window means the previous good backup is now further back than
   whatever RPO you're working against, that's the actual incident: the corruption plus
   the already-completed remote delete together cost you the interval between the two
   restore points. Say so plainly rather than letting "we have a backup" imply "we have as
   recent a backup as you think."

## Step 4: the newest row is `QUARANTINED` (not `_LOST`)

This is different and better: the remote copy may still exist. `QUARANTINED` is reachable
from `VERIFYING` (a hash mismatch or a failing validator caught it before commit), from
`COMMITTED`, or from `REMOTE_DELETE_PENDING` (reconciliation found the durably committed
local copy had gone bad after the fact, while the remote side was still there or unconfirmed
gone). Its one exit is back to `DISCOVERED`, meaning a fresh attempt has a real chance of
succeeding.

The design intends this to self-heal automatically the next time discovery and
reconciliation run against this backup set. Today, with no daemon or scheduled runner (see
[Status](../README.md#status-what-actually-runs-today)), that pass doesn't happen on its
own. Your options, in order of how much you should trust the result:

1. If you or someone else has already wired a runner against these packages (calling
   `discovery.Discover`, `reconcile.Reconcile`, and the `core/internal/lifecycle` steps
   yourself), run it against this backup set.
2. Otherwise, fetch the artifact by hand: `sftp` in with the same key and `known_hosts`
   `docs/ssh-setup.md` describes, confirm it's still there under the `remote_path` the
   journal recorded, and pull it down. Re-run whatever the backup set's `validation.command`
   names against the file before trusting it, since that's the check that flagged it in the
   first place.
3. Don't manually flip the journal row's `state` column back to `DISCOVERED` by hand unless
   you understand exactly what `core/internal/state/journal.go`'s idempotency-key scheme expects;
   an ad hoc `UPDATE` can leave `state_transitions` and `artifacts` disagreeing in a way the
   append-only log was specifically built to prevent.

## Step 5: a row has been sitting at `REMOTE_DELETE_PENDING` for a long time

Check `remote_delete_error` on that row before assuming anything is stuck:

```bash
sqlite3 /path/to/state.db "
  SELECT artifact_name, remote_path, updated_at, remote_delete_error
  FROM artifacts
  WHERE state = 'REMOTE_DELETE_PENDING'
  ORDER BY updated_at ASC;
"
```

If `remote_delete_error` is non-empty, this is very likely not a bug. Read the README's
[TOCTOU protection on delete](../README.md#toctou-protection-on-delete): against the
shell-less SFTP account this project's own setup guide recommends, `CompareIdentity` can
usually only reach `ConfidenceWeak` on the remote side, because there's no remote hash and
usually no backend-stable identifier to check against, only size and modification time. A
weak-confidence comparison always preserves the remote object rather than deleting it, per
the rule at the top of the README. So a persistent refusal here means: the remote copy is
still sitting on the source server, unpruned, on purpose, and it will very likely stay that
way for every backup in this deployment shape, not just this one artifact.

That's a real operational consequence, not a cosmetic one:

- The backup itself is not at risk from this. If anything, an unpruned remote is an extra
  copy you didn't have to ask for.
- **The remote source disk is.** If nothing else prunes it (a separate retention job on the
  producer side, manual cleanup, a shorter retention window configured at the source), it
  will fill up on a long enough timeline, in every deployment that follows this project's
  own hardening advice. Monitor remote disk usage independently of this project; don't
  assume `backup-manager` is freeing space on the source just because backups keep landing
  successfully on the NAS.
- If you need remote pruning to actually happen in this deployment shape, the honest options
  are: relax the SFTP account's hardening to allow a remote hash command (trading delete-
  safety confidence for automatic pruning), add a backend-reported stable identifier the
  comparison can use instead of a hash, or prune the remote through some mechanism entirely
  outside this project's control (a retention job on the producer side that doesn't depend
  on this project's confidence bar). Weakening `CompareIdentity`'s bar itself is not a
  fix worth taking without a new ADR; it exists at that bar on purpose.

## Step 6: retention decided this backup should be deleted, but it's still there

That's expected, not a bug, for two independent reasons documented in the README's
[Retention](../README.md#retention) section:

- `core/internal/retention.GFSDecide` only classifies artifacts into keep/not-kept-by-GFS. It
  contains no deletion code at all. A `Keep: false` verdict is a candidate, not an order.
- There is no code path anywhere in this repository that deletes a local file yet (FR-20,
  issue #21, the mandatory dry-run local deletion safety, is open). Local disk usage only
  grows today; nothing prunes it automatically.
- Last-known-good protection (FR-19, issue #20) is also not implemented, so don't rely on
  "the newest good backup is protected" as a reason retention hasn't touched something,
  either. Nothing is currently touching anything.

If local disk usage is a concern before #21 lands, that's a manual housekeeping problem
today, and the GFS verdicts above (`GFSDecide`, callable against the journal) are the
closest thing to a source of truth for "what would be safe to remove," not a live guarantee
that removal is happening or will happen automatically.

## Quick reference

| Symptom in the journal | Meaning | What to do |
|---|---|---|
| Newest row is `COMMITTED`/`REMOTE_DELETE_PENDING`/`COMPLETE`, recent | Healthy | Nothing |
| No good row inside `stale_after` | Stale | Investigate why new backups aren't landing |
| `FAILED`, no `next_retry_at` | Retry budget exhausted | Human intervention needed |
| `QUARANTINED_LOST` anywhere | Irrecoverable loss | Restore from the next-newest good row; report the gap honestly |
| Newest good row is `QUARANTINED` | Content suspect, source may still exist | Manual re-fetch or re-run reconciliation yourself |
| `REMOTE_DELETE_PENDING` stuck, `remote_delete_error` set | Expected refusal under a hardened SFTP account | Monitor remote disk directly; this is not corrupting anything |
| `Keep: false` from GFS but the file is still there | Expected; nothing deletes local files yet | Manual housekeeping if disk space matters before #21 lands |
