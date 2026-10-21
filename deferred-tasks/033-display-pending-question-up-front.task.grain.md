---
grainfile: v1
title: "Display the pending question text and reply box up front on tasks waiting for user input"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2099-01-01T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/corp-research
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 33
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-20T23:10:11Z"
x_deferred_until: "2099-01-01T00:00:00Z"
x_capabilities:
  - corp-research
  - grain-ci
---
## Problem

When a non-interactive task is parked in `awaiting_reply` waiting for user input (e.g. after an agent calls `ask_question`), opening the task in `ui/src/components/DetailOverlay.jsx` shows the task's title and full original description at the top, while the actual question the agent asked is down inside the `<Timeline />` as a comment. On the Inbox page (`ui/src/components/InboxPage.jsx`), the user also has to click `Reply` on the row before `<Responder />` fetches and displays the question text.

The specific text of the question should be put right up front on the task so it is immediately visible and easy for the user to read and respond to.

## Changes Required

1. **Put the pending question and reply box up front in `DetailOverlay.jsx` (`ui/src/components/DetailOverlay.jsx`, new `ui/src/components/detail/PendingQuestion.jsx` or similar)**:
   - When a task is waiting for a user reply (`t.state === "awaiting_reply" && !t.interactive && !t.pendingSecret`) and has a question/comment in `t.comments`:
     - Render a prominent callout/card at the top of `.detail-main` (right above or below the title/header, ahead of the long task description and timeline — the same prominent position alerts and `<PendingSecret />` occupy) that displays:
       - A clear heading/label indicating the task is waiting for the user's answer.
       - The full rendered markdown text of the question (the latest comment on `t.comments`, plus any attachments on that comment).
       - An inline reply composer (`TextField` + attachment picker + `Send` button) right inside the card so the user can answer immediately at the top of the task without scrolling to the timeline.
   - Ensure drafted reply text in the card survives background 3-second poll re-renders.

2. **Show the question text up front in Inbox (`ui/src/components/InboxPage.jsx`)**:
   - For tasks in the `awaiting_reply` group in `InboxPage.jsx` (or via the task list / pre-fetched detail), surface the question text directly on the waiting item so the user can read the question immediately without having to click a button first to reveal what is being asked.

3. **Tests (`ui/src/components/DetailOverlay.test.jsx`, `ui/src/components/InboxPage.test.jsx`)**:
   - Add tests verifying that a task in `awaiting_reply` displays the specific question text and reply box up front at the top of the task detail pane, and that submitting a reply posts the comment and updates the task.
