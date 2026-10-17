---
kind: task
schema: grain/task@v1
title: Version the widget schema
submitter: user:dev@example.com
created_at: "2026-10-16T09:00:00Z"
moved_from: ["https://github.com/acme/hub.git//intake/tasks/2026-10-16-widget-schema-v2.task.md"]
context:
  - {ref: "https://github.com/acme/widgets.git@main", mode: write}
---
Add `schema_version` to every widget, defaulting to 1.
