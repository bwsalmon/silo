-- An agent's session as the control surface serves it (docs/silo/
-- agents.md, §8, SV-112), kept in the database so that any control VM
-- can serve it, and for AuditRetention like audit.
--
-- agent_events is the transcript: turns, messages, tool calls and their
-- results, model usage, requests, answers and errors, numbered per
-- agent by seq. data is JSON.
CREATE TABLE agent_events (
    agent TEXT   NOT NULL,
    seq   BIGINT NOT NULL,
    at    BIGINT NOT NULL,
    kind  TEXT   NOT NULL,
    data  TEXT   NOT NULL,
    PRIMARY KEY (agent, seq)
);
CREATE INDEX agent_events_at ON agent_events (at);

-- Messages to an agent (direction in), which it takes at its next turn
-- (delivered_at is 0 until then), and from it (out). sender is the
-- caller's silo id, and member its member identity, for one in.
CREATE TABLE agent_messages (
    id           TEXT   PRIMARY KEY,
    agent        TEXT   NOT NULL,
    at           BIGINT NOT NULL,
    direction    TEXT   NOT NULL,
    text         TEXT   NOT NULL,
    sender       TEXT   NOT NULL DEFAULT '',
    member       TEXT   NOT NULL DEFAULT '',
    delivered_at BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX agent_messages_agent ON agent_messages (agent, at);

-- The agent's ask (kind question) and request_approval (kind approval)
-- calls. state is OPEN or ANSWERED; an answer is text, or for an
-- approval approve (1 or 0) and an optional text.
CREATE TABLE agent_requests (
    id          TEXT   PRIMARY KEY,
    agent       TEXT   NOT NULL,
    at          BIGINT NOT NULL,
    kind        TEXT   NOT NULL,
    text        TEXT   NOT NULL,
    state       TEXT   NOT NULL,
    answer      TEXT   NOT NULL DEFAULT '',
    approve     BIGINT NOT NULL DEFAULT 0,
    answered_at BIGINT NOT NULL DEFAULT 0,
    answered_by TEXT   NOT NULL DEFAULT ''
);
CREATE INDEX agent_requests_agent ON agent_requests (agent, state);
