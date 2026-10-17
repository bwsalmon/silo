---
kind: config
schema: grain/config@v1
role: deployment
deployment: {id: us, hub: "https://silo.example.com"}
owner: {team: hub}
backlog:
  lanes:
    - name: hub
      path: hub/
---
Deployment `us`: the hub team's Grain, with the widgets toolchain.
