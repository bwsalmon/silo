---
grainfile: v1
title: "Prevent frequent handoffs and short session lengths after a session summarizes and hands off"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2099-01-01T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/corp-research
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 35
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-20T23:14:46Z"
x_deferred_until: "2099-01-01T00:00:00Z"
x_capabilities:
  - corp-research
  - grain-ci
---
## Problem

After a session summarizes its output and hands off (either via Grain's `write_handover` mechanism in `pkg/orchestrator/conversation.go` or via the underlying agent CLI's context compaction / cross-dispatch session resume), subsequent session segments between handoffs become noticeably short, causing the session to summarize and hand off frequently.

There are two compounding mechanisms behind this:

### 1. `boundThread` in `pkg/orchestrator/conversation.go` never resets the window after a handover is written
- In `pkg/orchestrator/conversation.go` (`commentThreadSection` and `boundThread`, lines 257–372), all comments ever posted on a task are rendered into a single slice and bounded against `maxConversationBytes = 64 << 10` (64 KiB: `headBudget = 21 KiB` and `tailBudget = 43 KiB`).
- Once a task's cumulative comment history crosses 64 KiB (`cut == true`), **`cut` remains `true` on every single subsequent run forever**, because `boundThread` still measures all comments from the beginning of the task rather than measuring comments added *since* the most recent handover (`c.Handover == true`).
- Consequently, `if cut { b.WriteString(handoverRequest(carried != "")) }` fires on **every** dispatch after the initial 64 KiB threshold is crossed—even if only a single short message was added since the last handover—prompting the agent to call `write_handover` again on every run/turn.
- Furthermore, when `relayHandover` (`pkg/orchestrator/finish.go:963`) records a `write_handover` call, it inserts a new `task_comment` row with `Handover: true`. Every handover comment is rendered as part of `rendered` in `commentThreadSection`, so old handover comments accumulate inside the `tailBudget` (43 KiB) window and crowd out actual conversation messages, shrinking the effective tail window further with each handoff.

### 2. Cross-dispatch `ResumeSession` (`SuspendedPreamble`) re-injects the entire assembled prompt into the resumed CLI conversation
- When a task resumes a suspended CLI session across dispatches (`RunConfig.ResumeSession` / `SuspendedPreamble` in `pkg/agent/retry.go`, `pkg/agent/antigravity/resume.go`, `pkg/agent/claude`, `pkg/agent/codex`), the CLI's on-disk session already contains the initial prompt and all prior turns.
- However, `SuspendedPreamble` sends the **entire** full prompt again (30–80+ KiB of system instructions, tool rules, attempt history, and the full 64 KiB `commentThreadSection`) as a new `USER_INPUT` message on every resumed dispatch.
- When `antigravity` (`agy`) compacts its context window into `<CONTEXT_SUMMARY>`, it retains recent `USER_INPUT` messages under `# User Requests` alongside `<summary>`. Because each resumed dispatch injects a massive `USER_INPUT` prompt (and interactive sessions also carry all 30+ eager MCP tool declarations plus untruncated tool outputs up to the MCP output cap), a large fraction of the post-compaction context window is already consumed immediately after a handoff, causing the next compaction/handoff to trigger after only a few turns.

## Proposed Changes

1. **Reset the conversation window at the latest handover in `pkg/orchestrator/conversation.go`**:
   - When a task has one or more handover comments (`c.Handover == true`), use the **latest** handover comment as the summary of all conversation prior to that point, and exclude older/superseded handover comments from the rendered thread.
   - Measure the active tail budget against comments posted *after* the latest handover comment (plus the initial head context and the carried handover summary), so `cut` (and `handoverRequest`) only triggers again once post-handover comments have accumulated another full `tailBudget` (~43 KiB) of conversation.
   - Ensure `write_handover` during long-running interactive `chat` sessions does not spam `task_comment` with redundant handover rows on every turn when the post-handover delta is still small.
2. **Avoid duplicating the full prompt and conversation history on `ResumeSession` (`pkg/agent/retry.go`, `pkg/orchestrator/dispatch.go`)**:
   - Investigate trimming or delta-encoding what is sent after `SuspendedPreamble` when `ResumeSession` succeeds (or only sending new comments/events since the suspended run ended rather than repeating the static instructions and entire 64 KiB conversation history in the new user turn), while keeping the full prompt as the fallback when session resume is refused or lost (`suspendedResumeLostNote`).
3. **Tests**:
   - Add unit tests in `pkg/orchestrator/conversation_test.go` verifying that after a handover comment is recorded, subsequent runs with small incremental comments do not immediately re-trigger `cut = true` / `handoverRequest`, and that superseded handover comments do not consume `tailBudget`.
