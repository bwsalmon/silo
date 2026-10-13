-- Registry exports and the images policy (docs/silo/local-registry.md,
-- §5 "Registry exports", call L64; §8 "The timing channel, and
-- images.sharing", call L73).
--
-- A registry export is an export of protocol "registry". It has no
-- target, so target_ip is '' and target_port 0, and neither the fence
-- nor the gateway carries anything for it. repos is what it covers, as
-- a JSON array of the exporter's own image repo names, each a name or a
-- prefix ending in "*"; it is [] for every other protocol.
ALTER TABLE exports ADD COLUMN repos TEXT NOT NULL DEFAULT '[]';

-- The images policy, beside the others in silo_policies and read in Go:
-- {"sharing": "shared"|"isolated"}. Effective sharing is isolated if the
-- silo or any ancestor sets isolated, so shared widens nothing.
ALTER TABLE silo_policies ADD COLUMN images TEXT NOT NULL DEFAULT '{"sharing":"shared"}';
