---
grainfile: v1
title: "[HOLD for grain refactor] pkg/hubnode, pkg/agent: support multi-user Hub shared chat sessions and Google Chat-linked rooms"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2026-10-25T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 149
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-19T01:17:58Z"
x_deferred_until: "2026-10-25T00:00:00Z"
x_capabilities:
  - grain-ci
---
Update `bwsalmon/grain` (`pkg/hubnode`, `pkg/agent`, `pkg/orchestrator`) after the refactor so a Grain deployment connected to the Hub can serve as the backing agent (or `@mentioned` secondary agent) for **Hub Shared Chat Sessions** and **Google Chat-linked rooms** (building on Task #119):

1. **Multi-User Transcript & Attribution (`pkg/hubnode`, `pkg/agent`):**
   - When receiving `tasks.TypeMessage` batches on an interactive session stream from the Hub (`hub/chats`), format each human participant's message with their verified identity and source (`[alice@example.com]: ...` or `[bob@example.com via Google Chat]: ...`) in the agent's conversation history so the agent knows who is speaking in a group room of engineers, PMs, or operators.
2. **`@mention` vs. `All messages` Turn-Taking & Cross-Deployment Proposals:**
   - Support the Hub shared chat session's wakeup semantics (only running an agent turn when the Hub dispatches a turn trigger, while keeping passive room messages in context).
   - When the agent in a shared chat calls `propose_task` (or generates `.task.grain.md` files), allow targeting any deployment in the Hub via its canonical URL (`recipient: https://<hub-host>/deployments/<name>`) and link the proposed task back to the Hub shared chat session.

### Verification
- Run `go test -race ./pkg/hubnode/... ./pkg/agent/...`.
