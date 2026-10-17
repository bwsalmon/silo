---
title: Publish the widget schema to the registry
submitter: "@carol"
created_at: "2026-10-16T09:00:00Z"
implements:
  - {ref: "../../designs/widget-schema.design.md#WS-2", hash: "sha256:fe3b294753797aaeaacf317479f83babf0578c69b721f0d85f15cf0ac4e42fb2"}
context:
  - {ref: "git::https://github.com/acme/widgets.git?ref=main", mode: write}
capabilities: ["commons:capabilities/gcp-key.capability.md"]
lifecycle: "commons:lifecycles/reviewed.lifecycle.md"
settings:
  model: claude-opus-5-5
  auto_merge: false
---
Publish the widget schema (`src/widget.go`) to the sandbox project's
artifact registry on every release, as design requirement WS-2 asks.
