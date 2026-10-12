-- silod's leader (docs/silo/state.md, "More than one silod, and the
-- leader", SV-52) and its reconcile (docs/silo/gcp-backend.md,
-- "Reconcile", SV-77).
--
-- leader_epoch holds the leader epoch: a counter each new holder of
-- the "leader" lease bumps as it takes it. Every leader-only write
-- checks, in its own transaction, that the epoch is still the one its
-- leader took, so a leader that stalled past losing the lease writes
-- nothing.
CREATE TABLE leader_epoch (
    name     TEXT    PRIMARY KEY,
    epoch    BIGINT  NOT NULL,
    holder   TEXT    NOT NULL,
    taken_at BIGINT  NOT NULL
);

-- What a replica's heartbeat says besides its release: how many
-- sessions it serves, and the address it serves them on.
ALTER TABLE replicas ADD COLUMN sessions INTEGER NOT NULL DEFAULT 0;
ALTER TABLE replicas ADD COLUMN address TEXT NOT NULL DEFAULT '';

-- The replica watching each facade operation: the one that started it,
-- or the leader once it has adopted it from a replica whose heartbeat
-- stopped. Empty for an operation started before this column.
ALTER TABLE facade_operations ADD COLUMN replica TEXT NOT NULL DEFAULT '';

-- The objects the reconcile has reported: orphans, cloud objects with
-- the label or prefix of a silo that doesn't exist, which it deletes an
-- hour of database time after first_seen unless reconcile is
-- report-only; and foreign objects, in the VPC with neither, whose silo
-- is ''. object is "<collection>:<scope>/<real name>", or
-- "<collection>:<real name>" for a global one.
CREATE TABLE reconcile_orphans (
    object     TEXT   PRIMARY KEY,
    silo       TEXT   NOT NULL,
    first_seen BIGINT NOT NULL
);
