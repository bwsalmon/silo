-- Messages and requests numbered per agent by seq, as agent_events is
-- (docs/silo/agents.md, §8, SV-112), so they are listed and delivered in
-- the order they were made: at is in milliseconds and id is random, so
-- two made in the same millisecond had no order of their own.
ALTER TABLE agent_messages ADD COLUMN seq BIGINT NOT NULL DEFAULT 0;
UPDATE agent_messages SET seq = (SELECT COUNT(*) FROM agent_messages m
    WHERE m.agent = agent_messages.agent
      AND (m.at < agent_messages.at OR (m.at = agent_messages.at AND m.id <= agent_messages.id)));
CREATE UNIQUE INDEX agent_messages_seq ON agent_messages (agent, seq);

ALTER TABLE agent_requests ADD COLUMN seq BIGINT NOT NULL DEFAULT 0;
UPDATE agent_requests SET seq = (SELECT COUNT(*) FROM agent_requests r
    WHERE r.agent = agent_requests.agent
      AND (r.at < agent_requests.at OR (r.at = agent_requests.at AND r.id <= agent_requests.id)));
CREATE UNIQUE INDEX agent_requests_seq ON agent_requests (agent, seq);
