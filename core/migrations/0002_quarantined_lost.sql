-- Widen the artifacts.state CHECK to admit QUARANTINED_LOST.
--
-- WHY THIS EXISTS. 0001 was written against the eleven states the EPIC lists
-- under FR-10. The state machine then grew a twelfth, QUARANTINED_LOST, for
-- the case the EPIC does not cover: an artifact whose local copy is found
-- corrupt after COMPLETE, when the remote source has already been deleted and
-- so there is nothing left to re-fetch. That is irrecoverable loss of a
-- restore point, it is terminal by design, and it needs to be recordable.
--
-- The two landed in parallel and nothing caught the mismatch, because a CHECK
-- constraint is not a compile error. Building and testing the repository
-- passes either way; the failure only appears at runtime, the first time
-- something tries to write the state, in the exact path that handles data
-- loss. The regression test in internal/state now compares this list against
-- the state machine's own set so the two cannot drift apart again silently.
--
-- SQLite cannot alter a CHECK constraint in place, so this recreates the
-- table. state_transitions references artifacts(id), but foreign keys are not
-- enabled on this connection, so the drop and rename below do not disturb it.

CREATE TABLE artifacts_new (
    id                    INTEGER PRIMARY KEY,

    source                TEXT NOT NULL,
    backup_set            TEXT NOT NULL,
    artifact_name         TEXT NOT NULL,

    remote_path           TEXT NOT NULL,
    local_path            TEXT NOT NULL DEFAULT '',

    state                 TEXT NOT NULL DEFAULT 'DISCOVERED'
                          CHECK (state IN (
                              'DISCOVERED',
                              'TRANSFERRING',
                              'TRANSFERRED',
                              'VERIFYING',
                              'VERIFIED',
                              'COMMITTING',
                              'COMMITTED',
                              'REMOTE_DELETE_PENDING',
                              'COMPLETE',
                              'FAILED',
                              'QUARANTINED',
                              'QUARANTINED_LOST'
                          )),

    discovered_at         TEXT NOT NULL,
    updated_at            TEXT NOT NULL,

    remote_size           INTEGER,
    remote_mtime          TEXT,
    remote_hash           TEXT NOT NULL DEFAULT '',
    remote_hash_alg       TEXT NOT NULL DEFAULT '',
    remote_backend_id     TEXT NOT NULL DEFAULT '',

    transfer_bytes        INTEGER,
    transfer_checksummed  INTEGER NOT NULL DEFAULT 0 CHECK (transfer_checksummed IN (0, 1)),

    local_hash            TEXT NOT NULL DEFAULT '',
    local_hash_alg        TEXT NOT NULL DEFAULT '',
    validation_passed     INTEGER CHECK (validation_passed IN (0, 1)),
    validation_detail     TEXT NOT NULL DEFAULT '',

    retry_count           INTEGER NOT NULL DEFAULT 0,
    last_error            TEXT NOT NULL DEFAULT '',
    next_retry_at         TEXT,

    remote_deleted_at     TEXT,
    remote_delete_error   TEXT NOT NULL DEFAULT '',

    retention_tier        TEXT NOT NULL DEFAULT '',
    retention_expires_at  TEXT,

    UNIQUE (source, backup_set, artifact_name)
);

INSERT INTO artifacts_new SELECT * FROM artifacts;

DROP TABLE artifacts;

ALTER TABLE artifacts_new RENAME TO artifacts;

CREATE INDEX idx_artifacts_state ON artifacts (state);
CREATE INDEX idx_artifacts_backup_set ON artifacts (source, backup_set);
