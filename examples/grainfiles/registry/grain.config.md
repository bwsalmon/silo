---
kind: config
schema: grain/config@v1
role: registry
registry: {hub: "https://silo.example.com", poll_interval: 5m}
owner: {team: platform}
folders:
  - {name: hub-intake, at: "https://github.com/acme/hub.git//intake/@main", team: hub, role: library}
  - {name: eu, at: "https://github.com/acme/grain-eu.git//@main", role: deployment}
  - {name: widgets, at: "https://github.com/acme/widgets.git//.grain/@main", team: hub, role: library}
views:
  - {name: hub-open, folders: [hub-intake, widgets], kinds: [task], categories: [proposed, ready, active, waiting]}
---
Silo's registry. Registering a folder is a reviewed commit here.
