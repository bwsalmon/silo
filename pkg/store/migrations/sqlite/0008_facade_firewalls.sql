-- Tenant firewall rules as the tenant wrote them
-- (docs/silo/gcp-backend.md, "Firewall rules", SV-72). The facade pins
-- each rule to its silo before GCE sees it, so the real rule holds
-- intersected ranges and prefixed tags; get and list show this original
-- instead. name is the tenant's name for the rule in silo's facade, and
-- original its JSON body. previous is the original a patch or update
-- replaced while its operation is still running, put back if it fails,
-- and '' otherwise.
CREATE TABLE facade_firewalls (
    silo        TEXT    NOT NULL,
    name        TEXT    NOT NULL,
    original    TEXT    NOT NULL,
    previous    TEXT    NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (silo, name)
);
