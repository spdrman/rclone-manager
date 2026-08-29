-- 0001_init: the FR-9 lifecycle journal.
--
-- One row per artifact, keyed by its FR-7 backup set plus its FR-8 artifact
-- name. state_transitions is the append-only, idempotency-keyed log that
-- internal/state.Journal.RecordTransition writes to; it is what makes a
-- transition survive a crash-and-retry exactly once rather than zero or two
-- times. schema_migrations is bootstrapped separately by the Go migration
-- runner (see internal/state/migrate.go), not by a numbered migration here,
-- because it has to exist before any numbered migration can be recorded
-- against it.

CREATE TABLE artifacts (
    id                    INTEGER PRIMARY KEY,

    -- Identity (FR-7, FR-8): keys on model.BackupSetID + model.ArtifactID,
    -- never on an ad-hoc string.
    source                TEXT NOT NULL,
    backup_set            TEXT NOT NULL,
    artifact_name         TEXT NOT NULL,

    -- Paths.
    remote_path           TEXT NOT NULL,
    local_path            TEXT NOT NULL DEFAULT '',

    -- Lifecycle state (FR-10). The state machine itself, and the Go type
    -- for these values, belongs to a different package (see the FR-10
    -- issue); this column only stores whatever string that package
    -- produces, guarded by the same set of names the EPIC specifies so a
    -- typo upstream can't silently invent a new state.
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
                              'QUARANTINED'
                          )),

    discovered_at         TEXT NOT NULL,
    updated_at            TEXT NOT NULL,

    -- Remote metadata captured at discovery (FR-16). Every attribute here
    -- is optional because backends do not all report every one of them:
    -- NULL (or '' for the text columns) means "this backend did not
    -- supply this", not "the value is zero/empty".
    remote_size           INTEGER,
    remote_mtime          TEXT,
    remote_hash           TEXT NOT NULL DEFAULT '',
    remote_hash_alg       TEXT NOT NULL DEFAULT '',
    remote_backend_id     TEXT NOT NULL DEFAULT '',

    -- Transfer results (FR-11, FR-13).
    transfer_bytes        INTEGER,
    transfer_checksummed  INTEGER NOT NULL DEFAULT 0 CHECK (transfer_checksummed IN (0, 1)),

    -- Hashes and validation (FR-13).
    local_hash            TEXT NOT NULL DEFAULT '',
    local_hash_alg        TEXT NOT NULL DEFAULT '',
    validation_passed     INTEGER CHECK (validation_passed IN (0, 1)),
    validation_detail     TEXT NOT NULL DEFAULT '',

    -- Retry information. FR-22 owns retry policy; this column set only
    -- stores what that policy decides.
    retry_count           INTEGER NOT NULL DEFAULT 0,
    last_error            TEXT NOT NULL DEFAULT '',
    next_retry_at         TEXT,

    -- Remote deletion status (FR-15).
    remote_deleted_at     TEXT,
    remote_delete_error   TEXT NOT NULL DEFAULT '',

    -- Retention classification. FR-18 owns GFS policy; this column set
    -- only stores its verdict.
    retention_tier        TEXT NOT NULL DEFAULT '',
    retention_expires_at  TEXT,

    UNIQUE (source, backup_set, artifact_name)
);

CREATE INDEX idx_artifacts_state ON artifacts (state);
CREATE INDEX idx_artifacts_backup_set ON artifacts (source, backup_set);

-- state_transitions is the append-only idempotency log. idempotency_key is
-- supplied by the caller and is unique: replaying the same key is how a
-- crashed-and-retried transition is recognised as already applied instead
-- of being applied a second time. See RecordTransition in internal/state.
CREATE TABLE state_transitions (
    id               INTEGER PRIMARY KEY,
    artifact_id      INTEGER NOT NULL REFERENCES artifacts (id),
    idempotency_key  TEXT NOT NULL UNIQUE,
    from_state       TEXT NOT NULL,
    to_state         TEXT NOT NULL,
    occurred_at      TEXT NOT NULL,
    detail           TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_state_transitions_artifact ON state_transitions (artifact_id);
