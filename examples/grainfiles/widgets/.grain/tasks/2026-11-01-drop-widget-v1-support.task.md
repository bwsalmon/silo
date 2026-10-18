---
title: Remove legacy v1 widget decoder support after the migration window
requestor: Bruce Salmon <bsalmon@google.com>
recipient: git::https://github.com/acme/grain-eu.git
created_at: "2026-10-16T09:30:00Z"
draft: true
not_before: "2026-11-01"
depends_on:
  - ./2026-10-16-widget-schema-v2-migration.task.md
references:
  - /docs/widget-schema.md#schema-version-header
  - /src/widget.go
---
Once the v2 migration window closes, remove the fallback v1 branch from
`src/widget.go` so decoders reject payloads without the `0x57 0x02`
header.
