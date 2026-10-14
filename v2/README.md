# Silo v2

This is Silo v2, a fresh and smaller design of Silo, under construction.
There is nothing here to run yet.

The code at the repository root (`cmd/`, `pkg/`, the root `go.mod`) and
its specification in [`docs/silo/`](../docs/silo/README.md) are Silo v1.
v1 keeps working, and keeps being developed, until v2 replaces it.

v2's design documents will go under [`docs/`](docs/) in this directory.

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
