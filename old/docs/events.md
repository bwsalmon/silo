# The session event stream

A silo session reports what it does as one stream of typed events
([`grain.md`](grain.md) §2.1). The same schema reaches a caller two ways:

- `silo run --json <Silofile|dir>` writes each event as one JSON line on
  stdout. `--json` needs `agent.mode: headless`. The CLI's own output
  goes into the session's raw transcript, not to stdout. silo's progress
  lines and the CLI's stderr still go to stderr.
- The Go library, `silo.Start(ctx, silofile.Silofile, silo.Options)`
  (`v2/silo.go`), delivers the same values on `Session.Events()` and to
  `Options.Sink`.

- Every session also writes the stream to `events.jsonl` in its state
  directory, which `silo attach` and `silo.Attach` replay and follow
  from a cursor ([detach and re-attach](#detach-and-re-attach)).

A running session is acted on through its [control socket](#the-control-socket)
or, in the library, through the `Session` methods that share its
implementation.

The Go types are in package `events` (`v2/events`).

## Envelope

```json
{"v":1,"seq":7,"time":"2026-10-14T12:00:00Z","session":"s-abcdefghijkl","type":"transcript","transcript":{...}}
```

| Field | Meaning |
| --- | --- |
| `v` | Schema version, currently `1`. It changes only when a field changes meaning or is removed. New fields and new event types do not change it, so a reader must ignore fields and types it does not know. |
| `seq` | Numbers the session's events from 1, in emission order. |
| `time` | When the event was emitted. For `tool_call`, it is the audit line's time. |
| `session` | The silo session id. It is empty only on a `result` written before a session existed, such as for an invalid Silofile. |
| `type` | One of the types below. The payload is the field with the same name. |

Every string an event carries goes through the session's redactor before it
is delivered. Each value of a resolved credential (four bytes or longer) is
replaced by `[credential:<name>]`, as in the audit log (design §8.4). This
covers transcript text, tool arguments and results, and the final text.

## Types

**`status`**: a setup or teardown step, or a line of progress.
`{phase: setup|running|teardown, step, state, message}`.
`state` is `started`, `done` or `failed` for a step: the setup steps of
design §3 (`credentials`, `location`, `tools`, `gateway`, `stage`),
`agent` once the CLI starts, and each teardown undo. `state` is `info` or
`warning` for a progress line, which then carries `message`.

**`transcript`**: one item of the CLI's transcript, normalised across
CLIs. `{kind, text, id, tool, args, is_error}`, where `kind` is one of:

| kind | claude (stream-json) | codex (`exec --json`) |
| --- | --- | --- |
| `text` | an assistant `text` block | `agent_message` item |
| `thinking` | a `thinking` block | `reasoning` item |
| `tool_call` | a `tool_use` block | `mcp_tool_call` start; a `command_execution` start as tool `shell` with `args.command` |
| `tool_result` | a `tool_result` block | the same items completed |
| `error` | an `error` event | an `error` event or item, or `turn.failed` |

`id` pairs a `tool_call` with its `tool_result`. `tool` is the bare tool
name, with claude's `mcp__silo__` prefix removed. These are the CLI's own
tool calls: built-in tools and the gateway's both appear here.

**`tool_call`**: one gateway call, at its start and at its end, exactly as
its line in `audit.jsonl`. `{phase: start|end, call, tool, as, args,
outcome, reason, size, head, duration_ms}`. `call` pairs a start with its
end. A call the gateway refuses is reported here too. `tool` is silo's
name for the tool. `as` is the name the agent called it by, and is set
only when the session's rename map gives the tool another name (see
[the library](#the-library)).

**`usage`**: tokens as the CLI reports them, tagged `source: agent`.
`{source, model, input_tokens, output_tokens, cache_read_tokens,
cache_write_tokens}`. Claude reports usage per assistant message, which is
emitted once even though it is repeated on each of the message's blocks.
Codex reports usage per turn (`turn.completed`). Codex does not name the
model, so `model` is `agent.model` and is empty when that is unset.

**`approval_request`**: a gateway call to a tool with `approve: true`,
waiting for an answer (design §12.3). `{id, tool, args, message,
deadline}`. For a local script, `args` is `{script, args, stdin_bytes}`,
and `message` is the request in a sentence, with the `silo approve`
command that answers it. Answer it with `silo approve <session> <id>
[--deny]`, or with the control socket's `approve` method. An
interactive session shown on silo's own terminal lines does not emit
this event: the CLI owns that terminal, so a desktop notification
announces the request instead.

**`resumable`**: the CLI's own session id, emitted when the CLI first names
it or names a new one. `{cli, cli_session}`. For claude it is the
`session_id` from stream-json; for codex it is `thread.started`'s
`thread_id`.

**`result`**: always the last event.

```json
{"exit": {"code": 0, "reason": "completed", "detail": "the agent CLI exited",
          "cli_error": {"kind": "usage_limit", "message": "...", "reset_at": "...", "retry_after_seconds": 0}},
 "final_text": "...", "cli_session": "...",
 "usage_totals": [{"source": "agent", "model": "...", "input_tokens": 0, "output_tokens": 0}],
 "audit_path": ".../audit.jsonl",
 "transcript_path": ".../transcript.raw.jsonl",
 "normalized_transcript_path": ".../transcript.jsonl",
 "errors": ["..."]}
```

`exit.code` is `silo run`'s exit code (design §3.3). `exit.reason` is one
of the following:

| reason | when |
| --- | --- |
| `completed` | The CLI exited 0 and its output reported success. |
| `cli_error` | The CLI's output reported a failure, or the CLI exited non-zero. `cli_error` says which. A usage limit that claude reports as an ordinary answer is a `cli_error` with exit code 0. |
| `cancelled` | `Session.Cancel`, the caller's context, SIGINT or SIGTERM ended the session, during setup or after it. `Cancel` and the context exit 143, as SIGTERM does. |
| `deadline` | `agent.limits.deadline`, or a time given to `ExtendDeadline`, passed (exit 124). |
| `connection_lost` | The agent host's connection dropped (exit 75). No location reports this yet. |
| `setup_failed` | The session did not get as far as running the CLI: an invalid Silofile, a refusal or a missing prerequisite. |

`cli_error.kind` is grain's run-failure classification, which moved into
silo's per-CLI parsers (`agentcli/claude`, `agentcli/codex`):

| kind | meaning |
| --- | --- |
| `usage_limit` | The account's usage or rate limit. `reset_at` (claude) or `retry_after_seconds` (codex) is set when the CLI said when. |
| `overloaded` | The provider had no capacity (529, `overloaded_error`, 502/503/504, codex's retry-limit 429). |
| `refused` | The provider refused the request: an API error, authentication, a quota. |
| `max_turns` | The CLI's own turn budget ran out (claude's `error_max_turns`). |
| `error` | Any other failure the CLI reported, or a non-zero exit with no failure in the output. |

`usage_totals` sums the `usage` events, with one entry per model.

## Transcripts

A headless session keeps both transcripts beside its audit log, in
`$XDG_STATE_HOME/silo/sessions/<id>/` ([`grain.md`](grain.md) open
question 3 asks for both):

- `transcript.raw.jsonl` holds the CLI's stdout as the CLI wrote it, with
  credentials redacted line by line.
- `transcript.jsonl` holds the normalised transcript: the session's
  `transcript` events, one per line, exactly as emitted. It is written only
  for a CLI whose output silo parses: claude, codex, and `cli: custom` with
  `events: claude-stream-json`.

## The library

```go
s, err := silo.Start(ctx, f, silo.Options{
	Credentials: map[string]string{"github": tok},
	CallerMCP: []silo.CallerMCP{{Name: "grain", URL: "unix:/run/grain/t42.sock", Token: perSession,
		Tools: []string{"ask_question", "wait_for_checks"}, Blocking: []string{"ask_question", "wait_for_checks"}}},
	Rename: map[string]string{"sandbox_exec": "run_command"}, // optional; silo picks no names
})
for e := range s.Events() { ... }          // ends with result, then closes
res, err := s.Wait()
s.Cancel("superseded")                     // ends the session as cancelled
s.ExtendDeadline(time.Now().Add(time.Hour))
d, err := s.Inject(msg)                    // silo.Delivered or silo.Queued
err = s.Approve(callID, true)              // answers an approval_request
```

- A library session runs headless. A Silofile without `agent.mode` is run
  headless, and any other mode is refused.
- `Options.Credentials` passes credentials as values, keyed by the name a
  Silofile credential has. That credential's `from:` is then not read.
- `Options.Environ` decides the session and state directories and the
  environment the CLI inherits.
- A session never touches process-wide state. It installs no signal
  handler and runs no `silo gc` unless `Options.GC` is set, and it has its
  own session directory, gateway port, token and audit log. Many sessions
  can therefore run in one process.
- `Events()` never blocks the session: events queue until they are read.
  Once it has been called, the channel must be drained.
- `Inject`, `Cancel`, `ExtendDeadline` and `Approve` do what the control
  socket's methods of the same names do. `Inject` and `Approve` wait for
  the session to exist, which is almost at once. A message injected
  during setup goes with the prompt (claude) or is queued (codex).
- `Options.CallerMCP` and `Options.Rename` are described under
  [caller-provided MCP servers](#caller-provided-mcp-servers).

## The control socket

Every session listens on `control.sock` in its session directory,
`$XDG_RUNTIME_DIR/silo/<id>/control.sock` ([`grain.md`](grain.md) §2.1).
The socket is mode 0600 in a 0700 directory, and that is its only
authentication: whoever can open it is the invoking user. It speaks
JSON-RPC 2.0, one request or response per line. Requests on one
connection may be answered out of order, so a long `approve` does not
hold up a short `inject`. A request without an `id` is a notification
and gets no answer.

| Method | Params | Result |
| --- | --- | --- |
| `inject` | `{"message": "..."}` | `{"delivery": "delivered" \| "queued"}` |
| `cancel` | `{"reason": "..."}` (optional) | `{}` |
| `extend_deadline` | `{"deadline": "<RFC 3339 time>"}` | `{"deadline": "..."}` |
| `approve` | `{"id": "<call>", "deny": false}` (both optional) | the call answered: `{id, tool, script, args, ..., approved}` |

- **`inject`** gives the CLI a user message mid-run.
  - **claude**: silo keeps the CLI's stream-json stdin open until its
    turn's `result`, and writes the message there. The result is
    `delivered`. Silo then closes stdin, so the CLI exits once it has
    answered everything it read.
  - **codex**: `codex exec` takes no input once it has started. The
    message is `queued`. When the turn ends cleanly (exit 0, and no
    failure in its output), silo runs `codex exec resume <thread> -` with
    the queued messages on stdin, joined by blank lines, as the next
    turn.
  - **claude, after its turn has ended**: a message is `queued`, and
    silo resumes the CLI with `claude -p --resume <session>` in the same
    way. Messages still queued when the session ends any other way (a
    failed turn, cancel, the deadline) are dropped, with a warning.
  - **Refused**: an interactive session, `cli: custom`, and a session
    that is ending all refuse `inject`.
  - Each delivery is reported as a `status` line.
- **`cancel`** ends the session as `cancelled`. `exit.detail` carries the
  reason, and the exit code is 143. It works during setup too.
- **`extend_deadline`** moves the session's deadline to the given time. A
  session without a deadline gets one. A move made during setup takes
  effect when the CLI starts.
- **`approve`** answers a gateway call waiting for approval (an
  `approval_request` event, design §12.3). It is present only in a
  session with local scripts.

Errors use JSON-RPC's codes. Malformed JSON gets -32700, and a request
that is not JSON-RPC 2.0 gets -32600. An unknown method gets -32601,
with the methods the socket has. Bad params get -32602: the wrong type,
an unknown field, a missing `message` or `deadline`, or a request the
method refuses. `silo approve` is a client of this socket.

## Detach and re-attach

A session can outlive the process that started it
([`grain.md`](grain.md) §2.7):

```sh
id=$(silo run --detach --label grain.run=r-42 --prompt "..." ./silo)
silo ls --label grain.run=r-42 --json      # live sessions, one session.json a line
silo attach "$id" --from 17 --json         # events after seq 17, then follows to the result
```

```go
live, _ := silo.List(map[string]string{"grain.run": "r-42"}, silo.AttachOptions{})
a, _ := silo.Attach(ctx, live[0].ID, cursor, silo.AttachOptions{})
for e := range a.Events() { cursor = e.Seq; ... }   // ends with result
res, err := a.Wait()                               // err is silo.ErrNoResult if the supervisor was killed
a.Inject(msg); a.Cancel("superseded")              // over the control socket
```

- **`--detach`** starts a supervisor (`silo __supervise`, in its own
  session, with no terminal and no pipes to the caller) that owns the
  CLI, the gateway and teardown, prints the session id once the session
  exists, and exits. It needs `agent.mode: headless`, and is refused
  with `--json`. The supervisor logs to `supervisor.log` in the state
  directory.
- **`events.jsonl`** is in the state directory,
  `$XDG_STATE_HOME/silo/sessions/<id>/events.jsonl`, so it outlives the
  session as the audit log does. Every session writes it, detached or
  not. It holds every event, numbered by `seq` from 1 with no gaps, one
  JSON line each, appended as it is emitted. Events emitted before the
  session had an id (the start-up `silo gc`) are written with it once it
  has one.
- **The cursor** is the `seq` of the last event the caller has handled.
  Attaching from it delivers the events after it, so a caller that
  stores it as it goes sees each event once across its restarts.
  `--from 0` replays everything. An ended session is replayed to its
  result. Attaching ends at the result. When the session's process is
  gone and there is none, `silo attach` exits 1 and `silo.Attach`'s
  `Wait` returns `silo.ErrNoResult`; `silo gc` removes what it left.
  Otherwise `silo attach` exits with the session's exit code.
  Interrupting `silo attach`, or `Attached.Detach`, leaves the session
  running.
- **Labels** (`--label k=v`, `Options.Labels`) are kept in
  `session.json`. `silo ls` and `silo.List` show the sessions whose
  process is running and that carry every label asked for.
- **Caller MCP servers** are dialled again when they come back on the
  same address ([below](#caller-provided-mcp-servers)).

## Caller-provided MCP servers

A caller can pass the session MCP servers of its own, such as grain's
task tools ([`grain.md`](grain.md) §2.2). The gateway connects to each
one as a client and serves its tools as `<name>__<tool>`. Each call goes
through the server's allowlist and is audited like any `mcp[]` call.
Silo neither starts nor stops these servers.

- **Where.** `URL` is an `http://` or `https://` Streamable HTTP
  endpoint, or a Unix socket, written `unix:/path` or as a bare absolute
  path. On a socket, the endpoint is `/mcp`. Only Streamable HTTP is
  spoken on a socket; HTTP+SSE is not.
- **Token.** `Token` is sent as `Authorization: Bearer <token>`. Silo
  redacts it like a credential, as `[credential:caller-mcp:<name>]`. A
  server that refuses the token fails setup: every caller server is
  required.
- **Allowlist.** `Tools` is the allowlist of the server's own tool names.
  When it is nil, every tool the server lists is allowed.
- **Blocking tools.** `Blocking` names tools the gateway runs with no
  timeout, such as `wait_for_checks` and `ask_question`. In a session
  with a blocking tool, silo raises the CLI's own MCP call timeout to 7
  days: `MCP_TOOL_TIMEOUT` for claude, `tool_timeout_sec` for codex. The
  session's deadline, `cancel` or the server itself ends such a call, not
  the CLI.
- **Reconnecting** (stage 9). A caller server that goes away, as while
  its caller restarts, is dialled again on the next call. A call whose
  request never reached it (a refused dial, or HTTP 404 for an MCP
  session a restarted server does not know) is made once more on the
  fresh connection. A call that still fails returns a tool error saying
  that the server cannot be reached and the call is retryable, and is
  audited as an error. Once the caller serves the same address again,
  calls succeed. The token is not re-read: the caller comes back with
  the session's token.
- **Names.** A caller server's name follows `mcp[]`'s rule, may not
  contain `__`, and may not be the name of a Silofile `mcp[]` entry.
- **On the command line.** `silo run --caller-mcp <name>=<url>` gives
  the same thing; repeat the flag for each server. The token is read
  from `$SILO_CALLER_MCP_TOKEN_<NAME>`, with the name upper-cased and
  `-` written as `_`, so that it never appears in silo's argv.
  `--caller-mcp-tools <name>=a,b` sets the allowlist, and
  `--caller-mcp-blocking <name>=a,b` sets the blocking tools.

`Options.Rename` is the per-session rename map (design open question
19). Each key is silo's name for a gateway tool (`sandbox_exec`,
`local_<script>`, `<server>__<tool>`). Its value is the name the agent
sees instead. The gateway lists and answers the tool under the new name
only. The audit log and `tool_call` events record both names, as `tool`
and `as`. Two tools renamed to one name, or a rename onto another
tool's name, fail setup with exit 64. A key the session does not serve
is warned about. Silo ships no map of its own: which names to keep is
the caller's choice.
