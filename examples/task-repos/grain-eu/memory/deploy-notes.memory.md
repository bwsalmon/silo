---
kind: memory
schema: grain/memory@v1
title: Merging in the widgets repo
applies_to: {repos: ["https://git.example.com/hub/widgets.git"]}
updated_by: "grain:eu"
updated_at: "2026-10-16T21:00:00Z"
---

Use squash merges for the widgets repo: its release notes are generated
from one commit per change. This is more specific than the hub team's
shared guidance on merge style, and `eu`'s own memory comes first in its
`grain.config.md`, so it wins (docs/task-repos.md §3.3, §8.15).
