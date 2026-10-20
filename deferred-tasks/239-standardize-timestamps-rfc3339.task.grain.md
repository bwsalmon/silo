---
grainfile: v1
title: "Standardize all timestamps across Grain to RFC 3339"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
x_original_task_id: 239
x_state_at_dump: "deferred, closed"
x_closed_at: "2026-10-20T04:54:28Z"
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-20T01:59:59Z"
x_deferred_until: null
x_capabilities: []
---
Standardize all timestamp inputs, parsers, schemas, and documentation across `bwsalmon/grain` in its entirety to **RFC 3339** (`time.RFC3339` / `time.RFC3339Nano`), removing ad-hoc date (`YYYY-MM-DD`) and multi-layout timestamp amalgams:

## Required Changes

1. **`not_before` on Tasks, Proposals, UI/API, and CLI (`pkg/mcp/`, `pkg/orchestrator/`, `pkg/ui/`, `pkg/cli/`):**
   - Update `mcp.ParseNotBefore` in `pkg/mcp/mock_tools.go` (and its schema/description on `propose_task`, plus `pkg/orchestrator/finish.go`, `pkg/ui/`, and `pkg/cli/`) to accept strictly **RFC 3339 timestamps** (`time.RFC3339`, e.g. `2026-10-14T09:00:00Z`), removing the bare `YYYY-MM-DD` (`2006-01-02`) fallback.
   - Update tests (`pkg/mcp/live_proposal_test.go`, `pkg/daemon/proposal_not_before_test.go`, `pkg/orchestrator/finish_test.go`, `pkg/ui/task_not_before_test.go`, etc.) to pass RFC 3339 timestamps.

2. **`read_grain_metrics` and CLI/API Window Bounds (`since` / `until`):**
   - Update `since` and `until` parsing and tool schema descriptions in `pkg/capability/selfdebug/` and `pkg/cli/` (e.g. `grain metrics`) so `since` and `until` take strictly **RFC 3339 timestamps** (matching `read_grain_log` in `pkg/capability/grainlog/grainlog.go`), removing bare `2006-01-02` date parsing.

3. **CI & Task Filter Timestamp Parsing (`pkg/ci/filter.go`, `pkg/cli/taskfilters.go`, `pkg/cli/list.go`):**
   - Replace multi-layout timestamp loops (`[]string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"}` in `pkg/ci/filter.go` and bare date parsing in `pkg/cli/taskfilters.go` / `pkg/cli/list.go`) with standard RFC 3339 timestamp parsing (`time.RFC3339`).
   - Keep custom field type `date` (`CustomFieldDateLayout` in `pkg/model/custom_field.go`) only if explicitly representing a calendar-date custom field, or migrate if appropriate; ensure all point-in-time timestamps across Grain use RFC 3339.

## Verification
- Run `go test ./...` across `bwsalmon/grain`.
