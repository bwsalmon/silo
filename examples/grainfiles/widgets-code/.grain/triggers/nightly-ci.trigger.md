---
kind: trigger
schema: grain/trigger@v1
title: Nightly CI on main, reviewed
"on":
  schedule:
    daily: {at: "02:00"}
    overlap: cancel-previous
    catch_up: {mode: latest-only}
  manual: {}
fires:
  workflow: /.grain/workflows/ci.yml
  branch: main
  review:
    inputs: {quiet_on_success: true}
enabled: true
team: hub
fires_in: grain
created_at: "2026-10-16T18:05:00Z"
---

A trigger that runs a workflow rather than filing a templated task: the
nightly CI run on `main`, followed by a review task with the run's
result, as a workflow's `on.schedule` behaves in grain today (§6.6,
§9.4). The workflow stays YAML; this file is the cadence and what
happens after the run.

It says `fires_in: grain`, because only a Grain runs CI. The Grain that
fires it is any whose `grain.config.md` names this folder as a trigger
root. Each run is recorded
in that Grain's database; the review task is filed in its root at
`tasks/fired/widgets/nightly-ci/<fired_at>.task.md`, never in this repo.
`overlap: cancel-previous`: a run still going at 02:00 the next night is
stale, so it is cancelled and its review is filed with the cancellation.

`review.template` is unset, so the review uses grain's built-in workflow
review, which finishes cleanly without reporting when the run is green.
