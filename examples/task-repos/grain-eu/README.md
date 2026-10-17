---
kind: repo
schema: grain/repo@v1
role: grain-root
grain: {id: eu, silo: https://silo.example.com}
title: Root of Grain "eu"
owner:
  grain: eu
  user: bsalmon@google.com
  silo: https://silo.example.com
host: silo
default_ref: main
ref_policy:
  protected: [main]
  fast_forward_only: true
backlog: [backlog/]
lanes:
  mode: weighted
  order:
    - {lane: "placements:grains/eu/backlog/", weight: 3}
    - {lane: backlog/, weight: 1}
aliases:
  intake: https://silo.example.com/git/hub/intake.git
  shared: https://silo.example.com/git/hub/shared.git
  placements: https://silo.example.com/git/hub/placements.git
  us: https://silo.example.com/git/grains/us.git
  widgets: https://git.example.com/hub/widgets.git
writers: ["grain:eu", "user:bsalmon@google.com"]
---

# Root of Grain "eu"

This repo is the root of Grain `eu`, owned by bsalmon@google.com: the
folder whose namespace the Grain acts on (docs/task-repos.md §3.2). Silo
hosts it, because the Grain runs behind NAT, but **only the Grain and
its owner write it**. Silo writes Silo's repos, and the Grain reads them
through the includes below.

This README is metadata only. The Grain's objects are files:

- `backlog/` is the Grain's own lane: its owner's tasks, ranked here.
  `backlog/silo.ref.md` is a `kind: include` of Silo's folder for this Grain,
  `placements:grains/eu/backlog/`, tracking `main` and accepting only
  backlog references. Those placements are a second lane, with ranks of
  their own. `lanes` above weights Silo's lane 3 to 1 against the owner's
  (§5.2).
- `templates/hub.template.md` includes the hub team's templates, pinned, and
  through them the org's, less the one template the team excludes.
- `capabilities/hub.capability.md` includes the hub team's capability objects:
  available to request here, granted only if `eu`'s local policy admits
  them. `capabilities/bases/` holds `eu`'s declarations of the base
  capabilities it implements, which `eu` writes from its provider code
  (docs/task-repos.md §5.7).
- `tasks/` holds the tasks the owner writes for this Grain alone.
- `runs/` holds one execution record (`kind: run`) per task this Grain
  took on, from either lane, with its thread in `comments/`.
- Attempts, events and transcripts stay in the Grain's database.

To unlink from Silo, the owner deletes `backlog/silo.ref.md`. This repo, and
all of its history, stays the Grain's.
