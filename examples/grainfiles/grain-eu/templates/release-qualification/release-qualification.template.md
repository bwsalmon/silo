---
form: folder
title: "Release qualification of {{ branch }}"
inputs:
  - {name: branch, type: string, default: candidate}
  - {name: platforms, type: list, default: [linux-amd64, linux-arm64]}
capabilities: ["commons:capabilities/gcp-key.capability.md"]
settings:
  mode: count
  count: 1
  require_approval: false
---
grain's release-qualification suite as a folder template: a smoke run
per platform, then a report that waits for it.
