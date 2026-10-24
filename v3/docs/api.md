# silo gateway API (M1a)

The gateway (`silo gateway`, also runnable as `silo-gatewayd`) is silo's root
daemon. This page documents what it implements today: agents (`spawn`,
`stop`, `list`, `status`) and the audit log (`audit.read`, `audit.verify`).
The rest of the API in [design §4.2](design/silo.md#42-api) comes in later
milestones.

## Transport

- Unix stream socket `/run/silo/gateway.sock`, mode `0660`, owner
  `root:silo-gw`. The gateway creates the group `silo-gw` if it is missing.
  Only root and members of `silo-gw` (the bridge's user, and the humans who
  use `silo call`) can connect.
- **JSON-RPC 2.0**, one JSON object per line in each direction (the framing
  ACP uses over stdio). A request without an `id` is a notification and gets
  no reply. Batch arrays are not supported. A line may be at most 1 MiB.
- Several requests can be sent on one connection; they are answered in order.

```
→ {"jsonrpc":"2.0","id":1,"method":"list","params":{"state":["running"]}}
← {"jsonrpc":"2.0","id":1,"result":[{"id":"root", …}]}
```

From a shell: `silo call [-socket PATH] METHOD [PARAMS-JSON]`, for example
`sudo silo call spawn '{"name":"web","cli":"claude","brief":"…"}'`.

## Callers

Every connection is attributed to the peer's uid and pid with
`SO_PEERCRED`. There are no tokens. A caller is one of:

| Role | Who | Audit actor |
|---|---|---|
| admin | uid 0, or a uid in `admin_uids` | `admin:uid=<n>` |
| bridge | the `bridge_user` (default `silo`): the human, through the UI | `bridge` |
| agent | a running agent's uid, *and* the peer pid is inside that agent's unit (`/proc/<pid>/cgroup`) | `agent:<id>` |

The bridge may make a call on behalf of an agent by adding
`"on_behalf_of": {"agent": "<id>", "pid": <pid>}` to the params. The gateway
then checks that `pid` is inside that agent's unit and treats the call as the
agent's. Only the bridge may do this.

Agents' units have the socket in `InaccessiblePaths=` (design §4.1/§7.2): in
production agents reach the gateway only through the bridge. Direct agent
callers are still identified as above, so the rule holds either way.

Any other uid is refused, and the refusal is audit-logged (`auth.deny`).

### Authorization (M1)

| Method | admin / bridge | root agent | other agents |
|---|---|---|---|
| `spawn` | yes, anywhere (`parent` optional) | yes; the child's parent is root | no (`may_spawn` is M2) |
| `stop` | any agent | its descendants | its descendants |
| `list`, `status` | every agent | itself and its descendants | itself and its descendants |
| `audit.read`, `audit.verify` | yes | no | no |

Every denial is audit-logged. In M1a a root-agent spawn does not yet wait for
human approval; that gate arrives with approvals in M1c (design §8.2).

## Errors

Standard JSON-RPC codes (`-32700` parse error, `-32600` invalid request,
`-32601` no such method, `-32602` invalid params, `-32603` internal), plus:

| Code | Meaning |
|---|---|
| `-32001` | permission denied |
| `-32002` | no such agent (also returned for agents the caller may not see) |
| `-32003` | conflict: already running, user in use, not running |

Unknown fields in `params` are rejected with `-32602`.

## Methods

### `spawn`

```ts
params: {
  name: string;        // agent id: 1-25 of [a-z0-9-], starting with a letter,
                       // not ending in "-"; "root" is reserved
  user?: string;       // default "silo-a-<name>"; must be "silo-a-…"
  parent?: string;     // humans only; agents always spawn their own children
  cli: string;         // a name from /etc/silo/clis.d (or the built-in "idle")
  brief: string;       // the first prompt
  limits?: {memory_max?: string, memory_high?: string,   // "512M", "2G", "50%", "infinity"
            cpu_quota?: string,                          // "150%"
            tasks_max?: number};                         // unset fields take default_limits
  lifetime?: {kind: "until-done"} | {kind: "ttl", seconds: number} | {kind: "persistent"};
}
result: {agent: string, user: string, unit: string, slice: string}
```

What it does:

1. If the user does not exist, creates it with `useradd --system`, a uid from
   `uid_min`–`uid_max` (default 61000–61999), its own group, home
   `/var/lib/silo/home/<user>` (mode 0700), shell `/usr/sbin/nologin` and no
   password. An existing user is used only if silo created it (it is in the
   gateway's records, or its uid is in silo's range) and no running agent
   uses it. uid 0 is never used.
2. Starts the transient service `silo-agent-<name>.service` with
   `systemd-run`, as that user, running the command configured for `cli`.
3. Applies the limits to the service and to the agent's slice, so the agent
   and all its descendants are capped together.
4. Records the agent and its parent, and audit-logs `spawn` with the full
   request and the command line.

The depth below root is capped at 3 (design §8.3). `until-done` and
`persistent` both mean "until the process exits or someone stops it" in M1a;
`ttl` sets `RuntimeMaxSec=`.

### `stop`

```ts
params: {agent: string, mode?: "graceful" | "kill"}  // default graceful
result: {stopped: string[],         // the agent, then its descendants
         tombstone: Tombstone}
```

Stops the agent and every descendant: the agent's slice holds its subtree, so
stopping the slice stops them all. `graceful` is SIGTERM, then SIGKILL after
30 s (`TimeoutStopSec=30`); `kill` sends SIGKILL to every process first.
Stopping `root` stops root's children too. Every stopped agent gets a
tombstone:

```ts
interface Tombstone {
  time: string;
  last_state: string;     // the unit's "active_state/sub_state" before the stop, or "gone"
  result?: string;        // systemd's Result=, e.g. "exit-code"
  exit_status?: number;   // ExecMainStatus=
  cause: string;          // "stopped by admin:uid=0", "parent p stopped", "exited", …
  children?: string[];    // the agent's children when it stopped
}
```

The Unix user and home are kept (design §8.4).

### `list`

```ts
params: {parent?: string, state?: ("running" | "stopped" | "exited")[], history?: boolean}
result: AgentInfo[]   // sorted by id; without briefs
```

`parent: ""` selects top-level agents spawned by a human. `history` adds
earlier incarnations of reused ids.

### `status`

```ts
params: {agent: string}
result: AgentInfo     // with the brief, and the live unit_status if running
```

```ts
interface AgentInfo {
  id: string; root?: boolean; user: string; uid: number; home: string;
  cli: string; brief?: string; parent?: string; unit: string; slice: string;
  limits: Limits; lifetime: Lifetime;
  state: "running" | "stopped" | "exited";
  created: string; spawned_by: string;    // audit actor
  tombstone?: Tombstone;
  children: string[];
  unit_status?: {exists, active_state, sub_state, result, main_pid, exec_main_status};
}
```

### `audit.read`, `audit.verify`

```ts
audit.read(params: {after?: number, limit?: number}): AuditEntry[]  // seq > after, at most limit (≤ 1000)
audit.verify(): {ok: boolean, entries: number, first_bad_seq?: number, reason?: string}
```

## Agents and units

```
silo.slice
├─ silo-agent-root.service               root agent, user silo-root
├─ silo-agent-<id>.slice                 a top-level agent's subtree (limits)
│   ├─ silo-agent-<id>.service
│   └─ silo-agent-<id>-<id2>.slice       its child's subtree, nested
│       └─ silo-agent-<id2>.service
```

Dashes inside an id are escaped as `\x2d` in slice names, because systemd
reads a slice's parent from the dashes in its name.

Every agent's service has `PrivateTmp=yes`, `NoNewPrivileges=yes`,
`ProtectSystem=full`, `ProtectKernelTunables=yes`, `ProtectKernelModules=yes`,
`ProtectControlGroups=yes`, `RestrictSUIDSGID=yes`, `MemoryMax=`,
`MemoryHigh=`, `CPUQuota=`, `TasksMax=`, and `InaccessiblePaths=` for
`/etc/silo/secrets`, the gateway state directory, `/var/log/silo` and the
gateway socket (plus `inaccessible_paths` from the config).

- **Children** also get `ProtectHome=tmpfs`, an empty `/var/lib/silo/home`
  with only their own home bound into it, and an empty capability bounding
  set.
- **The root agent** gets `ProtectHome=read-only` and
  `AmbientCapabilities=CAP_DAC_READ_SEARCH` with that as its whole bounding set
  (design §7.2). It can read everything but silo's own state, and write only
  its home.

Unix-user isolation is **accident containment, not adversary containment**:
it stops agents from trampling each other and the system by mistake. It is not
a defence against an agent that attacks the kernel or a setuid-free local
privilege escalation (design §13).

### Root agent

On start the gateway makes sure the root agent (id `root`, user `silo-root`,
never uid 0) is running, creating it through the same spawn path. If its unit
dies after running at least a minute, the reconciler restarts it; its
children are not touched. If it dies sooner, it is restarted at the next
gateway start.

### Restarts

Agent units are independent of the gateway's own unit, so they keep running
when the gateway stops. On start the gateway reloads its state, re-adopts
units that are still running (`agent.adopt`), and tombstones agents whose
units ended meanwhile. While running it polls every `reconcile_interval_s`
(default 5) for exited agents; when a non-root agent exits, its descendants
are stopped (design §8.4, `on_parent_exit = "stop"`).

## Files

| Path | What |
|---|---|
| `/etc/silo/gateway.toml` | configuration (optional; see below) |
| `/etc/silo/clis.d/*.toml`, `*.json` | how to start each CLI |
| `/var/lib/silo/gateway/state.json` | agents, tombstones and silo users (0600 in a 0700 root directory) |
| `/var/lib/silo/home/<user>/` | agent homes (0700, owned by the agent) |
| `/var/log/silo/audit.jsonl` | audit log (0600 root, `chattr +a` where supported) |
| `/run/silo/gateway.sock` | the API socket |

### `gateway.toml`

Every key is optional; these are the defaults.

```toml
admin_uids   = []              # humans' uids, in addition to root
bridge_user  = "silo"
socket_group = "silo-gw"
uid_min      = 61000
uid_max      = 61999
reconcile_interval_s = 5
inaccessible_paths   = []      # more paths hidden from every agent

[default_limits]
memory_max = "2G"
cpu_quota  = "100%"
tasks_max  = 512

[root_agent]
cli    = "idle"                # M1a: keep the unit up; M1b runs the CLI over ACP
brief  = ""
[root_agent.limits]
memory_max = "4G"
cpu_quota  = "200%"
tasks_max  = 1024
```

The paths (`socket`, `state_dir`, `home_root`, `audit_log`, `clis_dir`) can be
overridden too, which is mainly useful for tests.

silo reads a subset of TOML (see `internal/tomlite`): no multi-line strings,
dates or arrays of tables.

### `clis.d`

```toml
# /etc/silo/clis.d/claude.toml
name    = "claude"                      # defaults to the file name
command = ["/opt/silo/cli/claude", "--print", "{brief}"]

[env]
DISABLE_AUTOUPDATER = "1"
```

`{brief}`, `{agent}` and `{home}` in `command` are replaced at spawn time.
The same object works as JSON. The built-in `idle` runs `/bin/sleep infinity`.
The brief is visible to local users through the unit's command line until
M1b delivers it over ACP instead.

The command is chosen by a `Launcher` (`gateway/launcher.go`); M1b replaces
the CLI launcher with one that starts the CLI's ACP adapter.

## Audit log

One JSON object per line, appended with `O_APPEND` and fsynced:

```json
{"seq":5,"time":"2026-10-24T22:22:10.35Z","actor":"admin:uid=0","event":"spawn",
 "payload":{"agent":"smoke","argv":["/bin/sleep","infinity"],…},"prev":"5dc4…"}
```

`prev` is the hex sha256 of the previous line (without its newline), so
`audit.verify` detects edited, dropped or reordered lines. Events so far:
`gateway.start`, `user.create`, `spawn`, `spawn.failed`, `stop`,
`agent.exit`, `agent.adopt`, `auth.deny`.

## Running it

```sh
cd v3 && CGO_ENABLED=0 go build -o silo ./cmd/silo
sudo ./silo gateway            # -config /etc/silo/gateway.toml
```

Under systemd use `Type=notify`: the gateway sends `READY=1` once it is
listening. Packaging and the unit file come with the installer (M1e).

## Tests

`cd v3 && go vet ./... && go test ./...` runs unprivileged: systemd, user
creation, `/proc` and the clock are interfaces with fakes. The integration
test really spawns and stops agents and needs root on a systemd host:

```sh
cd v3 && go test -c -tags integration -o /var/tmp/gw.test ./gateway
sudo /var/tmp/gw.test -test.v -test.run Integration
```
