---
grainfile: v1
title: "Hide underlying task for console sessions and fix session restart, cancel, and delete"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2099-01-01T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/corp-research
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 34
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-20T23:10:13Z"
x_deferred_until: "2099-01-01T00:00:00Z"
x_capabilities:
  - corp-research
  - grain-ci
---
## Problem

Console sessions (such as Director and any custom sessions in `ConsoleSessionPage.jsx`) are implemented on top of an underlying `model.Task`, but today that implementation detail leaks into the UI and breaks expected session lifecycle operations (restarting, cancelling, and deleting):

1. **The underlying task is visible in the UI (`ui/src/components/ConsoleSessionPage.jsx`)**:
   - The session header shows `task {task.id}` (`ConsoleSessionPage.jsx:495`).
   - The compose toolbar shows `Open the task behind this session` (`href="/tasks/${task.id}"`, lines 563–570) and `Open the chat before this one` (`href="/tasks/${task.continues}"`, lines 574–583).
   - The restart panel mentions `"readable as closed task {task?.id}"` (line 263).

2. **Sometimes you cannot restart the chat (`ui/src/components/ConsoleSessionPage.jsx:236–237`)**:
   - `Start a fresh chat` is gated on `const offerFresh = Boolean(session.crowded) && Boolean(task) && task.state !== "closed";`. Unless `session.crowded` is true (prompt tokens past `model.PromptWarnTokens` or thread cut), the restart button is completely hidden and the user has no way to restart or reset the session from the session UI.
   - There is also no **Cancel / Stop** button in `ConsoleSessionPage.jsx` / `TaskChat.jsx` when a session turn is currently `running` or `queued`, so the user cannot cancel an in-flight turn from the chat view.

3. **Restarting or deleting a session sometimes does not actually reset the session (`pkg/ui/console_sessions.go`, `pkg/orchestrator/consolesessions.go`, `ui/src/components/ConsoleSessionPage.jsx`)**:
   - **Restart delay & handover carryover**: Clicking `Close this chat and start a fresh one` only calls `POST /api/tasks/${task.id}/close`. Until the daemon's 10-second reconcile loop runs `reconcileConsoleSessions`, `Store.TaskIDWithTag` still returns the closed task ID, leaving the UI stuck on the closed task. Furthermore, `fileConsoleSession` (`pkg/orchestrator/consolesessions.go:234`) always copies the previous task's handover summary (`carryConsoleSessionHandover`) onto the replacement task, and does not stop an in-flight run first if the session was running.
   - **Deleting a session leaves its open task behind (`pkg/ui/console_sessions.go:510–529`)**:
     - `DeleteConsoleSession` only deletes the entry from `cfg.ConsoleSessions` and **never stops or closes the session's underlying task**. If a session with the same ID is created again (or if the last session is `"director"`, where `model.ConsoleSessionsOr(cfg.ConsoleSessions)` immediately falls back to `DefaultConsoleSessions()` when the slice becomes empty!), `TaskIDWithTag` re-attaches to the **exact same open task** with the old conversation still intact.

## Changes Required

1. **Make the underlying task invisible for console sessions (`ui/src/components/ConsoleSessionPage.jsx`, `ui/src/App.jsx`)**:
   - Remove `task {task.id}` from the `ConsoleSessionPage` header (`ConsoleSessionPage.jsx:495`).
   - Remove the `Open the task behind this session` and `Open the chat before this one` buttons (`ConsoleSessionPage.jsx:563–583`), and remove task ID references from session copy.
   - If a user navigates to `/tasks/{id}` for a task that backs a console session (`t.consoleSession`), open/redirect to the session view rather than exposing the raw task detail pane.

2. **Add first-class Session Restart/Reset, Cancel/Stop, and proper Delete (`pkg/ui/console_sessions.go`, `pkg/orchestrator/consolesessions.go`, `ui/src/components/ConsoleSessionPage.jsx`, `ui/src/components/TaskChat.jsx`)**:
   - **Cancel / Stop in-flight turn**: When the session's underlying task is `running` or `queued`, show a **Cancel** / **Stop** control in the session chat (`ConsoleSessionPage.jsx` / `TaskChat.jsx`) that stops the active run (`POST /api/console-sessions/{id}/cancel` or `StopTask`) and returns the session to `awaiting_reply` ready for the next message.
   - **Always-available Restart / Reset session (`POST /api/console-sessions/{id}/restart`)**:
     - Always show the **Restart chat** / **Reset session** control in `ConsoleSessionPage.jsx` (regardless of `session.crowded`; when `session.crowded` is true, surface `session.crowdedNote` as a hint/badge).
     - Allow restarting cleanly (stopping any in-flight run, closing the old underlying task, and **synchronously** filing the new session task right in the endpoint so the response returns the new `taskId` immediately without waiting 10s for the reconcile loop).
     - Support resetting completely fresh (without carrying over the old handover when the user resets the session, or let the user choose whether to keep the handover summary on a crowded split vs. full reset).
   - **Fix Delete (`DELETE /api/console-sessions/{id}`)**:
     - When deleting a console session, stop any in-flight run on its task and close the underlying task so its old conversation is never re-attached if a session with that ID is created later.
     - Handle deleting or resetting `"director"` cleanly so `ConsoleSessionsOr` does not silently resurrect the old unclosed task.

3. **Tests**:
   - Add backend tests in `pkg/ui/console_sessions_test.go` and `pkg/orchestrator/consolesessions_test.go` covering session restart (while idle and while running), cancel, and delete (verifying the old task is stopped/closed and a recreated session starts with a fresh conversation).
   - Update `ui/src/components/ConsoleSessionPage.test.jsx` to verify that task IDs/links are not shown and that Restart, Cancel, and Delete controls work in all session states.
