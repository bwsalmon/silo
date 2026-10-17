---
title: "Tidy {{ area }}"
inputs:
  - {name: area, type: enum, options: [run-notes, branches], required: true}
settings:
  nothing_needed: close
---
Tidy up {{ area }}: delete what is stale, and say what you removed.
