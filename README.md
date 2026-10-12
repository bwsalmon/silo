# Silo

Silo is a system of nested virtual clouds. A silo is a node in a tree.
Inside one, a tenant gets a GCE-compatible `compute/v1` API (the
facade), fenced so that everything it makes stays inside its silo. The
control plane, `silod`, keeps the tree, the allocators, the grants and
the audit log in a database (SQLite or PostgreSQL).

The specification is [`docs/silo/`](docs/silo/README.md); its README is
the overview and wins on any conflict. Requirement headings there carry
stable ids, `{#SV-n}`, which code and commits cite.

## Layout

| Path | What it is |
| --- | --- |
| `cmd/silo` | The `silo` CLI: install, proxy, agents, links, exports |
| `cmd/silod` | The control plane: `silod serve`, `gateway`, `migrate`, `pause`, `harness-init`, and its container images |
| `cmd/silo-tools` | The MCP tool server agents' tool VMs run |
| `pkg/...` | Everything they are built from |
| `pkg/facade/<cloud>/<service>` | The cloud APIs tenants call inside a silo, such as `facade/gcp/gce`, the `compute/v1` facade ([`docs/silo/facades.md`](docs/silo/facades.md)) |
| `pkg/backend/<cloud>/<service>` | silod's own clients of the real cloud, with their fakes, such as `backend/gcp/gce` |
| `docs/silo/` | The specification |
| `docs/test-runs/` | Reports of end-to-end test runs |

## Building and testing

```sh
make            # gofmt check, build into bin/, vet, go test -race
make silod-image silod-harness-image
```

The store's PostgreSQL tests start a throwaway server per test from
Debian's `postgresql` binaries, and skip when those are not installed
unless `SILO_REQUIRE_POSTGRES` is set. CI is grain's
(`.grain/workflows/tests.yml`).

## History

Silo was developed inside
[bwsalmon/grain](https://github.com/bwsalmon/grain) under `pkg/silo`,
`cmd/silo*` and `docs/silo`, and moved here with that layout. The
packages have since been flattened: commits and test reports from
before then (including grain's, and `docs/test-runs/`) cite
`pkg/silo/<x>`, which now lives at `pkg/<x>`. Its earlier history is
grain's.
