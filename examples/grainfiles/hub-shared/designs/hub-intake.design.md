---
kind: design
schema: grain/design@v1
aka: ["https://github.com/bwsalmon/silo.git//docs/hub.md#6-task-intake-and-federation-built-hubtasks"]
title: Hub task intake, approval and signed claims
status: implementing
owners: ["user:bsalmon@google.com"]
applies_to:
  - "silo-code:/"
  - "grain-code:pkg/daemon/hub.go"
supersedes: []
requirements:
  prefix: HS
  next: 6
  retired: [HS-4]
planning:
  team: hub
  max_tasks_per_change: 10
  approval: required
---

# Hub task intake, approval and signed claims

This design is taken from bwsalmon/silo `docs/hub.md` (§1.2, §6.1 and
§6.4) and rewritten as a `kind: design` object. It shows how the hub's
existing design text carries requirement anchors. Its requirements are
`HS-n`, the hub series. Tasks cite them by
`shared:designs/hub-intake.design.md#HS-n`, and commits cite them as
`Design-Item: https://silo.example.com/git/hub/shared.git//designs/hub-intake.design.md#HS-n`.

## Background

The hub is the Policy Decision Point: it decides who may run what and
puts claims on each task it dispatches. Each Grain is a Policy
Enforcement Point. It takes those claims on trust and holds every task to
a local policy that only its owner can edit (hub.md §1.2).

![Silo signs a placement's attestation; the Grain verifies it against
silo_issuer before it accepts the run.](img/claims-flow.png)

grain's side of the claims is in
[`pkg/daemon/`](grain-code:pkg/daemon/).

## Requirements

### Intake records its submitter from sign-on {#HS-1}

`POST /v1/tasks` creates a task targeting a repository or a team. The
caller must be in the team that owns the repository, or in the team
itself. The **submitter** is the principal from the caller's sign-on,
never from the request body. A repository that no team owns is refused.

### Offers carry the approver {#HS-2}

`tasks.Spec` carries `approved_by`: the human who approved the task at
the hub, never its submitter. It is stored with the task, offered to
nodes with the rest of the spec, and signed as the attestation's
`approved_by` claim. A spec whose approver is its own submitter is
refused, and the submit API does not accept the field. So nobody can
approve their own task.

### Offers carry signed claims {#HS-3}

With a signing key, the hub signs each offer's claims for the node it
offers the task to: `iss`, `aud`, `iat`, `exp`, `requester`, `team`,
`approved_by`, `granted_capabilities` and `spec_digest`. They travel as
`requester_attestation`, and the keys are published at
`/v1/claims/keys`, past IAP. A Grain whose policy names `silo_issuer`
verifies the signature and refuses an offer without one.

### Lost running tasks are requeued after a team's grace {#HS-5}

A running task whose node goes away is `lost`. If its team turns on
requeueing, and the node is away longer than the team's grace, the task
goes back to the queue. The hub then refuses the old node's events and
result for it.

## Non-goals

A Grain does not verify who requested or approved a task beyond the
hub's signature. Trusted claims bounded by local policy remain the
design.

## Retired

`HS-4` ("a submission may name its approver") was dropped before it was
built, because it let a submitter choose their approver. HS-2 replaced
it. The ID stays taken.
