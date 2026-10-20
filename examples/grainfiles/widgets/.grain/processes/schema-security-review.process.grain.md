---
grainfile: v1
title: Security review for widget schema and parser changes
when:
  pull_request:
    branches: ["main"]
    paths: ["src/**"]
    ignore: ["**/*.md"]
  push:
    branches: ["main"]
    paths: ["src/**"]
gate: true
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/self-debug
references:
  - /docs/widget-schema.md
  - /src/widget.go
---
Review the changes under `src/` against the wire-framing and version
header rules in `docs/widget-schema.md`. Verify that untrusted lengths
are bounds-checked before allocation and leave inline review comments on
any findings.
