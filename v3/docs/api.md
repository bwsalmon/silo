# Silo v3 API (`v3/gateway` & `v3/bridge`)

The gateway (`silo gateway`, also runnable as `silo-gatewayd`) is Silo's root
daemon: the Silo Hub's registry of agents, page links, secret requests,
escalation requests, and the append-only audit log. It does not start agents
itself. An agent is registered (`agent.register`), started by its creator in
any location (a systemd unit, a container, a microVM, a subprocess), and joins
the hub when `silo connect` presents its token to the session bridge (`silo
bridge`, also runnable as `silo-bridge`).

The session bridge owns the ACP sessions, persists transcripts under
`/var/lib/silo/sessions/<agent>/`, holds pending permission prompts under the
default `permissions: "ask"` policy, reverse-proxies `/p/<slug>/...` page links,
routes human-provided secrets directly to `silo connect` without logging them,
and serves `silo-mcp` (`silo mcp`).

## Transport

- **Gateway socket:** Unix stream socket `/run/silo/gateway.sock`, mode `0660`,
  owner `root:silo-gw`.
- **Bridge socket:** Unix stream socket `/run/silo/bridge.sock`, mode `0666`.
  Accepts `silo connect` streams and JSON-RPC 2.0 requests from `silo-mcp` and
  `silo` CLI subcommands.
- **Bridge HTTP / WebSocket server:** default `127.0.0.1:7070`, serving
  `/connect` (WebSocket for `silo connect --url ws://...`),
  `/acp/v1/sessions/:id` (WebSocket for ACP session history + live updates),
  `/p/:slug/...` (HTTP/WebSocket reverse proxy and external URL redirect), and
  `/api/v1/...` REST endpoints.
- **JSON-RPC 2.0**, one JSON object per line in each direction (the framing ACP
  uses over stdio). A request without an `id` is a notification and gets no
  reply. Batch arrays are not supported. A line may be at most 1 MiB.

```
→ {"jsonrpc":"2.0","id":1,"method":"agent.list","params":{"state":["connected"]}}
← {"jsonrpc":"2.0","id":1,"result":[{"id":"main", …},{"id":"root", …}]}
```

From a shell: `silo call [-socket PATH] METHOD [PARAMS-JSON]`, for example
`sudo silo call agent.register '{"name":"web","location":{"kind":"container"}}'`.

## Callers

Every request's params may carry `agent_token` (an agent's session token) or,
from the bridge only, `on_behalf_of: {"agent": "<id>", "approved"?: true}`. A
caller is:

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
| `agent.register` | yes, `parent` optional | only with `may_register`; the parent is always the caller (enters `pending` state when caller has `permissions: "ask"`) |
| `agent.approve`, `agent.permissions` | yes | no |
| `agent.connect` | anyone with an approved registration token | (refused with a session token or while `pending`) |
| `agent.heartbeat`, `agent.disconnect` | no | its own session |
| `agent.stop` | any agent | its descendants |
| `agent.list`, `agent.status` | every agent | itself and its descendants |
| `agent.report` | no | yes (records structured completion report) |
| `page.register` | yes | yes (owner = caller) |
| `page.unregister`, `page.list` | every link | its own and its descendants' links |
| `secret.request` | no | yes |
| `secret.provide`, `secret.reject` | yes | no |
| `secret.list` | every request | its own |
| `request.file` | no | yes |
| `request.list` | every request | its own |
| `request.resolve` | yes | no |
| `audit.read`, `audit.verify` | yes | no |

Every denial is audit-logged (`auth.deny`).

## Errors

Standard JSON-RPC codes (`-32700` parse error, `-32600` invalid request,
`-32601` no such method, `-32602` invalid params, `-32603` internal), plus:

| Code | Meaning |
|---|---|
| `-32001` | permission denied |
| `-32002` | not found (also returned for agents and links the caller may not see) |
| `-32003` | conflict: already registered, already stopped, slug or port taken |

Unknown fields in gateway `params` are rejected with `-32602`. That is also why
no gateway method can carry a secret value.

## Standing agents & Default `permissions: "ask"`

On first start the gateway seeds two agents (design §7.6):

| id | user | parent | permissions | notes |
|---|---|---|---|---|
| `root` | `silo-root` | none | `"ask"` | the root agent; `root: true`, `may_register: true` |
| `main` | `silo-user` | `root` | `"ask"` | the default user agent; `may_register: true` |

Both standing agents — and all newly registered agents unless the human
explicitly configures otherwise — default to **`permissions: "ask"`**. Under
`permissions: "ask"`, every ACP `session/request_permission` call from the agent
and any child `register_agent` call is held in the bridge's pending-permissions
queue until the human approves or denies it.

The gateway writes each standing agent's registration token to
`<state_dir>/tokens/<id>` (0600; `silo-agent@<id>.service` loads it with
`LoadCredential=`), deletes the file when the agent connects, and writes a
fresh one whenever a standing agent disconnects or times out.

## Gateway & Bridge Methods

### `agent.register`, `agent.approve`, `agent.permissions`

```json
{"name": "web", "parent": "main", "user": "silo-a-web",
 "location": {"kind": "container", "detail": "podman:web"},
 "runtime": "claude", "brief": "Build the landing page.",
 "permissions": "ask",
 "limits": {"memory_max": "2G", "cpu_quota": "100%", "tasks_max": 512},
 "budget_usd": 5, "may_register": false,
 "stop_cmd": ["podman", "rm", "-f", "web"]}
```

Only `name` is required. `permissions` defaults to `"ask"` (`"ask"`, `"auto"`,
or `"deny-shell"`); only the human may set `"auto"`. When called by an agent
whose permission policy is `"ask"`, the child is created in `state: "pending"`
until the human approves it via `agent.approve` `{"agent": "web", "approve": true}`
(or via the bridge's `permission.decide` / `POST /api/v1/permissions/:id/decision`).

```json
{"agent": "web", "token": "silo_reg_…", "hub_url": "unix:///run/silo/gateway.sock", "state": "registered"}
```

The human may change an agent's permission policy at any time with
`agent.permissions` `{"agent": "web", "permissions": "auto"}`.

### `agent.connect`

`{"token": "silo_reg_…", "host": "web-7f", "pid": 4242}` →

```json
{"agent": "web", "session_token": "silo_ses_…", "parent": "main",
 "runtime": "claude", "brief": "…", "permissions": "ask", "heartbeat_s": 30}
```

The registration token is used up. The agent becomes `connected`, with
`connected_at`, `last_seen` and `peer` (host, pid, and the socket peer's
uid) recorded. If the agent is still `pending` human approval, `agent.connect`
is refused with `-32001`.

### `agent.heartbeat`, `agent.disconnect`

Called by the wrapper with its `agent_token`.

- `agent.heartbeat` → `{"state": "connected", "secrets": 1}`, or
  `{"state": "stopped", "stop": "graceful" | "kill"}` once the agent has been
  stopped: the wrapper then signals its CLI's process group (`SIGTERM`, then
  `SIGKILL` after 30 s; or `SIGKILL` at once).
- `agent.disconnect` `{"reason": "cli exited 0", "exit_code": 0}` → the agent's
  record. The agent becomes `disconnected` and its session token is revoked.

### `agent.stop`

`{"agent": "web", "mode": "graceful"}` (`mode` defaults to `graceful`; or
`kill`) →

```json
{"stopped": ["web", "web-helper"],
 "tombstone": {"time": "…", "cause": "stopped by agent:main", "children": ["web-helper"]},
 "stop_cmds": {"web": ["podman", "rm", "-f", "web"]}}
```

Stops the agent and every active descendant, parents first: each becomes
`stopped` with a tombstone, loses its registration token, has its page links
marked inactive and its pending secret requests cancelled, and any connected
`silo connect` wrapper immediately signals its child process group.

### `agent.list`, `agent.status`, `agent.read`, `agent.report`

- `agent.list` takes optional `parent`, `state` (`pending`, `registered`,
  `connected`, `disconnected`, `stopped`) and `history`, and returns the agents
  the caller may see, sorted by id, without their briefs.
- `agent.status` `{"agent": "web"}` returns one agent, brief included.
- `agent.report` `{"status": "ok" | "failed" | "needs_attention", "summary": "…"}`
  records the calling agent's completion report.
- `agent.read` (on the bridge) returns structured status and `report` for
  `root`, and includes the session `transcript` for non-root callers (design §8.6).

### `page.register`, `page.unregister`, `page.list`

An internal service:
`{"slug": "app", "title": "My app", "port": 3000, "host": "127.0.0.1", "expose_port": 8443}`;
or an external link: `{"slug": "docs", "title": "Docs", "url": "https://example.com/docs"}`.
Returns the page, with `owner` and `active: true`.

The bridge reverse-proxies HTTP and WebSocket requests under `/p/<slug>/...`
(stripping `Cookie` and `Authorization` headers) to `<host>:<port>`, opens an
optional listener on `expose_port`, or redirects `/p/<slug>/` (`302 Found`) to
an external `http://` or `https://` URL.

### `secret.request`, `secret.provide`, `secret.reject`, `secret.list`

`secret.request` `{"name": "OPENAI_KEY", "reason": "to call the API", "path": "~/.config/app/key"}` →

```json
{"id": "sec-1", "agent": "web", "name": "OPENAI_KEY", "reason": "…",
 "path": "~/.config/app/key", "state": "pending", "created": "…"}
```

`path` defaults to `~/.config/silo/secrets/<name>`; it must be absolute or
`~/…`, without `..`.

The secret value never reaches the gateway, the session transcript, or the
audit log. When the human provides the value via the bridge
(`POST /api/v1/secrets/:id/provide` or `silo secret provide`), the bridge sends
a `secret.write` control frame over the agent's `silo connect` stream;
`silo connect` atomically writes the file with mode `0600` and acknowledges
`{path}`, and the waiting `request_secret` tool/CLI call returns only
`{"status": "stored", "name": "OPENAI_KEY", "path": "…"}`.

### `session.*` and `permission.*` (Bridge)

- `session.list` `{"agent"?: "web"}` → active and on-disk sessions.
- `session.transcript` `{"session": "web", "after"?: 0}` → JSONL transcript events.
- `session.prompt` `{"session": "web", "text": "…"}` → sends an ACP `session/prompt` turn.
- `session.cancel` `{"session": "web"}` → sends an ACP `session/cancel` notification.
- `permission.list` `{"agent"?: "web", "pending"?: true}` → pending or resolved
  permission requests (`acp` tool permissions and `register_agent` approvals).
- `permission.decide` `{"id": "perm-1", "decision": "approve" | "reject", "option_id"?: "…", "comment"?: "…"}` →
  resolves a pending permission request.

### `audit.read`, `audit.verify`

`audit.read` `{"after": <seq>, "limit": <n>}` returns entries with `seq >
after`, at most `limit` (1000 max). `audit.verify` returns `{"ok": true,
"entries": n}` or `{"ok": false, "entries": n, "first_bad_seq": n, "reason": "…"}`.

## Files

| Path | What |
|---|---|
| `/etc/silo/gateway.toml` | gateway configuration (optional) |
| `/etc/silo/clis.d/*.toml` | coding-agent ACP CLI presets (`gemini`, `claude`, `codex`) |
| `/usr/share/silo/root.md` | shipped default root agent instructions (`v3/instructions/root.md`) |
| `/var/lib/silo/gateway/state.json` | agents, tombstones, pages, secret requests, agent requests (0600) |
| `/var/lib/silo/gateway/tokens/<id>` | standing agents' registration tokens (0600) |
| `/var/lib/silo/sessions/<agent>/` | bridge-persisted ACP session transcripts (JSONL) |
| `/var/log/silo/audit.jsonl` | append-only hash-chained audit log (0600 root) |
| `/run/silo/gateway.sock` | the gateway socket |
| `/run/silo/bridge.sock` | the session bridge socket |

## Audit log

One JSON object per line, appended with `O_APPEND` and fsynced:

```json
{"seq":5,"time":"2026-10-24T22:22:10.35Z","actor":"admin:uid=0","event":"agent.approve",
 "payload":{"agent":"web","approved":true,"comment":""},"prev":"5dc4…"}
```

`prev` is the hex sha256 of the previous line (without its newline), so
`audit.verify` detects edited, dropped or reordered lines. Events:
`gateway.start`, `agent.register`, `agent.approve`, `agent.permissions`,
`agent.connect`, `agent.disconnect`, `agent.stop`, `agent.report`,
`page.register`, `page.unregister`, `secret.request`, `secret.provide`,
`secret.reject`, `request.file`, `request.resolve`, `auth.deny`. No entry
contains a token or a secret value.

## Running & Testing

```sh
cd v3 && CGO_ENABLED=0 go build -o silo ./cmd/silo
cd v3 && go vet ./... && go test ./...
```
