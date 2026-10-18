---
title: Security review for widget schema and parser changes
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/acme/grain-eu
created_at: "2026-10-16T10:00:00Z"
"on":
  pull_request:
    branches: ["main"]
    paths: ["src/**"]
    ignore: ["**/*.md"]
  push:
    branches: ["main"]
    paths: ["src/**"]
gate: true
references:
  - /docs/widget-schema.md
  - /src/widget.go
---
Review the changes under `src/` against the wire-framing and version
header rules in `docs/widget-schema.md`. Verify that untrusted lengths
are bounds-checked before allocation and leave inline review comments on
any findings.
