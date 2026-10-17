# The task interface: what a Grain speaks to the hub

Part of the [hub design](hub.md) (§1.3). This is the **contract**
between the Silo hub and a Grain. The hub depends on nothing in a Grain
but these messages. A Grain may be customised by its owner, or be
different code altogether, as long as it speaks them as written here.

Everything below is built: the connection and control stream in
`hub/rendezvous` (`rendezvous.go`), the task stream in `hub/tasks`
(`stream.go`), filing a task and its dependencies in `hub/tasks`
(`filing.go`, `depends.go`), and the event and view streams in
`hub/live`. The
sections of [hub.md](hub.md) cited give the full semantics.

Whether a Grain speaks it is checked by the conformance kit,
`silo conformance` ([conformance.md](conformance.md)), which drives a
Grain through the [conformance workloads](#conformance-workloads)
below. The reference Grain in `hub/conformance/refgrain` passes it.

## Connection

The Grain dials out to `wss://<hub>/v1/nodes/connect`, subprotocol
`silo-node.v1`, presenting its node token as a bearer token (hub.md
§4.1, §4.2, §5.2). The WebSocket carries the mux; each message below is
one JSON line on a mux stream. The hub never dials a Grain.

## Register, heartbeat and capacity (the control stream)

The Grain opens the first stream (hub.md §4.3):

```
node → hub  {"type":"hello","node":{"id":"...","repos":["owner/name",...],"slots":N,"version":"...","labels":{...},"features":["interactive"]}}
hub → node  {"type":"welcome","owner":"<from the credential>","heartbeat_seconds":15}
            or {"type":"error","error":"..."}, then close
node → hub  {"type":"heartbeat","free":N}      every heartbeat_seconds
hub → node  {"type":"goodbye","error":"<why>"}  the hub is shutting down, then close
```

`features` lists the optional parts of this interface the Grain
supports. The hub reads only `interactive` (below). A Grain that
lists none is never offered a task that needs one. The hub keeps, but does not read,
`approvals`, which says the Grain sends `approval_request` and takes
`approve` (below), and `filing`, upward filing; the conformance kit
reads both, to skip what a Grain does not claim. `hello` must come
within 30s of the handshake. `free` is the Grain's
own count of free slots, counting every task it has accepted. After
`goodbye`, or any drop, the Grain reconnects with backoff: the hub's
replacement is usually a few seconds away (a rolled pod). A Grain that
does not know `goodbye` loses nothing by ignoring it, since the
connection then ends anyway.

## File a task at the hub (the control stream, optional)

A Grain whose agent proposes work for a repository or team the Grain
does not serve, typically one another team owns, hands it to the hub on
the control stream (hub.md §6.6):

```
node → hub  {"type":"file_task","request_id":"<the node's name for it>","task":{
              "repo":"owner/name" | "team":"<team>",
              "title":"...","prompt":"...",
              "origin_task":"<hub id>",            optional: the hub task whose run files this
              "depends_on":["<hub id>",...],       optional: hub tasks the new one waits for
              "node_task":"<local id>"}}           optional: the Grain's own task that waits on the new one
hub → node  {"type":"filed","request_id":"...","task":{...}}       the task filed, a `tasks.Task`
            or {"type":"filed","request_id":"...","error":"..."}   nothing filed, and why
```

| Field | Meaning |
| --- | --- |
| `request_id` | The Grain's name for the filing, 1-128 bytes; the `filed` answering it carries it back. Answers may come in any order. |
| `repo`, `team` | The target, exactly one, as in `POST /v1/tasks`. |
| `title`, `prompt` | What to do; the title defaults to the prompt's first line. |
| `origin_task` | The hub task the filing comes from, if the agent is running one: it must be a task this Grain runs now (`running`, offered to this node as its owner). Absent for work from a task that came from no hub task. |
| `depends_on` | Up to 16 hub task IDs the new task waits for: each one the Grain's owner may see. |
| `node_task` | The Grain's own ID, at most 200 bytes, for a local task that waits on the new hub task: the hub sends `dependency_done` (below) when the new task ends. |
| `node` | Optional; if present it must be the Grain's own node ID. The hub records the filer from the connection, never from the filing. |

The hub, not the filing, decides who the task is for and whether it
needs approval:

- Its **submitter** is the submitter of `origin_task`, the human the
  agent works for; with no `origin_task` it is the Grain's owner. The
  Grain (`filed_by`, its node ID) and its owner (`filed_by_owner`) are
  recorded as its **filer**, beside it.
- It is **queued** only if both the submitter and the Grain's owner are
  members of the team that owns the repository, by `user:<email>`; it
  is **proposed** otherwise, for a member of that team to approve, as a
  human's proposal is. A team-targeted filing is refused unless both
  are in the team. `approved_by` is never carried over from
  `origin_task`, and the submitter may not approve the task they are
  submitter of. An agent cannot launder approval by filing through a
  Grain: whatever it files outside its submitter's team waits for a
  member of that team.

A Grain may file at most **10 tasks at once, and one more each minute**
(the hub's `tasks.Filer` defaults), and have at most 4 filings awaiting
their `filed` at once. A filing over either limit is answered with
`error`, and refusals count against the rate. `filed` with `error`
does not end the connection, unlike `error`. A hub that takes no
filings answers every one with `error`. A Grain that does not file
tasks never sends `file_task`.

## Accept or refuse a task, report its result (the task stream)

The hub opens one stream per task (hub.md §6.2):

```
hub → node  {"type":"offer","task":{...},"requester_attestation":"<JWT>"}    run this task
            {"type":"resume","task":{...},"requester_attestation":"<JWT>"}   a task you ran when the hub lost you: still yours?
            {"type":"attest","task":{"id":...},"requester_attestation":"<JWT>"}  a fresh attestation for a task you accepted
            {"type":"cancel","task":{"id":...}}                              stop it
            {"type":"approve","task":{"id":...},"request_id":"...",          a human's answer to an approval_request
             "decision":"allow"|"deny","answered_by":"<email>","reason":"..."}
            {"type":"message","task":{"id":...},"message":{"id":"...",       a human's message to an interactive task
             "text":"...","attachments":[...],"sender":"<email>","sent_at":"<RFC 3339>"}}
            {"type":"dependency_done","task":{"id":...},"state":"...",      a hub task your own task waits on has ended
             "reason":"...","node_task":"<local id>"}
node → hub  {"type":"accept","node_task":"<local id>"}
            {"type":"refuse","reason":"..."}
            {"type":"result","state":"done"|"failed","reason":"..."}
```

`task` is `tasks.Spec`, and carries the hub's claims about the task
(hub.md §1.2):

| Field | Meaning |
| --- | --- |
| `id` | The hub's task ID. |
| `team` | The team the task belongs to. |
| `repo` | `owner/name`, or empty for a team-targeted task, where the Grain chooses the repository. |
| `kind` | `interactive` for a session (below); absent for a task that runs to its result. Not among the attested claims. |
| `title`, `prompt` | What to do. |
| `submitter` | The requester, from their sign-on to the hub. |
| `approved_by` | The human who approved the task at the hub: a member of the task's team, never its submitter, who approved it while it was proposed. Absent for a task nobody approved there, which is every task a member of the team filed directly. |
| `approved_at` | When `approved_by` approved it (RFC 3339); absent with it. |
| `depends_on` | Hub tasks that were done before the task was offered; absent for none. |
| `filed_by`, `filed_by_owner` | The Grain that filed the task (above), and the owner it was registered by: the hub's record of its filer. Absent for a task a human filed. |
| `origin_task` | The hub task whose run filed it; absent with `filed_by`, or for a Grain's filing from no hub task. |
| `constraints` | The labels or node ID the task was placed by. |
| `created_at` | When it was submitted. |

`capabilities` are not carried yet. A Grain files a task without
`approved_by` for its own local approval. A task filed at the hub by
someone outside the team that owns its repository is *proposed*, and
is never offered until a member of that team approves it (hub.md
§6.1), so a Grain is only ever offered such a task with `approved_by`
set. Whether a Grain runs a task with or without `approved_by`, and
whose approval it accepts, is its own local policy's.

An offer or resume from a hub with a signing key also carries
`requester_attestation`: a JWT, signed for this Grain's node ID, of the
claims grain's local policy checks (`iss`, `aud`, `iat`, `exp`,
`hub_task_id`, `requester`, `team`, `approved_by`,
`granted_capabilities`, `spec_digest`), with the hub's keys at
`/v1/claims/keys` (hub.md §6.4). A Grain whose policy names
`silo_issuer` verifies it and refuses an offer without one; any other
Grain may ignore it. `spec_digest` is over the task's `repo`, `title`
and `prompt` exactly as offered, so a Grain that checks it files the
task's body as the prompt, unchanged, and links the task to the hub by
`hub_task_id` rather than by anything added to the body.

An attestation is good for an hour (`exp`), and a Grain checks the one
kept beside a hub task again before each dispatch of it, which may be
hours after it accepted the task. So while the task is the Grain's,
the hub writes `attest` on its stream every half hour: a newly signed
`requester_attestation` for the same task, with the same claims but
`iat` and `exp`. A Grain verifies it as it did the offer's, against
the task it filed, and keeps it in place of the old one; one it cannot
verify it drops, keeping the old. A Grain that does not know `attest`
may ignore it, and then holds a task whose attestation has lapsed
until the hub next resumes it.

The Grain takes these claims on trust and holds the task to its own
local policy, refusing what that policy does not allow (hub.md §1.2).
No answer to an offer within a minute counts as a refusal. After
`accept`, the stream stays open for the task's life: the Grain writes
`result` there, and the hub writes `cancel`, `approve` and `message` there. A cancel for a task
with no open stream arrives as the only message on a stream of its own.

A Grain that was away while it ran a task is normally sent `resume`
for it when it is back. If the task's team requeues lost tasks (hub.md
§6.3), and the Grain was away longer than the team's grace, the task
has been put back in the queue instead: no `resume` comes, the hub
refuses the Grain's events for it, and its result is not recorded. A
Grain may stop such a task when, some minutes after reconnecting, it
has been sent neither `resume` nor `cancel` for it.

### Dependencies on hub tasks (optional)

A hub task with `depends_on` is offered only once each task it names is
`done`. If one of them ends any other way (`failed`, `cancelled`,
`rejected`), the hub cancels it, with the reason `dependency <id> is
<state>`, so a Grain is never offered a task whose dependencies did
not all succeed. A dependency that is `lost` has not ended, and the
task waits.

A Grain that filed a task with `node_task` is told when that task ends,
whichever way, on a stream of its own, as the only message on it, as a
cancel for a task the Grain was away for is:

```
{"type":"dependency_done","task":{"id":"<hub id>"},"state":"done"|"failed"|"cancelled"|"rejected","reason":"...","node_task":"<local id>"}
```

- It is sent to the node that filed the task, only while that node is
  registered by the owner it was when it filed, and when it is next
  online if it is away then. The hub writes it once; it may write it
  again if it restarted before recording the first, so a Grain takes
  one (`task.id`, `node_task`) once.
- `done` is the Grain's cue to unblock its local task. Any other
  `state` means the hub task will never be done: the Grain must not
  run its local task as if it were, and what it does instead, such as
  failing or cancelling the local task as the hub cancels a hub task
  whose dependency failed, is its own local policy.
- A Grain ignores a `dependency_done` for a local task it no longer
  has, or one that no longer waits: cancelling the local task does not
  cancel the hub task, which is the hub's and its team's to cancel, by
  its submitter, a team admin or a hub admin. Likewise the hub task
  outlives `origin_task`: the origin ending, whichever way, does not
  end it.

A Grain that never sets `node_task` is never sent `dependency_done`.

### Approvals inside a run (optional)

A Grain that wants a human's go-ahead before it goes on, for instance
before a tool call its policy holds for approval, sends an
`approval_request` event on the task's event stream (below):

```
{"type":"approval_request","seq":N,"task":"<hub id>","approval_request":{"id":"<request id>","tool":"...","args":{...},"message":"...","deadline":"<RFC 3339>"}}
```

`id` must be unique within the task; `deadline`, if set, is when the
Grain stops waiting. A member of the task's team answers it at the hub,
and the hub writes the answer on the task stream:

```
{"type":"approve","task":{"id":"<hub id>"},"request_id":"<request id>","decision":"allow"|"deny","answered_by":"<email>","reason":"..."}
```

| Field | Meaning |
| --- | --- |
| `request_id` | The `id` of the `approval_request` it answers. |
| `decision` | `allow` or `deny`. |
| `answered_by` | The member of the task's team who answered, from their sign-on: the hub's claim, as `approved_by` is. |
| `reason` | Optional, the answerer's words. |

The hub sends at most one `approve` for each request: it refuses a
second answer, an answer after the request's `deadline`, and one for a
task that is not running, and it answers only an `id` the Grain sent.
An answer it could not write because the stream had ended is written
after the Grain's next `accept` of the task (its resume), so a Grain
may see an `approve` for a request it sent in an earlier session. A
Grain ignores an `approve` for a request it is no longer waiting on,
and applies its own local policy to the answer: an `allow` is the
hub's decision, which the Grain may still refuse to act on.

The `approve` message is not signed beyond what the stream already
gives: it arrives on the authenticated connection the Grain dialled.
A Grain that does not support answers simply never sends
`approval_request`; one that sends it must expect `approve` on the
task stream, and should treat an unknown message type there as
ignorable.

### Interactive sessions (optional)

An **interactive** task (`"kind":"interactive"`) is a session: it stays
open across many turns, taking messages from the humans at the hub,
and ends only when the hub cancels it or the Grain closes it with
`result`. The hub offers one only to a Grain that listed `interactive`
in its `hello`'s `features`, and writes `message` only for an
interactive task, so a Grain without the feature never sees either.

```
{"type":"message","task":{"id":"<hub id>"},"message":{"id":"m-...","text":"...","attachments":[{"name":"...","ref":"...","media_type":"...","size":N}],"sender":"<email>","sent_at":"<RFC 3339>"}}
```

| Field | Meaning |
| --- | --- |
| `id` | Names the message within the task. Delivery is **at least once**: the same message may arrive again (after a resume, say), and the Grain takes one `id` once. |
| `text` | What the human wrote, Markdown. |
| `attachments` | References only, never content: the hub keeps no uploaded file. `ref` is an `https` URL or one of the task's own artifact refs; the Grain fetches or resolves it under its own policy, or not at all. |
| `sender` | The task's submitter or a member of its team, from their sign-on: the hub's claim, which the Grain takes on trust as it does `approved_by`. |
| `sent_at` | When the hub took the message. |

A message posted while the task is not yet accepted, or while the
Grain is offline, is **queued** at the hub and written after the
Grain's next `accept` of the task (its offer or resume). The hub
refuses (409) a message to a task that is over, so none arrives after
the task's `result` or `cancel`.

On the task's event stream (below) a Grain running a session sends:

```
{"type":"awaiting_reply","seq":N,"task":"<hub id>","awaiting_reply":{"state":"waiting","question":"..."}}
{"type":"awaiting_reply","seq":N,"task":"<hub id>","awaiting_reply":{"state":"resumed","message_id":"m-..."}}
{"type":"message","seq":N,"task":"<hub id>","message":{"id":"m-..."}}
```

- `awaiting_reply` `waiting` when the agent stops for a human, with the
  question it asks if it asks one, and `resumed` when it goes on, with
  the `id` of the message that woke it if one did. It is a lifecycle
  event: stored and broadcast, and what the hub's UI shows as "waiting
  for you".
- `message` when the session takes a message in, naming its `id`. It
  is a conversation event: the hub stores and broadcasts its own record
  of the message in its place, whatever else the event carries, and
  drops one naming an `id` it never sent for the task. This is how a
  message appears in the task's conversation, so a Grain that supports
  sessions sends it once for each message it takes in.

## Stream events (the event stream)

For each task it runs, the Grain opens a stream to the hub and writes
the event envelope of package `events` (`events/events.go`), one event per line,
each with `"task":"<hub id>"` added (hub.md §7.1). The first line binds
the stream to a task the hub placed on that Grain. The hub writes
nothing back. Events at or below the task's last `seq` are dropped as
duplicates, so `seq` must keep increasing across the sessions the Grain
runs for the task. Node-only events (diffs, file contents, tool calls
and logs) are sent as references only:
`{"type":"artifact","kind":"...","ref":"<opaque>","size":...}`.

## Serve a view (the view stream)

When a viewer opens an artifact, the hub opens a stream (hub.md §7.2):

```
hub → node  {"type":"view","task":"<hub id>","node_task":"<local id>","ref":"<ref>"}
node → hub  {"type":"ok"}, then the artifact's bytes until the node closes the stream
            or {"type":"error","error":"..."}
```

The hub asks only for a ref the node listed in the task's artifact
events, but a node must still serve only those itself, never resolve a
ref as a path, and answer an error to any other ref: the hub's check is
defence in depth, not the node's boundary.

## Serve its own UI (the http stream, optional)

When the node's owner opens `https://<node>.<the hub's node UI
domain>`, the hub opens a stream (hub.md §11):

```
hub → node  {"type":"http","node":"<id>","viewer":"<email>"}
node → hub  {"type":"ok"}, then one HTTP/1.1 request is read and its response written,
            a 101 leaving the stream as the upgraded connection
            or {"type":"error","error":"..."}
```

A Grain answers it only if its owner has turned this on, and refuses it
otherwise. `nodeui.Serve` and `nodeui.Refuse` (`hub/nodeui`) are this
end.

A Grain serves only the streams above. It serves a view only of an
artifact it has itself listed, and an http stream only to its own UI.

## Conformance workloads

A task whose prompt's first line is

```
silo-conformance/v1 <workload>
```

is a **conformance workload**, filed by the conformance kit
([conformance.md](conformance.md)) or by anyone checking a Grain. A
Grain may short-circuit it: run no model and touch no repository, and
do exactly what the workload says, through the messages above. Its
title is `silo conformance: <workload>`, and the rest of the prompt is
prose for a human. Package `hub/conformance/workload` has these as Go
constants. A Grain that does not recognise the marker runs the task as
any other, which the kit's checks will not, in general, accept.

| Workload | What the Grain does, once it has accepted the task |
| --- | --- |
| `run` | Sends a `status` event, then an `artifact` event `{"kind":"diff","ref":"<its own ref>","size":N}` for the change below, then a `result` event, then `result` `done` on the task stream. It serves the ref on the view stream, with exactly the change's bytes. |
| `hold` | Sends a `status` event with the message `silo-conformance: started`, then a `status` event every second or so. It never ends on its own: it runs until cancelled, holding its slot. |
| `approve` | Sends `silo-conformance: started`, then an `approval_request` with `id` `silo-conformance`. On its `approve`, it ends with `result` `done` for `allow`, or `failed` for `deny`. This workload is only filed on a Grain that lists `approvals`. |
| `interactive` | Filed with `"kind":"interactive"`, only on a Grain that lists `interactive`. It sends `silo-conformance: started` and `awaiting_reply` `waiting`. On the first message it sends a `message` event naming its `id`, then `awaiting_reply` `resumed` with that `message_id`, then `result` `done`. |

Each workload says `silo-conformance: started` once per run of it. A
Grain that runs the same task twice therefore says it twice, which is
what the kit's restart scenario looks for.

The fixed change a `run` makes, byte for byte:

```
--- /dev/null
+++ b/SILO-CONFORMANCE
@@ -0,0 +1 @@
+This change was made by a Grain running the silo conformance kit.
```
