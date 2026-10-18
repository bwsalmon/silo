---
grainfile: v1
title: Migrate stored widgets to the v2 binary header
recipient: https://github.com/acme/grain-eu
not_before: "2026-10-25T09:00:00Z"
depends_on:
  - ./2026-10-16-widget-schema-v2.task.grain.md
references:
  - /docs/widget-schema.md#schema-version-header
  - /src/widget.go
---
Once the v2 binary header has landed in `src/widget.go`, add a migration
helper that re-encodes v1 fixtures to v2 and verifies that no truncated
records remain.
