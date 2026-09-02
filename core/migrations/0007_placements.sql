-- 0007_placements: FR-29, placement is journal truth
-- (docs/EPIC-E-alternative-storage.md, issue #236).
--
-- WHY A TABLE AND NOT COLUMNS. An artifact's location stops being a single
-- fact the moment EPIC E's move engine exists: during a move an artifact
-- has two durable copies, and after one it has no local copy at all.
-- Neither shape fits a column on the artifact row, so location moves into
-- its own table with one row per durable copy.
--
-- WHY THE NUMBER IS 0007 AND NOT 0004. FR-29 and issue #236 both name this
-- file 0004_placements.sql. They were written when 3 was the highest
-- applied version; 0004, 0005 and 0006 have since been taken by
-- backup_set_halts, its KEY_PERMISSIONS widening, and REMOTE_RETAINED. The
-- runner keys everything off the number in the filename and refuses two
-- files claiming one version (see internal/state/migrate.go), so this takes
-- the next free one. Nothing else about FR-29 changes.
--
-- WHAT THE BACKFILL IS ALLOWED TO SAY. This migration is SQL inside the
-- runner's transaction. It cannot stat a file, so it cannot discover
-- anything, so every value it writes is a restatement of something the
-- journal already records. That is the whole design: after 0007 an ACTIVE
-- local placement asserts exactly what artifacts.local_path asserts today
-- and not one thing more, which is what makes the sweep of the
-- "LocalPath is readable" call sites behaviour-neutral rather than merely
-- believed to be.
--
-- In particular it does NOT reconstruct a location from a configured
-- backup root. A path recomputed from config would be right on a
-- developer's machine and wrong on any deployment whose local_path has
-- ever changed, and it would be a claim about the filesystem this
-- migration has no way to check. The location is artifacts.local_path,
-- verbatim, empty string included.

CREATE TABLE placements (
    id                  INTEGER PRIMARY KEY,

    artifact_id         INTEGER NOT NULL REFERENCES artifacts (id),

    -- medium is config.MediumLocal ('local') or the id of one of
    -- config.StorageMediums (#360). It is a plain string here for the same
    -- reason artifacts.state is: the vocabulary is owned by the config
    -- package, this schema only stores what that package produces. It is
    -- deliberately NOT a foreign key to a mediums table, because there
    -- isn't one and shouldn't be: a medium is declared in config.yaml, and
    -- an artifact's recorded placement has to survive an operator deleting
    -- that declaration, so the journal can still say where the bytes went.
    medium              TEXT NOT NULL,

    -- location is an absolute path for a local placement and an object key
    -- for a medium one. Empty is meaningful and is not an error: it is what
    -- the journal says for an artifact that has not landed anywhere yet
    -- (a DISCOVERED row's local_path), and it reads as "no location
    -- recorded", exactly as an empty local_path does today.
    location            TEXT NOT NULL,

    -- size_bytes is what this product measured, not what a remote claimed.
    -- NULL means it never measured one. The backfill takes it from
    -- artifacts.transfer_bytes and never from remote_size: the remote's
    -- reported size is the remote's assertion about the remote object, and
    -- a placement is a statement about a copy this product wrote.
    size_bytes          INTEGER,

    -- hash/hash_alg are the locally computed content hash (FR-13), copied
    -- from artifacts.local_hash / local_hash_alg. Empty means none
    -- recorded, the same convention the artifacts table uses.
    hash                TEXT NOT NULL DEFAULT '',
    hash_alg            TEXT NOT NULL DEFAULT '',

    -- verification_class is FR-31's ladder: what has actually been PROVEN
    -- about this copy, never what is assumed about it. Empty means nothing
    -- has been. The ladder itself, and everything that can raise a medium
    -- placement above 'existence', is #237's; what this column does today
    -- is refuse to record a class nobody achieved.
    verification_class  TEXT NOT NULL DEFAULT ''
                        CHECK (verification_class IN ('', 'existence', 'attested', 'content')),

    -- verified_at is when that class was last achieved, NULL when it never
    -- was. A non-empty class with a NULL verified_at is legal and real: a
    -- catalog-rebuilt row carries a hash out of its sidecar manifest with
    -- no recoverable moment attached to it.
    verified_at         TEXT,

    -- status is FR-29's placement state machine. ACTIVE means this copy is
    -- the journal's answer to "where does this artifact live". GONE and
    -- DELETE_PENDING are the move engine's (#238); nothing writes them yet.
    status              TEXT NOT NULL DEFAULT 'ACTIVE'
                        CHECK (status IN ('ACTIVE', 'DELETE_PENDING', 'GONE')),

    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);

-- One artifact has at most one copy on any one medium at any one location.
CREATE UNIQUE INDEX idx_placements_artifact_medium_location
    ON placements (artifact_id, medium, location);

-- And exactly one LOCAL copy, ever. There is one landing path per artifact
-- (internal/lifecycle derives it from the backup set's local_path and the
-- artifact name), so two local placements for one artifact is not a state
-- this product has, and the database refuses it rather than trusting every
-- future writer to remember. A partial index rather than a plain UNIQUE
-- because a move genuinely does put one artifact on two DIFFERENT mediums
-- at once, which stays legal.
CREATE UNIQUE INDEX idx_placements_one_local_per_artifact
    ON placements (artifact_id) WHERE medium = 'local';

CREATE INDEX idx_placements_artifact ON placements (artifact_id);
CREATE INDEX idx_placements_medium_status ON placements (medium, status);

-- placement_moves is FR-30's move journal: one row per migration of one
-- artifact from one placement to one destination, written BEFORE every
-- side effect so a crash mid-move is recoverable from the row rather than
-- from the filesystem. The move engine that writes it is #238; this
-- migration builds the table and pins its phase vocabulary so that engine
-- cannot quietly invent a phase, the same way artifacts.state's CHECK
-- pins FR-10's.
CREATE TABLE placement_moves (
    id                    INTEGER PRIMARY KEY,

    artifact_id           INTEGER NOT NULL REFERENCES artifacts (id),

    -- The placement the bytes are being copied FROM. It is a hard
    -- reference because the whole safety argument of FR-30 is that the
    -- source copy survives every uncertainty: a move row that cannot say
    -- which copy it is preserving is worse than no row.
    source_placement_id   INTEGER NOT NULL REFERENCES placements (id),

    -- Where they are going. Recorded as medium plus location rather than
    -- as a destination placement id, because at PLANNED and COPYING there
    -- is no destination placement yet, and the key is deterministic
    -- (FR-28), so restarting an interrupted upload targets the same object.
    destination_medium    TEXT NOT NULL,
    destination_location  TEXT NOT NULL,

    phase                 TEXT NOT NULL DEFAULT 'PLANNED'
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

    bytes_copied          INTEGER NOT NULL DEFAULT 0,
    error                 TEXT NOT NULL DEFAULT '',

    started_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL,
    finished_at           TEXT
);

CREATE INDEX idx_placement_moves_phase ON placement_moves (phase);
CREATE INDEX idx_placement_moves_artifact ON placement_moves (artifact_id);

-- The backfill, in the same transaction as the CREATEs above and as the
-- schema_migrations row the runner writes: either every pre-existing
-- artifact comes out of this with its placement or the database comes out
-- of it untouched at version 6. There is no arrangement in between.
--
-- Every artifact row gets exactly one, including a DISCOVERED one that has
-- landed nowhere yet, because the point of this backfill is that no code
-- path ever has to handle "an artifact with no placements". An artifact
-- that has landed nowhere gets a placement whose location is empty, which
-- is the same "nowhere yet" its local_path already says.
--
-- verification_class is 'content' only where the journal holds BOTH a
-- locally computed hash and a recorded VERIFIED entry, which together are
-- this product's own record of having read the bytes back and hashed them
-- (FR-13). A hash with no VERIFIED entry is what catalog rebuild produces
-- out of a sidecar manifest: real evidence, no recoverable moment, so the
-- class stays empty rather than borrow a timestamp from somewhere else.
--
-- created_at/updated_at are the artifact row's own timestamps, not the
-- moment this migration ran. A backfilled placement did not come into
-- existence when the operator upgraded, and stamping it with the upgrade
-- time would make every pre-existing copy look brand new to anything that
-- later reasons about placement age.
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
    CASE WHEN a.local_hash <> '' AND (
             SELECT count(*) FROM state_transitions t
              WHERE t.artifact_id = a.id AND t.to_state = 'VERIFIED' AND t.from_state <> 'VERIFIED'
         ) > 0 THEN 'content' ELSE '' END,
    CASE WHEN a.local_hash <> '' THEN (
             SELECT t.occurred_at FROM state_transitions t
              WHERE t.artifact_id = a.id AND t.to_state = 'VERIFIED' AND t.from_state <> 'VERIFIED'
              ORDER BY t.id DESC LIMIT 1
         ) ELSE NULL END,
    'ACTIVE',
    a.discovered_at,
    a.updated_at
FROM artifacts a;
