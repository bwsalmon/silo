---
kind: repo
schema: grain/repo@v1
role: grain
title: Backlog and execution records of Grain "eu"
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
aliases:
  intake: https://silo.example.com/git/hub/intake.git
  shared: https://silo.example.com/git/hub/shared.git
  us: https://silo.example.com/git/grains/us.git
  widgets: https://git.example.com/hub/widgets.git
writers:
  - who: [silo]
    may: [create, delete]
    kinds: [ref]
    paths: ["backlog/**"]
    fields: [kind, schema, to, spec_rev, rank, priority, expedite, previous, added_by, added_at, attestation, body]
    bind: {added_by: $pusher}
    note: >-
      Placement adds a reference (the offer of hub-interface.md, as a
      commit); re-placement, cancel and close remove it. The rank and
      priority it is created with are the intake's.
  - who: [silo]
    may: update
    kinds: [ref]
    paths: ["backlog/**"]
    fields: [expedite, to, spec_rev]
    note: >-
      expedite is Silo's at_front. to may change only to the end of the
      redirect chain it already resolves through (docs/task-repos.md §4),
      with spec_rev moving to the commit of the new home it was read at.
  - who: ["grain:eu", "user:bsalmon@google.com"]
    may: update
    kinds: [ref]
    paths: ["backlog/**"]
    fields: [rank, priority, ranked_by, ranked_at]
    bind: {ranked_by: $pusher}
    note: The Grain and its owner own this queue's order. Silo never changes it.
  - who: ["grain:eu"]
    may: create
    kinds: [run]
    paths: ["runs/**"]
    where: {grain: eu}
    fields: "*"
    note: An execution record starts as accepted, or as refused by local policy.
  - who: ["grain:eu"]
    may: update
    kinds: [run]
    paths: ["runs/**"]
    transition: {field: state, from: [accepted, running, waiting], to: [running, waiting, done, failed, cancelled]}
    fields: [state, started_at, closed]
  - who: [silo]
    may: update
    kinds: [run]
    paths: ["runs/**"]
    transition: {field: state, from: [accepted, running, waiting], to: [superseded]}
    fields: [state, superseded]
    note: >-
      Re-placement only, in the same commit that deletes the run's backlog
      reference (§8.8). The hook checks that the reference is gone.
  - who: ["grain:eu", silo, "user:bsalmon@google.com"]
    may: create
    kinds: [comment]
    paths: ["runs/**/comments/**"]
    fields: "*"
    note: The thread of each run. Silo writes users' replies with via silo.
  - who: ["grain:eu", "user:bsalmon@google.com"]
    may: create
    kinds: [task, ref, run]
    paths: ["local/**"]
    fields: "*"
    note: >-
      Work the Grain authors itself: firings of the schedules its own
      config lists, and its owner's own tasks, with their references and
      runs. Add local/backlog/ to backlog to queue them.
  - who: ["user:bsalmon@google.com"]
    may: update
    kinds: [repo]
    fields: [title, aliases, backlog, require_signed, keys, moved_to, body]
    note: The owner field of a Grain repo can never change.
---

# Backlog and execution records of Grain "eu"

This repo belongs to Grain `eu`, owned by bsalmon@google.com. It is
hosted by Silo because the Grain runs behind NAT. The Grain pulls from it
and pushes to it, and Silo's WebSocket tells it when to pull. The Grain
never pushes anywhere else.

- `backlog/` is the queue: one small `kind: ref` per task, pointing at
  the task where it lives: the hub intake, or the widgets code repo for a
  promoted task. Silo adds and removes
  references. The Grain and its owner set `rank` and `priority`.
  Order is dependencies first, then `expedite`, then `priority`, then
  `rank` (docs/task-repos.md §5.2).
- `runs/` holds one execution record (`kind: run`) per task this Grain
  took on, with its thread in `comments/`. Closed records stay here as
  history.
- Attempts, events and transcripts stay in the Grain's database.

If the Grain is unlinked from Silo (`grain tasks unlink`), its local clone
of this repo becomes its backlog again, with all of this history.
