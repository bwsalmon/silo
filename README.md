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

For spec verification of the `SV-N` ids in `docs/silo`, which needs
grain's history as well as this repository's (grain's classification
is `docs/test-runs/2026-10-12-silo-sv-classification.md` there):

- Commit 97e94df here moved Silo out of grain at grain `c2044e70f`.
  The `Design-Item: docs/silo/...` trailers for 115 of the 123 ids are
  in grain: `git log c2044e70f --grep='Design-Item: docs/silo/'`. Only
  SV-96, SV-97, SV-108, SV-110, SV-113, SV-114, SV-121 and SV-122 have
  trailers in this repository.
- Every implementing commit cites
  `Design-Rev: 33a3b9ef35042788edacccde067fa790fb9d07e8`, a grain
  commit from before the ids were added in grain `604dd85aa`. That
  commit changes only heading id markers, so read `604dd85aa` in
  `33a3b9e`'s place; otherwise `designdoc.Classify` reports all 123
  ids as "changed, done".
- Three commits edited `docs/silo` sections together with code but
  carry no `Design-Item` trailers: 22f863e (SV-37), 17a84d1 (SV-108,
  SV-118) and 7027ed7 (SV-4, 24, 26, 56, 65, 66, 70, 71, 72, 75, 76,
  101, 104, 105, 122).
