# silo gateway API (M1a)

The gateway (`silo gateway`, also runnable as `silo-gatewayd`) is silo's root
daemon: the Silo Hub's registry of agents. It does not start agents itself.
An agent is registered (`agent.register`), started by its creator in any
location (a systemd unit, a container, a microVM, a subprocess), and joins the
hub when `silo connect` presents its token (`agent.connect`). This page
documents what the gateway implements today: agents, parent → child messages,
page links, secret requests and the audit log. The rest of the API in
[design §4.2](design/silo.md#42-api) comes in later milestones.

## Transport

- Unix stream socket `/run/silo/gateway.sock`, mode `0660`, owner
  `root:silo-gw`. The gateway creates the group `silo-gw` if it is missing.
  Only root and members of `silo-gw` (the bridge's user, the humans who use
  `silo call`, and local `silo connect` wrappers) can connect. Wrappers in
  other locations reach it through the bridge's `/connect` endpoint (M1b).
- **JSON-RPC 2.0**, one JSON object per line in each direction (the framing
  ACP uses over stdio). A request without an `id` is a notification and gets
  no reply. Batch arrays are not supported. A line may be at most 1 MiB.
- Several requests can be sent on one connection; they are answered in order.

```
→ {"jsonrpc":"2.0","id":1,"method":"agent.list","params":{"state":["connected"]}}
← {"jsonrpc":"2.0","id":1,"result":[{"id":"main", …},{"id":"root", …}]}
```

From a shell: `silo call [-socket PATH] METHOD [PARAMS-JSON]`, for example
`sudo silo call agent.register '{"name":"web","location":{"kind":"container"}}'`.

## Callers

Every request's params may carry `agent_token` (an agent's session token) or,
from the bridge only, `on_behalf_of: {"agent": "<id>"}`. A caller is:

| Role | Who | Audit actor |
|---|---|---|
| admin | uid 0, or a uid in `admin_uids` (by `SO_PEERCRED`), with no `agent_token` | `admin:uid=<n>` |
| bridge | the `bridge_user` (default `silo`): the human, through the UI | `bridge` |
| agent | anyone presenting an agent's session token; or the bridge relaying `on_behalf_of` a **connected** agent | `agent:<id>` |
| other | any other peer: may call only `agent.connect` | `uid=<n>` |

Tokens are 32 random bytes, prefixed `silo_reg_` (registration, single use)
or `silo_ses_` (session). The gateway stores only their sha256 and never logs
them. A session token is revoked when its agent disconnects; once the agent
is stopped it answers only `agent.heartbeat` and `agent.disconnect`.

### Authorization

| Method | human (admin / bridge) | agent |
|---|---|---|
| `agent.register` | yes, `parent` optional | only with `may_register`; the parent is always the caller |
| `agent.connect` | anyone with the registration token | (refused with a session token) |
| `agent.heartbeat`, `agent.inbox`, `agent.disconnect` | no | its own session |
| `agent.stop` | any agent | its descendants |
| `agent.list`, `agent.status` | every agent | itself and its descendants |
| `agent.message` | any agent | **only its own children** (agents whose `parent` it is) |
| `page.register` | yes | yes (owner = caller) |
| `page.unregister`, `page.list` | every link | its own and its descendants' links |
| `secret.request` | no | yes |
| `secret.provide`, `secret.reject` | yes | no |
| `secret.list` | every request | its own |
| `audit.read`, `audit.verify` | yes | no |

Every denial is audit-logged (`auth.deny`). In M1a a root-agent registration
does not yet wait for human approval; that gate arrives with approvals
(design §8.2).

## Errors

Standard JSON-RPC codes (`-32700` parse error, `-32600` invalid request,
`-32601` no such method, `-32602` invalid params, `-32603` internal), plus:

| Code | Meaning |
|---|---|
| `-32001` | permission denied |
| `-32002` | not found (also returned for agents and links the caller may not see) |
| `-32003` | conflict: already registered, already stopped, slug or port taken, inbox full |

Unknown fields in `params` are rejected with `-32602`. That is also why no
method can carry a secret value.

## Standing agents

On first start the gateway seeds two agents (design §7.6):

| id | user | parent | notes |
|---|---|---|---|
| `root` | `silo-root` | none | the root agent; `root: true` |
| `main` | `silo-user` | `root` | the default user agent; `may_register: true` |

Both are `standing`. The gateway writes each one's registration token to
`<state_dir>/tokens/<id>` (0600; `silo-agent@<id>.service` loads it with
`LoadCredential=`), deletes the file when the agent connects, and writes a
fresh one whenever a standing agent disconnects or times out. Standing agents
that were stopped are re-armed on the next gateway start. Either can be
turned off in `gateway.toml` (`[root_agent] enabled = false`, `[user_agent]
enabled = false`); with root off, `main` has no parent.

## Methods

### `agent.register`

```json
{"name": "web", "parent": "main", "user": "silo-a-web",
 "location": {"kind": "container", "detail": "podman:web"},
 "runtime": "claude", "brief": "Build the landing page.",
 "limits": {"memory_max": "2G", "cpu_quota": "100%", "tasks_max": 512},
 "budget_usd": 5, "may_register": false,
 "stop_cmd": ["podman", "rm", "-f", "web"]}
```

Only `name` is required. `name`: 1–25 of `[a-z0-9-]`, starting with a letter;
`root` is reserved, and an active agent's name conflicts (a stopped agent's
is reused, the old record going to history). `parent`: human callers only.
`user`: optional, never `root` or the root agent's user, and not a standing
agent's. `location.kind`: `systemd`, `container`, `microvm`, `subprocess`,
`remote` or `other` (the default). Limits default to `default_limits`.
Depth is capped at 3 levels below root.

```json
{"agent": "web", "token": "silo_reg_…", "hub_url": "unix:///run/silo/gateway.sock"}
```

The token is shown once. Pass it to `silo connect` as `SILO_AGENT_TOKEN`.

### `agent.connect`

`{"token": "silo_reg_…", "host": "web-7f", "pid": 4242}` → 

```json
{"agent": "web", "session_token": "silo_ses_…", "parent": "main",
 "runtime": "claude", "brief": "…", "heartbeat_s": 30}
```

The registration token is used up. The agent becomes `connected`, with
`connected_at`, `last_seen` and `peer` (host, pid, and the socket peer's
uid) recorded.

### `agent.heartbeat`, `agent.inbox`, `agent.disconnect`

Called by the wrapper with its `agent_token`.

- `agent.heartbeat` → `{"state": "connected", "inbox": 2, "secrets": 1}`, or
  `{"state": "stopped", "stop": "graceful" | "kill"}` once the agent has been
  stopped: the wrapper then signals its CLI's process group (`SIGTERM`, then
  `SIGKILL` after 30 s; or `SIGKILL` at once).
- `agent.inbox` → the waiting messages, oldest first, which are then
  forgotten (audited as `agent.deliver`):
  `[{"seq": 1, "from_agent": "main", "from": "agent:main", "text": "…", "time": "…"}]`.
  `from_agent` is absent for the human.
- `agent.disconnect` `{"reason": "cli exited 0"}` → the agent's record. The
  agent becomes `disconnected` and its session token is revoked.

A connected agent with no heartbeat for `heartbeat_timeout_s` (default 90) is
marked `disconnected` by the gateway (checked every `reconcile_interval_s`,
default 15).

### `agent.stop`

`{"agent": "web", "mode": "graceful"}` (`mode` defaults to `graceful`; or
`kill`) →

```json
{"stopped": ["web", "web-helper"],
 "tombstone": {"time": "…", "cause": "stopped by agent:main", "children": ["web-helper"]},
 "stop_cmds": {"web": ["podman", "rm", "-f", "web"]}}
```

Stops the agent and every active descendant, parents first: each becomes
`stopped` with a tombstone (`"parent web stopped"` for descendants), loses
its registration token and undelivered messages, has its page links marked
inactive and its pending secret requests cancelled. `stop_cmds` are for the
caller to run (the gateway never runs them). Stopping an already stopped
agent is a conflict.

### `agent.list`, `agent.status`

`agent.list` takes optional `parent`, `state` (any of `registered`,
`connected`, `disconnected`, `stopped`) and `history` (include earlier
incarnations of reused names), and returns the agents the caller may see,
sorted by id, without their briefs. `agent.status` `{"agent": "web"}` returns
one, brief included. Both report `children` and `inbox` (the number of
waiting messages); neither ever reports a token or a message's text.

### `agent.message`

`{"agent": "web", "text": "Also add a dark mode."}` →
`{"agent": "web", "seq": 3, "queued": 1}`.

Strictly downward (design §8.7): an agent may message only agents whose
`parent` it is; the human may message any agent. Messages up (child →
parent, anything → root), sideways (to siblings) or skipping a generation
(to grandchildren) are refused and audited. Text is 1–64 KiB; at most 100
messages wait per agent. A registered but unconnected child still queues.
Messages to a stopped agent are a conflict. The audit entry includes the
text.

### `page.register`, `page.unregister`, `page.list`

An internal service:
`{"slug": "app", "title": "My app", "port": 3000, "host": "127.0.0.1", "expose_port": 8443}`;
or an external link: `{"slug": "docs", "title": "Docs", "url": "https://example.com/docs"}`.
Returns the page, with `owner` (the calling agent; absent for the human) and
`active: true`.

Slugs are 1–32 of `[a-z0-9-]` (not `api`, `acp`, `static`, `login`), unique
among active links; the owner may re-register its own slug to change it.
`host` defaults to `127.0.0.1`. `expose_port` is 1024–65535 and unique among
active links. URLs must be `http` or `https`, with a host and no credentials.
An agent may own at most 50 active links.

`page.unregister` `{"slug": "app"}` removes a link (its owner, an ancestor of
the owner, or the human). `page.list` `{"all": false}` lists active links the
caller may manage; `all` includes inactive ones.

### `secret.request`, `secret.provide`, `secret.reject`, `secret.list`

`secret.request` `{"name": "OPENAI_KEY", "reason": "to call the API", "path": "~/.config/app/key"}` →

```json
{"id": "sec-1", "agent": "web", "name": "OPENAI_KEY", "reason": "…",
 "path": "~/.config/app/key", "state": "pending", "created": "…"}
```

`path` defaults to `~/.config/silo/secrets/<name>`; it must be absolute or
`~/…`, without `..`. One pending request per name per agent, and at most 20.

The value never reaches the gateway. The UI hands it to the bridge, which
sends it over the agent's `silo connect` session; `silo connect` writes it
with mode 0600 and answers `{status: "stored", name, path}`. The bridge then
calls `secret.provide` `{"id": "sec-1", "path": "<as written>"}` (`path`
optional) and the request becomes `stored`. `secret.reject`
`{"id": "sec-1", "note": "use the deploy key"}` makes it `rejected`.
`secret.list` `{"pending": true}` lists the human's or the agent's own.

### `audit.read`, `audit.verify`

`audit.read` `{"after": <seq>, "limit": <n>}` returns entries with `seq >
after`, at most `limit` (1000 max). `audit.verify` returns `{"ok": true,
"entries": n}` or `{"ok": false, "entries": n, "first_bad_seq": n, "reason": "…"}`.

## Files

| Path | What |
|---|---|
| `/etc/silo/gateway.toml` | configuration (optional; see below) |
| `/var/lib/silo/gateway/state.json` | agents, tombstones, messages, pages, secret requests (0600 in a 0700 root directory) |
| `/var/lib/silo/gateway/tokens/<id>` | standing agents' registration tokens (0600) |
| `/var/log/silo/audit.jsonl` | audit log (0600 root, `chattr +a` where supported) |
| `/run/silo/gateway.sock` | the API socket |

### `gateway.toml`

Every key is optional; these are the defaults.

```toml
admin_uids   = []              # humans' uids, in addition to root
bridge_user  = "silo"
socket_group = "silo-gw"
hub_url      = "unix:///run/silo/gateway.sock"   # what agent.register tells agents to dial
heartbeat_timeout_s  = 90
reconcile_interval_s = 15

[default_limits]
memory_max = "2G"
cpu_quota  = "100%"
tasks_max  = 512

[root_agent]
enabled = true
user    = "silo-root"
runtime = "claude"
brief   = ""
[root_agent.limits]
memory_max = "4G"
cpu_quota  = "200%"
tasks_max  = 1024

[user_agent]
enabled = true
name    = "main"
user    = "silo-user"
runtime = "claude"
brief   = ""
[user_agent.limits]
memory_max = "4G"
cpu_quota  = "200%"
tasks_max  = 1024
```

The paths (`socket`, `state_dir`, `audit_log`) can be overridden too, which is
mainly useful for tests.

silo reads a subset of TOML (see `internal/tomlite`): no multi-line strings,
dates or arrays of tables.

## Audit log

One JSON object per line, appended with `O_APPEND` and fsynced:

```json
{"seq":5,"time":"2026-10-24T22:22:10.35Z","actor":"agent:main","event":"agent.message",
 "payload":{"agent":"web","from_agent":"main","seq":1,"text":"…"},"prev":"5dc4…"}
```

`prev` is the hex sha256 of the previous line (without its newline), so
`audit.verify` detects edited, dropped or reordered lines. Events:
`gateway.start`, `agent.register`, `agent.connect`, `agent.disconnect`,
`agent.stop`, `agent.message`, `agent.deliver`, `page.register`,
`page.unregister`, `secret.request`, `secret.provide`, `secret.reject`,
`auth.deny`. No entry contains a token or a secret value.

## Running it

```sh
cd v3 && CGO_ENABLED=0 go build -o silo ./cmd/silo
sudo ./silo gateway            # -config /etc/silo/gateway.toml
```

Under systemd use `Type=notify`: the gateway sends `READY=1` once it is
listening. Agents keep running while the gateway restarts, and their
sessions survive it (they are in `state.json`). Packaging, the
`silo-agent@.service` template and `silo connect` come in M1b.

## Tests

`cd v3 && go vet ./... && go test ./...` runs unprivileged: the tests drive
the gateway through its JSON-RPC dispatch with a fake clock, plus a real Unix
socket for framing and `SO_PEERCRED`.
