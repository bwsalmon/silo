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
hub → node  {"type":"offer","task":{...}}         run this task
            {"type":"resume","task":{...}}        a task you ran when the hub lost you: still yours?
            {"type":"cancel","task":{"id":...}}   stop it
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
| `constraints` | The labels or node ID the task was placed by. |
| `created_at` | When it was submitted. |

`approved_by` and `capabilities` are not carried yet.

The Grain takes these claims on trust and holds the task to its own
local policy, refusing what that policy does not allow (hub.md §1.2).
No answer to an offer within a minute counts as a refusal. After
`accept`, the stream stays open for the task's life: the Grain writes
`result` there, and the hub writes `cancel` there. A cancel for a task
with no open stream arrives as the only message on a stream of its own.

A Grain that was away while it ran a task is normally sent `resume`
for it when it is back. If the task's team requeues lost tasks (hub.md
§6.3), and the Grain was away longer than the team's grace, the task
has been put back in the queue instead: no `resume` comes, the hub
refuses the Grain's events for it, and its result is not recorded. A
Grain may stop such a task when, some minutes after reconnecting, it
has been sent neither `resume` nor `cancel` for it.

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
