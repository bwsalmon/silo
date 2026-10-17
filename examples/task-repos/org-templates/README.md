---
kind: repo
schema: grain/repo@v1
role: shared
title: Org-wide task templates
owner:
  team: platform
  silo: https://silo.example.com
host: silo
default_ref: main
ref_policy:
  protected: [main]
  fast_forward_only: true
  review_prefix: review/
writers: ["team:platform#maintainer"]
---

# Org-wide task templates

Templates every team may use, released as tags (`v3`). Teams include
this folder from their own shared repo, usually pinned to a tag, and
exclude what they do not want (`hub/shared` excludes
`templates/legacy-triage.template.md`). Nothing here knows who includes it.
