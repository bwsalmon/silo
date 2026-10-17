---
kind: config
schema: grain/config@v1
role: deployment
deployment: {id: eu, hub: "https://silo.example.com"}
owner: {user: bsalmon@google.com}
backlog:
  mode: weighted
  lanes:
    - name: hub
      path: hub/
      weight: 3
    - name: own
      path: backlog/
      weight: 1
triggers:
  - path: triggers/
memory:
  - path: memory/
    write: true
---
Deployment `eu`: bsalmon@google.com's own Grain. The hub creates
executions in `hub/`; `backlog/` is the owner's own lane.
