-- Each silod replica's heartbeat (docs/silo/state.md, "More than one
-- silod" and "Schema migrations and upgrades", SV-53). id is the
-- replica's own, chosen at start; release is its release, as it names
-- itself; schema_latest is the newest migration its binary carries,
-- which is what silod migrate compares with a contract step's
-- requirement. A replica rewrites heartbeat_at every ten seconds and
-- deletes its row when it stops; one silent for 30 seconds is gone.
CREATE TABLE replicas (
    id            TEXT    PRIMARY KEY,
    release       TEXT    NOT NULL,
    schema_latest INTEGER NOT NULL,
    started_at    BIGINT  NOT NULL,
    heartbeat_at  BIGINT  NOT NULL
);

-- Settings install hands silod through the database. reconcile is
-- report-only after install adopts a restored instance (state.md
-- "Backups and recovery", SV-54), until an operator runs silo
-- reconcile --apply.
CREATE TABLE settings (
    name       TEXT   PRIMARY KEY,
    value      TEXT   NOT NULL,
    updated_at BIGINT NOT NULL
);
