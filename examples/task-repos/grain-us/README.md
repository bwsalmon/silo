---
kind: repo
schema: grain/repo@v1
role: grain
title: Backlog and execution records of Grain "us"
owner:
  grain: us
  team: hub
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
  eu: https://silo.example.com/git/grains/eu.git
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
    fields: [expedite, to]
    note: >-
      expedite is Silo's at_front. to may change only to the end of the
      redirect chain it already resolves through (docs/task-repos.md §4).
  - who: ["grain:us", "team:hub#admin"]
    may: update
    kinds: [ref]
    paths: ["backlog/**"]
    fields: [rank, priority, ranked_by, ranked_at]
    bind: {ranked_by: $pusher}
    note: The Grain and its owners, the hub team's admins, own this queue's order. Silo never changes it.
  - who: ["grain:us"]
    may: create
    kinds: [run]
    paths: ["runs/**"]
    where: {grain: us}
    fields: "*"
    note: An execution record starts as accepted, or as refused by local policy.
  - who: ["grain:us"]
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
  - who: ["grain:us", silo, "team:hub#admin"]
    may: create
    kinds: [comment]
    paths: ["runs/**/comments/**"]
    fields: "*"
    note: The thread of each run. Silo writes users' replies with via silo.
  - who: ["grain:us", "team:hub#admin"]
    may: create
    kinds: [task, ref, run]
    paths: ["local/**"]
    fields: "*"
    note: >-
      Work the Grain authors itself: firings of the schedules its own
      config lists, and its owner's own tasks, with their references and
      runs. Add local/backlog/ to backlog to queue them.
  - who: ["team:hub#admin"]
    may: update
    kinds: [repo]
    fields: [title, aliases, backlog, require_signed, keys, moved_to, body]
    note: The owner field of a Grain repo can never change.
---

# Backlog and execution records of Grain "us"

This repo belongs to Grain `us`, a team Grain owned by the hub team's
admins. The example shows a task re-placed here from Grain `eu`
(docs/task-repos.md §8.8): `backlog/` references it, and its execution
record names the superseded record in `eu` as `previous`. The task
depends on a task that Grain `eu` completed, which this Grain read from
`eu`'s execution record (§8.9).
