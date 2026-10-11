-- Tool channels end on the session's host (docs/silo/agents.md, "The
-- tool channel", SV-106, and "silod restarts", SV-115).
--
-- A replica's heartbeat says where its control VM's gateway is reached
-- (host[:port], '' if it runs no sessions), which an attach that lands
-- on another control VM's gateway is redirected to.
ALTER TABLE replicas ADD COLUMN gateway_address TEXT NOT NULL DEFAULT '';
