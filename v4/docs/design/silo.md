# Silo v4 (Project Edition) Design

Silo v4 Project Edition provides single-project or multi-project autonomous agent orchestration without the overhead or latency of dedicated virtual machines. Instead of provisioning KVM hypervisors and guest operating systems, Silo enforces isolation and least privilege at the **tool and identity boundaries**. A Silo project agent is granted a Google Cloud Service Account with access to one or more target projects, empowered to perform any `gcloud` operation against that service account, and physically constrained from doing anything else.

## Process and Execution Model

Silo operates as a lightweight host daemon and process supervisor. It launches a primary root agent process (using Claude Code, Codex, or Gemini CLI) and manages its execution lifecycle. Rather than granting the agent raw terminal access, shell tools, or host filesystem primitives, Silo connects the agent exclusively to structured Model Context Protocol (MCP) servers via local `stdio` communication channels.

Each agent (the root agent and all sub-agents) is launched with a structured **system and objective prompt** and an isolated, persistent **editable memory area**.

The root agent is provisioned with the following MCP tools:

1. **`gcloud` MCP Tool**: A local instance of Google's official `@google-cloud/gcloud-mcp` server. This exposes structured tools that translate natural language and structured calls into `gcloud` CLI invocations against Google Cloud APIs.
2. **`subagents` MCP Tool**: Silo's native sub-agent management server. This enables the agent to dynamically spawn, supervise, query, message, and terminate child workers to execute specialized or parallel tasks.
3. **`memory` MCP Tool**: An agent-scoped key-value and document memory store. This enables the agent to record notes, state machines, discovered resource identifiers, and architectural decisions without requiring host filesystem access.

Because the agent possesses neither a shell execution tool (`bash`), an arbitrary HTTP networking tool, nor a host filesystem write tool, its blast radius is strictly confined to Google Cloud APIs mediated by its assigned Service Account.

## Identity and Credential Scoping

Authentication is anchored to Google Cloud IAM. The operator configures Silo with a designated Service Account in the target project. The host Silo daemon authenticates against Google Cloud and mints short-lived (1-hour) OAuth2 access tokens via Service Account impersonation (`iamcredentials.googleapis.com`).

Tokens are injected directly into the `gcloud-mcp` process environment through standard Google Cloud SDK configuration variables (`CLOUDSDK_AUTH_ACCESS_TOKEN` and `CLOUDSDK_CORE_PROJECT`). Long-lived private keys (such as JSON service account credentials) are never written to disk or exposed to the agent.

To enforce multi-project boundaries and prevent lateral movement, Silo applies Google Cloud **Credential Access Boundaries (CAB)** when generating access tokens. A token downscoped with CAB restricts API calls strictly to the resource hierarchy of declared projects (e.g. `//cloudresourcemanager.googleapis.com/projects/<PROJECT_NUMBER>`). Even if the underlying Service Account has broader organizational roles, Google's API gateways cryptographically reject requests directed at unapproved projects.

## Hierarchical Sub-Agent Delegation

Complex cloud engineering requires dividing labor among specialized workers. Through the `subagents` MCP tool, the root agent can spawn sub-agents to parallelize work or isolate risky operations.

Sub-agents support two distinct operational lifecycles:

- **One-Shot Agents**: Ephemeral workers created with an objective prompt. They execute tool calls against their own `gcloud-mcp` instance, emit a final output report to the parent, and exit. Once finished, they remain stopped.
- **Persistent Agents**: Continuous background daemons. Like the root agent, persistent sub-agents are actively supervised by Silo: if the process exits, fails, or crashes, Silo restarts it with exponential backoff. Persistent agents communicate asynchronously with their parent via message queues.

When creating a sub-agent, the creator can delegate the ability to spawn sub-agents (`can_spawn_subagents: true`), allowing the creation of manager-worker trees.

## Agent Prompts and Editable Memory

Every agent operates with a defined prompt context and an isolated, editable memory store:

1. **System and Objective Prompts**:
   - The root agent's prompt is specified via configuration (`SYSTEM.md` or CLI flag), declaring its operational parameters, cloud project boundary, and high-level mission.
   - When a parent spawns a sub-agent, it injects a targeted objective prompt describing the specific deliverable or monitoring directive.
2. **Editable Agent Memory**:
   - Because agents lack host filesystem access, Silo provisions an agent-scoped memory store accessible via the `memory` MCP tool (`read_memory`, `write_memory`, `append_memory`, `list_memory`).
   - The memory area acts as an editable scratchpad where agents record discovered resource IDs, architectural decisions, deployment state, and task checklists.
   - For **persistent agents**, memory persists across process restarts and crashes, allowing resumed execution with full state continuity.
   - When spawning a child agent, the parent can seed initial memory entries. Parents can also inspect a child agent's memory to review working notes or extracted cloud telemetry.

## Creation-Time Quotas and Budget Accounting

To prevent runaway recursion, resource exhaustion, or unmonitored agent sprawl, Silo provides an optional quota engine configured at creation time.

The operator can launch the root agent with strict quota limits or leave it unconstrained (`unlimited`):

- **Lifetime Budget (`max_subagents`)**: The maximum total number of child agents that can be spawned over the root agent's lifetime.
- **Concurrency Limit (`max_concurrent`)**: The maximum number of child agents allowed to execute simultaneously.
- **Recursion Depth (`max_depth`)**: The maximum allowable nesting depth in the delegation tree.

When a manager agent spawns a child and delegates creation rights, Silo enforces **monotonic budget carve-out**: the child's quota is deducted directly from the parent's remaining allocation. A sub-agent can never be granted a quota or depth exceeding its creator's remaining budget.

## Transitive Privilege Ceilings

Silo enforces strict downward privilege inheritance across all agent generations:

1. **Identity Narrowing**: A sub-agent may inherit the creator's Service Account, receive a further-downscoped Credential Access Boundary (e.g. restricted to a single GCS bucket or GKE cluster), or be assigned an alternative Service Account in the project only if the creator's identity possesses `iam.serviceAccounts.actAs` rights.
2. **Tool Whitelisting**: Sub-agents receive `gcloud-mcp`. If `can_spawn_subagents` is false or the allocated depth reaches zero, the `subagents` tool is completely omitted from the child agent's environment, turning it into a strict leaf worker.
3. **Cascading Teardown**: Silo maintains process tree ownership. When any parent agent terminates or is deleted, Silo automatically issues a cascading graceful stop (`SIGTERM` followed by `SIGKILL`) to all descendants in its subtree, ensuring no orphaned processes or rogue API callers linger.
