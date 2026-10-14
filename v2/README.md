# Silo v2

This is Silo v2, a fresh and smaller design of Silo, under construction.
There is nothing here to run yet.

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

## Layout

`v2/` is its own Go module, `github.com/bwsalmon/silo/v2`, the
major-subdirectory form of Go's semantic import versioning. The root
module's `./...` stops at this directory's `go.mod`, so neither module
builds, vets or tests the other's packages.

## Building and testing

```sh
cd v2
gofmt -l .
go build ./... && go vet ./... && go test -race ./...
```

grain CI runs the same in `.grain/workflows/v2.yml`.
