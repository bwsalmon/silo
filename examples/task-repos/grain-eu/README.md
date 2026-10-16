---
kind: repo
schema: grain/repo@v1
role: grain
title: Backlog of Grain "eu"
owner:
  grain: eu
  user: bsalmon@google.com
  silo: https://silo.example.com
host: silo
default_ref: main
ref_policy:
  protected: [main]
  fast_forward_only: true
aliases:
  intake: https://silo.example.com/git/hub/intake.git
  shared: https://silo.example.com/git/hub/shared.git
  us: https://silo.example.com/git/grains/us.git
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
  - who: ["grain:eu"]
    may: update
    kinds: [task]
    transition: {field: state, from: [placed], to: [running]}
    fields: [state]
  - who: ["grain:eu"]
    may: update
    kinds: [task]
    transition: {field: state, from: [placed], to: [refused]}
    fields: [state, refusal]
  - who: ["grain:eu"]
    may: update
    kinds: [task]
    transition: {field: state, from: [placed, running], to: [done, failed, cancelled]}
    fields: [state, closed]
  - who: ["grain:eu"]
    may: create
    kinds: [task]
    where: {state: running, submitted_via: schedule}
    fields: "*"
    paths: ["local/**"]
    note: Firings of the schedules the Grain's own config lists (fires_in grain).
  - who: ["grain:eu", silo]
    may: create
    kinds: [comment]
    fields: "*"
  - who: ["user:bsalmon@google.com"]
    may: update
    kinds: [repo]
    fields: [title, aliases, require_signed, keys, body]
    note: The owner field of a Grain repo can never change.
---

# Backlog of Grain "eu"

This repo is the backlog of Grain `eu`, owned by bsalmon@google.com. It is
hosted by Silo because the Grain runs behind NAT. The Grain pulls from it
and pushes to it, and Silo's WebSocket tells it when to pull.

- Silo writes placements, cancel requests and tombstones.
- The Grain writes a task's state, its thread (one file per comment) and
  its closing snapshot.
- Runs, events and transcripts stay in the Grain's database.

If the Grain is unlinked from Silo (`grain tasks unlink`), its local clone
of this repo becomes its backlog again, with all of this history.
