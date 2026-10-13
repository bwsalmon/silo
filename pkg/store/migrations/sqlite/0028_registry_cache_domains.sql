-- The cache index per sharing domain (docs/silo/local-registry.md, §8,
-- "The timing channel, and images.sharing", call L73). Each cache entry
-- records the domain that fetched it, and a caller gets a hit only on
-- its own domain's entries. The whole host's entries stay in
-- registry_cache_objects and registry_cache_tags; an isolated domain's
-- are in these, keyed by domain: the ID of the nearest silo below the
-- root, the caller or an ancestor, that sets images.sharing isolated
-- itself. The columns mean what they do there. The bytes are still in
-- the content store once by digest, whichever domains index them.
CREATE TABLE registry_cache_domain_objects (
    domain      TEXT    NOT NULL,
    upstream    TEXT    NOT NULL,
    repo        TEXT    NOT NULL,
    digest      TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    media_type  TEXT    NOT NULL DEFAULT '',
    size        INTEGER NOT NULL,
    hosts       TEXT    NOT NULL,
    fetched_at  INTEGER NOT NULL,
    accessed_at INTEGER NOT NULL,
    PRIMARY KEY (domain, upstream, repo, digest)
);

-- A tag's revalidation is its domain's too: a stale tag is served only
-- to the domain that last revalidated it.
CREATE TABLE registry_cache_domain_tags (
    domain       TEXT    NOT NULL,
    upstream     TEXT    NOT NULL,
    repo         TEXT    NOT NULL,
    tag          TEXT    NOT NULL,
    digest       TEXT    NOT NULL,
    validated_at INTEGER NOT NULL,
    PRIMARY KEY (domain, upstream, repo, tag)
);
