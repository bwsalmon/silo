---
title: Publish the widget schema to the registry
definition: "git::https://github.com/acme/widgets.git//.grain/tasks/2026-10-16-widget-schema-v2.task.md?ref=3f6a1c9e2b7d4058a1e6c3b9d2f7084e5a6b1c2d"
outcome: succeeded
deployment: "@acme-grain-eu"
started_at: "2026-10-17T02:00:00Z"
finished_at: "2026-10-17T02:41:12Z"
links:
  pull_request: https://github.com/acme/widgets/pull/412
  branch: grain/eu/widget-schema-v2
  commit: 5b1e0c7d9a2f4e6b8c0d1e2f3a4b5c6d7e8f9a0b
---
Added a `publish-schema` release step that pushes the schema to the
sandbox registry with the `gcp-key` capability. Merged as
[acme/widgets#412](https://github.com/acme/widgets/pull/412).
