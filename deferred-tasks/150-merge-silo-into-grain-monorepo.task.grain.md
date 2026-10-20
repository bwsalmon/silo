---
grainfile: v1
title: "[HOLD for grain refactor] repo: merge bwsalmon/silo into bwsalmon/grain as a single Go module preserving git history"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2026-10-25T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 150
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-19T01:32:22Z"
x_deferred_until: "2026-10-25T00:00:00Z"
x_capabilities:
  - grain-ci
---
Merge `bwsalmon/silo` (Grain Hub, Grainfile `v1` specification/linter/schemas, TypeScript SDK, and Hub UI) into `bwsalmon/grain` as a single repository and single Go module (`module github.com/bwsalmon/grain`) while preserving full `git log` / `git blame` history from both repositories:

### 1. Target Unified Directory Layout in `bwsalmon/grain`
- **Single Go module (`go.mod` at root: `module github.com/bwsalmon/grain`):**
  - Remove the external `github.com/bwsalmon/silo` dependency from `go.mod`; `pkg/hubnode`, `pkg/daemon`, and `hub/...` live in the same Go module and compile/test together atomically.
- **Directory mapping from `bwsalmon/silo` into `bwsalmon/grain`:**
  - `hub/` -> `hub/` (all Hub packages: `hub/server`, `hub/directory`, `hub/tasks`, `hub/live`, `hub/nodeui`, `hub/grainfiles`, `hub/chats`, `hub/gchat`, `hub/rendezvous`, `hub/registry`, `hub/identity`, `hub/claims`, `hub/lease`, `hub/conformance`, `hub/cujtest`, `hub/faulttest`, `hub/security`, etc.)
  - `events/` -> `events/`
  - `internal/fakegrain/` (and other `silo/internal/*` packages) -> `internal/fakegrain/` (or `hub/internal/...`)
  - `cmd/silo/` -> `cmd/silo/` (and also wire `grain hub serve` / `grain hub migrate` / `grain hub token` subcommands into `cmd/grain/`)
  - `ui/` (from `silo`) -> `ui-hub/` (or `hub/ui/web/`, updating `hub/ui` build/embed scripts and `Dockerfile.hub` while keeping `bwsalmon/grain`'s `ui/` at root)
  - `sdk/ts/` -> `sdk/ts/`
  - `schemas/grainfiles/` -> `schemas/grainfiles/`
  - `tools/grainfiles/` -> `tools/grainfiles/`
  - `examples/grainfiles/` -> `examples/grainfiles/`
  - `deploy/` (from `silo`: `k8s/`, `terraform/`) -> `deploy/hub/`
  - `Dockerfile` (from `silo`) -> `Dockerfile.hub`
  - `docs/grainfile-v1.md`, `docs/grainfiles-spec.md`, `docs/grainfiles.md`, `docs/cujs.md` -> `docs/`, and remaining Hub docs -> `docs/hub/`
  - **Drop `old/`** from `silo` before merging so legacy pre-Hub code is not carried into the open-source repository.

---

### 2. Step-by-Step Merge Procedure (Preserving Git History)
1. Fetch `bwsalmon/silo` `main` (`http://192.168.9.1:9728/bwsalmon/silo.git` / `https://github.com/bwsalmon/silo.git`) into a temporary staging branch.
2. In the staging branch of `silo`:
   - `git rm -rf old/`
   - `git mv` the overlapping top-level paths (`ui/` -> `ui-hub/`, `deploy/` -> `deploy/hub/`, `Dockerfile` -> `Dockerfile.hub`, and Hub-specific docs into `docs/hub/`).
   - Rewrite Go imports and module references from `github.com/bwsalmon/silo` to `github.com/bwsalmon/grain` across all `.go` files.
3. Merge the prepared `silo` branch into `bwsalmon/grain` with `git merge --allow-unrelated-histories` (preserving full commit history and `git blame` across both trees).
4. Rewrite `github.com/bwsalmon/silo/...` imports in `pkg/hubnode/`, `pkg/daemon/`, and `pkg/policy/` to `github.com/bwsalmon/grain/...`, remove `github.com/bwsalmon/silo` from `go.mod`, and run `go mod tidy`.
5. Unify `.grain/workflows/tests.yml` (and `Makefile`) so the CI pipeline runs both the Grain daemon/node test suites and the Hub (`hub`, `grainfiles`, `sdk`, `ui-hub`, `ui-hub e2e`, `manifests`, `terraform`, `kind`) jobs in one repository.

### Verification
- Run `go test -race ./hub/... ./pkg/hubnode/... ./pkg/daemon/...`, `python3 tools/grainfiles/grainlint.py --self-test`, `python3 tools/grainfiles/grainlint.py examples/grainfiles`, and `npm test` in `sdk/ts`, `ui/`, and `ui-hub/`.
