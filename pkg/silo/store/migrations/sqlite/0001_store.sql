-- The store's own tables (docs/silo/state.md, SV-46).

-- The change log. seq comes from change_counter, bumped in the same
-- transaction, so entries are gap-free and in commit order.
CREATE TABLE changes (
    seq  INTEGER PRIMARY KEY,
    kind TEXT    NOT NULL,
    key  TEXT    NOT NULL,
    data TEXT    NOT NULL DEFAULT '',
    at   INTEGER NOT NULL
);

CREATE TABLE change_counter (
    id  INTEGER PRIMARY KEY CHECK (id = 1),
    seq INTEGER NOT NULL
);
INSERT INTO change_counter (id, seq) VALUES (1, 0);

-- request_id idempotency: a retried call finds its first result.
CREATE TABLE requests (
    request_id TEXT    NOT NULL,
    result     TEXT    NOT NULL,
    at         INTEGER NOT NULL
);
CREATE UNIQUE INDEX requests_request_id ON requests (request_id);
