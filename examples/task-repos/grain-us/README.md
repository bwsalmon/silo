---
kind: repo
schema: grain/repo@v1
role: grain
title: Backlog of Grain "us"
owner:
  grain: us
  team: hub
  silo: https://silo.example.com
host: silo
default_ref: main
ref_policy:
  protected: [main]
  fast_forward_only: true
aliases:
  intake: https://silo.example.com/git/hub/intake.git
  shared: https://silo.example.com/git/hub/shared.git
  eu: https://silo.example.com/git/grains/eu.git
writers:
  - who: [silo]
    may: create
    kinds: [task]
    where: {state: placed}
    fields: "*"
    note: A placement (the offer of hub-interface.md, as a commit).
  - who: [silo]
    may: update
    kinds: [task]
    fields: [cancel]
  - who: [silo]
    may: update
    kinds: [task]
    transition: {field: state, from: [placed, running, refused], to: [moved]}
    fields: [state, moved_to]
    note: A tombstone after re-placement (§8.6).
  - who: ["grain:us"]
    may: update
    kinds: [task]
    transition: {field: state, from: [placed], to: [running]}
    fields: [state]
  - who: ["grain:us"]
    may: update
    kinds: [task]
    transition: {field: state, from: [placed], to: [refused]}
    fields: [state, refusal]
  - who: ["grain:us"]
    may: update
    kinds: [task]
    transition: {field: state, from: [placed, running], to: [done, failed, cancelled]}
    fields: [state, closed]
  - who: ["grain:us"]
    may: create
    kinds: [task]
    where: {state: running, submitted_via: schedule}
    fields: "*"
    paths: ["local/**"]
    note: Firings of the schedules the Grain's own config lists (fires_in grain).
  - who: ["grain:us", silo]
    may: create
    kinds: [comment]
    fields: "*"
  - who: ["team:hub#admin"]
    may: update
    kinds: [repo]
    fields: [title, aliases, require_signed, keys, body]
    note: The owner field of a Grain repo can never change.
---

# Backlog of Grain "us"

This repo is the backlog of Grain `us`, a team Grain owned by the hub
team's admins. The example shows a task re-placed here from Grain `eu`
(§8.6), which depends on a task that Grain `eu` completed (§8.7).
