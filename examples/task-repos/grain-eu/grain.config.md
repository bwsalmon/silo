---
kind: config
schema: grain/config@v1
grain: {id: eu, silo: https://silo.example.com}
owner: {user: bsalmon@google.com}
backlog:
  mode: weighted
  expedite: lane
  lanes:
    - name: silo
      path: "placements:grains/eu/backlog/@main"
      ref_mode: track
      weight: 3
    - name: own
      path: backlog/
      weight: 1
      ignore: ["drafts/"]
triggers:
  - path: triggers/
  - path: "shared:triggers/@main"
    ref_mode: track
memory:
  - path: memory/
    write: true
  - path: "shared:memory/@main"
    ref_mode: track
limits: {objects_per_entry: 2000, objects: 20000}
---

Grain `eu`'s root configuration (docs/task-repos.md §3.3): what `eu`
acts on, and nothing else.

- **Backlog lanes.** Silo's folder for `eu` in `hub/placements`, which the
  owner added when linking (§8.12), and `eu`'s own `backlog/`, weighted 3
  to 1. References under `backlog/drafts/` are not active.
- **Trigger roots.** `eu`'s own `triggers/`, and the hub team's
  `triggers/` in `hub/shared`, tracking `main`: `eu` fires the team's
  `fires_in: grain` triggers itself, and lists the `fires_in: silo` ones
  without firing them. The team's `triggers/archive/` is skipped.
- **Memory roots.** `eu`'s own `memory/`, which its memory agent writes,
  first, so its guidance wins over the team's shared `memory/`.

Templates, capability definitions and designs are not here: `eu` reaches
them only by reference (§3.1).
