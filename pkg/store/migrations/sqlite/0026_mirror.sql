-- The outbound mirror into the uber repo (docs/silo/local-git.md, §8,
-- calls L46 to L53). Package pkg/mirror owns these tables. The mirror's
-- configuration (its URL, prefix and index branch) is in settings, as
-- mirror.*; its credential is a file in keys/mirror/, never here.
--
-- mirror_refs: what the uber repo has under silod's layout, as far as
-- silod knows: an external ref name and the SHA silod last pushed to it
-- (or adopted there). Every push leases against it. local is the repo's
-- own ref the external one mirrors (refs/heads/main, or
-- refs/silo-archive/<ts>-<seq>/heads/main), or '' for a ref only the
-- mirror made, such as an adopted external tip's archive.
CREATE TABLE mirror_refs (
    ref        TEXT    NOT NULL PRIMARY KEY,
    repo       TEXT    NOT NULL,
    local      TEXT    NOT NULL,
    sha        TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX mirror_refs_repo ON mirror_refs (repo);

-- mirror_repos: each repo's place in the mirror loop. mirrored_seq is
-- the change-log sequence number its last good pass covered; a newer
-- repo or repo.push change makes it dirty again. pending_since is when
-- it first failed since it was last in sync (0 while it is), stalled
-- whether that has lasted past the stall time and been audited. A
-- repo that has been deleted keeps its row while the mirror archives
-- it: deleting is the delete operation's id (or '-' for a repo deleted
-- with its silo), and silo and path are where it was.
CREATE TABLE mirror_repos (
    repo          TEXT    NOT NULL PRIMARY KEY,
    silo          TEXT    NOT NULL DEFAULT '',
    path          TEXT    NOT NULL DEFAULT '',
    mirrored_seq  INTEGER NOT NULL DEFAULT 0,
    pending_since INTEGER NOT NULL DEFAULT 0,
    stalled       INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT    NOT NULL DEFAULT '',
    deleting      TEXT    NOT NULL DEFAULT '',
    updated_at    INTEGER NOT NULL
);

-- mirror_diverged: external refs whose lease failed. silod leaves them
-- alone until an operator adopts them (silo mirror adopt).
CREATE TABLE mirror_diverged (
    ref        TEXT    NOT NULL PRIMARY KEY,
    repo       TEXT    NOT NULL,
    local      TEXT    NOT NULL,
    expected   TEXT    NOT NULL,
    actual     TEXT    NOT NULL,
    since      INTEGER NOT NULL
);
