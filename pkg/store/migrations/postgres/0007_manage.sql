-- manage grants (docs/silo/fence-api.md, "Manage grants", SV-17 to
-- SV-22). target is the silo the grant is on, and name is unique
-- among its grants; holder is the holder's id, or '' when the path the
-- offerer named, kept in holder_path, held no silo then, so the offer
-- can never be accepted. scope is the grant's scope as JSON. ttl_ms is
-- the lease; renew_until is the latest a renewal may reach, 0 for none.
-- expires_at is 0 until the grant is accepted. state is PENDING,
-- ACTIVE, EXPIRED or REVOKED; a grant is live while it is PENDING, or
-- ACTIVE and expires_at is still ahead of database time.
CREATE TABLE manage_grants (
    id           TEXT    NOT NULL PRIMARY KEY,
    target       TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    holder       TEXT    NOT NULL,
    holder_path  TEXT    NOT NULL,
    offered_by   TEXT    NOT NULL,
    scope        TEXT    NOT NULL,
    ttl_ms       BIGINT  NOT NULL,
    renew_until  BIGINT  NOT NULL,
    state        TEXT    NOT NULL,
    created_at   BIGINT  NOT NULL,
    accepted_at  BIGINT  NOT NULL,
    expires_at   BIGINT  NOT NULL,
    updated_at   BIGINT  NOT NULL,
    UNIQUE (target, name)
);
CREATE INDEX manage_grants_holder ON manage_grants (holder);
CREATE INDEX manage_grants_state ON manage_grants (state, expires_at);
