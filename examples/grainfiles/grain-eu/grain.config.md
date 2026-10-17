---
format: 1
title: Grain eu
owner: "@acme/grain-admins"
aliases:
  # The one place this folder pins commons. Bumping it is this one line
  # (and its hash).
  commons:
    source: "git::https://github.com/acme/commons.git?ref=v1.4.0"
    sha256: "h1:OMr9S/HWXCshF7edOUGlFvIn9APPVCoTXk1XoC/4sVs="
check:
  pinning: required
deployment:
  account: "@acme-grain-eu"
  offers:
    - "commons:capabilities/gcp-key.capability.md"
settings:
  hub: https://silo.example.com
  backlog: {lanes: [hub, own]}
  sandbox: {cpus: 4, memory_mb: 8192}
---
Deployment eu's folder: its templates and triggers, the capabilities it
offers, and the records of what it ran. Its backlog and live state are
in its own database, not here.
