-- The silo/v1 fence API (docs/silo/fence-api.md, SV-1 to SV-26).
-- Package pkg/fenceapi owns these tables.

-- Each silo's own policies, as JSON written and read in Go: egress
-- (allow and deny host lists), grant_policy (an open map of grant kind
-- to allow or deny), agent_policy (allowlists and max_lease) and
-- control (full, read or none). The effective policies are worked out
-- down the tree when read.
CREATE TABLE silo_policies (
    silo         TEXT    PRIMARY KEY,
    egress       TEXT   NOT NULL,
    grant_policy TEXT   NOT NULL,
    agent_policy TEXT   NOT NULL,
    control      TEXT   NOT NULL,
    updated_at   BIGINT NOT NULL
);

-- Members: a front-door identity (user:... or serviceAccount:...) maps
-- to exactly one silo.
CREATE TABLE silo_members (
    identity   TEXT    PRIMARY KEY,
    silo       TEXT   NOT NULL,
    updated_at BIGINT NOT NULL
);
CREATE INDEX silo_members_silo ON silo_members (silo);

-- Long-running operations: a silo's deletion.
CREATE TABLE silo_operations (
    id          TEXT    PRIMARY KEY,
    kind        TEXT   NOT NULL,
    silo        TEXT   NOT NULL,
    path        TEXT   NOT NULL,
    caller      TEXT   NOT NULL,
    state       TEXT   NOT NULL,
    done        BOOLEAN NOT NULL,
    error       TEXT   NOT NULL DEFAULT '',
    created_at  BIGINT NOT NULL,
    updated_at  BIGINT NOT NULL
);
CREATE INDEX silo_operations_silo ON silo_operations (silo);

-- Audit records, kept for 90 days of database time. lineage is the
-- ids from the root to silo at the time, as /root/.../silo/, so that a
-- subtree query finds records of descendants since deleted. fields
-- holds what depends on the kind, as JSON. n is the order records were
-- written in.
CREATE TABLE audit (
    id         TEXT    PRIMARY KEY,
    n          BIGINT NOT NULL,
    at         BIGINT NOT NULL,
    silo       TEXT   NOT NULL,
    silo_path  TEXT   NOT NULL,
    lineage    TEXT   NOT NULL,
    actor      TEXT   NOT NULL,
    member     TEXT   NOT NULL DEFAULT '',
    via        TEXT   NOT NULL DEFAULT '',
    kind       TEXT   NOT NULL,
    request_id TEXT   NOT NULL DEFAULT '',
    result     TEXT   NOT NULL,
    fields     TEXT   NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_silo_at ON audit (silo, at);
CREATE INDEX audit_at ON audit (at);
CREATE UNIQUE INDEX audit_n ON audit (n);
