# The task interface: what a Grain speaks to the hub

Part of the [hub design](hub.md) (§1.3). This is the **contract**
between the Silo hub and a Grain. The hub depends on nothing in a Grain
but these messages. A Grain may be customised by its owner, or be
different code altogether, as long as it speaks them as written here.

Everything below is built: the connection and control stream in
`hub/rendezvous` (`rendezvous.go`), the task stream in `hub/tasks`
(`stream.go`), and the event and view streams in `hub/live`. The
sections of [hub.md](hub.md) cited give the full semantics.

## Connection

The Grain dials out to `wss://<hub>/v1/nodes/connect`, subprotocol
`silo-node.v1`, presenting its node token as a bearer token (hub.md
§4.1, §4.2, §5.2). The WebSocket carries the mux; each message below is
one JSON line on a mux stream. The hub never dials a Grain.

## Register, heartbeat and capacity (the control stream)

The Grain opens the first stream (hub.md §4.3):

```
node → hub  {"type":"hello","node":{"id":"...","repos":["owner/name",...],"slots":N,"version":"...","labels":{...}}}
hub → node  {"type":"welcome","owner":"<from the credential>","heartbeat_seconds":15}
            or {"type":"error","error":"..."}, then close
node → hub  {"type":"heartbeat","free":N}      every heartbeat_seconds
hub → node  {"type":"goodbye","error":"<why>"}  the hub is shutting down, then close
```

`hello` must come within 30s of the handshake. `free` is the Grain's
own count of free slots, counting every task it has accepted. After
`goodbye`, or any drop, the Grain reconnects with backoff: the hub's
replacement is usually a few seconds away (a rolled pod). A Grain that
does not know `goodbye` loses nothing by ignoring it, since the
connection then ends anyway.

## Accept or refuse a task, report its result (the task stream)

The hub opens one stream per task (hub.md §6.2):

```
hub → node  {"type":"offer","task":{...},"requester_attestation":"<JWT>"}    run this task
            {"type":"resume","task":{...},"requester_attestation":"<JWT>"}   a task you ran when the hub lost you: still yours?
            {"type":"attest","task":{"id":...},"requester_attestation":"<JWT>"}  a fresh attestation for a task you accepted
            {"type":"cancel","task":{"id":...}}                              stop it
            {"type":"approve","task":{"id":...},"request_id":"...",          a human's answer to an approval_request
             "decision":"allow"|"deny","answered_by":"<email>","reason":"..."}
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
| `title`, `prompt` | What to do. |
| `submitter` | The requester, from their sign-on to the hub. |
| `approved_by` | The human who approved the task at the hub: a member of the task's team, never its submitter, who approved it while it was proposed. Absent for a task nobody approved there, which is every task a member of the team filed directly. |
| `approved_at` | When `approved_by` approved it (RFC 3339); absent with it. |
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
`result` there, and the hub writes `cancel` and `approve` there. A cancel for a task
with no open stream arrives as the only message on a stream of its own.

A Grain that was away while it ran a task is normally sent `resume`
for it when it is back. If the task's team requeues lost tasks (hub.md
§6.3), and the Grain was away longer than the team's grace, the task
has been put back in the queue instead: no `resume` comes, the hub
refuses the Grain's events for it, and its result is not recorded. A
Grain may stop such a task when, some minutes after reconnecting, it
has been sent neither `resume` nor `cancel` for it.

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
