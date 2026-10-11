-- Harness sessions on the control VMs (docs/silo/agents.md, "silod
-- restarts", SV-115, and "Placement on the control VMs", SV-116).
--
-- An agent's host is the control VM its session runs on ('' before it
-- is placed), and start_failures how many times in a row its harness
-- has failed to start: three is FAILED.
ALTER TABLE agents ADD COLUMN host TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN start_failures INTEGER NOT NULL DEFAULT 0;

-- What a replica's heartbeat says of its control VM's sessions: the VM
-- (host), how many sessions it can run (agent_slots) and how many it
-- runs (agent_sessions).
ALTER TABLE replicas ADD COLUMN host TEXT NOT NULL DEFAULT '';
ALTER TABLE replicas ADD COLUMN agent_slots INTEGER NOT NULL DEFAULT 0;
ALTER TABLE replicas ADD COLUMN agent_sessions INTEGER NOT NULL DEFAULT 0;

-- The harness's own session state, saved after every turn so that any
-- control VM can resume it: the CLI's session id, and a tar.gz of its
-- session files (Claude Code's session file, Codex's rollout), base64.
CREATE TABLE agent_session_state (
    agent      TEXT   PRIMARY KEY,
    adapter    TEXT   NOT NULL,
    session_id TEXT   NOT NULL,
    state      TEXT   NOT NULL,
    saved_at   BIGINT NOT NULL
);
