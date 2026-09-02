-- 0007_placements: where an artifact's bytes actually are (EPIC E, FR-29).
--
-- WHY THIS EXISTS. Until now an artifact had exactly one durable copy and
-- the journal named it in one column, artifacts.local_path. EPIC E makes
-- the storage medium selectable per retention tier, which means an
-- artifact can have several copies at once (mid-move) and no local copy at
-- all (after one), so "where is this artifact" stops being a property of
-- the artifact row and becomes a set of rows of its own.
--
-- WHY THE FILE IS 0007 AND THE ISSUE SAYS 0004. Issue #236 and the spec
-- were written when 0003 was the newest migration; three more landed
-- (0004 backup-set halts, 0005 key permissions, 0006 remote-retained)
-- before this one. The spec's own instruction is "the next free
-- core/migrations/NNNN_*.sql", and that is 0007. Nothing depends on the
-- number beyond ordering, and loadMigrations refuses two files claiming
-- one version, so taking 0004 was never an option.
--
-- Nothing in Phase 1 writes a non-local placement and nothing deletes one.
-- This migration and the code around it are the load-bearing wall the move
-- engine (#238) is built on, and the exit gate for this phase is that no
-- new deletion path exists anywhere.

CREATE TABLE placements (
    id                  INTEGER PRIMARY KEY,
    artifact_id         INTEGER NOT NULL REFERENCES artifacts (id),

    -- medium is 'local' (config.MediumLocal, the implicit medium every
    -- deployment already has) or the id of a configured storage medium.
    -- It is not constrained to a closed set here on purpose: the set of
    -- legal medium ids lives in the operator's configuration, which this
    -- schema cannot see and must not freeze a copy of.
    medium              TEXT NOT NULL,

    -- location is an absolute path for a local placement and an object key
    -- for a medium placement. Only the store named by `medium` knows how
    -- to interpret it, which is exactly artifactstore.Store.Locator's own
    -- contract.
    location            TEXT NOT NULL,

    -- size_bytes is what this copy measures, and it is NULL rather than 0
    -- when unknown: an artifact can genuinely be zero bytes, so a zero
    -- must not double as "nobody recorded this".
    size_bytes          INTEGER,

    -- hash / hash_alg are the content hash recorded FOR THIS COPY. For a
    -- backfilled local placement that is the artifact's own local_hash,
    -- computed by reading the bytes during FR-13 verification.
    hash                TEXT NOT NULL DEFAULT '',
    hash_alg            TEXT NOT NULL DEFAULT '',

    -- verification_class is the strongest class of verification this copy
    -- has actually ACHIEVED (FR-31), never the strongest one configured.
    -- Empty means nothing has verified it. The vocabulary is owned by
    -- core/internal/placement (#237), the same way artifacts.state's
    -- vocabulary is owned by core/internal/lifecycle rather than by this
    -- schema; the CHECK here is the same kind of backstop 0001 wrote for
    -- that column, and #237 carries the test that pins the two lists
    -- together.
    verification_class  TEXT NOT NULL DEFAULT ''
                        CHECK (verification_class IN ('', 'content', 'attested', 'existence')),

    -- verified_at is when verification_class was last achieved, or NULL.
    verified_at         TEXT,

    -- status is this copy's own lifecycle, distinct from the artifact's.
    -- ACTIVE: the copy is meant to be there. DELETE_PENDING: a delete has
    -- been decided and durably recorded but may not have happened yet.
    -- GONE: the copy is no longer there and the journal knows it.
    status              TEXT NOT NULL DEFAULT 'ACTIVE'
                        CHECK (status IN ('ACTIVE', 'DELETE_PENDING', 'GONE')),

    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,

    -- One placement per artifact per medium, which is not merely a tidiness
    -- rule: FR-28's key layout is deterministic and carries no timestamp,
    -- so one artifact has exactly one key on any given medium. Two rows
    -- here for one (artifact, medium) pair would mean the journal believed
    -- in two objects that are in fact the same object, and an interrupted
    -- upload that resumes would create the second one.
    UNIQUE (artifact_id, medium)
);

-- "which artifacts live on medium X, and which of those copies are still
-- meant to be there" is the question retention planning and reconciliation
-- both ask. artifact_id is already indexed as the leading column of the
-- UNIQUE constraint above, so it needs nothing of its own.
CREATE INDEX idx_placements_medium_status ON placements (medium, status);

-- placement_moves is FR-30's journal: one row per attempted migration of
-- one artifact from one placement to one medium, written to BEFORE every
-- side effect so a crash can be reconciled rather than guessed at.
--
-- It is created empty here and stays empty for the whole of Phase 1.
-- Nothing writes it until the move engine (#238) exists. It lands now
-- because the migration that introduces placements is the migration that
-- should introduce the journal that moves them: splitting the two would
-- mean a second schema bump for a table whose shape is already decided.
CREATE TABLE placement_moves (
    id                  INTEGER PRIMARY KEY,
    artifact_id         INTEGER NOT NULL REFERENCES artifacts (id),

    -- source_placement_id is the copy being moved FROM. It is nullable
    -- because a move whose source has already been deleted (the last
    -- phase) must still be able to name what it did.
    source_placement_id INTEGER REFERENCES placements (id),

    destination_medium  TEXT NOT NULL,
    destination_key     TEXT NOT NULL,

    -- phase is FR-30's three-phase state machine. The ordering matters and
    -- is stated in the spec: the source copy is deleted only after
    -- VERIFIED is durably recorded, and ABANDONED means the destination
    -- was cleaned up and the source was never touched.
    phase               TEXT NOT NULL DEFAULT 'PLANNED'
                        CHECK (phase IN (
                            'PLANNED',
                            'COPYING',
                            'COPIED',
                            'VERIFYING',
                            'VERIFIED',
                            'SOURCE_DELETE_PENDING',
                            'DONE',
                            'ABANDONED'
                        )),

    bytes_copied        INTEGER,
    error               TEXT NOT NULL DEFAULT '',

    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);

-- Restart reconciliation reads every non-terminal move on startup, so the
-- index it needs is on phase.
CREATE INDEX idx_placement_moves_phase ON placement_moves (phase);
CREATE INDEX idx_placement_moves_artifact ON placement_moves (artifact_id);

-- THE BACKFILL. Every artifact that already has a durable local copy gets
-- the placement row describing it, so no code reading placements has to
-- have a special case for "this database predates placements".
--
-- It runs inside this migration's own transaction, along with the
-- schema_migrations row that records the version: internal/state's
-- applyMigration wraps every statement in a file plus that insert in one
-- BEGIN/COMMIT, so an interruption anywhere leaves the database at version
-- 6 with no placements table at all, never at version 7 with a half-filled
-- one. That is asserted by a test rather than assumed.
--
-- WHICH ROWS. Only those in a state that means the local copy is durable,
-- and only those with a local path. local_path carries two meanings during
-- an artifact's life: at TRANSFERRING it is the .partial file being
-- written, and only from COMMITTED onward is it the final artifact. A
-- placement is one row per DURABLE copy, so an in-flight partial is not
-- one, and backfilling it would put a row in this table claiming a
-- committed copy exists where only a half-written file does.
--
-- An artifact before its transfer therefore has zero placements, and that
-- is correct rather than a gap: it has zero copies. The invariant this
-- table is here to hold is that an artifact with durable bytes always has
-- a row saying where they are, and the test for it checks both halves,
-- that every durable row got one and that every non-durable row did not.
--
-- WHAT IS NOT INVENTED. verified_at stays NULL. The journal never recorded
-- when a local hash was checked as a fact separate from the artifact row's
-- own updated_at, so there is no honest value to put there, and putting
-- updated_at in would make every backfilled placement claim a verification
-- time it does not have. created_at comes from the artifact's own
-- COMMITTED transition where the transition log still has one, because
-- that is genuinely when the copy came into being, and falls back to the
-- artifact's updated_at where it does not.
--
-- size_bytes comes from transfer_bytes, which is what was actually written
-- locally, and never from remote_size, which is what the REMOTE said. They
-- are usually equal and they are not the same fact.
INSERT INTO placements (
    artifact_id, medium, location, size_bytes, hash, hash_alg,
    verification_class, verified_at, status, created_at, updated_at
)
SELECT
    a.id,
    'local',
    a.local_path,
    a.transfer_bytes,
    a.local_hash,
    a.local_hash_alg,
    CASE
        -- A quarantined artifact is precisely the one whose local copy did
        -- NOT verify, whatever hash is recorded beside it, so it must not
        -- be backfilled as content-verified.
        WHEN a.state IN ('QUARANTINED', 'QUARANTINED_LOST') THEN ''
        WHEN a.local_hash <> '' THEN 'content'
        ELSE ''
    END,
    NULL,
    'ACTIVE',
    COALESCE(
        (SELECT MAX(t.occurred_at) FROM state_transitions t
          WHERE t.artifact_id = a.id AND t.to_state = 'COMMITTED'),
        a.updated_at
    ),
    a.updated_at
FROM artifacts a
WHERE a.local_path <> ''
  AND a.state IN (
      'COMMITTED',
      'REMOTE_DELETE_PENDING',
      'COMPLETE',
      'REMOTE_RETAINED',
      'QUARANTINED',
      'QUARANTINED_LOST'
  );
