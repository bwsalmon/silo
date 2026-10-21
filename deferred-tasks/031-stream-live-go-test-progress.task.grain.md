---
grainfile: v1
title: "Stream live progress output during Go test runs in Grain CI workflows"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2099-01-01T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/corp-research
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 31
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-20T23:10:09Z"
x_deferred_until: "2099-01-01T00:00:00Z"
x_capabilities:
  - corp-research
  - grain-ci
---
## Problem

When the `go` CI job (and other Go test steps in `.grain/workflows/tests.yml`) runs, the `Test` step often produces **no live output at all** for 15–35 minutes until `go test` finishes at the very end.

### Root Cause
1. In `.grain/workflows/tests.yml` (lines 662–678), the `go` job's `Test` step redirects all output of `go test -race -timeout 45m ./...` to a file:
   ```sh
   set +e
   go test -race -timeout 45m ./... > /tmp/gotest.log 2>&1
   status=$?
   ```
   Nothing is written to stdout while `go test` is running; only after `go test` exits does the step run `grep` and `tail` on `/tmp/gotest.log`.
   This redirection was originally added because noisy passing tests (such as `cmd/grain` tests dumping CLI/daemon output to `os.Stdout`) produced >256 KiB of output, causing `pkg/ci/live.go` (`ci.maxStepOutput`) to elide the middle of the step log and lose `--- FAIL:` lines.
2. In single-package or few-package steps (`postgres`, `run-host`, `package-cache`), `go test` without `-v` or `-json` buffers all output for a package until the entire package finishes, leaving the live CI log blank for minutes at a time.

## Changes Required

1. **Stream concise live progress during `go test` while preserving full failure diagnostics at the head of the final output (`.grain/workflows/tests.yml` + helper script in `scripts/`)**:
   - Add a small helper script (e.g., `scripts/run-go-tests.sh` or `go test -json` stream processor) used by `.grain/workflows/tests.yml` (`go`, `postgres`, `run-host`, `package-cache`):
     - While `go test` runs, stream concise live progress lines to stdout as packages/tests progress (e.g., each package completion line `ok` / `FAIL` / `?`, immediate `--- FAIL:` / `panic:` / `WARNING: DATA RACE` notifications, and periodic heartbeat lines every ~15–30s naming which packages or slow tests are still in flight) while capturing the full raw output in `/tmp/gotest.log`.
     - Because ~68 package completion lines and periodic heartbeats total only a few KiB, the live stream stays well below the 256 KiB `ci.maxStepOutput` limit (`pkg/ci/live.go`) and never triggers middle-elision of failure output.
     - When `go test` finishes, print the structured failure summary (`=== failing tests ===`, `=== panics ===`, `=== data races ===`, `=== per-package results ===`, and tail of `/tmp/gotest.log` on failure) and exit with `go test`'s status code.

2. **Tests**:
   - Add unit/deploy tests (e.g. in `tests/deploy/`) verifying the test runner script streams live progress lines incrementally as input arrives, surfaces failing tests/panics/races, stays compact on passing runs, and preserves the exit status.
   - Verify `tests/deploy`'s workflow checks (`TestTheGoJobDeclaresEveryFileItsOwnTestsRead`, etc.) stay green.
