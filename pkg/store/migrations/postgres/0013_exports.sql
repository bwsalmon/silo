-- Proxied exports (docs/silo/fence-api.md, "Exports (proxied)", SV-13
-- and SV-14; gateway.md, "Proxied exports", SV-83). Package
-- pkg/exports owns these tables.
--
-- exports: one row per export a silo offers. id is the export's own id,
-- which names its fence rule export-<id>-in; (exporter, name) is how the
-- API names it. owner is who may withdraw it: "tenant" in v1
-- (custom-allocators.md item 10). target_ip is '' while an instance
-- target has no address yet; instance and zone are '' for an ip:port
-- target.
CREATE TABLE exports (
    id             TEXT    NOT NULL PRIMARY KEY,
    exporter       TEXT    NOT NULL,
    name           TEXT    NOT NULL,
    owner          TEXT    NOT NULL,
    protocol       TEXT    NOT NULL,
    target_ip      TEXT    NOT NULL,
    target_port    INTEGER NOT NULL,
    instance       TEXT    NOT NULL,
    zone           TEXT    NOT NULL,
    proxy_protocol BOOLEAN NOT NULL,
    created_at     BIGINT  NOT NULL,
    updated_at     BIGINT  NOT NULL,
    UNIQUE (exporter, name)
);

-- export_offers: who an export is offered to. to_path is the path the
-- exporter named; to_silo is the silo that held it then, or '' if none
-- did, so that the offer can never be accepted and nothing tells the
-- exporter so.
CREATE TABLE export_offers (
    export     TEXT    NOT NULL,
    to_path    TEXT    NOT NULL,
    to_silo    TEXT    NOT NULL,
    created_at BIGINT  NOT NULL,
    PRIMARY KEY (export, to_path)
);
CREATE INDEX export_offers_to ON export_offers (to_silo);

-- imports: an accepted offer. state is ACTIVE, or SUSPENDED while either
-- side's effective grant policy doesn't allow exports. port is the
-- gateway's TCP port for a tcp export, 0 for http.
CREATE TABLE imports (
    id         TEXT    NOT NULL PRIMARY KEY,
    export     TEXT    NOT NULL,
    importer   TEXT    NOT NULL,
    port       INTEGER NOT NULL,
    state      TEXT    NOT NULL,
    created_at BIGINT  NOT NULL,
    updated_at BIGINT  NOT NULL,
    UNIQUE (export, importer)
);
CREATE INDEX imports_importer ON imports (importer);
