---
kind: config
schema: grain/config@v1
role: registry
registry: {hub: "https://silo.example.com", poll_interval: 5m}
owner: {team: platform}
folders:
  - {name: hub-intake, at: "https://github.com/acme/hub.git//intake/@main", team: hub, role: library}
  - {name: widgets, at: "https://github.com/acme/widgets.git//.grain/@main", team: hub, role: library}
  - {name: eu, at: "https://github.com/acme/grain-eu.git//@main", role: deployment}
  - {name: us, at: "https://github.com/acme/grain-us.git//@main", team: hub, role: deployment}
views:
  - {name: hub-definitions, folders: [hub-intake, widgets], kinds: [task]}
  - {name: hub-open, folders: [eu, us], kinds: [exec], categories: [proposed, ready, active, waiting]}
---
Silo's registry. Registering a folder is a reviewed commit here.
