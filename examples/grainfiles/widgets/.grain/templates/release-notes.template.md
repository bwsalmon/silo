---
kind: template
schema: grain/template@v1
title: "Release notes for {{ version }}"
inputs:
  - {name: version, type: string, default: v0.0.0}
context:
  - {ref: "https://github.com/acme/widgets.git@main", mode: write}
---
Write `CHANGELOG.md`'s entry for {{ version }}.
