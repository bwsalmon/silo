-- Repos as silo objects (docs/silo/local-git.md, §2 "The object", call
-- L41; §4 "What egress allowlists mean here", call L54; §5 "Quota",
-- call L55). Package pkg/repos owns the repos table.
--
-- repos: one row per repo. (silo, name) is how the API names it, and id
-- (r- and six base-32 characters) is what the data directory keeps it
-- under: silos/<silo>/repos/<id>.git. default_branch is the bare repo's
-- HEAD. seq is the change-log sequence number it was created at, which
-- its directory's .silo-seq also holds, so that a restore can tell a
-- repo newer than the store. size_bytes is its size when last measured.
CREATE TABLE repos (
    id             TEXT    NOT NULL PRIMARY KEY,
    silo           TEXT    NOT NULL,
    name           TEXT    NOT NULL,
    default_branch TEXT    NOT NULL,
    seq            INTEGER NOT NULL,
    size_bytes     INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    UNIQUE (silo, name)
);

-- The repos policy, beside the others in silo_policies and read in Go:
-- {"mirror": true|false}, whether the silo's repos are mirrored into the
-- uber repo. It is never looser than the parent's, so an older row's
-- true widens nothing.
ALTER TABLE silo_policies ADD COLUMN repos TEXT NOT NULL DEFAULT '{"mirror":true}';

-- The repos count class, held while a repo exists. Its kind is silo's
-- own: repos belong to no backend's API. The root's capacity of it is
-- unlimited (ledger.Unlimited), as bootstrap sets it on a new root.
INSERT INTO quota_classes (kind, name, unit, sort, scope, period) VALUES
    ('silo', 'repos', 'count', 'held', 'global', '');
INSERT INTO allocations (from_silo, to_silo, kind, class, region, amount, updated_at)
    SELECT '', id, 'silo', 'repos', 'global', 4611686018427387904, updated_at FROM silos WHERE parent = '';
INSERT INTO quota (silo, kind, class, region, granted)
    SELECT id, 'silo', 'repos', 'global', 4611686018427387904 FROM silos WHERE parent = '';
