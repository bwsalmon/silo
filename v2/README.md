# Silo v2

This is Silo v2, a fresh and smaller design of Silo, under construction.
Stage 1 is in: `silo run` runs `claude`, `codex` or a custom CLI on the
local machine, interactive or headless, with `remote_permissions` none,
all or a subset, its only reach a gateway serving container and gVisor
sandboxes. `silo validate`, `silo schema` and `silo gc` are there too.

The code at the repository root (`cmd/`, `pkg/`, the root `go.mod`) and
its specification in [`docs/silo/`](../docs/silo/README.md) are Silo v1.
v1 keeps working, and keeps being developed, until v2 replaces it.

v2 runs one agent CLI in a chosen location, with access to a declared
set of MCP tools, credentials, cloud permissions, git access and local
scripts, and nothing else. A silo is a `Silofile`, or a folder holding
one; `silo run` carries it out once.

## Design

- [`docs/design.md`](docs/design.md): the design, organised around the
  user journeys (`silo create`, a Google project, a remote agent with
  narrow local reach, existing MCP servers), with the staged
  implementation plan.
- [`docs/cli-mapping.md`](docs/cli-mapping.md): how each setting maps
  onto the real `claude`, `codex` and `agy` flags.
- [`docs/grain.md`](docs/grain.md): grain as silo's first client, and
  its migration path.
- [`docs/examples/`](docs/examples/): example silo folders.

## Module

`v2/` is its own Go module, `github.com/bwsalmon/silo/v2`, the
major-subdirectory form of Go's semantic import versioning. The root
module's `./...` stops at this directory's `go.mod`, so neither module
builds, vets or tests the other's packages.

## Commands

```sh
go run ./cmd/silo run [--prompt text] <Silofile|dir>   # design §3
go run ./cmd/silo gc                        # what crashed sessions left
go run ./cmd/silo validate <Silofile|dir>   # design §2.4
go run ./cmd/silo schema [section]          # the Silofile's JSON Schema
```

`silo run` validates the Silofile, then runs one session: it resolves
credentials, finds the CLI and checks its version, starts the
sandboxes, starts the gateway, writes the CLI's MCP config and settings
(cli-mapping.md), and runs the CLI until it exits, the deadline passes
(exit 124) or Ctrl-C ends it (130). It exits with the CLI's own code
otherwise; a setting the CLI cannot express exits 76, a missing
prerequisite 69. The session directory is
`$XDG_RUNTIME_DIR/silo/<id>/`; the audit log and the final
`session.json` stay in `$XDG_STATE_HOME/silo/sessions/<id>/`.

`silo validate` decodes the Silofile strictly (design §2.1), resolves
every path inside the silo folder (§2.2) and checks names,
cross-references, files, `remote_permissions` against the CLI, and
credential destinations (§2.4). A value v2.0 reserves but does not ship
yet (`kind: granule`, `lifetime: persistent`, ...) is refused with an
error naming the stage that adds it. Errors name the file, line and YAML
path, and exit 65; bad usage exits 64 (§3.3). The script checks of
§12.3/§12.4 come with stage 4a (`silofile.Options.CheckScripts`).

The schema `silo schema` prints is generated from `silofile`'s Go types
into `silofile/silofile.schema.json` and embedded in the binary. After
changing a type, run `go generate ./silofile` and
`go test ./cmd/silo -update`; tests fail until both are current.

## Layout

| Package | What it is |
| --- | --- |
| `silofile` | The Silofile: types, strict decoding, folder resolution, validation, JSON Schema. |
| `cmd/silo` | The `silo` command. |
| `session` | One `silo run` session (design §3): setup steps and tool providers (registries), teardown, `session.json`, signals, exit codes, `silo gc`'s local part. |
| `location` | The `Location` and `Agent` interfaces (design §5.1). |
| `location/local` | The CLI as a child process with a scrubbed environment (design §5.2). |
| `agentcli` | Per-CLI mappings onto argv, env and staged files: `claude`, `codex` (with the reviewed-version gate for `none`), `custom`. Golden files in `agentcli/testdata`. |
| `mcp` | MCP over Streamable HTTP: JSON-RPC dispatch, the authenticated loopback server (bearer token, Host and Origin checks), and a client. |
| `gateway` | The session's MCP server (design §4): tool registry, refusal, timeouts, and the sandbox tools `sandbox_exec`, `sandbox_read_file`, `sandbox_write_file`, `sandbox_list_dir`. |
| `gateway/audit` | `audit.jsonl`: a start line before each call, an end line after, credentials redacted. |
| `sandbox` | The `Backend`/`Sandbox` interfaces (design §7.3), specs from a Silofile, and the images in `.silo/lock.json`. |
| `sandbox/container` | Docker or Podman containers; `kind: gvisor` is the same with `--runtime=runsc`. |
| `credentials` | Sources `env`, `file`, `ambient` (`gh`, `git-credential`, `gcloud`), and sandbox destinations. `store` comes later. |
| `gitproxy` | The git smart-HTTP proxy (design §9), copied and adapted from grain's `pkg/gitproxy` behind a `Policy` interface: `StaticPolicy` for a Silofile's `git:` entries, ref patterns, the fast-forward proof, one audit entry per request. Grain imports it. Wired into `silo run` by stage 7b. |
| `internal/safepath` | Path confinement, copied from grain's `pkg/safepath`. |
| `internal/procgroup` | Process-group cancellation, copied from grain's `pkg/procgroup`. |
| `internal/headtail` | Bounded command output, adapted from grain's `pkg/mcp/exec_output.go`. |

## Building and testing

```sh
cd v2
gofmt -l .
go build ./... && go vet ./... && go test -race ./...
```

grain CI runs the same in `.grain/workflows/v2.yml`.

The gateway's, `sandbox/container`'s and `session`'s tests create real containers,
with `busybox:1.37`, and run the same suite with `kind: gvisor`. They
skip where there is no Docker or Podman, or no runsc registered as a
runtime; CI installs runsc and sets `SILO_V2_REQUIRE_DOCKER=1` and
`SILO_V2_REQUIRE_RUNSC=1`, which turn those skips into failures.

`session`'s tests use the test binary itself as a fake `claude`: it
answers `--version`, reads the staged MCP config, calls the gateway and
exits as told. No real `claude` or `codex` is needed (stage 1d checks
those).

`gitproxy`'s tests clone and push with the real `git` binary, through
the proxy, to `git http-backend` over repositories on disk. They skip
without git; CI sets `SILO_V2_REQUIRE_GIT=1`.
