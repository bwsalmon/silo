# Conformance: proving a Grain speaks the task interface

A Grain may be customised by its owner, or be different code
altogether, as long as it speaks [the task interface](hub-interface.md).
The **conformance kit** checks that it does. It runs a real hub, hands
the Grain under test a node token, and drives scripted scenarios
through the hub's public API, the same API a human or a custom UI uses.
Then it reports what the Grain did, scenario by scenario.

The kit is package `hub/conformance`, and `silo conformance` on the
command line. The reference fake Grain, `hub/conformance/refgrain`
(`silo conformance grain`), passes it and runs in silo's CI. It is also
a worked example for anyone writing a Grain. Each of its parts is one
section of hub-interface.md, and it is about 600 lines of Go.

## Running it

```
silo conformance [flags] -- <command that starts your Grain>
```

The kit starts its hub on `127.0.0.1` (on SQLite, in a temporary
directory). It makes a team that owns the two repositories it files in
and mints a node token for `-node`. Then it starts the command with the
Grain's settings in its environment:

| Variable | Meaning |
| --- | --- |
| `SILO_HUB_URL` | The hub's base URL, `http://127.0.0.1:<port>`: the Grain dials `/v1/nodes/connect` on it. |
| `SILO_NODE_TOKEN` | The node token, the bearer credential. |
| `SILO_NODE_ID` | The node ID the token is bound to (`-node`, default `conformance-grain`). |
| `SILO_CONFORMANCE_REPO` | The repository the kit's tasks are in (`-repo`, default `conformance/allowed`). The Grain registers for it, and its local policy allows it. |
| `SILO_CONFORMANCE_OUTSIDE_REPO` | A repository the Grain registers for but its local policy refuses (`-outside-repo`, default `conformance/outside`). |

Without a command, the kit prints those settings, writes the token to
`-token-file` if one is given, and waits `-register-timeout` (two
minutes) for a Grain started by hand. When the run ends the command is
sent SIGTERM.

The reference Grain against the kit, as silo's CI runs it:

```
go build -o /tmp/silo ./cmd/silo
/tmp/silo conformance -json /tmp/conformance.json -- /tmp/silo conformance grain
```

The kit takes these flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-json` | none | Write the machine-readable verdict to this file (`-` for stdout). |
| `-only` | every scenario | Comma-separated scenarios to run. `register` always runs. |
| `-heartbeat` | `1s` | The heartbeat the hub asks for in `welcome`. |
| `-timeout` | `90s` | How long each wait inside a scenario may take. This is past the hub's minute for an answer to an offer. |
| `-max-slots` | `8` | The most slots the capacity scenario fills. A Grain with more has that scenario skipped. |
| `-addr` | `127.0.0.1:0` | Where the kit's hub listens. |
| `-v` | off | Log the hub and the kit's progress to stderr. |

`silo conformance` exits 0 when the Grain conforms and 1 when it does
not.

From Go, `conformance.Run(ctx, conformance.Config{Start: ...})` runs the
same kit. `Start` is handed the `Env` above and starts the Grain:
`hub/conformance/conformance_test.go` runs the reference Grain in
process this way.

## The workloads

The Grain needs something to run that does not depend on a model.
Every task the kit files is a **conformance workload**: a prompt whose
first line is `silo-conformance/v1 <workload>`. A Grain that recognises
it short-circuits the task and does exactly what the workload says. The
workloads (`run`, `hold`, `approve`, `interactive`) are part of the
interface, and are specified in
[hub-interface.md](hub-interface.md#conformance-workloads). Package
`hub/conformance/workload` has them as Go constants. A Grain is free to
handle the marker any way it likes, for instance with a stub agent, as
long as what reaches the hub is what the workload says.

## The scenarios

They run in this order. Each passes, fails, or is skipped:

| Scenario | What the kit checks |
| --- | --- |
| `register` | The Grain registers within `-register-timeout`: it is online in `GET /v1/nodes`, owned by the token's owner, with at least one slot, and registered for `SILO_CONFORMANCE_REPO`. Every other scenario needs this one. |
| `heartbeat` | Its heartbeats arrive (`last_seen` advances), and while it is idle `free` equals `slots`. |
| `refuse` | An offer of a `run` task in `SILO_CONFORMANCE_OUTSIDE_REPO` is refused: the hub excludes the node, and the Grain never accepts it. This is skipped when the Grain does not register for that repository (so the hub never offers it there) or `-outside-repo -` is given. |
| `run` | A `run` task is accepted, with a `node_task`, and runs to the result `done`, with a `result` event streamed. |
| `events` | That task's events arrive with strictly increasing `seq`, and include a `status`, an `artifact` and the `result`, ending with the `result`. |
| `view` | The run's diff, SILO_ONLY content ([hub.md](hub.md) §7.2), is streamed only by reference (an `artifact` event with `kind` `diff` and a `ref`). Viewing that ref through `GET /v1/tasks/{id}/artifacts?ref=` reaches the Grain on the view stream, which serves exactly the workload's diff, of the size the event gave. |
| `unlisted` | The Grain's own side of INV-11 ([threat-model.md](threat-model.md)), asked past the hub's check of listed refs (SA1-M3): the kit opens `view` streams to the Grain itself. The run's listed ref is served (`ok`, then bytes). A ref never listed (`diff-999`), traversal refs (`../../.git-credentials`, the listed ref followed by `/../../.git-credentials`, `/etc/passwd`), the listed ref asked under a `hold` task that listed nothing, and the listed ref under that task's `node_task` are each refused: an `error` with no bytes after it, or the stream closed unanswered. A Grain that serves no views fails here as in `view`, since its refusals prove nothing. |
| `capacity` | `slots` `hold` tasks all run at once. The Grain then reports `free` 0, and a further task is not run while the slots are full. |
| `cancel` | Cancelling one held task is delivered, and its slot is freed: the waiting task runs. Cancelling the rest returns `free` to `slots`. |
| `approve` | Optional, `approvals`. An `approve` task sends its `approval_request`. Answered `allow` it ends `done`; answered `deny`, `failed`. |
| `interactive` | Optional, `interactive`. An interactive `interactive` session sends `awaiting_reply` `waiting`. A message posted to it comes back as a `message` event with its ID, then `awaiting_reply` `resumed` naming it, and the session closes `done`. |
| `restart` | With a `hold` task running, the hub shuts down (saying `goodbye`) and starts again on the same address and database. The Grain reconnects and keeps the task as the same `node_task`, its events continuing past the last `seq` the hub had. It says `silo-conformance: started` only once (no second run), and reports one slot busy. |
| `filing` | Upward filing and dependencies (task 4233). The hub does not implement these yet, so this is always skipped, and says whether the Grain lists `filing`. |

The **optional features** are the ones a Grain lists in its `hello`'s
`features`. The hub reads `interactive` (it offers sessions only to a
Grain that lists it). It keeps, but does not read, `approvals`
(the Grain sends `approval_request` and takes `approve`) and `filing`.
The kit reads all three. A scenario for a feature the Grain does not
list is **skipped and reported as such, not failed**. A Grain that lists
a feature and then gets it wrong fails.

## The report

For a human, one line per scenario, then the verdict:

```
Grain conformance-grain (version "refgrain"): 2 slots, repos [conformance/allowed conformance/outside], features [approvals interactive]

pass  register     102ms   node conformance-grain, version "refgrain", 2 slots, ...
pass  heartbeat    1.019s  heartbeats every 1s, 2 of 2 slots free while idle
pass  refuse       70ms    refused task t-213328df3a2be3d8 in conformance/outside
...
skip  filing       0s      this hub does not implement upward filing and dependencies yet (task 4233); ...

CONFORMS: 11 passed, 0 failed, 1 skipped
```

`-json` writes the machine-readable verdict (`conformance.Report`):

```json
{
  "node": {"id": "conformance-grain", "repos": ["..."], "slots": 2, "version": "refgrain", "features": ["approvals", "interactive"]},
  "started_at": "2026-10-17T09:21:11Z",
  "passed": true,
  "scenarios": [
    {"name": "register", "status": "pass", "detail": "...", "duration_ns": 102000000},
    {"name": "filing", "status": "skip", "detail": "...", "duration_ns": 0}
  ]
}
```

`status` is `pass`, `fail` or `skip`. `passed` is false when any
scenario failed. `detail` says what was seen, or, for a failure, what
was wrong.

## What it does not check

The kit sees what the hub's public API shows, which brings some limits:

- It cannot see what the Grain sent but the hub dropped. The hub keeps
  only a reference for a node-only (SILO_ONLY) event whatever the line carried
  (hub-interface.md, the event stream). So a Grain that sends a diff's
  content inline is not caught, though its view of the ref must still
  serve the diff.
- It runs no signing key, so it does not check `requester_attestation`
  or `attest`. The `grain-interop` CI job checks those against stock
  grain.
- It does not check the http stream, which is optional and off by
  default.
