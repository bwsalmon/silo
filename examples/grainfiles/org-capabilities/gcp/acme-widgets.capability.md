---
kind: capability
schema: grain/capability@v1
title: GCP, project acme-widgets
base: {type: gcp-service-account@v1}
reducers:
  - language: builtin
    id: project
    type: subset
    dimension: projects
    allow: [acme-widgets]
owners: ["team:platform#maintainer"]
created_at: "2026-10-16T10:00:00Z"
---

The org's one definition of "GCP access to the widgets project". It
takes the `gcp-service-account@v1` base, whatever accounts and projects
the Grain implementing it can reach, and keeps only project
`acme-widgets`. It narrows nothing else, so on its own it is as broad as
every role the Grain's accounts hold in that project.

Teams are expected to narrow it further, by role and token lifetime,
rather than request it directly: `hub/shared`'s
`capabilities/widgets-deployer.capability.md` does (docs/grainfiles.md §6.8, §10.13).
Released as tag `v1`; teams pin it.
