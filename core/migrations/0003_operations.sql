-- 0003_operations: the B1.5 durable operation table (docs/EPIC-B-multi-nas.md
-- §14, issue #94).
--
-- The HTTP API layer (apps/common/webhost, via core/service) persists one
-- row here BEFORE it starts executing anything, so a browser disconnect, a
-- page close, or a gateway timeout can never implicitly cancel the work: the
-- row, not the HTTP request, is the source of truth for "is this operation
-- still going". idempotency_key is UNIQUE for exactly the same reason
-- state_transitions.idempotency_key is in 0001_init: a client retry that
-- reuses the same key must find the original row instead of creating a
-- second one. See internal/state/operations.go for the Go side of this
-- contract.

CREATE TABLE operations (
    id                INTEGER PRIMARY KEY,

    -- operation_id is the public identifier ("op_...") returned to the API
    -- caller; it is independent of the local integer rowid so it never
    -- reveals table cardinality.
    operation_id      TEXT NOT NULL UNIQUE,
    idempotency_key   TEXT NOT NULL UNIQUE,

    -- Snapshot fields §14 requires captured before execution begins.
    actor             TEXT NOT NULL DEFAULT '',
    -- backup_set is empty for an operation that is not scoped to one
    -- configured backup set (the only action this schema currently serves,
    -- run_cycle, always is): later phases that add per-backup-set actions
    -- populate it with that backup set's source/name identity.
    backup_set        TEXT NOT NULL DEFAULT '',
    config_revision   TEXT NOT NULL DEFAULT '',
    action            TEXT NOT NULL,
    -- parameters is an opaque JSON object: this package does not know or
    -- care what a given action's safety-relevant parameters look like, it
    -- only has to persist and return them unchanged.
    parameters        TEXT NOT NULL DEFAULT '{}',

    status            TEXT NOT NULL DEFAULT 'queued'
                      CHECK (status IN ('queued', 'running', 'completed', 'failed')),

    created_at        TEXT NOT NULL,
    started_at        TEXT,
    finished_at       TEXT,

    -- result/error are opaque, human/JSON-readable summaries of how the
    -- operation ended. Neither is allowed to carry an rclone-native or
    -- SQLite-schema type; the boundary that enforces that lives in
    -- core/service, not in this schema.
    result            TEXT NOT NULL DEFAULT '',
    error             TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_operations_status ON operations (status);
