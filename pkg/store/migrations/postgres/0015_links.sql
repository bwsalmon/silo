-- L3 links (docs/silo/fence-api.md, "Links (L3)", SV-15; gcp-backend.md,
-- "The fence", SV-57). Package pkg/links owns this table.
--
-- links: one row per side's proposal. (silo, name) is how the API names
-- it. peer_path is the path the proposer named; peer_silo is the silo
-- that held it then, or '' if none did (or it has since been deleted),
-- so the proposal can never match and nothing tells the proposer so.
-- direction is outbound, inbound or both, from silo's side. rules is
-- the canonical JSON of the agreed protocols and ports. link is the
-- shared id both rows carry once the peer has proposed the mirror
-- image, '' before; it names the fence rules link-<link>-*. state is
-- PENDING until then, and ACTIVE or SUSPENDED after, as both sides'
-- effective grant policies allow links or not.
CREATE TABLE links (
    id         TEXT    NOT NULL PRIMARY KEY,
    silo       TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    peer_path  TEXT    NOT NULL,
    peer_silo  TEXT    NOT NULL,
    direction  TEXT    NOT NULL,
    rules      TEXT    NOT NULL,
    link       TEXT    NOT NULL,
    state      TEXT    NOT NULL,
    created_at BIGINT  NOT NULL,
    updated_at BIGINT  NOT NULL,
    UNIQUE (silo, name)
);
CREATE INDEX links_peer ON links (peer_silo);
CREATE INDEX links_link ON links (link);
