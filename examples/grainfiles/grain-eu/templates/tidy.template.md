---
kind: template
schema: grain/template@v1
form: task
title: "Tidy {{ folder }}"
inputs:
  - {name: folder, type: string, default: memory/}
---
Tidy `{{ folder }}`: merge duplicates, drop stale notes.
