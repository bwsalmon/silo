---
kind: task
schema: grain/task@v1
title: Deploy a preview of widget schema v2 to Cloud Run
team: hub
context:
  - ref: "https://git.example.com/hub/widgets.git@main"
    mode: write
  - ref: "shared:capabilities/widgets-deployer.md"
    mode: use
    reducers:
      - language: builtin
        type: limit
        dimension: token_lifetime
        max: 30m
      - language: builtin
        type: not_after
        at: "2026-10-24T00:00:00Z"
state: approved
submitter: "user:bsalmon@google.com"
submitted_via: ui
approved_by: "user:approver@example.com"
approved_at: "2026-10-17T09:40:00Z"
created_at: "2026-10-17T09:31:00Z"
depends_on:
  - "widgets:.grain/tasks/2026-10-16-widget-schema-v2/task.md"
---

An illustrative task. Build the widgets service at `main` and deploy it
to Cloud Run in `acme-widgets` as the service `preview-schema-v2`, in
`europe-west1`, then post the preview URL as the run's summary.

`context:` gives the run two things (docs/task-repos.md §5.6, §5.7):

1. the widgets repo, to push the run's branch to if the deploy needs a
   change;
2. a **request** for the hub team's `widgets-deployer` capability: the
   org's GCP definition for `acme-widgets`, narrowed by the team to two
   roles and 1-hour tokens and a per-use Anselm check, and narrowed again
   here to 30-minute tokens and a week's validity. This entry is
   `mode: use`: nothing of the definition file lands in the sandbox, only
   what the capability materialises.

The approver read both entries when approving, and `spec_digest` covers
them. The capability needs no approval of its own. `eu` offers it,
because `eu:capabilities/hub.md` includes the team's capabilities, but
offering is not granting: `eu`'s local policy decides, and §8.17 walks
the check.
