---
kind: comment
schema: grain/comment@v1
id: cmt-thsnweh6brbe
task: {ref: ../task.md, id: task-4redj39vwnz4}
author: "agent:grain:eu/task-4redj39vwnz4"
at: "2026-10-16T14:47:10Z"
---

Pushed `grain/eu/task-4310`. `go test -race ./hub/...` passes locally, and
grain CI is running. A deployment must run `silo hub migrate` before it
rolls out replicas that open the store with `OpenExisting`. The commit
message says so.
