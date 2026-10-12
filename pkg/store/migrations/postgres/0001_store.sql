-- The store's own tables (docs/silo/state.md, SV-46). The same as
-- sqlite/0001_store.sql, with BIGINT where SQLite's INTEGER is 64-bit.

-- The change log. seq comes from change_counter, bumped in the same
-- transaction, so entries are gap-free and in commit order. Not a
-- SEQUENCE: its values are not handed out in commit order.
CREATE TABLE changes (
    seq  BIGINT PRIMARY KEY,
    kind TEXT   NOT NULL,
    key  TEXT   NOT NULL,
    data TEXT   NOT NULL DEFAULT '',
    at   BIGINT NOT NULL
);

CREATE TABLE change_counter (
    id  INTEGER PRIMARY KEY CHECK (id = 1),
    seq BIGINT  NOT NULL
);
INSERT INTO change_counter (id, seq) VALUES (1, 0);

-- request_id idempotency: a retried call finds its first result.
CREATE TABLE requests (
    request_id TEXT   NOT NULL,
    result     TEXT   NOT NULL,
    at         BIGINT NOT NULL
);
CREATE UNIQUE INDEX requests_request_id ON requests (request_id);
