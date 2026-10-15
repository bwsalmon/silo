# Silo v2: CLI mapping

How a Silofile's `agent`, `remote_permissions` and mode settings become
real command lines for `claude`, `codex` and `agy`, on each location.
Part of the [v2 design](design.md) (§6 there is the summary).

## 1. What was checked, and how

| CLI | Version | How it was checked |
| --- | --- | --- |
| `claude` | Claude Code 2.1.197 | Installed from npm (`@anthropic-ai/claude-code`) in task 3800's sandbox. `claude --help` and `claude auth --help` read. Run against a model through `silo run` in stage 1d (task 3822): `cmd/silo`'s live tests. |
| `codex` | codex-cli 0.162.1 | Installed from npm (`@openai/codex`) in the same sandbox. `codex --help`, `codex exec --help`, `codex mcp --help` and `codex login --help` read. Run against a model through `silo run` in stage 1d (task 3822), and against a stub model with silo's `none` configuration (`agentcli/codex`'s live probe), beside 0.153.0 for comparison. |
| `agy` | 1.2.2 | **Not installed here.** Read from grain's `docs/agy-surface.md`, which grain's `.grain/workflows/agy-surface.yml` captures from the pinned binary, and from grain's `pkg/agent/antigravity`. |

Grain's own drivers (`pkg/agent/claude`, `pkg/agent/codex`,
`pkg/agent/antigravity`) are the second source. They run these CLIs in
production every day, so a flag grain passes is known to work at grain's
pinned version. Each row below says which source backs it:

- **V**: the flag appears in the help of the version above.
- **G**: grain passes it today (works in production at grain's pinned
  version).
- **L**: run against the real CLI in stage 1d, by the live tests
  (`SILO_V2_LIVE_CLI=1`, see the README), and behaved as described.
- **A**: an assumption, not checked. Listed again in §4.

## 2. Remote permissions

### 2.1 `claude`

| Setting | Arguments | Source |
| --- | --- | --- |
| every setting | `--mcp-config <file> --strict-mcp-config` (the gateway only), `--allowedTools mcp__silo` (pre-approve every gateway tool), `--disable-slash-commands` | V, G, L (every gateway tool is called with no prompt, under `default` and `dontAsk`) |
| `none` | `--tools ""` | V, G ("Use `""` to disable all tools"), L (the init event lists only `mcp__silo__*`; the model calls nothing else and cannot read a host file) |
| subset `[Read, Grep, Edit]` | `--tools Read,Grep,Edit` | V ("or specify tool names") |
| `all` | no `--tools` (the default roster) | V |
| headless + `all` or subset | `--permission-mode dontAsk` plus `--allowedTools` for the listed tools, or `--dangerously-skip-permissions` when the Silofile says `all` | V (modes listed: `acceptEdits`, `auto`, `bypassPermissions`, `default`, `dontAsk`, `plan`); L: in print mode `dontAsk` refuses a tool not in `--allowedTools` (it is in the result's `permission_denials`) and the run goes on to its result |
| interactive + `all` or subset | `--permission-mode default`: the user approves in the terminal as usual | V |

Hardening for `none` and subsets: the CLI starts in an empty private
working directory, so no project `CLAUDE.md`, `.claude/settings.json` or
`.mcp.json` is read; `--setting-sources user` so project and local
settings (and their hooks) are not loaded (V, flag; L: `SessionStart`
and `UserPromptSubmit` hooks in the workdir's `.claude/settings.json`
and `.claude/settings.local.json` do not run, and do run for the same
directory without the flag); `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`, which grain sets
(G). Hooks in the user's own `~/.claude/settings.json` still run, as the
user. With `auth` set to a credential, `--bare` turns hooks, plugins and
`CLAUDE.md` discovery off as well (V), but it reads only API-key auth
(V: "OAuth and keychain are never read"), so it cannot be used with
`auth: ambient`. See design open question 4.

Subset names are checked against the built-in roster. Claude Code does
not print its roster in `--help`, and `--tools` drops a name it does not
know without a word, so silo keeps a table per reviewed version and
refuses a name not in it. The 2.1.197 table (L) is every name for which
the binary, given it in `--tools`, lists a tool in its print-mode init
event:

| | Tools |
| --- | --- |
| roster | `Bash`, `CronCreate`, `CronDelete`, `CronList`, `DesignSync`, `Edit`, `EnterWorktree`, `ExitWorktree`, `Glob`, `Grep`, `Monitor`, `NotebookEdit`, `PushNotification`, `Read`, `ReportFindings`, `ScheduleWakeup`, `SendMessage`, `Task`, `TaskCreate`, `TaskGet`, `TaskList`, `TaskOutput`, `TaskStop`, `TaskUpdate`, `WebFetch`, `WebSearch`, `Workflow`, `Write` |
| aliases, also accepted | `Agent` (loads `Task`), `BashOutput` (`TaskOutput`), `KillShell` (`TaskStop`) |
| refused | `AskUserQuestion`, `EnterPlanMode`, `ExitPlanMode`, `LSP`, `MultiEdit`, `TodoWrite`, `SlashCommand`, `LS`: in the binary or earlier tables, but no tool loads for them in print mode. `Skill`: loads, except under `--disable-slash-commands`, which silo always passes |

The default roster (no `--tools`) leaves out `Glob` and `Grep`, which
load only when named. `Monitor` and `PushNotification` were missing
from one default-roster run and present in the next, so the default
roster may vary with server-side settings; a named subset was the same
on every run. Stage 1c's table was a guess, and named seven tools that
do not load.

MCP config file (G):

```json
{"mcpServers": {"silo": {"type": "http", "url": "http://127.0.0.1:41234/mcp",
  "headers": {"Authorization": "Bearer <token>"}}}}
```

`MCP_TOOL_TIMEOUT` is set so long gateway calls are not abandoned (G):
32 minutes, sandbox_exec's 30-minute cap plus slack.

As built in stage 1c: `--setting-sources user` and
`CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` are set for `none` and subsets, not
for `all`. Headless `none` adds no permission flag (nothing is left to
approve but `mcp__silo`). `agent.system_prompt` is staged as a file and
passed with `--append-system-prompt-file`. The headless prompt is one
stream-json user message on stdin, never argv. Since stage 6b, stdin
stays open until the turn's `result` so that a message can be injected
(§3.2); before that it was closed after the prompt. The interactive prompt follows `--`. A Claude
version with no roster table is checked against the newest table, with a
warning. `--bare` is not used (design open question 4).

### 2.2 `codex`

Codex is given a **private `CODEX_HOME`** in the session directory for
every setting, so the user's own `config.toml` MCP servers and profiles
do not apply. For `auth: ambient`, that home links the user's
`auth.json` (grain's `WithAuthFile`, G).

| Setting | Configuration | Source |
| --- | --- | --- |
| every setting | `config.toml`: `[mcp_servers.silo]` with `url`, `http_headers = { Authorization = "Bearer ..." }`, `required = true` | G |
| `none` | `environments.toml` with `default = "none"`, `include_local = false`; `CODEX_EXEC_SERVER_URL=none`; `approval_policy = "never"`, `sandbox_mode = "read-only"`, `web_search = "disabled"`, `include_environment_context = false`; `[features]` `apps`, `plugins`, `multi_agent` off; skills off | G, reviewed by grain for **0.153.0** (grain `docs/codex-isolation.md`); L, 0.162.1 by stage 1d's probe (below) |
| subset | **refused**: Codex has no per-tool allowlist for its built-ins | V (no such flag in 0.162.1's help); G (grain's doc says the same) |
| `all` | no environment manifest; `-s`/`--sandbox` (`read-only`, `workspace-write`, `danger-full-access`) left at Codex's built-in default; interactive keeps Codex's approval prompts, headless adds `-c approval_policy="never"` so a run never blocks on an approval nobody can give (`codex exec` has no `-a`) | V |

As built in stage 1c, following grain's `configTOML`: `config.toml` also
sets `tool_timeout_sec`, `enabled_tools` naming the gateway's listed
tools, and `approval_mode = "approve"` for each of them;
`cli_auth_credentials_store = "file"` with `auth: ambient`, whose
`auth.json` is `$CODEX_HOME/auth.json` or `~/.codex/auth.json` of the
invoking user (missing: exit 69, "run `codex login`");
`model_reasoning_effort` from `agent.effort`; and
`developer_instructions` from `agent.system_prompt` (A).

With `auth: {credential: x}`, `OPENAI_API_KEY` is set, and since stage 1d
the key is also written to the private home's `auth.json` as
`{"auth_mode": "apikey", "OPENAI_API_KEY": ...}`, the file
`codex login --with-api-key` writes, with
`cli_auth_credentials_store = "file"`. L: `codex exec` 0.162.1 sends no
credential when the key is only in `OPENAI_API_KEY` (every request
failed 401); it does read `CODEX_API_KEY`, but only in `exec`, so the
file serves both modes. An `OPENAI_BASE_URL` in the agent's environment
(a credential delivered `{agent: env, name: OPENAI_BASE_URL}`, say)
becomes `openai_base_url` in `config.toml`: Codex's built-in provider
ignores the variable (L), and the private home hides the user's own
`openai_base_url`.

**Version pinning for `none`.** Grain found that read-only mode alone
did not stop controller reads, and that only the no-environment manifest
removes the shell and file tools. It is a reviewed configuration, not a
documented contract, so grain refuses any other Codex version, and silo
does the same: `none` on an unreviewed version exits 76 naming the
reviewed versions: 0.153.0 (grain's review) and 0.162.1 (stage 1d,
below).

Stage 1d ran `agentcli/codex`'s `TestLiveCodexNoneIsolation` on 0.162.1
and on 0.153.0. It runs the real binary with silo's `none`
configuration against a stub Responses API, which calls every native
tool whether advertised or not. Both versions advertised the same tools
(`create_goal`, `get_goal`, `update_goal`, `list_mcp_resources`,
`list_mcp_resource_templates`, `read_mcp_resource`,
`request_user_input`, `tool_search`, with the gateway's tools behind
`tool_search`), refused `exec_command`, `shell`, `shell_command`,
`apply_patch`, `view_image`, `write_stdin` and `request_permissions` as
"unsupported call", and wrote nothing on the host. On that evidence the
owner added 0.162.1 to the reviewed versions (task 3822). It is a
behavioural check with one model profile, not a source-level review
like grain's of 0.153.0; the probe is what to rerun for the next
version.

### 2.3 `agy`

| Setting | Configuration | Source |
| --- | --- | --- |
| every setting | a private `HOME` holding `.gemini/config/mcp_config.json` with one server, `silo` | G (grain writes the same path); HTTP entries: agy 1.2.2's changelog says `agy mcp add` covers "stdio and HTTP servers through `--type`, `--env` and `--header`" (A: the JSON field names) |
| `none` | **refused**: agy has no flag or setting that withholds its 57 native tools; `disabledTools` applies to MCP servers only | G (grain `pkg/agent/antigravity`, "agy exposes no equivalent switch") |
| subset | **refused** | G |
| `all` | default; headless adds `--dangerously-skip-permissions` | V (agy-surface), G |

`--sandbox` ("Run in a sandbox with terminal restrictions enabled") is
not a substitute for `none`: it restricts the terminal tool, not file
tools. `AGY_CLI_DISABLE_AUTO_UPDATE=true` is always set, so the binary
silo checked is the binary that runs (G).

### 2.4 `custom`

A custom CLI declares its own mapping in the Silofile, and silo refuses
any setting it does not declare:

```yaml
agent:
  cli: custom
  binary: /usr/local/bin/my-agent
  custom:
    argv:
      interactive: ["--tui"]
      headless: ["--prompt-file", "{prompt}", "--jsonl"]
    mcp_config:
      format: claude-json          # claude-json | codex-toml | stdio-bridge
      arg: ["--mcp", "{file}"]     # or env: MY_AGENT_MCP_CONFIG
    permissions:
      none: ["--no-builtin-tools"]
      all: []
    events: claude-stream-json     # how silo parses headless output, or: none
```

As built in stage 1c: `{prompt}` is a staged file holding the prompt,
`{file}` the staged MCP config; `stdio-bridge` is refused (exit 76)
until `silo mcp-bridge` exists; a custom CLI's version is not checked.

## 3. Modes

### 3.1 `interactive`

| CLI | Command | Source |
| --- | --- | --- |
| `claude` | `claude [--model m] [<mapping>] [--append-system-prompt-file f] [prompt]` | V |
| `codex` | `codex [-m m] [-C dir] [PROMPT]` | V |
| `agy` | `agy [--model m] [-i <prompt>]` | V (agy-surface) |

Local: the CLI shares silo's terminal. `ssh`: `ssh -t`, so the remote
side gets a pty and the CLI its TUI.

### 3.2 `headless`

| CLI | Command | Events | Injection mid-run | Source |
| --- | --- | --- | --- | --- |
| `claude` | `claude -p --output-format stream-json --verbose --input-format stream-json` | stream-json lines | yes: a user message on stdin (stream-json input) | V, G |
| `codex` | `codex exec --json --skip-git-repo-check [-m m] -` (the prompt on stdin, as grain passes it, so it is not in argv) | JSONL events | between turns only: `codex exec resume <id> <msg>`; `codex queue` (V, exists; A, semantics) | V, G |
| `agy` | `agy -p --output-format stream-json --input-format stream-json --disable-slash-commands --print-timeout <d>` | stream-json lines | yes: stdin, one NDJSON message per turn | V (agy-surface), G |

`--print-timeout` for agy is set from the session deadline, because agy's
default of 5 minutes ends long runs (G). `--max-turns` is not passed to
claude: grain found it turns a long run into an error rather than a
result (G).

On `ssh`, headless runs use `ssh -T` with the same argv.

As built in stage 1c, `silo run` without `--json` passes the CLI's
stdout through to its own; parsing it into events is stage 6a.

Injection is as built in stage 6b. Claude's stdin stays open until the
turn's `result`, and each injected message is written as one stream-json
user line. A message that arrives later resumes the session as
`claude -p ... --resume <id>`, with the message on stdin. Codex's
messages always wait for the turn to end, then go through
`codex exec resume --json --skip-git-repo-check [-c ...] [-m m] <thread> -`,
with the messages on stdin. That argv carries the same flags as the
first turn (A: that `exec resume` takes `-c`). `codex queue` is not
used.

`codex queue` (L, 0.162.1, for stage 6b): `codex queue --thread <id>
--message <text>`, with the same `CODEX_HOME`, accepts a message for a
thread a `codex exec` is running ("Queued message ... for thread ...",
exit 0), but that `exec` never delivers it: the turn ended without it,
and a later `codex exec resume` of the thread did not deliver it either.
It is not a mid-run injection path for `codex exec`; it presumably
feeds the TUI or the app server. Stage 6b's `exec resume` stays.

### 3.3 `attached` (reserved)

| CLI | Flag or command | Source |
| --- | --- | --- |
| `claude` | `--remote-control [name]` ("Start an interactive session with Remote Control enabled") | V; semantics A |
| `codex` | `codex remote-control` ("[experimental] Manage the app-server daemon with remote control enabled"), `codex app-server`, `codex --remote ws://...` | V; semantics A |
| `agy` | `agy remote-control` ("Manage the remote-control background daemon") | V (agy-surface); semantics A |

Not shipped in v2.0. Recorded so the `Location` and mode interfaces have
a place for them.

### 3.4 Resuming a session

Needed for detach and re-attach ([`grain.md`](grain.md) §2.7):
`claude --resume <id>` (also `--session-id <uuid>` to choose the id up
front, V), `codex exec resume <id>` (V, G), `agy --conversation <id>`
(V, G).

## 4. Assumptions to check in stage 1

Stage 1d (task 3822) ran the live tests against claude 2.1.197 and
codex-cli 0.162.1.

1. Claude Code's exact built-in tool roster at the pinned version, for
   validating subsets. **Corrected**: the table in §2.1, checked by
   `TestLiveClaudeRoster`; seven names in stage 1c's table loaded
   nothing, and are now refused.
2. That `--setting-sources user` excludes project and local hooks.
   **V** (`TestLiveClaudeNone`, with a control).
3. That `--permission-mode dontAsk` refuses, rather than prompts for,
   tools not in `--allowedTools`, in print mode. **V**
   (`TestLiveClaudeDontAsk`).
4. That `claude --allowedTools mcp__silo` pre-approves every tool of the
   `silo` server (grain lists each tool by name instead). **V**
   (`TestLiveClaudeNone`, `TestLiveClaudeSubset`); silo keeps the server
   name.
5. Whether Codex 0.162.1's tool registry still matches grain's 0.153.0
   review. **Same surface** in the stage 1d probe (§2.2); 0.162.1 is
   now a reviewed version, and `none` runs on it. The live run also found `auth: {credential: x}`
   broken on 0.162.1, now fixed (§2.2).
6. agy's JSON field names for an HTTP MCP server with a header. Still
   unchecked: agy is not installed where stage 1d ran.
7. The behaviour of every `attached` command. Still unchecked:
   `attached` is reserved (design open question 5).
8. `codex queue` as a mid-run injection path. **Not one** for
   `codex exec` (§3.2); stage 6b's `codex exec resume` stays.
