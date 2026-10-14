# Silo v2

This is Silo v2, a fresh and smaller design of Silo, under construction.
Nothing runs an agent yet: so far there is the Silofile and two commands,
`silo validate` and `silo schema`.

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
go run ./cmd/silo validate <Silofile|dir>   # design §2.4
go run ./cmd/silo schema [section]          # the Silofile's JSON Schema
```

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
| `internal/safepath` | Path confinement, copied from grain's `pkg/safepath`. |

## Building and testing

```sh
cd v2
gofmt -l .
go build ./... && go vet ./... && go test -race ./...
```

grain CI runs the same in `.grain/workflows/v2.yml`.
