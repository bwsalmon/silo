---
kind: template
schema: grain/template@v1
name: Release notes
title: "Draft release notes for changes since {{ since }}"
inputs:
  - {name: since, type: string, required: true, label: The tag the notes start from}
team: hub
repo: https://git.example.com/hub/widgets.git
base: main
auto_merge: false
created_at: "2026-10-16T18:05:00Z"
---

Draft release notes for every change merged to `main` since `{{ since }}`.

- Group the changes by package, and lead each group with anything that
  changes the widget format (docs/design/widgets.md).
- Write them to `CHANGELOG.md` under an `## Unreleased` heading, and open
  a pull request. Do not tag a release.
