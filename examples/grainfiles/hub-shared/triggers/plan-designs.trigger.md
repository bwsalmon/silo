---
kind: trigger
schema: grain/trigger@v1
aka: ["repo:trigger:bwsalmon:grain:design-docs"]
title: Plan design-doc changes
"on":
  design-change:
    designs: ["../designs/"]
    classes: [new, changed, removed]
    debounce: 5m
    max_tasks_per_change: 10
fires:
  template: ../templates/design-plan.template.md
  per: event
  inputs: {mode: plan}
  bind:
    group: event.group
    paths: event.paths
    from_sha: event.from_sha
    to_sha: event.to_sha
    branch: event.branch
team: hub
fires_in: silo
auto_approve_proposals: false
enabled: true
created_at: "2026-10-17T01:30:00Z"
---

The design-doc planner, as a trigger (docs/grainfiles.md §6.9). Task
4446 made planning a Silo operation; it is now this object, in the
team's triggers folder beside the others, and Silo is only what fires it.

`on: design-change` watches the designs under `designs/`. When a push to
a design's `default_ref` is accepted, Silo diffs its requirements by ID
and section hash (§6.9). If any requirement is new, changed or removed,
the change is an event: one per design per push, coalesced over five
minutes so a run of small merges plans once. `fires.per: event` files
one planner task for it, binding the event's range and paths to the
template's inputs, as bwsalmon/grain's in-repo planner trigger
(`.grain/triggers/design-docs.yml`) fills `{{group}}` and `{{from_sha}}`
today. `fixtures/design-plan.inputs.json` is one such event.

The task lands at
`tasks/fired/plan-designs/<design name>/<to_sha, 12 hex>.task.md` in the
hub intake, `proposed`: a planner's output is reviewed before anything it
proposes is filed. `max_tasks_per_change` caps what one planner run may
file; the design's own `planning:` block may lower it, never raise it.
