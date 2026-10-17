---
title: Widget schema v2
status: approved
owners: ["@carol"]
requirements:
  - {id: WS-1, title: Versioned schema, hash: "sha256:d6aaccc368fba3258a50a0f93e732a9a8df891da1870d9b0b86a7cadb1291755"}
  - {id: WS-2, title: Schema published to the registry, hash: "sha256:fe3b294753797aaeaacf317479f83babf0578c69b721f0d85f15cf0ac4e42fb2"}
---
# Widget schema v2

## WS-1 Versioned schema

Every widget carries the schema version it was written with, as an
integer, starting at 2.

## WS-2 Schema published to the registry

The schema is published to the sandbox project's artifact registry on
every release, so other services can validate widgets.
