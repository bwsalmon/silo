---
kind: design
schema: grain/design@v1
title: Widget description format
status: implementing
owners: ["user:bsalmon@google.com"]
applies_to: ["/"]
requirements:
  prefix: WG
  next: 3
  retired: []
---

# Widget description format

This design doc lived in `docs/design/` before the repo had a grain
folder, with its `{#WG-n}` anchors, and it stays where it is. It became a
`kind: design` object by gaining the front matter above and the
`.design.md` suffix, and nothing else (docs/task-repos.md §3.2, §12.3).

## Requirements

### Widgets are described in one file each {#WG-1}

Each widget is one YAML file under `widgets/`, named after the widget.
`widgets check` validates every file and reports each problem with its
path and line.

### The format is versioned {#WG-2}

Every widget file names its format version (`format: widgets/v2`).
`widgets check` reads every version it ever accepted and upgrades on
read, so old files keep loading and are never rewritten just to bump the
version. `widgets migrate` rewrites a file only when asked.
