---
kind: repo
schema: grain/repo@v1
role: placements
title: Hub team placements, one folder per Grain
owner:
  team: hub
  silo: https://silo.example.com
host: silo
default_ref: main
ref_policy:
  protected: [main]
  fast_forward_only: true
backlog: ["grains/*/backlog/"]
aliases:
  intake: https://silo.example.com/git/hub/intake.git
  shared: https://silo.example.com/git/hub/shared.git
  eu: https://silo.example.com/git/grains/eu.git
  us: https://silo.example.com/git/grains/us.git
  widgets: https://git.example.com/hub/widgets.git
writers: [silo]
---

# Hub team placements

Silo's own repo, and Silo is its only writer (docs/task-repos.md §8.1).
It holds one folder per Grain that serves the hub team:

- `grains/<grain>/backlog/` is that Grain's **placements**: one
  `kind: ref` per task Silo placed there, with the rank, `spec_rev` and
  the attestation. Placement adds one, and close, cancel and
  re-placement remove it. Re-placement is one commit here: the reference
  leaves one Grain's folder and arrives in another's (§8.8).
- `grains/<grain>/schedules/` holds the team schedules assigned to that
  Grain, as `kind: include` objects naming them where they live. Silo
  never copies a schedule.

A Grain sees its folder only because its own root includes it
(`backlog/silo.md` and `schedules/silo.md` in the Grain's repo). Linking
a Grain to Silo is adding those includes, and unlinking is deleting them
(§8.12). People who reorder a Grain's placements do it through Silo's UI
or API, and Silo commits with `ranked_by` set to them.

Read access is per repo, so the team's Grains can read each other's
placements here. They could already learn as much from the intake. A
deployment that wants Grains isolated from each other gives each Grain's
folder a repo of its own; nothing else changes, because an include names
any folder (§11.4).
