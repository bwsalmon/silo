# silo

silo runs an agent CLI (Claude Code, Codex or Gemini CLI) in your
terminal against a **context repository**: a git repository that holds
the session's prompt, its context documents and its tools. The agent gets
no shell and no file-editing tools of its own. It works only through the
MCP tools silo grants. Every change it wants to make to the repository is
shown to you as a diff, and nothing is written until you approve it.

## Quick start

```sh
go install github.com/bwsalmon/silo/cmd/silo@latest

mkdir -p ~/.silo
cat > ~/.silo/config.yaml <<'EOF'
cli:
  name: claude            # or codex, gemini
repos:
  root: ~/context         # where checkouts live
  git_base: "git@github.com:"   # optional: clone <git_base><repo> if missing
identity:
  name: Your Name
  email: you@example.com
EOF

silo run team/payments-context            # the repo's default branch
silo run --branch staging team/payments-context
```

`silo run <repo>` then:

1. clones `<git_base><repo>` into `<root>/<repo>` (or uses the repository
   already there), fetches it, and checks out a session branch
   `silo/<session-id>` from the base branch;
2. starts silo's MCP router with the built-in tool sets and every tool
   set the repository defines in `.silo/tools/`;
3. starts the CLI in the checkout, with `.silo/prompt.md` and
   `.silo/context/` loaded into its system prompt, its own shell and
   edit tools turned off, and silo's router as its only MCP server.

You chat with the CLI as usual. When the agent proposes a commit or a
one-off tool, silo pauses the CLI, shows you the full diff or code, and
asks `Approve? [y/n]`. A rejection's reason goes back to the agent.

To try it on the example repository, see
[examples/context-repo](examples/context-repo/README.md).

## What the agent gets

The CLI connects to exactly one MCP server: silo's router. Tools are
named `<toolset>.<tool>`:

| Tool set | Tools | Notes |
|---|---|---|
| `repo-read` | `list_files`, `read_file`, `grep`, `git_log`, `git_diff`, `git_show` | Read-only, confined to the checkout (no `..`, symlinks out or into `.git`, dangling symlinks, or `.git`). |
| `repo-write` | `propose_change` | File edits plus a commit message. You approve the full diff; only then does silo write, commit as your configured identity, and push to `silo/<session-id>` (or the configured branch). |
| `scratch` | `define`, `remove`, `list`, `promote` | One-off tool sets for this session, written under `~/.silo/sessions/<id>/tools/`. Creating or changing one shows you its full code for approval. Deleted when the session ends. `promote` commits one to `.silo/tools/` through a normal `repo-write` approval. |
| `silo` | `refresh`, `vendor` (+ `list_tools`, `call` for CLIs that ignore `tools/list_changed`) | `refresh` fetches and fast-forwards the checkout, then reloads the tool sets that changed. `vendor` proposes copying tool sets from another repository (see below). |
| `<name>` | whatever `.silo/tools/<name>/` serves | Every tool set committed to the repository is enabled. |
| `<alias>.<name>` | whatever `.silo/tools/vendor/<alias>/<name>/` serves | Tool sets vendored from other repositories, enabled the same way. |

**Tool sets.** A tool set is a directory with exactly one entry point,
`start.sh` or `start.py`. silo picks a free loopback port, runs
`bash start.sh <port>` or `python3 start.py <port>` in the directory, and
connects to the MCP server it starts there over Streamable HTTP
(`http://127.0.0.1:<port>/mcp`). The script can install, build or `exec`
whatever it likes, because silo builds nothing. The server must bind to
loopback only, and on Linux silo refuses one that doesn't. An optional
`toolset.yaml` holds metadata: a description, environment and an
allowlist of tools. See
[docs/repo-layout.md](docs/repo-layout.md#silotoolstoolset).

> **Breaking change:** `toolset.yaml`'s `build` and `run` keys, and
> servers speaking MCP over stdio, are gone. A tool set still written
> that way fails to load with an error that says what to do instead.

**Hot loading.** When an approved commit, a `silo.refresh` or an approved
one-off tool changes a tool set, the router restarts that tool set's
server on a new port and sends `notifications/tools/list_changed`. You
don't need to restart the session. A server that fails to start goes
back to the agent as a tool error with its output, and the previous
version keeps running. Stopping a set sends its whole process group
`SIGTERM`, and then `SIGKILL` if anything is still running.

**Tool sets from other repositories** are vendored, never fetched by a
session. `silo vendor team/shared-tools@v3 --toolsets tickets --as shared`
copies them into `.silo/tools/vendor/shared/` with a `VENDOR.lock`
recording the upstream commit, for you to review and commit.
`silo vendor --update shared` refreshes them. In a session,
`silo.vendor` proposes the same change, and you approve the full diff of
the vendored code. See
[docs/repo-layout.md](docs/repo-layout.md#vendored-tool-sets).

## Not a sandbox

silo is a convenience wrapper, not a security boundary. It limits which
tools the agent CLI offers its model, and it gates commits and one-off
tools on your approval. It does not isolate anything:

- The CLI runs as you, with your credentials and network access. silo
  turns off the CLI's own shell and edit tools with the CLI's own flags
  (see [docs/config.md](docs/config.md#adapters)). A CLI bug, or a CLI
  version that ignores those flags, gets around them. Codex's shell can
  only be restricted, not always removed.
- Repository tool sets run as you, on your machine. silo starts every set
  committed to the repository without asking, so a context repository is
  as trusted as code you run. That includes vendored tool sets, which is
  why vendoring copies their code into the repository for you to review,
  rather than fetching it at session start. Their environment is cut
  down to what their manifests declare, and their servers must listen on
  loopback only, but they are not otherwise confined.
- Approved one-off tools run as you, too. Read their code before you
  approve.

What silo does guarantee is that approvals come from you. The CLI runs on
a pseudo-terminal that silo owns. While a prompt is up, silo stops
passing your keystrokes and the CLI's output through, and reads your
answer from your terminal itself. Nothing the agent or the CLI does can
type the answer. Keys you type in the first half-second after a prompt
appears are thrown away, so a reply you were typing to the CLI cannot
answer it by accident.

Every tool call, approval, rejection, commit, push and tool-set reload is
logged to `~/.silo/sessions/<id>/audit.jsonl`.

## Documentation

- [docs/config.md](docs/config.md): silo's config file, and what each
  CLI adapter does.
- [docs/repo-layout.md](docs/repo-layout.md): the context repository
  layout, the tool-set entry point and metadata, how to write a tool
  set, and vendoring.

## Development

```sh
go build ./...
go vet ./...
go test -race ./...
```

`internal/session`'s end-to-end test drives a whole session with a fake
CLI that speaks MCP through the real bridge and router.

| Package | Role |
|---|---|
| `cmd/silo` | The command: `silo run`, `silo vendor`, `silo mcp-bridge`. |
| `internal/config` | silo's config, `.silo/config.yaml`, `toolset.yaml`, entry points and tool-set paths. |
| `internal/session` | Wires a session together, composes the prompt, and serves the router on a Unix socket that `silo mcp-bridge` connects the CLI to. |
| `internal/adapter` | The claude, codex and gemini adapters. |
| `internal/router` | The MCP router, plus the host that starts tool sets' entry points on loopback ports, checks their binding, hot-reloads them and stops their process groups. |
| `internal/builtin` | `repo-read`, `repo-write`, `scratch`, `silo`. |
| `internal/vendor` | Copying tool sets from another repository, and `VENDOR.lock`. |
| `internal/approval` | The approval prompt and the terminal console it runs on. |
| `internal/mcp` | JSON-RPC/MCP over stdio (the CLI's side, adapted from silo v4) and the Streamable HTTP client the router reaches tool sets with. |
| `internal/audit` | The JSONL audit log (adapted from silo v4). |
| `internal/gitrepo` | Checkout preparation and git helpers. |

The earlier silo code (Grain Hub, v3, v4) was removed in favour of this
tool. It is still in git history.
