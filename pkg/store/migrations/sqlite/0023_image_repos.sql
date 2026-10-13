-- Image repos as silo objects (docs/silo/local-registry.md, §3 "Repos
-- as silo objects", call L58; §4 "One content store, records per
-- repo", call L59). Package pkg/imagerepos owns these tables. The
-- store is the truth for repos, tags and which manifests a repo holds;
-- the bytes are in silo-containerd's content store.
--
-- image_repos: one row per repo, created by its first push. (silo,
-- name) is how the API and the registry name it, and id (ir- and six
-- base-32 characters) is what the store and the silo-registry records
-- use. state is READY, or DELETING while a delete removes its
-- manifests, which makes every registry request on it a 404. seq is the
-- change-log sequence number it was created at.
CREATE TABLE image_repos (
    id           TEXT    NOT NULL PRIMARY KEY,
    silo         TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    state        TEXT    NOT NULL DEFAULT 'READY',
    seq          INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    last_push_at INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    UNIQUE (silo, name)
);

-- image_repo_manifests: the manifests a repo holds, tagged or not.
-- size is the manifest's own; refs is a JSON list of {"digest", "size"}
-- for every blob and child manifest it references (config, layers, an
-- index's manifests, never its subject), which is what the repo's
-- charge is the distinct sum of (call L61).
CREATE TABLE image_repo_manifests (
    repo       TEXT    NOT NULL,
    digest     TEXT    NOT NULL,
    media_type TEXT    NOT NULL,
    size       INTEGER NOT NULL,
    refs       TEXT    NOT NULL DEFAULT '[]',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (repo, digest)
);

-- image_tags: a repo's tags and the manifest each points at.
CREATE TABLE image_tags (
    repo       TEXT    NOT NULL,
    tag        TEXT    NOT NULL,
    digest     TEXT    NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (repo, tag)
);

-- The image_repos count class, held while a repo exists, handed down
-- like repos. The root's capacity of it is unlimited (ledger.Unlimited),
-- as bootstrap sets it on a new root.
INSERT INTO quota_classes (kind, name, unit, sort, scope, period) VALUES
    ('silo', 'image_repos', 'count', 'held', 'global', '');
INSERT INTO allocations (from_silo, to_silo, kind, class, region, amount, updated_at)
    SELECT '', id, 'silo', 'image_repos', 'global', 4611686018427387904, updated_at FROM silos WHERE parent = '';
INSERT INTO quota (silo, kind, class, region, granted)
    SELECT id, 'silo', 'image_repos', 'global', 4611686018427387904 FROM silos WHERE parent = '';
