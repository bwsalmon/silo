---
kind: exclude
schema: grain/exclude@v1
patterns: ["../y/templates/*.md"]
---

W excludes Y again, by pattern. Includes are gathered first and excludes
applied after, so W contributes no Y, whichever file is read first.
