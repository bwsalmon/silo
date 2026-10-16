---
kind: repo
schema: grain/repo@v1
role: shared
title: Hub team shared definitions
owner:
  team: hub
  silo: https://silo.example.com
host: silo
default_ref: main
ref_policy:
  protected: [main]
  fast_forward_only: true
  review_prefix: review/
aliases:
  intake: https://silo.example.com/git/hub/intake.git
  grain-code: https://github.com/bwsalmon/grain.git
  silo-code: https://github.com/bwsalmon/silo.git
writers:
  - who: ["team:hub#maintainer"]
    may: [create, update, delete]
    kinds: [template, schedule, design]
    fields: "*"
  - who: ["team:hub#member"]
    may: [create, update]
    kinds: [template, schedule, design]
    fields: "*"
    refs: ["review/*"]
    note: >-
      Members propose definition changes on review branches. main takes
      them only as a merge commit pushed by an approver who is not the
      branch's author (docs/task-repos.md §8.9).
  - who: ["team:hub#approver"]
    may: update
    kinds: [template, schedule, design]
    fields: "*"
    merge_of: "review/*"
  - who: [silo]
    may: update
    kinds: [design]
    fields: [requirements, body]
    note: Planning assigns IDs to new requirement headings (§7.1), and nothing else.
  - who: ["team:hub#admin"]
    may: update
    kinds: [repo]
    fields: "*"
---

# Hub team shared definitions

This repo holds templates, schedules and design docs for the hub team.
Tasks reference them across repos by URL, or by an alias declared in the
referring repo's README.

- `templates/` holds task templates. Their bodies use the strict Jinja2
  subset (§6.1). `templates/partials/` holds include-only pieces.
- `schedules/` holds cadences that fire templates. They fire in Silo and
  land in the hub intake.
- `designs/` holds design docs with `{#HS-n}` requirement anchors. Silo
  plans them on every merge to `main`.
