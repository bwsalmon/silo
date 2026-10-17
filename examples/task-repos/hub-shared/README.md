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
  org: https://silo.example.com/git/org/templates.git
  org-caps: https://silo.example.com/git/org/capabilities.git
writers:
  - who: ["team:hub#maintainer"]
    may: [create, update, move]
    kinds: [template, trigger, gate, design, include, exclude, capability]
    fields: "*"
    note: >-
      Definitions are never deleted: a rename or reorganisation leaves a
      kind: moved tombstone at the old path (docs/task-repos.md §2.1).
  - who: ["team:hub#member"]
    may: [create, update]
    kinds: [template, trigger, gate, design, include, exclude, capability]
    fields: "*"
    refs: ["review/*"]
    note: >-
      Members propose definition changes on review branches. main takes
      them only as a merge commit pushed by an approver who is not the
      branch's author (docs/task-repos.md §8.11).
  - who: ["team:hub#approver"]
    may: update
    kinds: [template, trigger, gate, design, include, exclude, capability]
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

This repo holds templates, triggers and design docs for the hub team.
Tasks reference them across repos by URL, or by an alias declared in the
referring repo's README.

- `templates/` holds task templates. Their bodies use the strict Jinja2
  subset (§6.1). `templates/partials/` holds include-only pieces, and
  `templates/ghcr/` the GHCR ones. `templates/prune-packages.md` is the
  tombstone the move to `ghcr/` left. `templates/org.md` includes the
  org-wide templates at `v3`, and `templates/no-legacy-triage.md`
  excludes one of them from what this folder contributes (§3.3).
- `triggers/` holds the team's triggers (`kind: trigger`, §6.2): two
  scheduled ones, and `plan-designs.md`, the planner as a
  `design-change` trigger. A `fires_in: silo` trigger fires in Silo and
  lands in the hub intake. A `fires_in: grain` one fires on the Grains
  Silo assigns it to, by including this folder from their folders in
  `hub/placements`, narrowed to `kinds: [trigger]`.
- `designs/` holds design docs with `{#HS-n}` requirement anchors.
  `triggers/plan-designs.md` plans them on every merge to `main`.
- `capabilities/` holds the team's capability objects (`kind:
  capability`). `widgets-deployer.md` narrows the org's
  `org-caps:gcp/acme-widgets.md`, pinned at `v1`, by role and token
  lifetime, and with a per-use Anselm reducer. A Grain that includes this
  folder can offer them to tasks; it honours them only if its local
  policy sanctions this folder as a source (§5.7).
