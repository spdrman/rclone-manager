-- 0008_backup_set_addresses: the address a backup set id was last
-- configured with, kept after its configuration is gone (issue #411).
--
-- WHY THIS EXISTS. A backup set is identified by its source and its name,
-- because that is what model.NewArtifactID keys every artifact by. So
-- removing a set's configuration and creating a set with the same id
-- again hands the new one every artifact the old one produced. That is
-- deliberate and it is what undoing a removal needs (see
-- core/service/backupsetremove.go), but it means a set can be created
-- over history while pointing at completely different data, which is the
-- state core/service/backupsetrepoint.go refuses on the update path and
-- had no way to even notice on the create path.
--
-- Noticing it needs an address to compare against, and the artifacts
-- table does not hold one: artifacts.remote_path is the path of one
-- object RELATIVE to its backup set's own root, and neither the root nor
-- the host it is on has ever been recorded anywhere. artifacts.local_path
-- is the exception, it is a full path, so the local root is derivable
-- from it, and that half is checked straight off the journal with no help
-- from this table.
--
-- WHY IT IS WRITTEN BY REMOVAL, AND ONLY BY REMOVAL. Removal is the one
-- event that frees an id up, so it is the event this is about, and it is
-- also the one moment when the address is both current and about to stop
-- being knowable. Writing it there means the record is complete for every
-- id whose configuration was removed by this build, with nothing to
-- backfill: a create can only follow a removal that already happened.
--
-- What a reader finds is therefore the address that id had when its
-- configuration was last REMOVED, which is the last address it was
-- configured with in every case that goes through this manager. The one
-- case where the two differ is a set that was removed, created again
-- somewhere else with an acknowledgement, and then taken out of
-- config.yaml by a hand edit rather than by a removal. The row then names
-- the older of the two addresses, and a create at the newer one is asked
-- about when it need not have been. That is a refusal an operator can
-- acknowledge, in the direction that errs toward asking, and closing it
-- would mean writing this row from every path that persists a set rather
-- than from the one event this is about.
--
-- WHY THIS IS NOT A TOMBSTONE. backupsetremove.go is explicit that a
-- removed set does not stay in the catalog in any form, and this does not
-- put it back: nothing lists these rows, no read surface can see one, and
-- GET on the id still answers 404. A row here says nothing about a set
-- existing. It says where a set that used to exist was pointing, which is
-- a question only the next create over that id ever asks.
--
-- WHY ONE ROW PER SET, UPDATED IN PLACE. The only question this answers
-- is "where was this id last pointing", and the most recent answer
-- answers it completely. backup_set_halts (0004) is the precedent for
-- both the shape and the identity columns.

CREATE TABLE backup_set_addresses (
    -- model.BackupSetID's own two halves, stored the way the artifacts
    -- and backup_set_halts tables already store them, so nothing has to
    -- split a joined "source/set" string back apart.
    source      TEXT NOT NULL,
    backup_set  TEXT NOT NULL,

    -- The three fields that together are what "the data this set is
    -- about" means (core/service/backupsetrepoint.go names them and says
    -- why these three and not port or user). host is '' for a remote
    -- type that has none, which is a real value here and not a gap: a
    -- local-type set genuinely has no host, and a later create naming one
    -- IS a move.
    host        TEXT NOT NULL DEFAULT '',
    remote_path TEXT NOT NULL,
    local_path  TEXT NOT NULL,

    -- When this address was recorded, which is when the set's
    -- configuration was removed. It is here for the same reason
    -- backup_set_halts.observed_at is: a durable fact with no time on it
    -- cannot be reasoned about at all.
    recorded_at TEXT NOT NULL,

    PRIMARY KEY (source, backup_set)
);
