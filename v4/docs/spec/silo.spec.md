# Silo v4 (Project Edition) Specification

## 1. Scope and Normative Status

This document defines the normative architecture, configuration schema, execution lifecycle, Model Context Protocol (MCP) tool interfaces, identity delegation, and quota accounting for **Silo v4 (Project Edition)**.

In this specification:
- The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" are to be interpreted as described in BCP 14 (RFC 2119, RFC 8174).
- "Silo" refers to the host daemon and process supervisor.
- "Root Agent" refers to the primary autonomous coding agent process.
- "Sub-Agent" refers to any child agent process spawned through Silo's sub-agent management interface.
- "gcloud-mcp" refers to the local Model Context Protocol server exposing Google Cloud SDK functionality.

---

## 2. System Architecture and Process Topology

Silo implements process-level supervision and tool-boundary isolation on the host or within a container runtime. No hypervisor or virtual machine is provisioned.

```
+---------------------------------------------------------------------------------------------------------+
| Silo Supervisor (Host / Container Process)                                                              |
|                                                                                                         |
|   Quota Ledger: [Root: Budget=20, Active=2, MaxConc=5, Depth=2]                                         |
|                                                                                                         |
|   +-------------------------------------------------------------------------------------------------+   |
|   | Root Agent Process (PID: 10420)                                                                 |   |
|   | Tools: [gcloud, subagents]                                                                      |   |
|   +-------------------------------+-----------------------------------+-----------------------------+   |
|                                   | (stdio)                           | (stdio)                         |
|                                   v                                   v                                 |
|                       +-----------------------+           +-----------------------+                     |
|                       | Local gcloud-mcp      |           | Silo subagents-mcp    |                     |
|                       | (Child of Silo)       |           | (Internal RPC Server) |                     |
|                       +-----------------------+           +-----------+-----------+                     |
|                                                                       |                                 |
|                                                     spawn_subagent    |                                 |
|                                                                       v                                 |
|                                                       [Quota Ledger Check]                              |
|                                                                       |                                 |
|                              +----------------------------------------+-------------------+             |
|                              | (can_spawn: true)                                          | (leaf)      |
|                              v                                                            v             |
|   +------------------------------------------------------+     +------------------------------------+   |
|   | Manager Sub-Agent (PID: 10455)                       |     | Worker Sub-Agent (PID: 10490)      |   |
|   | Lifecycle: persistent (Supervised Restart)           |     | Lifecycle: one_shot (Terminates)   |   |
|   | Tools: [gcloud, subagents]                           |     | Tools: [gcloud]                    |   |
|   | Quota: [Budget=5, Active=1, MaxConc=2, Depth=1]      |     | Quota: None (can_spawn: false)     |   |
|   +--------------------------+---------------------------+     +------------------+-----------------+   |
|                              |                                                    |                     |
|                              v (stdio)                                            v (stdio)             |
|                  +-----------------------+                            +-----------------------+         |
|                  | Local gcloud-mcp      |                            | Local gcloud-mcp      |         |
|                  +-----------------------+                            +-----------------------+         |
+---------------------------------------------------------------------------------------------------------+
```

### 2.1 Component Responsibilities

1. **Silo Host Supervisor**:
   - Manages process lifecycles for the root agent, all sub-agents, and their associated MCP servers.
   - Maintains the central **Quota Ledger**, validating all spawn requests against ancestor budgets.
   - Authenticates against Google Cloud, mints short-lived OAuth2 access tokens via Service Account impersonation, and applies Credential Access Boundaries.
   - Reaps child sub-agents when parents exit or are terminated.
2. **Coding Agent Processes** (`claude`, `codex`, `gemini`):
   - Run as unprivileged host processes inside isolated process groups (`setpgid`).
   - Possess **zero shell execution tools** (`bash`, `sh`, `exec`).
   - Interact with the world strictly via MCP standard input/output (`stdio`) streams.
3. **Local `gcloud-mcp` Instances**:
   - Distinct child processes running `@google-cloud/gcloud-mcp`.
   - Each instance receives its own isolated environment variables containing the specific agent's downscoped OAuth2 access token and default GCP project.
4. **Silo `subagents-mcp` Server**:
   - Built into Silo; served over `stdio` to agents authorized to manage child workers.
   - Enforces hierarchical quota validation and dispatches process management commands to the supervisor.
5. **Silo `memory-mcp` Server**:
   - Built into Silo; served over `stdio` to all active agent processes (both root and sub-agents).
   - Manages an isolated, persistent key-value and document memory store per agent, enabling state tracking, task notes, and discovered resource logging without requiring host filesystem access.

---

## 3. Configuration Schema

The operator defines the deployment via `silo.yaml` or through equivalent command-line flags.

### 3.1 `silo.yaml` Schema

```yaml
version: "v4"
mode: "project" # Specifies Project Edition (non-VM)

project:
  id: "my-gcp-project"                      # GCP Project ID
  number: "123456789012"                    # GCP Project Number (used for CAB resource matching)
  service_account: "root-agent@my-gcp-project.iam.gserviceaccount.com"
  downscope_credentials: true               # Apply Credential Access Boundaries

agent:
  cli: "gemini"                             # Enum: "claude" | "codex" | "gemini"
  model: "gemini-2.5-pro"                   # Optional: model override
  prompt_path: "SYSTEM.md"                  # Path to objective specification prompt

  # Sub-agent orchestration policy for the root agent
  subagents:
    enabled: true                           # Whether root agent receives the subagents MCP tool

    # Root creation quotas (omit, set to null, or set to -1 for unlimited)
    max_subagents: 20                       # Total lifetime sub-agents root can spawn
    max_concurrent: 5                       # Maximum child sub-agents running simultaneously
    max_depth: 3                            # Maximum delegation depth (0 = root only)
    allow_delegation: true                  # Whether root may grant spawn rights to child agents

logging:
  audit_log: "/var/log/silo/audit.log"
  level: "info"
```

### 3.2 CLI Creation-Time Configuration

Any configuration field MAY be supplied or overridden via CLI flags at creation time.

```bash
# Bounded root agent:
silo run --mode=project \
  --project=my-gcp-project \
  --service-account=root-agent@my-gcp-project.iam.gserviceaccount.com \
  --max-subagents=20 \
  --max-concurrent=5 \
  --max-depth=3

# Unrestricted root agent:
silo run --mode=project \
  --project=my-gcp-project \
  --service-account=root-agent@my-gcp-project.iam.gserviceaccount.com \
  --unlimited-subagents
```

### 3.3 Validation Invariants

1. `project.id`: MUST match Google Cloud project ID regex `^[a-z][a-z0-9-]{4,28}[a-z0-9]$`.
2. `project.service_account`: MUST be a valid email format matching `*.iam.gserviceaccount.com`.
3. `agent.subagents.max_subagents`: MUST be either an integer $\ge 1$, `null`, or `-1` (unlimited).
4. `agent.subagents.max_concurrent`: MUST be either an integer $\ge 1$, `null`, or `-1` (unlimited). If both are bounded, `max_concurrent` MUST NOT exceed `max_subagents`.
5. `agent.subagents.max_depth`: MUST be an integer $\ge 0$.

---

## 4. Identity, Authentication, and Credential Scoping

### 4.1 Token Minting via Host Impersonation

1. The host Silo supervisor authenticates using host Application Default Credentials (ADC) or Workload Identity.
2. For each agent process, Silo calls the Google IAM Credentials API:
   `POST https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/{sa_email}:generateAccessToken`
3. Silo requests standard cloud platform scopes:
   `["https://www.googleapis.com/auth/cloud-platform"]`
4. The generated access token has a lifetime of 3600 seconds (1 hour). Silo automatically refreshes the token at $T - 5\text{ minutes}$ and updates the running `gcloud-mcp` process environment.

### 4.2 Credential Access Boundaries (CAB)

When `project.downscope_credentials` is `true`, Silo MUST NOT supply the raw access token. Instead, Silo exchanges the token with the Google Security Token Service (STS) for a downscoped token enforcing a Credential Access Boundary policy:

```json
{
  "accessBoundary": {
    "accessBoundaryRules": [
      {
        "availableResource": "//cloudresourcemanager.googleapis.com/projects/PROJECT_NUMBER",
        "availablePermissions": ["*"]
      }
    ]
  }
}
```

Google Cloud API gateways cryptographically enforce this boundary. Even if the service account has broader organization-level permissions, calls targeting resources outside `PROJECT_NUMBER` fail with `403 PERMISSION_DENIED`.

---

## 5. Tool Sandbox & Local `gcloud-mcp` Integration

### 5.1 Zero-Shell Sandbox Invariant

The agent process MUST NOT be granted access to:
- A command-line shell (`bash`, `sh`, `zsh`).
- Host filesystem manipulation tools (arbitrary read/write outside its prompt working copy).
- Raw network socket primitives.

The agent's entire operational interface is defined by its registered MCP servers over `stdio`.

### 5.2 `@google-cloud/gcloud-mcp` Execution

1. Silo spawns the local `gcloud-mcp` process as a child process:
   ```bash
   npx -y @google-cloud/gcloud-mcp
   ```
2. Silo injects the following dedicated environment variables into the process:
   - `CLOUDSDK_AUTH_ACCESS_TOKEN=<scoped_oauth2_token>`
   - `CLOUDSDK_CORE_PROJECT=<target_project_id>`
   - `CLOUDSDK_CORE_DISABLE_PROMPTS=1`
   - `GOOGLE_APPLICATION_CREDENTIALS=""` (unset to prevent fallback to host files)
3. Stdio streams (`stdin`, `stdout`) are piped directly between the agent client and `gcloud-mcp`.

---

## 6. Agent Prompts and Editable Memory Area (`memory-mcp`)

Every agent operates with a dedicated system prompt context and an isolated, persistent memory area that it can dynamically inspect and edit.

### 6.1 Prompt Architecture

1. **System Prompt**:
   - Injected by Silo at agent initialization.
   - Declares the agent's role, operational boundaries, assigned GCP project identifier, and available MCP tools.
   - Instructs the agent on how to manage its memory and delegate to child sub-agents.
2. **Objective Prompt**:
   - For the **Root Agent**: Loaded from `agent.prompt_path` (e.g. `SYSTEM.md`) or passed inline via CLI `--prompt="..."`. Defines the target system architecture and primary deliverables.
   - For **Sub-Agents**: Supplied by the creator in the `spawn_subagent` invocation (`prompt: "..."`). Defines the sub-task, acceptance criteria, or monitoring directive.

### 6.2 Editable Memory Architecture

Because agents possess no host filesystem write capabilities, Silo provides a dedicated, structured memory area exposed through the `memory` MCP toolset.

#### 6.2.1 Memory Operations

The `memory` MCP server provides four core tools:

1. **`read_memory`**:
   - Reads an existing memory entry by key, or returns the entire memory index if `key` is omitted.
   - **Input Schema**:
     ```json
     {
       "key": "string (optional, e.g. 'discovered_resources' or 'checklist')"
     }
     ```
   - **Response**:
     ```json
     {
       "key": "discovered_resources",
       "value": {
         "vpc_network": "vpc-prod",
         "subnets": ["subnet-us-east1-a", "subnet-us-east1-b"],
         "db_instance": "pg-main-01"
       },
       "updated_at": "2026-11-01T05:35:10Z"
     }
     ```

2. **`write_memory`**:
   - Creates or overwrites a memory entry with a structured JSON object or raw markdown string.
   - **Input Schema**:
     ```json
     {
       "key": "string (required)",
       "content": "string or object (required, value to store)"
     }
     ```
   - **Response**: `{"status": "SAVED", "key": "checklist"}`

3. **`append_memory`**:
   - Appends an item to a list or appends text to an ongoing log/markdown section under `key`.
   - **Input Schema**:
     ```json
     {
       "key": "string (required)",
       "content": "string or object (required, entry to append)"
     }
     ```
   - **Response**: `{"status": "APPENDED", "key": "deployment_log"}`

4. **`list_memory`**:
   - Lists all keys currently stored in the agent's memory area with metadata (size in bytes, last modified timestamp).

#### 6.2.2 Storage & Persistence

1. **Storage Location**:
   - Each agent's memory is stored on the host under `/var/lib/silo/agents/{agent_id}/memory/` (or structured SQLite/JSON state store).
2. **Isolation Guarantee**:
   - Memory is strictly partitioned by `agent_id`. An agent can only read and write its own memory store.
3. **Persistence Across Restarts**:
   - For **persistent agents**, the memory area survives process crashes, unexpected exits, and supervisor restarts. When a persistent agent restarts, its memory remains intact, ensuring continuous operational state.
4. **Lifecycle Cleanup**:
   - When a `one_shot` agent finishes, its memory remains available for parent inspection until the sub-agent is explicitly reaped via `stop_subagent` or parent exit.

---

## 7. Sub-Agent Management MCP Server (`subagents-mcp`)

Silo provides a native MCP server exposing tools for hierarchical sub-agent orchestration.

### 7.1 `spawn_subagent`

Creates and starts a new sub-agent.

**Input Schema**:
```json
{
  "name": "string (required, unique identifier among siblings)",
  "description": "string (brief functional summary)",
  "prompt": "string (required, the objective task prompt)",
  "lifecycle": "string (enum: 'one_shot' | 'persistent', default: 'one_shot')",
  "initial_memory": {
    "key": "value (optional map of initial memory entries or markdown seeded to the child)"
  },
  "service_account": {
    "mode": "string (enum: 'inherit' | 'downscoped' | 'specific', default: 'inherit')",
    "email": "string (required if mode == 'specific')",
    "allowed_resources": [
      "string (resource URIs for CAB policy if mode == 'downscoped')"
    ]
  },
  "delegation": {
    "can_spawn_subagents": "boolean (default: false)",
    "max_subagents": "integer (optional lifetime budget)",
    "max_concurrent": "integer (optional concurrency limit)",
    "max_depth": "integer (optional depth limit)"
  },
  "timeout_seconds": "integer (default: 3600)"
}
```

**Success Response**:
```json
{
  "agent_id": "subagent-018f4a2b",
  "name": "network-migrator",
  "status": "RUNNING",
  "lifecycle": "one_shot",
  "created_at": "2026-11-01T05:30:00Z"
}
```

**Error Codes**:
- `QUOTA_EXCEEDED`: Creator has exhausted its `max_subagents` budget.
- `CONCURRENCY_LIMIT_REACHED`: Creator has reached its `max_concurrent` running limit.
- `DEPTH_LIMIT_EXCEEDED`: Creation violates creator's `max_depth`.
- `DELEGATION_NOT_PERMITTED`: Creator attempts to set `can_spawn_subagents: true` without holding delegation rights.
- `INVALID_SERVICE_ACCOUNT`: Requested service account is not permitted by IAM `actAs` or CAB narrowing rules.

### 7.2 `list_subagents`

Returns all active and completed child sub-agents owned by the calling agent.

**Response**:
```json
{
  "subagents": [
    {
      "agent_id": "subagent-018f4a2b",
      "name": "network-migrator",
      "lifecycle": "one_shot",
      "status": "COMPLETED",
      "uptime_seconds": 142,
      "exit_code": 0
    },
    {
      "agent_id": "subagent-018f4a5c",
      "name": "drift-monitor",
      "lifecycle": "persistent",
      "status": "RUNNING",
      "uptime_seconds": 3820,
      "restarts": 1
    }
  ]
}
```

### 7.3 `get_subagent_output`

Retrieves the execution logs, tool call trace, and final output report of a completed `one_shot` sub-agent.

**Input Schema**:
```json
{
  "agent_id": "string (required)"
}
```

**Response**:
```json
{
  "agent_id": "subagent-018f4a2b",
  "status": "COMPLETED",
  "output": "Successfully audited VPC 'vpc-main'. Found 3 subnets and 0 non-compliant firewall rules.",
  "tool_calls_count": 8,
  "completed_at": "2026-11-01T05:32:22Z"
}
```

### 7.4 `get_subagent_memory`

Allows a parent agent to inspect the working memory area of a child sub-agent.

**Input Schema**:
```json
{
  "agent_id": "string (required)",
  "key": "string (optional, reads specific key or all keys if omitted)"
}
```

**Response**:
```json
{
  "agent_id": "subagent-018f4a2b",
  "memory": {
    "checklist": ["inspect subnets [DONE]", "audit firewalls [DONE]"],
    "target_vpc": "vpc-main"
  }
}
```

### 7.5 `send_subagent_message`

Dispatches an instruction or query to an active `persistent` sub-agent.

**Input Schema**:
```json
{
  "agent_id": "string (required)",
  "message": "string (required, message text or directive)"
}
```

**Response**:
```json
{
  "agent_id": "subagent-018f4a5c",
  "status": "DELIVERED"
}
```

### 7.6 `stop_subagent`

Explicitly terminates a sub-agent and cascades termination to all descendants in its subtree.

**Input Schema**:
```json
{
  "agent_id": "string (required)"
}
```

**Response**:
```json
{
  "agent_id": "subagent-018f4a5c",
  "status": "TERMINATED",
  "reaped_children_count": 2
}
```

---

## 8. Hierarchical Delegation and Quota Engine

### 8.1 Quota Accounting Model

Silo maintains a hierarchical tree of quota allocations:

```
                    Root Agent
         [Budget: 20, Active: 2, Depth: 2]
                   /               \
         (carve out 5)           (carve out 1)
                 /                   \
        Manager Sub-Agent        Worker Sub-Agent
   [Budget: 5, Active: 1, Depth: 1]    [Leaf: can_spawn=false]
              /
        (carve out 1)
            /
     Leaf Worker
```

1. **Monotonic Allocation**:
   - If parent budget is finite ($B_p$), the delegated child budget ($B_c$) MUST satisfy:
     $$B_c \le B_p - \text{spawned}_p$$
   - $B_c$ is immediately deducted from the parent's available budget.
2. **Infinite Propagation**:
   - If parent budget is unlimited (`-1`), the parent MAY delegate either a finite budget $B_c$ or an unlimited budget (`-1`) to the child.
3. **Concurrency Ledger**:
   - Spawning increments active count for the creator and all ancestor nodes up to root.
   - Termination decrements active count along the ancestor chain.
4. **Depth Floor**:
   - When an agent is spawned, its `max_depth` is calculated as $\min(\text{requested\_depth}, \text{parent\_depth} - 1)$.
   - When `depth == 0`, `can_spawn_subagents` MUST be `false`, and Silo MUST NOT bind the `subagents` MCP server to that worker.

---

## 9. Sub-Agent Lifecycle Management

### 9.1 One-Shot Sub-Agent Lifecycle

```
[SPAWN] --> PENDING --> RUNNING --> [Agent Emits Final Output] --> COMPLETED
                           |
                           +------> [Exit Code != 0] ----------> FAILED
                           |
                           +------> [Timeout Exceeded] ---------> TIMEOUT
```

- When a `one_shot` agent finishes its objective, it calls `complete_task` (or closes its MCP session).
- Silo captures the final output in its local state store and places the process in `COMPLETED`.
- Silo MUST NOT restart a completed or failed `one_shot` agent.

### 9.2 Persistent Sub-Agent Supervision

- Persistent sub-agents run continuous watch or reconciliation loops.
- If a `persistent` agent process terminates for any reason:
  1. Silo records an audit event (`PERSISTENT_AGENT_RESTART`).
  2. Silo applies exponential backoff restart delay ($100\text{ms}, 200\text{ms}, 400\text{ms}, \dots, \max 10\text{s}$).
  3. Silo restarts the agent with fresh stdio pipes while preserving its ID and message queue.

### 9.3 Cascading Teardown

1. When a sub-agent is stopped (via `stop_subagent`, timeout, or parent exit), Silo queries the quota tree for all descendants.
2. Silo issues `SIGTERM` to the entire process group (`-pgid`).
3. If processes do not terminate within 5 seconds, Silo issues `SIGKILL`.

---

## 10. Structured Audit Logging

All system events are recorded to `/var/log/silo/audit.log` in structured JSON format.

```json
{
  "timestamp": "2026-11-01T05:32:00.125Z",
  "event": "SUBAGENT_SPAWN",
  "parent_id": "root",
  "agent_id": "subagent-018f4a2b",
  "name": "network-migrator",
  "lifecycle": "one_shot",
  "service_account": "root-agent@my-gcp-project.iam.gserviceaccount.com",
  "cab_downscoped": true,
  "quota_remaining": 19
}
```

### Audited Events Catalog:
- `ROOT_AGENT_START`, `ROOT_AGENT_EXIT`
- `SUBAGENT_SPAWN`, `SUBAGENT_EXIT`, `SUBAGENT_RESTART`
- `SUBAGENT_STOP_REQUESTED`, `SUBAGENT_CASCADE_REAPED`
- `QUOTA_EXCEEDED_REJECTED`, `CONCURRENCY_LIMIT_REJECTED`
- `TOKEN_MINTED`, `TOKEN_REFRESHED`
- `MEMORY_READ`, `MEMORY_WRITTEN`, `MEMORY_APPENDED`
- `TOOL_INVOCATION`

---

## 11. Threat Model and Security Analysis

### 11.1 Threat 1: Prompt Injection / Rogue Host Execution
- **Vector**: An agent reads untrusted cloud data (e.g. storage bucket object or log line) containing malicious instructions directing it to execute local shell commands or dump host secrets.
- **Mitigation**: The agent is provided **zero shell tools**. It does not possess `bash`, `sh`, or raw file execution tools. It can only call structured `gcloud-mcp` tools. Prompt injections cannot break out into arbitrary host shell execution.

### 11.2 Threat 2: Cross-Project Data Exfiltration
- **Vector**: A compromised agent attempts to use its credentials to access another GCP project or exfiltrate data to an external bucket.
- **Mitigation**: Credential Access Boundaries (CAB) enforce that the issued OAuth2 token is cryptographically bound to the declared project numbers. Google API gateways drop any request targeting unlisted projects.

### 11.3 Threat 3: Recursive Fork Bomb / Spawning Sprawl
- **Vector**: A buggy or malicious sub-agent recursively spawns hundreds of child agents, exhausting host memory and API quota.
- **Mitigation**: Strict monotonic quota accounting. The root creation-time quota sets an absolute ceiling. Depth limiting ensures spawning stops at `max_depth`. Children cannot exceed the remaining budget of their ancestors.

---

## 12. Implementation Tasks and Phased Roadmap

1. **Phase 1: Module Scaffolding & gcloud-mcp Bridge**
   - Initialize Go module `github.com/bwsalmon/silo/v4` under `v4/`.
   - Implement process supervisor for local `@google-cloud/gcloud-mcp` stdio management.
   - Implement GCP IAM impersonation token minter and CAB downscoping.
2. **Phase 2: Sub-Agent & Memory MCP Servers (`subagents-mcp`, `memory-mcp`)**
   - Implement JSON-RPC 2.0 MCP server over stdio for Silo supervisor.
   - Implement `memory-mcp` primitives: `read_memory`, `write_memory`, `append_memory`, `list_memory`.
   - Implement `subagents-mcp` tools: `spawn_subagent`, `list_subagents`, `get_subagent_output`, `get_subagent_memory`, `send_subagent_message`, `stop_subagent`.
3. **Phase 3: Hierarchical Quota Engine & Budget Accounting**
   - Implement thread-safe tree quota ledger with monotonic carve-out.
   - Implement concurrency limits and depth-based delegation enforcement.
4. **Phase 4: Dual Lifecycle Engine (One-Shot & Persistent Supervision)**
   - Implement one-shot completion tracking and result caching.
   - Implement persistent agent crash detection, backoff restart loop, and cascading SIGTERM/SIGKILL tree reaper.
5. **Phase 5: CLI, Validation & End-to-End Testing**
   - Implement `silo run --mode=project` with creation-time quota flags.
   - Author multi-agent orchestration integration test suite verifying least privilege and quota enforcement.
