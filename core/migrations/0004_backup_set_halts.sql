-- 0004_backup_set_halts: the durable per-backup-set connection refusal
-- (issue #245).
--
-- FR-6 refuses a connection whose SSH host key no longer matches
-- known_hosts, and that refusal is complete: nothing is listed, nothing is
-- transferred, nothing is deleted, and the set backs up nothing for as
-- long as it stands. Until this table the fact was computed transiently
-- inside the cycle's alert pass and handed to a notification sink, so an
-- operator was told once and every read surface afterwards reported the
-- set as merely stale.
--
-- Why this is not the transition log. state_transitions is the
-- append-only record of what happened to an ARTIFACT, and a refused
-- connection produces no artifact to transition: there is no row it could
-- be written as, which is the same reason GET /activity cannot carry it.
--
-- Why one row per backup set, updated in place, rather than a history.
-- The only question this answers is "can the manager currently reach this
-- backup set", and the most recent observation answers it completely. A
-- history would need a "latest" query to answer the same question, grow
-- without bound, and have no reader: the artifact journal is where this
-- deployment's history lives. The operations table is the precedent for a
-- mutable row here, and its status CHECK is the precedent for pinning a
-- small vocabulary in the schema.
--
-- A row exists exactly while the manager's last real observation of this
-- backup set was a refusal. A cycle that runs the set to completion
-- deletes it, and nothing else does: a cycle that failed for some other
-- reason never got far enough to say either way, so it leaves whatever is
-- here alone. That is §77 invariant 5 read from the storage side: absence
-- of the refusal is not evidence the key was re-trusted.

CREATE TABLE backup_set_halts (
    -- model.BackupSetID's own two halves, stored the way the artifacts
    -- table already stores them, so nothing has to split a joined
    -- "source/set" string back apart.
    source      TEXT NOT NULL,
    backup_set  TEXT NOT NULL,

    -- Why the manager could not connect. The CHECK is the vocabulary:
    -- these are the two transport categories that mean the connection
    -- itself was refused and therefore that no backup ran. Everything
    -- else the FR-22 classifier produces (a missing directory, a
    -- permission failure on the remote path, a transient network error
    -- mid-transfer) can happen AFTER a connection is established, so it
    -- is not evidence about the connection and never lands here.
    reason      TEXT NOT NULL CHECK (reason IN ('HOST_KEY_CHANGED', 'AUTHENTICATION_FAILED')),

    -- When this refusal was last observed. It is deliberately NOT served
    -- as a "halted since": it is the most recent observation, not the
    -- first one, and reporting the latest under a word that means the
    -- earliest would be a confident wrong number. It is here because a
    -- durable fact with no time on it cannot be reasoned about at all.
    observed_at TEXT NOT NULL,

    PRIMARY KEY (source, backup_set)
);
