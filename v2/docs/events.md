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
its line in `audit.jsonl`. `{phase: start|end, call, tool, args,
outcome, reason, size, head, duration_ms}`. `call` pairs a start with its
end. A call the gateway refuses is reported here too.

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
s, err := silo.Start(ctx, f, silo.Options{Credentials: map[string]string{"github": tok}})
for e := range s.Events() { ... }          // ends with result, then closes
res, err := s.Wait()
s.Cancel("superseded")                     // ends the session as cancelled
s.ExtendDeadline(time.Now().Add(time.Hour))
s.Inject(msg)                              // stage 6b; ErrNotYet until then
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
