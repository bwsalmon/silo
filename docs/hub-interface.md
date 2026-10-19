# The Grain Hub interface: what a Grain deployment speaks to the hub

Part of the [Grain Hub design](hub.md) (§1.3). This is the **contract**
between **Grain Hub** (formerly **Silo**) and a **Grain deployment**
(a **node** as the hub sees it). The hub depends on nothing in a Grain
deployment except these messages. A Grain deployment may be customised
by its owner, or be different code altogether, as long as it speaks them
as written here.

The interface reflects Grain Hub's two core responsibilities
([hub.md](hub.md) §1):

1. **Multi-tenant UI and reachability across Grain deployments:**
   outbound WebSocket connection and control-stream registration
   (`hub/rendezvous`, `hub/registry`), live task and process lifecycle
   and conversation event streaming (`hub/live`), on-demand artifact
   `view` streams (`hub/live`) and full node-UI `http` streams
   (`hub/nodeui`), and interactive UI actions (`cancel`, `approve`,
   `message`) routed over the tunnel to the Grain deployment that owns
   the task.
2. **Discovery and subscription for `.task.md` and `.process.md`
   Grainfiles ([`docs/grainfiles-spec.md`](grainfiles-spec.md)):**
   subscription registration (`subscriptions` in `hello` and `subscribe`
   on the control stream), `grainfile_event` and `trigger_event`
   notifications from forge webhooks, `catchup` queries on
   connect/reconnect, on-demand fetching of matched `.task.md` /
   `.process.md` bodies and authorized same-repo `references` through the
   hub (`fetch_grainfile`, `fetch_reference`), and cross-deployment
   `depends_on` completion and co-trigger notices.

> **Current build note:** In the current repository build
> (`github.com/bwsalmon/silo`, `cmd/silo`), the wire subprotocol
> identifier remains `silo-node.v1`, and `hub/tasks`, `hub/claims`,
> `hub/conformance`, and `scripts/grain-interop.sh` still carry the
> earlier imperative task-stream messages (`offer`, `accept`, `refuse`,
> `resume`, `attest`, `file_task`; see [Legacy imperative task stream in
> the current build](#legacy-imperative-task-stream-in-the-current-build)
> below) so `grain-interop` and existing tests remain unbroken until code
> migration tasks run.

## Connection

The Grain deployment dials out to `wss://<hub>/v1/nodes/connect`,
subprotocol `silo-node.v1` (retained as the wire subprotocol identifier
in the current build), presenting its node token as a bearer token
([hub.md](hub.md) §4.1, §4.2, §5.2). The WebSocket carries the binary
stream multiplexer (`internal/tunnel/mux`); each message below is one
JSON line on a mux stream. Grain Hub never dials inbound to a node.

## Register, subscribe, heartbeat and capacity (the control stream)

The Grain deployment opens the first stream ([hub.md](hub.md) §4.3,
§6.2):

```
node → hub  {"type":"hello","node":{
              "id":"...",
              "repos":["owner/name",...],
              "slots":N,
              "version":"...",
              "labels":{...},
              "features":["interactive","approvals","grainfiles"],
              "subscriptions":{
                "recipients":["https://github.com/acme/grain-eu",...],
                "repos":["https://github.com/acme/widgets",...],
                "folders":["https://github.com/acme/widgets/tree/main/.grain",...],
                "grainfiles":["https://github.com/acme/widgets/blob/main/.grain/processes/smoke.process.md",...]
              }}}
hub → node  {"type":"welcome","owner":"<from the credential>","heartbeat_seconds":15}
            or {"type":"error","error":"..."}, then close
node → hub  {"type":"heartbeat","free":N}      every heartbeat_seconds
node → hub  {"type":"subscribe","request_id":"...","subscriptions":{
              "recipients":["https://...",...],
              "repos":["https://...",...],
              "folders":["https://...",...],
              "grainfiles":["https://...",...]}}
hub → node  {"type":"subscribed","request_id":"...","revision":N}
            or {"type":"subscribed","request_id":"...","error":"..."}
hub → node  {"type":"goodbye","error":"<why>"}  the hub is shutting down, then close
```

- `hello` must arrive within 30s of the WebSocket handshake.
- `id`, `repos`, `slots`, `version`, and `labels` register the node in
  `hub/registry` ([hub.md](hub.md) §3). `owner` is taken from the node
  token credential, never from `hello`.
- `features` lists optional capabilities the deployment supports:
  `interactive` (multi-turn interactive sessions taking `message` turns),
  `approvals` (in-run `approval_request` events answered with `approve`),
  and `grainfiles` (Grainfile subscription and notification protocol).
- `subscriptions` declares what `.task.md` and `.process.md` Grainfiles
  and forge trigger events this deployment subscribes to ([hub.md](hub.md)
  §6.2):

| Field | Meaning |
| --- | --- |
| `recipients` | Plain `https://` deployment identity URL(s) (repository or `/tree/<ref>/<folder>` URL, [`docs/grainfiles-spec.md`](grainfiles-spec.md) §6.2.1) that identify this Grain deployment as a `recipient`. The hub notifies the node whenever a non-draft `.task.grain.md` naming one of these URLs as its resolved `recipient` is added, updated, or removed in any connected repository the node's owner/teams may access. |
| `repos` | Repository URLs (`https://github.com/owner/name`, or `owner/name` shorthand) that the deployment watches for `.task.grain.md` and `.process.grain.md` changes and forge trigger events (`push`, `pull_request`, `merge`). |
| `folders` | Specific repository folder URLs (`https://.../tree/<ref>/<path>`) the deployment watches for Grainfiles. |
| `grainfiles` | Specific `.task.grain.md` or `.process.grain.md` `https://` URLs the deployment subscribes to — including shared processes and cross-deployment `depends_on` targets executed by other deployments ([hub.md](hub.md) §6.5). |

- `subscribe` updates the deployment's active subscription set on the
  live control stream without reconnecting. Each `subscribe` replaces
  the node's prior subscription set and is answered by `subscribed` with
  thesame `request_id` and a monotonic `revision` (or `error` if a URL
  is malformed or outside the node owner's directory admission).
- `free` in `heartbeat` is the deployment's current count of free
  execution slots.
- After `goodbye`, or any connection drop, the deployment reconnects
  with exponential backoff: a replacement hub pod is usually a few
  seconds away during a rolling update.

## Grainfile discovery, trigger notifications and catch-up

Instead of the hub maintaining a parallel imperative task queue, cross-repo
tasks and standing processes live in git as `.task.md` and `.process.md`
files ([`docs/grainfiles.md`](grainfiles.md),
[`docs/grainfiles-spec.md`](grainfiles-spec.md)). Grain Hub consumes
external git repositories via a GitHub App or forge webhooks and API
(never hosting git repositories itself), indexes Grainfile front-matter
metadata across connected repositories (storing metadata only, never
repository checkouts or file bodies at rest), and notifies subscribed
Grain deployments so they can fetch matched Grainfile bodies and
authorized same-repo `references` on demand through the hub (or directly
from git when they hold repository credentials) and execute them under
their own local policy ([hub.md](hub.md) §6.1–§6.4).

### Catch-up on connect or reconnect (the control stream)

When a Grain deployment connects, reconnects after an outage, or updates
its subscriptions, it queries the hub on the control stream to catch up
on all active non-draft `.task.md` and `.process.md` files matching its
subscriptions and any trigger or dependency events since its last known
cursor:

```
node → hub  {"type":"catchup","request_id":"...","since_cursor":"<opaque cursor, or empty for full snapshot>"}
hub → node  {"type":"catchup_result","request_id":"...","cursor":"<new opaque cursor>",
              "grainfiles":[<grainfile metadata>,...],
              "triggers":[<trigger_event payload>,...],
              "dependencies":[<dependency_done payload>,...]}
            or {"type":"catchup_result","request_id":"...","error":"..."}
```

- When `since_cursor` is empty (or older than the hub's event retention
  window), `grainfiles` contains the full set of currently indexed
  non-draft `.task.md` and `.process.md` files matching the node's
  subscriptions, plus any unresolved `triggers` and `dependencies`.
- When `since_cursor` is current, `catchup_result` returns only the
  Grainfile additions/updates/removals, trigger firings, and dependency
  completions since `since_cursor`, advancing the node to `cursor`.

### Grainfile metadata notifications (`grainfile_event`)

When a forge push or merge adds, modifies, or removes a `.task.md` or
`.process.md` matching a deployment's `recipients` or watched
`repos`/`folders`/`grainfiles`, the hub sends `grainfile_event` to the
node (on a hub-opened notification stream or the control stream):

```
hub → node  {"type":"grainfile_event","cursor":"...","action":"added"|"updated"|"removed","grainfile":{
              "url":"https://github.com/acme/widgets/blob/main/.grain/tasks/2026-10-16-widget-schema-v2.task.grain.md",
              "kind":"task"|"process",
              "repo":"https://github.com/acme/widgets",
              "ref":"refs/heads/main",
              "commit":"<git commit SHA>",
              "path":".grain/tasks/2026-10-16-widget-schema-v2.task.grain.md",
              "front_matter":{
                "grainfile":"v1",
                "recipient":"https://github.com/acme/grain-eu",
                "requestor":"Alice <alice@example.com>",
                "draft":false,
                "not_before":"2026-10-25T09:00:00Z",
                "references":["/docs/widget-schema.md"],
                "depends_on":["https://github.com/acme/widgets/blob/main/.grain/tasks/2026-10-16-prep.task.grain.md"],
                "when":{},
                "gate":false
              }}}
```

| Field | Meaning |
| --- | --- |
| `cursor` | Monotonic hub cursor for resuming with `catchup` after a disconnect. |
| `action` | `added` (new non-draft Grainfile, or a draft whose `draft: true` was removed), `updated` (front matter or commit SHA changed while non-draft), or `removed` (file deleted, or `draft` changed from `false` to `true`). |
| `url` | Canonical plain `https://` forge URL of the `.task.grain.md` or `.process.grain.md` file ([`docs/grainfiles-spec.md`](grainfiles-spec.md) §8.1). |
| `kind` | `task` (`.task.grain.md`) or `process` (`.process.grain.md`). |
| `repo`, `ref`, `commit`, `path` | The authoritative repository URL, branch/ref, commit SHA, and repository-relative path where the Grainfile lives ([`docs/grainfiles-spec.md`](grainfiles-spec.md) §6.2.3, §11.1). |
| `front_matter` | Parsed YAML front-matter metadata (`grainfile`, `recipient`, `requestor`, `draft`, `not_before`, `references`, `depends_on`, `when`, `gate`) with repo-local references resolved against `repo` and `ref`. Never includes the Markdown prompt body or repository source files. |

**How the Grain deployment acts on `grainfile_event`:**

1. **Source authority check (local policy):** The deployment checks that
   the authoritative source repository and folder (`repo` + `path`) are
   trusted by its local policy to address work to this `recipient`
   ([`docs/grainfiles-spec.md`](grainfiles-spec.md) §6.2.3, §11.1). If
   not trusted, the deployment ignores or logs the refusal locally.
2. **Drafts and deferral:** Grainfiles with `draft: true` are never
   dispatched (`added` is not sent while `draft: true`; if an already
   registered Grainfile is edited to `draft: true`, the hub sends
   `action: "removed"` so the deployment unregisters its trigger or
   withdraws the unstarted task). If `not_before` is present, the
   deployment holds the task or process inactive until that UTC time or
   date (`YYYY-MM-DD`, meaning `00:00:00Z`) is reached.
3. **On-demand fetch and direct execution:** The deployment fetches the
   `.task.grain.md` or `.process.grain.md` body and its authorized same-repo
   `references` at `commit` — either **on demand through the hub**
   (`fetch_grainfile`, `fetch_reference`, below, without needing
   standing git credentials for `repo`) or directly from git when it
   holds host-scoped credentials for that repository — and either queues
   the `.task.grain.md` in its own backlog (waiting on any `depends_on`) or
   registers the `.process.grain.md` triggers (`when` and
   `gate`).

### Cross-repository content access and on-demand fetch (`fetch_grainfile`, `fetch_reference`)

A Grainfile in **Repo A** enables **Deployment B** (`recipient`) to read
content from Repo A through Grain Hub without Deployment B holding
permanent git credentials for Repo A ([hub.md](hub.md) §6.4.1,
[`docs/grainfiles.md`](grainfiles.md) §3):

- **Grainfile as a repo-scoped capability grant:** When a non-draft
  `.task.md` or `.process.md` is merged into **Repo A** with
  `recipient: <Deployment B>`, Repo A's owners have explicitly
  authorized `<Deployment B>` to receive the Grainfile itself and to
  read the **Repo A** files and folders named in its `references` (as
  well as reading/working on Repo A when the Grainfile in Repo A asks
  `<Deployment B>` to act on Repo A).
- **Same-origin authority rule ("a repository can only grant access to
  itself"):** A Grainfile committed in **Repo A** can authorize
  `<Deployment B>` to read `references` inside **Repo A** (resolved at
  the commit/ref of the Grainfile or the pinned ref in the URL). It
  **cannot** grant `<Deployment B>` access to a third private repository
  (**Repo C**) merely by listing a `https://.../repo-c/...` URL in
  `references` — reading Repo C requires either `<Deployment B>` already
  having access to Repo C, or Repo C itself publishing/authorizing it.
- **On-demand fetch through the hub (never stored at rest):** Because
  the hub database stores only Grainfile metadata and YAML front matter
  ([hub.md](hub.md) §1.1, §6.3), the subscribed Grain deployment opens a
  stream to the hub (`NodeStream`) to fetch the matched `.task.md` /
  `.process.md` body and its authorized same-repo `references` on
  demand:

```
node → hub  {"type":"fetch_grainfile","grainfile_url":"https://github.com/acme/widgets/blob/main/.grain/tasks/2026-10-16-widget-schema-v2.task.grain.md","commit":"<git commit SHA>","vars":{"region":"eu-west-1"}}
hub → node  {"type":"ok","kind":"file","commit":"<resolved commit SHA>","size":N}, then the raw (or rendered, when vars are supplied on a .grain.md.jinja2 template) UTF-8 .task.grain.md / .process.grain.md bytes until the hub closes the stream
            or {"type":"error","error":"..."}

node → hub  {"type":"fetch_reference","grainfile_url":"https://github.com/acme/widgets/blob/main/.grain/tasks/2026-10-16-widget-schema-v2.task.grain.md","commit":"<git commit SHA>","reference":"/docs/widget-schema.md#schema-version-header"}
hub → node  {"type":"ok","kind":"file"|"tree","commit":"<resolved commit SHA>","size":N}, then the file bytes (or tar stream for a tree/folder reference) until the hub closes the stream
            or {"type":"error","error":"..."}
```

| Field | Meaning |
| --- | --- |
| `grainfile_url` | Canonical `https://` URL of the indexed non-draft `.task.grain.md`, `.process.grain.md`, or `.grain.md.jinja2` template in **Repo A** whose `recipient` (or repository subscription) authorizes the fetch. |
| `commit` | Git commit SHA (or ref) at which the Grainfile was indexed in `grainfile_event`, `trigger_event`, or `catchup_result`. |
| `vars` | (`fetch_grainfile` optional; or `?var.<name>=<value>` query parameters on `GET /v1/grainfiles/content`) Optional variable bindings used to render a `.task.grain.md.jinja2` or `.process.grain.md.jinja2` template into a concrete `.grain.md` document. When omitted on a `.jinja2` file, the raw template bytes are returned. |
| `reference` | (`fetch_reference` only) A reference entry declared in that Grainfile's `references` (repo-local `/`-rooted or relative path, or an `https://` URL pointing to **Repo A**). Any `#<anchor>` suffix is stripped when resolving the file path; the deployment extracts the anchored Markdown section locally. |

**How the hub authorizes and serves `fetch_grainfile` and `fetch_reference`:**

1. **Recipient / subscription check:** The hub verifies that
   `grainfile_url` at `commit` is an indexed non-draft Grainfile whose
   resolved `recipient` is in the requesting node's admitted
   `subscriptions.recipients` (or that the node's owner is admitted to
   Repo A in `hub/directory`).
2. **Same-origin reference check (`fetch_reference`):** The hub checks
   that `reference` is listed in that Grainfile's `references` (or lies
   under a folder `/tree/` reference listed in `references`, or inside
   Repo A when `references` names Repo A itself) **and** that the
   resolved target repository is **Repo A** — the same repository where
   `grainfile_url` lives. Any `fetch_reference` targeting a third
   repository **Repo C** on the authority of a Grainfile in Repo A is
   refused (`{"type":"error","error":"cross-origin reference not authorized by source repository"}`),
   unless the requesting node's owner is independently admitted to
   Repo C in `hub/directory`.
3. **Streamed from the forge, never stored at rest:** The hub reads the
   file or tree from the external forge API at `commit` (or the pinned
   `<ref>` in a same-repo `https://` URL) using its GitHub App / forge
   credentials, writes the `ok` header line, streams the bytes directly
   to the node, and closes the stream without storing or caching the
   content in the hub database or disk.

### Forge trigger notifications (`trigger_event`)

When a forge webhook (`push`, `pull_request`, `merge`) matches the `when`
filter (`branches`, `paths`, `ignore`) of a non-draft `.process.grain.md`
that a deployment subscribes to (by `repos`, `folders`, or `grainfiles`), the hub sends `trigger_event`:

```
hub → node  {"type":"trigger_event","cursor":"...","id":"trig-...","process_url":"https://...",
              "source":"push"|"pull_request"|"merge",
              "gate":true|false,
              "event":{
                "repo":"https://github.com/acme/widgets",
                "ref":"refs/heads/main",
                "commit":"<git commit SHA>",
                "pull_request":42,
                "base_ref":"refs/heads/main",
                "changed_paths":["src/widget.go"]}}
```

- `id` deduplicates the trigger firing across reconnects (delivery is
  at least once).
- `source` names the trigger kind (`push`, `pull_request`, `merge` from
  a forge webhook). Schedule (`when.schedule`), `manual`, and `scenario` triggers
  can be evaluated locally by the subscribed deployment
  (or triggered via the UI for `manual`).
- When `gate` is `true`, the deployment runs the `.process.grain.md` as a
  commit or pull-request merge gate ([`docs/grainfiles-spec.md`](grainfiles-spec.md)
  §7.2.2).

### Cross-deployment `depends_on` notices (`dependency_done`)

A `.task.grain.md` on one Grain deployment may declare
`depends_on` referencing a concrete `.task.grain.md`
executed by a different Grain deployment
([`docs/grainfiles-spec.md`](grainfiles-spec.md) §8.3). The
waiting deployment includes those target `.task.grain.md` URLs in its
`subscriptions.grainfiles`.

When the executing deployment streams lifecycle events (below) or `grainfile_complete` reporting
that a run of a `.task.grain.md` has finished (`result` with state
`done`, `failed`, or `cancelled`), Grain Hub routes notices to every
subscribed deployment watching that Grainfile URL ([hub.md](hub.md)
§6.5):

```
hub → node  {"type":"dependency_done","cursor":"...",
              "grainfile_url":"https://github.com/acme/widgets/blob/main/.grain/tasks/2026-10-16-widget-schema-v2.task.grain.md",
              "run_id":"...",
              "state":"done"|"failed"|"cancelled",
              "reason":"..."}
```

- `done` unblocks the waiting deployment's `.task.grain.md`
  run.
- `failed` or `cancelled` means the upstream run did not succeed; the
  waiting deployment must not execute its dependent run as if the
  dependency succeeded, and marks or cancels its waiting run according
  to its local policy.

## Interactive actions from the multi-tenant UI (`cancel`, `approve`, `message`)

Humans signed into Grain Hub's multi-tenant UI (`hub/ui`) can interact
with tasks and process runs on the Grain deployments their team
directory access permits ([hub.md](hub.md) §5.3, §6.6). Grain Hub
authenticates the human caller, checks team access, and routes the
action over the reverse tunnel (`Server.Open`) to the Grain deployment
that owns the run:

```
hub → node  {"type":"cancel","task":{"id":"<hub id>","node_task":"<local id>"},"cancelled_by":"<email>","reason":"..."}
hub → node  {"type":"approve","task":{"id":"<hub id>","node_task":"<local id>"},"request_id":"<request id>","decision":"allow"|"deny","answered_by":"<email>","reason":"..."}
hub → node  {"type":"message","task":{"id":"<hub id>","node_task":"<local id>"},"message":{"id":"m-...","text":"...","attachments":[...],"sender":"<email>","sent_at":"<RFC 3339>"}}
```

### Cancellations (`cancel`)

When an authorized user cancels a task or process run in the hub UI
(`POST /v1/tasks/{id}/cancel`), the hub sends `cancel` to the owning
node. If the node is offline when the cancellation is requested, the
hub holds the pending cancellation and delivers it when the node
reconnects under the same owner.

### Approvals inside a run (`approval_request` and `approve`, optional)

A Grain deployment that wants a human's go-ahead before proceeding — for
instance before a tool call its local policy holds for approval — sends
an `approval_request` event on the run's event stream (below):

```
{"type":"approval_request","seq":N,"task":"<hub id>","approval_request":{"id":"<request id>","tool":"...","args":{...},"message":"...","deadline":"<RFC 3339>"}}
```

`id` must be unique within the run; `deadline`, if set, is when the
deployment stops waiting. A member of the owning team answers it in the
hub UI (`POST /v1/tasks/{id}/approvals/{request_id}`), and the hub sends
`approve` to the node:

| Field | Meaning |
| --- | --- |
| `request_id` | The `id` of the `approval_request` it answers. |
| `decision` | `allow` or `deny`. |
| `answered_by` | The team member who answered, from their hub sign-on. |
| `reason` | Optional explanation from the reviewer. |

The hub sends at most one `approve` for each request: it refuses a
second answer, an answer after `deadline`, and one for a run that has
already finished, and it answers only an `id` the node sent. The Grain
deployment ignores an `approve` for a request it is no longer waiting on
and applies its own local policy to the answer.

### Interactive sessions (`message` and `awaiting_reply`, optional)

An **interactive** session stays open across multiple turns, taking chat
messages from authorized users in the hub UI (`POST /v1/tasks/{id}/messages`)
and ending only when cancelled or when the node closes it with `result`.

```
{"type":"message","task":{"id":"<hub id>","node_task":"<local id>"},"message":{"id":"m-...","text":"...","attachments":[{"name":"...","ref":"...","media_type":"...","size":N}],"sender":"<email>","sent_at":"<RFC 3339>"}}
```

| Field | Meaning |
| --- | --- |
| `id` | Identifies the message within the task. Delivery is **at least once**: the node deduplicates by `id`. |
| `text` | Markdown message body written by the human. |
| `attachments` | References only, never file content: the hub stores no uploaded files at rest. `ref` is an `https` URL or one of the run's own artifact refs, resolved by the Grain deployment under its own policy. |
| `sender` | The authenticated human sender from the hub's sign-on. |
| `sent_at` | When the hub recorded the message (RFC 3339). |

On the run's event stream (below), a Grain deployment running an
interactive session sends:

```
{"type":"awaiting_reply","seq":N,"task":"<hub id>","awaiting_reply":{"state":"waiting","question":"..."}}
{"type":"awaiting_reply","seq":N,"task":"<hub id>","awaiting_reply":{"state":"resumed","message_id":"m-..."}}
{"type":"message","seq":N,"task":"<hub id>","message":{"id":"m-..."}}
```

- `awaiting_reply` `waiting` when the agent pauses for human input (shown
  in the hub UI as "waiting for you"), and `resumed` when it continues.
- `message` when the session ingests a human message, naming its `id`.
  The hub stores and broadcasts its own record of that message in the
  conversation timeline and drops any `id` it never sent for the task.

## Stream events (the event stream, `hub/live`)

For each task or process run it executes, the Grain deployment opens a
stream to the hub (`NodeStream`) and writes the event envelope of
package `events` (`events/events.go`), one JSON event per line
([hub.md](hub.md) §7.1):

```
node → hub  {"type":"status","seq":1,"time":"<RFC 3339>","task":"<id>","node_task":"<local id>","grainfile":"https://...","status":{"state":"running","message":"..."}}
node → hub  {"type":"artifact","seq":2,"time":"<RFC 3339>","task":"<id>","artifact":{"kind":"diff","ref":"<opaque>","size":N}}
node → hub  {"type":"result","seq":3,"time":"<RFC 3339>","task":"<id>","result":{"status":"done","reason":"..."}}
```

- The first line binds the stream to the run (`task` / `node_task`, and
  `grainfile` when the run executes a `.task.md` or `.process.md` file).
  The hub writes nothing back on the event stream.
- Events at or below the run's last `seq` are dropped as duplicates, so
  `seq` must increase monotonically across the sessions the Grain
  deployment runs for that task.
- Lifecycle (`status`, `usage`, `resumable`, `result`, `awaiting_reply`)
  and conversation (`message`, `approval_request`, assistant final text)
  events are stored by the hub and broadcast to authorized subscribers;
  when a run of a `.task.md` or `.process.md` finishes (`result`), the
  hub also uses that lifecycle event to route cross-deployment
  `dependency_done` notices (above).
- Token stream deltas (`transcript` text/thinking/error items) are held
  only in an in-memory ring buffer per run for late joiners.
- Node-only events (diffs, file contents, `tool_call` arguments and
  results, sandbox and tool logs) are never stored by the hub and are
  broadcast only as opaque references:
  `{"type":"artifact","kind":"...","ref":"<opaque>","size":...}`.

## Serve a view (the view stream, `hub/live`)

When an authorized viewer in the hub UI opens an artifact reference
(`GET /v1/tasks/{id}/artifacts?ref=<ref>`), the hub opens a stream to
the owning node ([hub.md](hub.md) §7.2):

```
hub → node  {"type":"view","task":"<hub id>","node_task":"<local id>","ref":"<ref>"}
node → hub  {"type":"ok"}, then the artifact's bytes until the node closes the stream
            or {"type":"error","error":"..."}
```

The hub asks only for a `ref` the node itself listed in the run's
artifact events (`task_refs`), and never stores or caches the streamed
bytes. A node must still serve only refs it has itself listed, never
resolve a `ref` as an arbitrary filesystem path, and return an error for
any other `ref`: the hub's check is defence in depth, not the node's
trust boundary.

## Serve its own UI (the http stream, `hub/nodeui`, optional)

When an authorized viewer opens `https://<node>.<the hub's node UI
domain>`, the hub opens a stream ([hub.md](hub.md) §11,
[grain-ui-proxy.md](grain-ui-proxy.md)):

```
hub → node  {"type":"http","node":"<id>","viewer":"<email>","role":"owner"|"team-admin"|"admin"|"member"|"proposer"|"viewer","team":"<team>","access":"read-only"|"read-write","require_approval":true|false,"theme":"light"|"dark"}
node → hub  {"type":"ok"}, then one HTTP/1.1 request is read and its response written,
            a 101 leaving the stream as the upgraded connection
            or {"type":"error","error":"..."}
```

A Grain deployment answers it only if its owner has enabled UI
view-through, and refuses it otherwise (`nodeui.Serve` and
`nodeui.Refuse` in `hub/nodeui`). When a node advertises `ui` in
`hello` (`{"offered":true,"team":"<team>","access":"read-only"|"read-write"}`,
configured via `-ui-team` and `-ui-access` in `internal/fakegrain` and
`hub/conformance/refgrain`, with `ui.team` inferred automatically when
all `repos` belong to a single team that the node's `owner` belongs to),
`nodeui.Serve` sets `X-Grain-Hub-UI-Role`, `X-Grain-Hub-UI-Viewer`,
`X-Grain-Hub-UI-Team`, `X-Grain-Hub-UI-Require-Approval`, and
`X-Grain-Hub-UI-Theme` (`light` | `dark`) on the tunneled HTTP request,
and both `hub/nodeui` and the node's local UI handler enforce the caller's
role permissions (`owner`, `admin`/`team-admin`, `member`, `proposer`,
`viewer`) and expose `theme`, `role`, `team`, and `require_approval` on
`/api/me` while rejecting any
request that still carries leaked Hub session cookies or
`X-Goog-Authenticated-User-*` headers (`G2–G4`). For tasks placed by or
mirrored to the hub, the node's local task status and UI responses
expose `hub_task_id` and `hub_url` (`<hub>/tasks/<id>`, `U5`).

A Grain deployment serves only the streams defined in this document. It
serves a `view` stream only for an artifact reference it has itself
emitted, and an `http` stream only to its own UI handler.

## Human directory & Hub Admin mode (`GET /v1/me`, `X-Silo-Admin-Mode`)

Human callers signed in through IAP or OIDC query `GET /v1/me` and
the team, task, node, Grainfile, and node-token endpoints
([hub.md](hub.md) §5, [ui.md](ui.md)). Team memberships support four
roles (`admin`, `member`, `proposer`, `viewer`), and per-team settings
(`GET`/`PUT /v1/teams/{team}/settings`) configure `anyone_may_propose`
(default `true`) and `require_approval` (default `false`, two-person
review for member/admin tasks). Configured Hub Admins
(`SILO_HUB_ADMINS` / `directory.API.HubAdmins`) can toggle global Hub
Admin privileges per request via the `X-Silo-Admin-Mode` request header:

- `GET /v1/me` returns both `"can_hub_admin"` (whether the signed-in
  principal is configured in `SILO_HUB_ADMINS`) and `"hub_admin"`
  (whether Hub Admin mode is active on this request).
- When a configured Hub Admin sends `X-Silo-Admin-Mode: off`
  (case-insensitive `off`, `false`, `0`), `Access.CanHubAdmin` remains
  `true` while `Access.HubAdmin` is `false`, scoping `/v1/teams`,
  `/v1/tasks`, `/v1/nodes`, `/v1/grainfiles`, and `/v1/node-tokens` to
  the caller's normal team memberships.
- When `X-Silo-Admin-Mode: on` is sent (or when the header is omitted by
  CLI or backend callers), `Access.HubAdmin` is `true` for configured
  Hub Admins. Non-admins sending `X-Silo-Admin-Mode: on` receive
  `can_hub_admin: false` and `hub_admin: false`.

---

## Legacy imperative task stream in the current build

Until the code migration tasks replace `hub/tasks`, `hub/claims`, and
`hub/conformance` with the Grainfile subscription indexer and router
above, the current Go build (`cmd/silo`, `hub/tasks`, `hub/claims`,
`hub/conformance`, and `scripts/grain-interop.sh`) still implements the
earlier imperative task-stream messages and conformance workloads:

- **Upward filing on the control stream (`file_task` / `filed`):**
  ```
  node → hub  {"type":"file_task","request_id":"...","task":{"repo":"owner/name" | "team":"<team>","title":"...","prompt":"...","origin_task":"<hub id>","depends_on":["<hub id>",...],"node_task":"<local id>"}}
  hub → node  {"type":"filed","request_id":"...","task":{...}}   or {"type":"filed","request_id":"...","error":"..."}
  ```
- **Imperative task stream (`offer`, `resume`, `attest`, `accept`,
  `refuse`, `result`):**
  ```
  hub → node  {"type":"offer","task":{...},"requester_attestation":"<JWT>"}
              {"type":"resume","task":{...},"requester_attestation":"<JWT>"}
              {"type":"attest","task":{"id":...},"requester_attestation":"<JWT>"}
              {"type":"cancel","task":{"id":...}}
              {"type":"approve","task":{"id":...},"request_id":"...","decision":"allow"|"deny","answered_by":"<email>","reason":"..."}
              {"type":"message","task":{"id":...},"message":{"id":"...","text":"...","attachments":[...],"sender":"<email>","sent_at":"<RFC 3339>"}}
              {"type":"dependency_done","task":{"id":...},"state":"done"|"failed"|"cancelled"|"rejected","reason":"...","node_task":"<local id>"}
  node → hub  {"type":"accept","node_task":"<local id>"}
              {"type":"refuse","reason":"..."}
              {"type":"result","state":"done"|"failed","reason":"..."}
  ```
- **Conformance workloads (`silo-conformance/v1 <workload>`):** checked
  by `silo conformance` ([conformance.md](conformance.md)) and
  `hub/conformance/refgrain` for `run`, `hold`, `approve`,
  `interactive`, and `file <owner/name>`, with `run` emitting the diff:
  ```
  --- /dev/null
  +++ b/SILO-CONFORMANCE
  @@ -0,0 +1 @@
  +This change was made by a Grain running the silo conformance kit.
  ```
