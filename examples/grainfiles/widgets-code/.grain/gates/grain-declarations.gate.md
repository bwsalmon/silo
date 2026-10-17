---
kind: gate
schema: grain/gate@v1
aka: ["repo:trigger:bwsalmon:silo:grain-declarations"]
title: Repository declarations
match:
  paths: [".grain/**"]
requires:
  security_review: {}
  human:
    when: always
    from: ["team:hub#approver"]
sensitive: true
---

A merge gate protecting the repo's grain folder, converted from
bwsalmon/silo `.grain/triggers/grain-declarations.yml` (docs/grainfiles.md
§6.7, §9.3). `.grain/` declares what CI and tasks may reach -- a
workflow's or template's `capabilities:` grant repository access -- so
any change to `.grain/**` is security-reviewed and then waits for a hub
approver's Submit. That includes the change that promotes a task into
`.grain/tasks/`, and any change to this file.

The mapping from the YAML: `on: pull_request` is what every gate holds,
a change on its way to a branch, so it has no key here. `paths` became
`match.paths`; `gate: true` became the kind itself; `human: required`
became `requires.human.when: always`, with the approvers named rather
than left to whoever the forge lets approve; and `sensitive: true` stays,
withdrawing auto-merge and requiring the security review, which
`requires.security_review` also says out loud.

A gate files no work except the reviews it requires. It is read from
`main`'s tip, never from the change it holds, so a change cannot remove
the gate that is holding it.
