-- The pull-through cache's index (docs/silo/local-registry.md, §8,
-- calls L70 and L71). Package pkg/registry owns these tables. The bytes
-- are in silo-containerd's content store, once by digest; these rows
-- say which upstream object each is, and the hosts its fetch used,
-- which every request is checked against the caller's egress policy
-- for.
--
-- registry_cache_objects: one cached manifest or blob of an upstream
-- repo. upstream is the configured upstream's name (docker.io), repo
-- the repo on it (library/alpine), kind manifest or blob. hosts is a
-- JSON list of {"host", "port", "role"}: the API host, the token
-- realm's host, and, for a blob, every host a GET was redirected
-- through to reach the bytes. A blob is served only in the repo it was
-- fetched for (call L62).
CREATE TABLE registry_cache_objects (
    upstream    TEXT    NOT NULL,
    repo        TEXT    NOT NULL,
    digest      TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    media_type  TEXT    NOT NULL DEFAULT '',
    size        INTEGER NOT NULL,
    hosts       TEXT    NOT NULL,
    fetched_at  INTEGER NOT NULL,
    accessed_at INTEGER NOT NULL,
    PRIMARY KEY (upstream, repo, digest)
);

-- registry_cache_tags: a tag's digest as the upstream last said, and
-- when it last did. A tag is revalidated on every request; when the
-- upstream can't be reached, it is served from here while validated_at
-- is less than stale_tag_max ago.
CREATE TABLE registry_cache_tags (
    upstream     TEXT    NOT NULL,
    repo         TEXT    NOT NULL,
    tag          TEXT    NOT NULL,
    digest       TEXT    NOT NULL,
    validated_at INTEGER NOT NULL,
    PRIMARY KEY (upstream, repo, tag)
);
