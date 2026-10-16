---
kind: trigger
schema: grain/trigger@v1
"on": pull_request
paths: [".grain/**"]
gate: true
human: required
sensitive: true
---

Path trigger protecting the repo's grain folder, converted from
bwsalmon/silo `.grain/triggers/grain-declarations.yml` with its keys
verbatim (docs/task-repos.md §12.1). `.grain/` declares what CI and
tasks may reach -- a workflow's or template's `capabilities:` grant
repository access -- so any change to `.grain/**` is security-reviewed
and then waits for a human's Submit. That includes the change that
promotes a task into `.grain/tasks/`.
