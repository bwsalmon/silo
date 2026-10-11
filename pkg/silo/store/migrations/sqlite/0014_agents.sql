-- Agents (docs/silo/agents.md, "The agent object", SV-101; fence-api.md,
-- "Agents", SV-23 and SV-24). An agent is a row in its silo: its
-- harness, model, budget, tool VM, lease and state. budget_micros is
-- the budget.usd it holds of its silo's model/usd (the ledger keeps the
-- reservation itself, as the charge on object agent:<id>), and
-- spent_micros what it has spent since period_start. tool_vm and
-- session are JSON. owner is the agent whose session created it, or ''.
-- op is the long-running operation in progress on it, if any.
CREATE TABLE agents (
    id             TEXT    PRIMARY KEY,
    silo           TEXT    NOT NULL,
    name           TEXT    NOT NULL,
    adapter        TEXT    NOT NULL,
    version        TEXT    NOT NULL,
    credential     TEXT    NOT NULL,
    model          TEXT    NOT NULL,
    budget_micros  BIGINT  NOT NULL,
    spent_micros   BIGINT  NOT NULL DEFAULT 0,
    period_start   BIGINT  NOT NULL,
    tool_vm        TEXT    NOT NULL,
    expire_at      BIGINT  NOT NULL,
    prompt         TEXT    NOT NULL,
    owner          TEXT    NOT NULL DEFAULT '',
    state          TEXT    NOT NULL,
    status_message TEXT    NOT NULL DEFAULT '',
    session        TEXT    NOT NULL DEFAULT '',
    labels         TEXT    NOT NULL,
    op             TEXT    NOT NULL DEFAULT '',
    created_at     BIGINT  NOT NULL,
    updated_at     BIGINT  NOT NULL,
    UNIQUE (silo, name)
);

-- The objects an agent's session created (call 43): child silos and
-- other agents, which are deleted with it. kind is silo or agent.
CREATE TABLE agent_owned (
    kind   TEXT NOT NULL,
    object TEXT NOT NULL,
    agent  TEXT NOT NULL,
    PRIMARY KEY (kind, object)
);
CREATE INDEX agent_owned_agent ON agent_owned (agent);

-- Root members' overrides of the release's model price table
-- (agents.md, "Prices", SV-111), in micro-dollars per million tokens.
CREATE TABLE model_prices (
    provider     TEXT   NOT NULL,
    model        TEXT   NOT NULL,
    input        BIGINT NOT NULL,
    output       BIGINT NOT NULL,
    cache_write  BIGINT NOT NULL,
    cache_read   BIGINT NOT NULL,
    updated_at   BIGINT NOT NULL,
    PRIMARY KEY (provider, model)
);
