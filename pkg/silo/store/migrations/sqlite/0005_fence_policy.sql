-- The fence's firewall policy (docs/silo/gcp-backend.md "The fence",
-- SV-57 to SV-60). Package pkg/silo/fence owns this table.
--
-- One row. quota_rules and quota_attributes are the policy quota silo
-- install read from GCE: how many rules, and how many rule attributes
-- (ranges, protocols and ports), silo-fence may hold. 0 means not
-- recorded, and nothing is refused. applied_seq is the last change-log
-- entry the policy in GCE carries.
CREATE TABLE fence_policy (
    id               INTEGER PRIMARY KEY CHECK (id = 1),
    quota_rules      INTEGER NOT NULL,
    quota_attributes INTEGER NOT NULL,
    applied_seq      INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
INSERT INTO fence_policy (id, quota_rules, quota_attributes, applied_seq, updated_at) VALUES (1, 0, 0, 0, 0);
