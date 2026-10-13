-- Two more policies of each silo's own (docs/silo/local-backend.md, §3
-- "In a silo's policy" and §4 "Published ports"; fence-api.md,
-- "Isolation and ingress policies"), kept beside the others in
-- silo_policies and written and read in Go:
--
--   isolation: the classes containers in the silo may run in, as JSON
--              {"allowed": ["vm", "sandbox"], "default": "vm"};
--   ingress:   host or none, whether published ports also listen on the
--              host's loopback.
--
-- A row an older release writes gets every class and no ingress. Each
-- is never looser than the parent's, and the effective policy is worked
-- out down the tree, so neither default widens anything. The root's
-- ingress is host, as silo install sets it.
ALTER TABLE silo_policies ADD COLUMN isolation TEXT NOT NULL DEFAULT '{"allowed":["vm","sandbox"],"default":"vm"}';
ALTER TABLE silo_policies ADD COLUMN ingress TEXT NOT NULL DEFAULT 'none';
UPDATE silo_policies SET ingress = 'host' WHERE silo IN (SELECT id FROM silos WHERE parent = '');
