# silo's config file

silo reads its config from `--config FILE`, else `$SILO_CONFIG`, else
`~/.silo/config.yaml`. Unknown keys are errors, so typos fail loudly.
`~` at the start of a path means your home directory.

```yaml
cli:
  name: claude              # required: claude, codex or gemini
  binary: ~/bin/claude      # default: the name, looked up on PATH
  args: [--model, opus]     # appended after silo's own flags
  env:                      # added to the CLI's environment
    ANTHROPIC_LOG: error
  list_changed: auto        # auto | yes | no (see below)

repos:
  root: ~/context           # required
  git_base: "git@github.com:"   # optional; quote values ending in ':'

identity:                   # required: author and committer of approved commits
  name: Ada Lovelace
  email: ada@example.com

push:
  branch: ""                # default: silo/<session-id>
  remote: origin            # default origin
  disabled: false           # true: commit locally, never push

toolsets:
  disabled: [slow-tool]     # repo tool sets not to load (escape hatch)

sessions_dir: ~/.silo/sessions   # default
```

## Repositories

`silo run <repo>` takes a name relative to `repos.root`, such as
`team/payments-context`. Names may contain only letters, digits, `.`,
`_`, `-` and `/`, and may not contain `..`.

- **With `git_base`**, silo clones `<git_base><repo>` into
  `<root>/<repo>` the first time, then reuses that checkout. A `/` is put
  between them unless `git_base` already ends in `/` or `:`.
- **Without `git_base`**, `<root>/<repo>` must already be a git
  repository.

Each session:

1. Refuses to start if the checkout has uncommitted changes.
2. Fetches the remote, if the repository has one.
3. Picks the base branch: `--branch`, or else the remote's default
   branch, or else the current branch.
4. Checks out the work branch with `git checkout -B`. The work branch is
   `push.branch`, else `.silo/config.yaml`'s `push_branch`, else
   `silo/<session-id>`. It starts from the remote's copy of that branch
   if there is one, otherwise from the base branch.

Approved commits go onto the work branch and are pushed to
`<remote>/<work branch>`.

Git never prompts for credentials during a session
(`GIT_TERMINAL_PROMPT=0`), because silo owns the terminal. Use an SSH key
or a credential helper.

## Sessions

Each session gets a directory `<sessions_dir>/<session-id>/` with:

- `audit.jsonl`: one JSON object per event (`session_start`,
  `tool_call`, `approval_requested`, `approved`, `rejected`, `commit`,
  `pushed`, `push_failed`, `toolset_build`, `toolset_loaded`,
  `toolset_unloaded`, `tools_list_changed`, `refresh`, `session_end`).
  It is kept after the session ends.
- `router.sock`: the router's Unix socket. A path too long for a socket
  falls back to `router.sock` in a private (0700) directory under `$TMPDIR`.
- `tools/`: one-off (scratch) tool sets. Deleted at the end.
- `build/`: tool-set build output (`${BUILD_DIR}`). Deleted at the end.
- `logs/<toolset>.stderr`: each tool-set server's stderr.
- The adapter's generated files (`claude-mcp.json`, `gemini/`).

## Adapters

The CLI is given one MCP server, `silo`, whose command is
`silo mcp-bridge <socket>`. The bridge connects the CLI's stdio to the
session's router. The CLI starts in the checkout.

| | Claude Code | Codex | Gemini CLI |
|---|---|---|---|
| MCP server | `--mcp-config <session>/claude-mcp.json --strict-mcp-config` | `-c mcp_servers.silo.command=…` `-c mcp_servers.silo.args=[…]` | settings file at `GEMINI_CLI_SYSTEM_SETTINGS_PATH`, with `mcpServers.silo` (`trust: true`), `mcp.allowed: [silo]`, plus `--allowed-mcp-server-names silo` |
| System prompt | `--append-system-prompt` (added to Claude Code's own) | `-c developer_instructions=…` (added to Codex's own) | `GEMINI_SYSTEM_MD=<session>/gemini/system.md` (**replaces** Gemini CLI's own) |
| Native tools off | `--disallowedTools Bash,BashOutput,KillShell,Edit,MultiEdit,Write,NotebookEdit,Read,Glob,Grep,LS,WebFetch,WebSearch,Task` | `-c features.shell_tool=false`, `-c tools.web_search=false`; **fallback:** `--sandbox read-only --ask-for-approval untrusted` | `tools.exclude: [run_shell_command, write_file, replace, read_file, read_many_files, list_directory, glob, search_file_content, web_fetch, google_web_search, save_memory]` |
| Other | `--allowedTools mcp__silo` (silo does its own approvals); `--setting-sources user`, so the repository's `.claude/settings*.json` (permissions, hooks) is ignored | `--cd <checkout>` | |
| `tools/list_changed` | refreshed | not refreshed: fallback tools on | treated as not refreshed: fallback tools on |

**Where the native tools can't be fully disabled.**

- **Codex**: Older Codex versions have no switch that removes the shell
  tool. On those, `features.shell_tool=false` is ignored and the shell
  stays available to the model. Silo still runs Codex read-only and asks
  you before any command that isn't on Codex's trusted list, so the shell
  can't write to the checkout without you, and Codex shows each command
  for your approval.
- **Gemini CLI**: The repository's `.gemini/settings.json` is a
  lower-precedence settings layer. It can't undo the system settings
  file's exclusions, but it can add MCP servers, which
  `--allowed-mcp-server-names` and `mcp.allowed` then filter out.
- **All three**: The restrictions are only as good as the CLI's handling
  of its own flags. See "Not a sandbox" in the README.

**`tools/list_changed` and the fallback.** The router sends
`notifications/tools/list_changed` whenever its tool list changes: a
hot-loaded or removed tool set, or an approved one-off tool. Claude Code
re-lists its tools when that arrives. Codex reads its tools only at
startup. Silo assumes Gemini CLI does the same, which is safe either way.
For those two CLIs, the router adds:

- `silo.list_tools`, which lists every current tool with its schema;
- `silo.call {tool, arguments}`, which calls any of them by name.

The system prompt tells the agent to use these. Set `cli.list_changed`
to `yes` or `no` to override the adapter's default, for example when a
newer Codex refreshes its tools.

Tool names contain dots (`repo-read.read_file`). Some model APIs allow
only `[A-Za-z0-9_-]` in tool names. The CLIs rewrite names to fit, so the
model may see `repo-read_read_file` or `mcp__silo__repo-read_read_file`,
but the call still reaches the right tool.
