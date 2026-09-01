-- Widen the artifacts.state CHECK to admit REMOTE_RETAINED (issue #282).
--
-- WHY THIS EXISTS. A backup set can now be declared read-only
-- (config.BackupSet.ReadOnly): FR-15's delete step is never offered its
-- artifacts, and an artifact that reaches COMMITTED under such a set is
-- routed to REMOTE_RETAINED instead of REMOTE_DELETE_PENDING -> COMPLETE.
-- Exactly the same drift 0002 fixed for QUARANTINED_LOST applies here: the
-- state machine (core/internal/lifecycle) and this CHECK constraint are two
-- independently-maintained lists, and internal/state's own
-- TestJournalAcceptsExactlyTheStatesTheMachineDefines is what catches them
-- disagreeing, at build time rather than the first time something tries to
-- retain an artifact's remote source.
--
-- SQLite cannot alter a CHECK constraint in place, so this recreates the
-- table, exactly as 0002 did. state_transitions references artifacts(id),
-- but foreign keys are not enabled on this connection, so the drop and
-- rename below do not disturb it.

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
                              'REMOTE_RETAINED',
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
