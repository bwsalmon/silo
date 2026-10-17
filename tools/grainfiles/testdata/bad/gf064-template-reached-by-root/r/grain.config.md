---
kind: config
schema: grain/config@v1
role: deployment
deployment: {id: eu}
owner: {user: a@example.com}
backlog:
  lanes:
    - {name: own, path: backlog/}
triggers:
  - path: triggers/
---
