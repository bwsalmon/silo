# Agents on Silo

## Status

Normative draft, part of the Silo design ([`README.md`](README.md)),
written in task 1945 as an addendum to task 1930's spec. It records
what bsalmon decided in director chat task 1913 on 2026-10-04. Marks
such as "call 39" refer to
[Calls made in this draft](README.md#calls-made-in-this-draft); calls
37 to 51 are this page's. Questions it leaves open are listed in
[Open questions](#open-questions) here and in the README.

**Implementation is on hold**, as for the rest of silo.

## Contents

1. [Agents are infrastructure](#1-agents-are-infrastructure)
2. [The agent object](#2-the-agent-object)
3. [v1 placement: harnesses on the control VM, tools remote](#3-v1-placement-harnesses-on-the-control-vm-tools-remote)
4. [The tool VM and its tool server](#4-the-tool-vm-and-its-tool-server)
5. [The session socket: how a harness reaches silod](#5-the-session-socket-how-a-harness-reaches-silod)
6. [Credentials](#6-credentials)
7. [Model budget and metering](#7-model-budget-and-metering)
8. [The agent control surface](#8-the-agent-control-surface)
9. [Lifecycle: leases, budget, restarts](#9-lifecycle-leases-budget-restarts)
10. [Capacity of the control VM](#10-capacity-of-the-control-vm)
11. [Security](#11-security)
12. [Mapping grain onto Silo](#12-mapping-grain-onto-silo)
13. [Later](#13-later)
14. [Open questions](#open-questions)

## 1. Agents are infrastructure {#SV-98}

These were decided in task 1913.

- **An agent is a resource in a silo.** It is created and deleted
  through the fence API, like a child silo or an export
  ([`fence-api.md`, Agents](fence-api.md#agents)). It has a harness
  (an adapter such as `claude-code`), a model, a budget, the shape and
  image of its tool VM, and a lease.
- **An agent lives on a VM in its silo: its tool VM.** The tool VM is a
  normal facade instance. It is charged to the silo's regional quota,
  its address is in the silo's own space, and it is subject to the
  silo's fence like any other VM.
- **The code an agent runs gets its silo's egress and grants**, no more.
  Every command runs in the tool VM, so it leaves from the tool VM's
  address, and the gateway knows it as the silo's
  ([`gateway.md`, Origin identity](gateway.md#origin-identity)).
- **Untrusted code goes in a child silo.** Code the agent did not write
  (builds, tests, a foreign repository's scripts) runs in a child silo,
  a **sandbox**, that the agent creates with a slice of its silo's
  allocations. This is Shibu's "untrusted code in another namespace"
  rule, restated as network isolation: the sandbox can't reach the
  agent's VM, its silo, or anything its silo was granted, unless the
  agent exports something to it.
- **Delegation is a child silo with an agent in it.** An agent that
  starts a helper creates a child silo, hands it quota, address space,
  model budget and policy, and creates an agent there. The helper's
  authority is whatever the child was given, and "never looser than
  its parent" bounds it.

### The authority of an agent {#SV-99}

An agent acts as its silo. It has no authority of its own: its tool VM
is a VM in the silo, and its harness's calls are made as the silo
([§5](#5-the-session-socket-how-a-harness-reaches-silod)). Inside an
silo everyone is the silo ([`README.md`, §8](README.md#8-limitations)),
and an agent is no exception. Two agents in one silo can see and
change each other's objects, and either can spend the silo's free
model budget on new agents.

So **an agent that needs a bound of its own gets a silo of its own**
**(call 37)**. That is the recommended layout and the one grain would
use ([§12](#12-mapping-grain-onto-silo)): a parent creates a child silo
per agent, sized for the job, and one agent in it. Several agents in
one silo is allowed, for agents that are meant to share everything.

Two rules stop an agent from loosening its own bounds even within its
silo **(call 38)**: an agent's own session may not renew its own lease,
and may not raise its own budget. Either is refused with
`PERMISSION_DENIED`. Anyone else in the silo, and any ancestor, may.

### Agents in a sealed silo {#SV-100}

A silo's **control policy** (`full`, `read` or `none`, set only by
its ancestors; [`fence-api.md`, Control policy](fence-api.md#control-policy))
binds its agents as it binds everything else acting as the silo: the
tool VM by origin, and the harness on its session socket. Added in
task 1947.

An agent in a sealed silo **can operate its service but can't change
its infrastructure**:

- **It keeps** its tools (`exec` and the rest, over the tool channel,
  which is on the gateway's port 80, not the API port), the model
  proxy, `ask` and `request_approval`, and the control surface of its
  silo's agents: status, sessions, messages and answers
  ([§8](#8-the-agent-control-surface)). It can read logs, restart a
  process, change a config file on its VM, talk to people, and spend
  its budget.
- **It loses** every other fence and facade call. On the socket, the
  fence tools and the `compute` tool are refused with
  `CONTROL_DENIED` or a GCE-shaped 403. From the tool VM, `gcloud` and
  `silo` can't reach the API port at all: the fence drops it. Under
  `read`, the reads among them work.
- **Its lifecycle is its ancestors'.** Creating, stopping, resuming,
  renewing and deleting agents in a sealed silo are mutations, so they
  are done by an ancestor or by the holder of a `manage` grant with
  the `agents` scope. silod makes the tool VM for such a create under
  the caller's authority, not the sealed silo's.

A deploy pipeline holding `manage` on a sealed production cell, with
an operator agent inside it, is walked through in
[`README.md`, A production cell](README.md#a-production-cell).

## 2. The agent object {#SV-101}

```json
{
  "id": "m3x8re",
  "name": "fixer",
  "silo": "p2m8xd",
  "silo_path": "/team-a/t-1945",
  "harness": {"adapter": "claude-code", "version": "2.1.4", "credential": "anthropic-api"},
  "model": "claude-opus-5-5",
  "budget": {"usd": "40.00", "spent_usd": "12.31", "period_start": "2026-10-01T00:00:00Z"},
  "tool_vm": {
    "zone": "us-central1-a",
    "subnet": "work",
    "machine_type": "e2-standard-4",
    "image": "projects/debian-cloud/global/images/family/debian-12",
    "disk_gb": 50,
    "spot": false,
    "instance": "agent-fixer",
    "ip": "10.1.16.3",
    "tool_server": "CONNECTED"
  },
  "lease": {"expire_time": "2026-10-04T22:00:00Z"},
  "prompt": "Fix the off-by-one in the header parser and open a pull request.",
  "owner": null,
  "state": "RUNNING",
  "session": {"id": "s-0f1c9a", "started": "2026-10-04T18:02:11Z", "turns": 41, "restarts": 0},
  "create_time": "2026-10-04T18:01:40Z",
  "labels": {"task": "1945"}
}
```

- **`id`** comes from the same generator as silo ids, so an agent id
  never equals a silo id. **`name`** is a DNS label unique among the
  silo's agents. An agent is named `/team-a/t-1945/agents/fixer` by
  path, or `-m3x8re` by id.
- **`harness`**: `adapter` is one of the adapters this deployment
  ships ([§3](#adapters)); `version` is the adapter's, filled in by
  silod; `credential` names one of the deployment's
  [credentials](#6-credentials). The silo's agent policy must allow all
  three.
- **`model`** must be in the credential's provider and the silo's agent
  policy, and must have a [price](#prices).
- **`budget.usd`** is how much model use the agent may spend per
  budget period, reserved out of its silo's model budget
  ([§7](#7-model-budget-and-metering)). `spent_usd` is what it has
  spent this period.
- **`tool_vm`** is the shape of the [tool VM](#4-the-tool-vm-and-its-tool-server).
  `subnet` must be one of the silo's. `instance`, `ip` and
  `tool_server` are filled in by silod. `tool_server` is `PENDING`,
  `CONNECTED` or `DISCONNECTED`.
- **`lease.expire_time`** is when the agent is deleted unless someone
  renews it ([§9](#9-lifecycle-leases-budget-restarts)). It may be at
  most the silo's effective `agent_policy.max_lease` from now.
- **`prompt`** is the first message of the session. More messages come
  through the [control surface](#8-the-agent-control-surface).
- **`owner`** is the agent whose session created this object, if one
  did. Objects an agent creates (child silos, other agents) record it
  too, as the label-like field `owner_agent`, and are deleted with it
  (call 43).
- **`state`** is one of:

| State | Meaning |
| --- | --- |
| `PROVISIONING` | The tool VM is being created, or its tool server hasn't connected yet |
| `RUNNING` | The harness is working on a turn |
| `IDLE` | The harness finished its turn and waits for a message |
| `WAITING` | The agent asked a question or for an approval, and waits for an answer |
| `STOPPED` | Stopped by a caller (`:stop`); the tool VM is kept |
| `BUDGET_EXHAUSTED` | The agent's budget for this period is spent ([§9](#budget-runs-out)) |
| `DELETING` | The lease ran out, or someone deleted the agent |
| `FAILED` | The tool VM couldn't be created or was lost, or the harness failed to start three times in a row; the reason is in `status_message` |

## 3. v1 placement: harnesses on the control VM, tools remote {#SV-102}

bsalmon's decision for v1:

```
 control VM (silo-control)                         the agent's silo
┌──────────────────────────────────────┐        ┌──────────────────────┐
│ silod ─ credentials, model proxy,     │        │ tool VM 10.1.16.3     │
│        budget meter, audit           │        │  silo-tools (MCP)      │
│  │ session socket (one per agent)    │ tool   │  exec, read_file, …   │
│  ▼                                   │channel │                      │
│ harness container: claude-code ──────┼─gateway┼─▶ commands run here,  │
│   (no local tools, no cloud creds)   │        │   with this silo's     │
│ gateway                              │        │   egress and grants   │
└──────────────────────────────────────┘        └──────────────────────┘
```

- **Harnesses run on the control VM**, `silo-control`, beside silod and
  the gateway. One harness process per agent session, each in its own
  container ([§11](#isolation-on-the-control-vm)).
- **The control VM holds every agent credential**: model API keys, and
  subscription and OAuth logins. It is the only place they are ever in
  the clear. They are stored encrypted in silod's database, and the key
  is only on the control VMs ([§6](#6-credentials)). **No
  credential enters a silo.**
- **Tools stay remote**, as in grain today. The agent's tool VM runs an
  MCP tool server, `silo-tools`. The harness calls it through the
  gateway, and every command runs in the tool VM, with the tool VM's
  origin.
- **The harness's own fence and facade calls** (create a sandbox silo,
  create instances in it, describe its silo) are made as the agent's
  silo, through the agent's [session socket](#5-the-session-socket-how-a-harness-reaches-silod).
- **Harnesses don't run tenant code.** A harness's built-in local
  tools (a shell, file edits) are turned off by its adapter, so the
  only tools it has are the MCP tools silod gives it. The container is
  the backstop if an adapter gets that wrong.

### Adapters {#SV-103}

An adapter is the code in silod that knows how to run one harness:

- how to start it with a prompt, a model and an MCP server list, and
  with its built-in tools off (for Claude Code, `--tools ""` and an
  MCP config naming the session socket; for Codex, the equivalent
  sandbox and tool settings);
- how to resume a session from its saved state
  ([§9](#silod-restarts));
- how to read its event stream: turns, tool calls, usage and errors;
- which credential kinds it takes ([§6](#6-credentials)).

v1 ships `claude-code` and `codex` **(call 39)**, because those are the
harnesses grain runs today, and their CLIs are pinned in the control
image. An adapter name is part of the agent policy, so a parent can
keep a subtree to one harness.

## 4. The tool VM and its tool server

### Creation {#SV-104}

When an agent is created, silod makes its tool VM through the facade,
as the agent's silo. The facade's checks, charges and rewrites all
apply ([`gcp-backend.md`, Instances](gcp-backend.md#instances)), so a
tool VM that wouldn't fit in the silo's quota fails the create with
`QUOTA_EXCEEDED`. The instance is named `agent-<name>`.

- **It shows in the facade** like any instance: listed, described and
  charged. silod marks it with the hidden label `silo-agent=<agent id>`.
- **Facade writes to it are refused** (call 40), with a 403 naming the
  agent: delete, stop, `setMachineType`, `setMetadata` and the rest.
  It is managed through the agent. This stops a tenant from deleting
  it and putting a different VM at its address, which would then
  receive the agent's tool calls.
- **The image** may be any image the facade allows. It needs systemd
  and a POSIX shell. silod sets its `startup-script` (and refuses one
  in the agent's request), which fetches `silo-tools` from
  `http://silo.internal/silo/v1/agents/-/tools/binary` (static, amd64 or
  arm64) and starts it as a systemd unit. An image that already
  carries `silo-tools` of the right version skips the fetch.

### Tools {#SV-105}

`silo-tools` serves MCP with these tools, modelled on grain's sandbox
tools (`pkg/mcp/sandbox_tools.go`):

| Tool | Does |
| --- | --- |
| `exec` | Runs a command with `bash -lc` in a working directory, with a timeout; returns exit status, stdout and stderr, each capped |
| `read_file` | Reads a file, or a range of its lines |
| `write_file` | Creates or overwrites a file |
| `edit_file` | Replaces an exact string in a file, once or everywhere |
| `list_dir` | Lists a directory, optionally recursively, to a depth |

Commands run as the unprivileged user `agent`, which has passwordless
`sudo` inside the VM: the VM is the agent's, and the boundary is the
fence, not the user. `exec` sets `HTTPS_PROXY`, `HTTP_PROXY` and
`NO_PROXY` for the gateway's egress proxy in the command's
environment (call 41), since an agent's tools expect the internet and
the proxy is the only way out. The tenant's own VMs are still left
alone, as [`gateway.md`](gateway.md#egress-proxy) says.

### The tool channel {#SV-106}

**The tool server dials out; nothing listens in the VM** (call 42).
On start, `silo-tools` opens a long-lived connection to
`http://silo.internal/silo/v1/agents/-/tools:attach` on port 80, which
the fence already allows (`n-gw-out`). The gateway upgrades it to a
multiplexed stream and binds it to an agent:

1. origin identity gives the silo from the source IP, as for any call;
2. silod looks up which agent's tool VM holds that IP, and checks that
   the GCE instance id behind the IP is the one it created;
3. if there is one, the channel is that agent's, and its
   `tool_server` becomes `CONNECTED`. Otherwise the gateway answers
   403.

The harness's MCP calls for tools go to the session socket, from
silod to the gateway, and down that agent's channel. If the channel
drops, the tool server reconnects with backoff, and calls made
meanwhile fail with an MCP error the harness can retry.

Dialing out means no new fence rule, no port open in the tool VM for
other VMs in the silo to reach, and the same origin identity as every
other call.

**The channel ends on the control VM that hosts the session.** With
`--control-replicas` or `--gateway-replicas`, `silo.internal` is a load
balancer, and an attach can land on any gateway. A gateway that isn't
on the session's [host](#placement-on-the-control-vms) answers the
attach with a redirect to the host's own gateway address, which is in
the gateway subnet and so already allowed by `n-gw-out`. `silo-tools`
follows it. When a session moves to another control VM, its channel
is closed, and the tool server's next attach is redirected to the new
host.

## 5. The session socket: how a harness reaches silod {#SV-107}

**Each session gets a Unix socket, and the socket is the session's
identity** (call 44). When silod launches a harness, it creates
`/run/silo/sessions/<agent id>/silo.sock` and mounts only that socket
into the session's container. silod knows which agent a connection
belongs to from which socket it arrived on. There is no token:
nothing in a request can change who the caller is, just as nothing in
a request inside the cloud can ([principle 5](README.md#2-principles)).

In-process calls were the other choice. They only work for a harness
written into silod, and v1's harnesses are other people's programs.
An in-process adapter, if one is ever written, would bind its calls
to the session in the same way, without the socket.

The socket serves:

- **MCP**, at `/mcp`. This is the one MCP server in the harness's
  configuration. It offers the [tool VM's tools](#tools), proxied
  down the tool channel; the fence API as tools (`silo_describe`,
  `silo_create_child`, `silo_delete`, `silo_patch`, `silo_export`,
  `silo_import`, `agent_create`, `agent_delete`, `agent_message`); a
  generic `compute` tool that sends one facade request; and the
  agent's own control tools, `ask` and `request_approval`
  ([§8](#8-the-agent-control-surface)).
- **The fence API and the facade over HTTP**, at `/silo/v1/` and
  `/compute/v1/`, for adapters that would rather call them directly.
  They are subject to the silo's control policy like the MCP fence
  tools ([Agents in a sealed silo](#agents-in-a-sealed-silo)).
- **The model proxy**, at `/model/<provider>/`, for API-key
  credentials ([§7](#metering)). It is reachable only on session
  sockets, never through the gateway.

Every call on the socket is made as the agent's silo, with exactly the
authority that silo has from inside the cloud. It is recorded in audit
with `actor: <agent's silo>`, `via: "harness"` and `agent: <agent id>`
([`fence-api.md`, Audit](fence-api.md#audit)). This acting-as is
internal to silod.

The agent can make the same calls from its tool VM, with `gcloud` or
`silo` in an `exec`. They then arrive at the gateway's API port
(`silo.internal:8080`) from the tool VM's address, as the same silo with
the same authority. silod notes the
agent on those records too (`from_tool_vm: <agent id>`), so that
audit can tell an agent's calls from other VMs' in its silo.

## 6. Credentials {#SV-108}

A **credential** is a model provider login held on the control VM:

```json
{"name": "anthropic-api", "provider": "anthropic", "kind": "api_key", "max_sessions": 0}
{"name": "claude-max-bob", "provider": "anthropic", "kind": "subscription", "max_sessions": 2}
```

- **Kinds.** `api_key` is a provider API key. `subscription` is a
  subscription or OAuth login (a Claude or ChatGPT plan), held as the
  harness's own login files.
- **Who manages them.** Root members only, through the front door:
  `silo credential add <name> --provider anthropic --kind api_key`
  reads the secret from stdin; `silo credential login <name>` runs the
  adapter's login flow on the control VM and stores the files it
  writes. Credentials can be listed and removed, never read back.
- **Where they live.** In silod's database
  ([`state.md`](state.md)), so that every control VM can use them,
  encrypted with a key that install puts in the control VMs' metadata
  (call 45). On a control VM only silod can read that key: the
  gateway's and the sessions' containers have no route to the metadata
  server. The database's backups hold the credentials only encrypted,
  without the key. `silo uninstall` deletes the credentials and the key
  before anything else ([`install.md`](install.md#silo-uninstall)).
- **Subscription login files** are decrypted by silod into a session's
  private directory when the session starts. If the harness refreshes
  the login, silod writes the new files back to the database, so the
  next session on that login, on any control VM, gets them.
- **Who may use them.** Each silo's **agent policy** (call 51) lists
  the credentials, adapters and models its subtree may use, and the
  longest lease an agent in it may have. Like egress, the effective
  policy is the intersection with every ancestor's, and a child's
  can't be looser ([`fence-api.md`, Agent policy](fence-api.md#agent-policy)).
- **`max_sessions`** caps how many sessions use the credential at once.
  `0` means no cap. Subscription logins come with rate limits shared by
  every session on them, so their cap defaults to 2.

An API key never reaches a harness. The harness is pointed at the
model proxy on its session socket, with no key, and silod adds the key
on the way out. A subscription login can't be proxied that way, so
its files are copied into that session's container, read-only, and
the harness uses them directly. That makes a subscription session's
container more valuable than an API-key one ([§11](#11-security)).

## 7. Model budget and metering

### The allocator {#SV-109}

**Model budget is a fourth allocator**, handed down the tree like
quota ([`allocators.md`, Model budget](allocators.md#model-budget)):

- The unit is **US dollars per budget period**, kept as micro-dollars
  (call 46). Dollars can be compared across models and providers, and
  tokens can't. The period is one for the whole deployment, set at
  install (`--budget-period`, default `month`: calendar months in UTC).
- It is a **hard partition**, as quota is (call 3). Each silo has
  `granted`, `delegated` (its children's grants) and `used` (its own
  agents' budgets). Creating an agent reserves its `budget.usd` out of
  the silo's free budget, and deleting it returns it.
- **The root's budget** is `--root-model-budget` at install, default
  0, so no agent can spend until an operator says how much.
- An agent's `spent_usd` is metered against its own budget. When a
  period starts, every agent's `spent_usd` goes back to 0. Grants and
  reservations are unchanged.

### Metering {#SV-110}

Every model call is priced and charged to the agent that made it.

- **API keys.** The harness's model calls go through silod's model proxy
  on its session socket. Before forwarding a call, the proxy works out
  its worst case (the prompt's tokens at the input price, plus
  `max_tokens` at the output price) and refuses it if that is more
  than the agent has left. After the call, it charges the actual usage
  from the provider's response, including cache reads and writes at
  their own prices. Refusals are the provider's own error shape (a 429
  with a message naming the budget), so the harness reports them
  sensibly.
- **Subscriptions.** The proxy can't see these calls. The adapter reads
  the usage the harness reports in its event stream after each turn,
  and silod prices it at the **API list price** for that model, and
  charges the agent the same way (call 47). This is an
  API-equivalent cost, not money paid: a subscription is a flat fee.
  It keeps budgets meaning the same thing whatever credential an agent
  uses, and it stops one agent from using up a shared login. Since
  usage is known only after a turn, a subscription session can
  overshoot its budget by up to one turn. silod records the overshoot
  and stops the session.
- **Each charge** is a `model.call` audit record: agent, model,
  credential, tokens by kind, dollars, and whether it was proxied or
  reported.

### Prices {#SV-111}

silod carries a price table per release: provider, model, and dollars
per million tokens for input, output, cache write and cache read.
Root members may override entries (`silo model-price set`). A model
with no price can't be used, so an agent naming one fails at create
with `INVALID_ARGUMENT`.

## 8. The agent control surface {#SV-112}

The agent object is thin. What people and tools need to watch and
steer a running agent is served by silod, under the agent, through the
fence API ([`fence-api.md`, Agents](fence-api.md#agents)). The front
door carries it to the UI and to `silo agent ...` on a laptop.

- **Status**: the agent object itself, with its state, lease and
  budget spent.
- **The session**: `GET .../agents/{agent}/session` returns the
  transcript as events (turns, tool calls and their results, model
  usage, errors). `?follow=true` streams new events as server-sent
  events.
- **Messages**: `POST .../messages` adds a message to the session.
  An `IDLE` agent starts a new turn with it. A `RUNNING` agent gets it
  at its next turn. `GET .../messages` lists them, both ways.
- **Ask and approve**: the agent's `ask` tool records a question and
  its `request_approval` tool records a proposed action. Each puts
  the agent in `WAITING` until it is answered, with
  `POST .../requests/{id}:answer` (`{"text": ...}` or
  `{"approve": true}`). `GET .../requests?state=open` lists them.
- **Actions**: `:stop`, `:resume`, `:renew`.

Who may use it follows the fence API's table: the agent's silo and its
ancestors, which includes the members mapped to them. An agent's own
session may answer none of its own requests.

## 9. Lifecycle: leases, budget, restarts

### The lease runs out {#SV-113}

Ten minutes before `lease.expire_time`, silod adds a message to the
session saying when it ends, so the agent can wrap up and a person can
renew. At expiry (call 43):

1. the harness is stopped and its container removed;
2. the agent becomes `DELETING`, and silod deletes its tool VM, then
   every child silo and agent whose `owner_agent` is this agent
   (bottom-up, as `delete --force` does);
3. its budget reservation goes back to its silo;
4. the agent is removed. Its transcript and audit records are kept
   for 90 days, like other audit records.

`:renew` sets a new `expire_time`, within the silo's
`agent_policy.max_lease` from now. The agent's own session may not
renew it ([call 38](#the-authority-of-an-agent)).

### Budget runs out {#SV-114}

When the agent's `spent_usd` reaches its budget, the proxy refuses its
next model call (or, for a subscription, silod stops it after the turn
that crossed). The agent becomes `BUDGET_EXHAUSTED`: the harness is
stopped and its session saved, and the tool VM is kept, still charged
to quota, until the lease ends. Raising the budget, or the start of
the next period, puts it back to `IDLE` with its session resumed.
Lowering a silo's model budget below what its agents hold fails with
`FAILED_PRECONDITION`, as for quota.

### silod restarts {#SV-115}

**A running harness doesn't outlive silod; its session does**
(call 48). After every turn, each adapter saves its harness's session
state (Claude Code's session file, Codex's rollout) to silod's
database, under the agent, with the transcript. Any control VM can
then resume it.

- When silod stops, for an upgrade, a crash or a reboot of its control
  VM, that VM's session containers stop with it.
- When silod starts, it removes any session container it finds, then
  relaunches every agent hosted on its VM that was `RUNNING`, `IDLE`
  or `WAITING`, with its adapter's resume.
- When a control VM is gone for good (its heartbeat is older than 30
  seconds, [`state.md`](state.md#more-than-one-silod-and-the-leader-call-34)),
  the leader moves its sessions to the other control VMs, which resume
  them the same way.
- A tool call that was in flight is reported to the harness as
  interrupted. The command it started may still be running in the
  tool VM, and `silo-tools` keeps its result for ten minutes, so the
  agent can ask for it.
- Tool VMs are untouched by all this. Their tool servers reconnect,
  and are redirected to the session's host.
- If the control VMs can't carry every session (one is down, or the
  machine type is smaller), the oldest agents start first and the
  rest wait in their state with `status_message` saying so.
- A harness that fails to start three times in a row is `FAILED`.

## 10. Capacity of the control VM

### Placement on the control VMs {#SV-116}

Each session runs on one control VM, its **host**, recorded on the
agent in the database. When a session starts or resumes, the silod
that starts it picks the control VM with the most free session slots,
from the session counts each replica reports in its heartbeat. The
harness's session socket is on its host, and its calls go to that
VM's silod. Any silod can serve them
([`state.md`](state.md#more-than-one-silod-and-the-leader-call-34)).
The heartbeat also carries the host's gateway address, which a tool
server's attach that lands on another control VM is
[redirected](#the-tool-channel) to. An attach for a session whose host
has no live heartbeat is not redirected; the leader moves that session.

### Sizing {#SV-117}

The control VM is now also the harness host. A harness session is an
interactive CLI that spends most of its time waiting on the model:
roughly 0.3 to 0.6 GB of memory and a tenth of a vCPU on average, with
bursts while it parses large tool results. Taking 0.5 GB and 0.2 vCPU
per session, plus 2 GB and 1 vCPU for silod, the gateway and the
container runtime:

| `--control-machine-type` | vCPU, memory | Sessions it can carry |
| --- | --- | --- |
| `e2-standard-2` (the default without agents) | 2, 8 GB | 0: keep it for no agents |
| `e2-standard-8` | 8, 32 GB | about 30 |
| `e2-highmem-8` | 8, 64 GB | about 35, CPU-bound |
| `n2-standard-16` | 16, 64 GB | about 70 |

These are per control VM, and are estimates to be measured. Install
sets `--agent-sessions` for the whole fleet
([`install.md`](install.md#input)). It checks that the control VMs
can carry it between them, and warns if they couldn't with one of them
down.

**Harness CPU and memory aren't charged to silo quota in v1** (call 49).
They aren't GCE resources in the silo, and charging them would make an
silo's `vcpu` mean two different things. But session slots are scarce,
so **concurrent sessions are a global count class**, `agent_sessions`,
handed down like the other [global counts](allocators.md#global-counts).
The root's is `--agent-sessions`. A running, idle or waiting agent
holds one; a stopped, exhausted or deleting one doesn't. If a silo has
none free, creating or resuming an agent fails with `QUOTA_EXCEEDED`.

Each session's container has a memory limit (1 GB by default) and a
CPU share, so one runaway harness can't starve the rest or silod.

## 11. Security

### The control VM is the crown jewel

It holds every agent credential, runs every harness, and runs silod,
which holds the cloud credentials. A compromise of the control VM is a
compromise of all of them: every model login, every silo, the host
project. This is true of silod already ([`gateway.md`, Placement](gateway.md#placement)),
and harnesses add a new way in: a harness is a large program that
parses whatever the model and the tools send it.

What keeps the risk down:

- **Harnesses don't run tenant code.** Only tools do, and tools are in
  the silo. A harness's local tools are off, and its container has no
  shell access to anything that matters.
- **A prompt injection can make an agent misuse its own authority**,
  which is its silo's, but not go further: the harness has no local
  tools to turn it into code on the control VM.
- **A harness exploit** (crafted tool output that breaks the harness
  process) gets the attacker that one container: its session socket,
  which is the agent's silo's authority, and, for a subscription
  session, that login's files. It doesn't get other sessions, the
  API keys, silod or the cloud credentials, as long as the isolation
  below holds.

### Isolation on the control VM {#SV-118}

Each session runs in its own container (call 50):

- **its own user id**, unprivileged, distinct per session, with no
  capabilities, `no_new_privs`, the container runtime's default
  seccomp profile, a read-only root filesystem, and a private
  writable directory for the harness's state;
- **one mount from silod**: its session socket. Subscription sessions
  also get their login's files. Nothing else of silod's;
- **no network for API-key sessions.** Their only way out is the
  socket. Subscription sessions get a network namespace whose only
  route is a small egress proxy on the control VM that allows the
  provider's hosts for that credential (for example `api.anthropic.com`
  and its login-refresh host) and nothing else;
- **no metadata server.** As for the gateway's container, an
  `iptables` rule in its namespace drops `169.254.169.254`, so it
  can't fetch silod's service-account token;
- **limits**: memory, CPU share and a process count.

silod itself runs as its own user. It is the only process on the VM
that can reach the metadata server, and so the only one that can log
in to the database or read the credentials' key.

With `--control-replicas`, every control VM holds the key and runs
harnesses, so each of them is the crown jewel.

Further hardening is an [open question](#open-questions): a
user-space kernel (gVisor) for session containers, moving harnesses
off the VM that holds the cloud credentials, and keeping subscription
logins on a host of their own.

## 12. Mapping grain onto Silo

| grain today | Silo v1 |
| --- | --- |
| task | agent, in a child silo of its own ([call 37](#the-authority-of-an-agent)) |
| sandbox | the agent's tool VM |
| CI jobs | child silos, made per run by the agent or by CI |
| capabilities | egress policy and exports granted to the agent's silo |
| the grain host running harnesses | the control VM |

Some of grain's other pieces have homes too: a task's question
(`ask_question`) is an agent's `ask` request; grain's model proxy
(`pkg/modelproxy`) is silod's model proxy; a capability that is a
credential file in the sandbox today (a GCP key, a GitHub token) would
become an export from a silo that holds it, so that the credential
stays out of the agent's silo. That last one is Shibu's "secrets behind
proxies", which is still [later](README.md#9-later).

## 13. Later

Recorded, not specified.

- **The harness inside the agent's own VM**, or on a remote runner
  whose only network exit is the agent's VM, through a shim (MCP in,
  proxy out). Then every call an agent makes, the harness's included,
  carries its VM's origin, and the control VM runs no harnesses.
  Credentials are injected by the gateway's egress policy, as in
  "`api.anthropic.com` allowed, inject key K, meter against budget B",
  so keys still never enter a VM. Metering moves from the session
  socket to the egress proxy.
- **The subscription and OAuth exception.** Those logins can't be
  proxied, so they stay on whatever runner holds the login, and the
  harness for such an agent runs there, as on the control VM in v1.
- **granule µVMs inside a nested-virt GCE VM** as cheap, fast agent
  VMs: one GCE VM, made through the facade, hosting many agents' tool
  VMs and sandboxes as a granule child silo (the README's "mixed
  nesting").

## Open questions

These are also listed in the README, as 21 to 28.

1. **Hardening the control VM** (open question 21). Is a container per
   session enough, or should sessions run under gVisor, or on a
   separate harness VM without the cloud credentials (with silod
   reached over mTLS, like gateway replicas)? A separate VM costs a
   second machine and keeps every v1 decision above.
2. **Where credentials are kept** (22). v1 encrypts them in silod's
   database with a key in the control VMs' metadata (call 45). Secret
   Manager or Cloud KMS would take the key off the VM, at the cost of
   new IAM for silod's service account.
3. **Subscription logins and their terms** (23). Can one person's
   subscription be used by agents in a silo they don't own, or by
   several sessions at once? v1 caps sessions per login and leaves the
   choice to root members.
4. **Subscription rate limits as an allocator** (24). A login's rate
   windows are shared by every session on it, and v1 doesn't hand
   them down. Should they be?
5. **Lifetime budgets** (25). v1 budgets are per period. A short task
   might want a total ("this agent may spend $5, ever"). It could be a
   second field on the agent, checked beside the period budget.
6. **Charging harness resources** (26). v1 doesn't charge harness CPU
   and memory to silo quota (call 49). If sessions grow, should they be
   a quota class of their own, charged to the agent's silo?
7. **Tool VM images** (27). v1 fetches `silo-tools` at boot. Should
   install publish an image family with it, so that agents start in
   seconds?
8. **Owned objects** (28). v1 deletes the child silos and agents an
   agent created when it is deleted (call 43). Should an agent be able
   to hand one over to its silo, so that it outlives the agent?
