-- Agent credentials (docs/silo/agents.md, §6, SV-108): model provider
-- logins, kept so that every control VM can use them. secret is the
-- API key, or a subscription's login files as JSON, sealed with
-- AES-256-GCM under silo-credential-key, which only the control VMs'
-- metadata holds: the database, and so its backups, hold it only
-- encrypted, without the key. base64 of nonce || ciphertext.
CREATE TABLE agent_credentials (
    name         TEXT    PRIMARY KEY,
    provider     TEXT    NOT NULL,
    kind         TEXT    NOT NULL,
    max_sessions INTEGER NOT NULL,
    secret       TEXT    NOT NULL,
    created_at   BIGINT  NOT NULL,
    updated_at   BIGINT  NOT NULL
);

-- The lease's end an agent was last warned of, ten minutes before it
-- (agents.md, "The lease runs out", SV-113): a renewal moves expire_at
-- past it, so the agent is warned again.
ALTER TABLE agents ADD COLUMN lease_warned BIGINT NOT NULL DEFAULT 0;
