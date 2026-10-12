-- The GCE operations the compute/v1 facade started
-- (docs/silo/gcp-backend.md, "Operations", SV-73). silo is the silo
-- whose facade the call was made in, which owns the object; caller is
-- the silo that made the call, the operation's user. scope is the
-- operation's scope as a partial path (zones/z, regions/r, global).
-- refund holds what to give back if the operation fails, as JSON for
-- the facade's refund hook. done is set once silod has seen the
-- operation finish and finished its own bookkeeping.
CREATE TABLE facade_operations (
    scope       TEXT    NOT NULL,
    name        TEXT    NOT NULL,
    silo        TEXT    NOT NULL,
    caller      TEXT    NOT NULL,
    method      TEXT    NOT NULL,
    object      TEXT    NOT NULL DEFAULT '',
    refund      TEXT    NOT NULL DEFAULT '',
    done        BOOLEAN NOT NULL,
    failed      BOOLEAN NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (scope, name)
);
CREATE INDEX facade_operations_silo ON facade_operations (silo);
CREATE INDEX facade_operations_done ON facade_operations (done);
