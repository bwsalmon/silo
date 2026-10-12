-- An own range's region (docs/silo/allocators.md, "Allowed regions",
-- SV-37): a subnet is an object in its region, so a silo can't have a
-- region removed while it or a descendant still has a subnet there.
-- '' for a range recorded without one.
ALTER TABLE address_own ADD COLUMN region TEXT NOT NULL DEFAULT '';
CREATE INDEX address_own_region ON address_own (region, silo);

-- The facade names a subnet's ranges subnetworks/<region>/<name>[/<range>].
UPDATE address_own SET region = substr(object, 13, instr(substr(object, 13), '/') - 1)
    WHERE object LIKE 'subnetworks/%/%';
