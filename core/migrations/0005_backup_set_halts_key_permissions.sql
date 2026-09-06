-- Widen the backup_set_halts.reason CHECK to admit KEY_PERMISSIONS
-- (issue #293).
--
-- WHY THIS EXISTS. 0004 declared exactly two halt reasons, both of them
-- refusals a REMOTE host produced: a changed SSH host key, or a rejected
-- login. Issue #293 found a third refusal that never touches the network
-- at all: internal/transport/rclone/ssh.go now refuses to build a
-- connection's own options when a configured key_file's on-disk mode no
-- longer matches what core/service/backupsets.go's importSSHKeyInto wrote
-- it with (0600), because nothing was checking that again before the key
-- was next used and a real deployment found it had silently drifted to
-- world-writable. That refusal is worth recording exactly like the other
-- two, and worth telling apart from AUTHENTICATION_FAILED specifically:
-- one is a question for the remote account, the other is a question for
-- this filesystem, and before this migration both looked identical from
-- the Web UI.
--
-- SQLite cannot alter a CHECK constraint in place, so this recreates the
-- table, exactly as 0002 already did for artifacts.state.

CREATE TABLE backup_set_halts_new (
    source      TEXT NOT NULL,
    backup_set  TEXT NOT NULL,

    reason      TEXT NOT NULL CHECK (reason IN (
                    'HOST_KEY_CHANGED',
                    'AUTHENTICATION_FAILED',
                    'KEY_PERMISSIONS'
                )),

    observed_at TEXT NOT NULL,

    PRIMARY KEY (source, backup_set)
);

INSERT INTO backup_set_halts_new SELECT * FROM backup_set_halts;

DROP TABLE backup_set_halts;

ALTER TABLE backup_set_halts_new RENAME TO backup_set_halts;
