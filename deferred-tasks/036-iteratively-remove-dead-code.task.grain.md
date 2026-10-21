---
grainfile: v1
title: "Iteratively find and remove dead code until no more is found"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2099-01-01T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/corp-research
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 36
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-20T23:14:47Z"
x_deferred_until: "2099-01-01T00:00:00Z"
x_capabilities:
  - corp-research
  - grain-ci
---
## Goal

Systematically search the repository (`pkg/`, `cmd/`, `ui/src/`) for dead code—unused functions, methods, types, constants, struct fields, unreferenced React components/exports/helpers, orphaned CSS classes, and obsolete compatibility shims—delete what is genuinely unused, and **repeat the pass iteratively until a full pass finds no more dead code**.

## Process

Run iterative passes in a loop until convergence (a pass yields zero new dead-code removals):

1. **Go codebase (`cmd/...`, `pkg/...`)**:
   - Run static analysis and symbol searches (e.g., `staticcheck`, `unused`, `deadcode` if available or via `go tool` / AST / `grep` across `cmd/` and `pkg/`) to identify:
     - Unexported functions, methods, types, vars, and constants with no callers or references.
     - Exported functions, methods, types, constants, and struct fields in internal/domain packages (`pkg/...`) that have zero callers outside their own declaration (or are only tested by a unit test that tests nothing else, where the production symbol itself is completely dead and unreferenced by any binary, CLI command, HTTP handler, or orchestrator flow).
     - Dead parameters, unreachable branches, and leftover migration shims.
2. **Frontend codebase (`ui/src/...`)**:
   - Check every file, exported function/component, hook, and utility in `ui/src/` for actual imports/usages across `ui/src/`.
   - Remove unused components, dead props/handlers, unreferenced helper functions, and orphaned CSS rules.
3. **Iterate until fixed point**:
   - Removing a dead function or component often makes the helpers, types, or tests it exclusively called dead as well.
   - After each removal pass, re-run the dead-code scan across both Go and UI until a complete pass finds **zero** remaining dead symbols.
4. **Verification**:
   - Ensure `go build ./...`, `go vet ./...`, `go test ./...`, and the UI build/tests pass cleanly after the final pass.
