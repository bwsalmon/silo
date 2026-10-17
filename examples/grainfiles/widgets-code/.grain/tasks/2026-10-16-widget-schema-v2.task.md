---
kind: task
schema: grain/task@v1
moved_from:
  - "https://silo.example.com/git/hub/intake.git//tasks/2026-10-16-widget-schema-v2.task.md"
title: Read every widget format version, and add widgets migrate
team: hub
context:
  - {ref: "https://git.example.com/hub/widgets.git@main", mode: write}
  - {ref: /docs/design/widgets.design.md}
state: approved
submitter: "user:bsalmon@google.com"
submitted_via: ui
approved_by: "user:approver@example.com"
approved_at: "2026-10-16T17:52:10Z"
created_at: "2026-10-16T17:31:44Z"
thread: "intake:tasks/2026-10-16-widget-schema-v2/comments/"   # stayed in the intake when the task was promoted (§9.2)
implements:
  - ref: "/docs/design/widgets.design.md#WG-2"
    rev: 3b1f0c9e7d5a4b2c8e6f0a1d3c5b7e9f2a4c6d8e
    hash: "sha256:90e126a4685a8adcc9b7d322470729c6a3d66742aee5bbaf8032eb340b3d4491"
---

Implement WG-2 of docs/design/widgets.design.md. Give the widget loader a
decoder per format version (`widgets/v1` and `widgets/v2`) that upgrades
on read, make `widgets check` accept both, and add `widgets migrate`,
which rewrites a file to the newest version only when asked. Add a
fixture per version under `testdata/`, and a test that every v1 fixture
loads to the same value as its v2 twin.
