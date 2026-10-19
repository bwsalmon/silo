---
grainfile: v1
title: Add version header to the widget binary schema
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/acme/grain-eu
capabilities:
  - /.grain/capabilities/gcp-key.capability.grain.md#agent-usage
references:
  - /docs/widget-schema.md#schema-version-header
  - /src/widget.go
---
Update `src/widget.go` so encoded widgets begin with the two-byte v2
schema header described in `docs/widget-schema.md`, while keeping the
decoder compatible with v1 payloads. Add unit tests covering both
versions and round-trip encoding.
