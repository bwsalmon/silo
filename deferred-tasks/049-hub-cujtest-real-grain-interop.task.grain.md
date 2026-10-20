---
grainfile: v1
title: "[HOLD for grain refactor] hub/cujtest, interop: run the 12 Grain Hub CUJs against real bwsalmon/grain instances"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/silo
draft: true
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 49
x_supersedes_task_id: 46
x_state_at_dump: deferred
x_target_repo: bwsalmon/silo
x_deferred_at: "2026-10-18T18:00:54Z"
x_deferred_until: null
x_capabilities:
  - grain-ci
---
**HOLD UNTIL `bwsalmon/grain` REFACTORING LANDS:** Once `bwsalmon/grain` has been updated to speak the new Grain Hub Grainfile subscription, notification, and same-repo content proxy protocol, replace the simulated/fake Grain driver in the 12 CUJ suite (`hub/cujtest`) and `scripts/grain-interop.sh` (`.grain/workflows/grain-interop.yml`) with real `bwsalmon/grain` daemon instances:

### 1. Plug real `bwsalmon/grain` instances into `hub/cujtest` (`docs/cujs.md` CUJ-01 through CUJ-12)
- Implement the `GrainNodeDriver` in `hub/cujtest` backed by real `cmd/grain` daemon instances built from `bwsalmon/grain` (running against local git repos and the GitHub simulator).
- Run all 12 Critical User Journeys (`CUJ-01` through `CUJ-12`) end-to-end with real Grain daemons subscribing to `recipient` URLs, receiving `grainfile_event` notifications over `rendezvous`, fetching `.task.grain.md` / `.process.grain.md` files and authorized same-repo `references` via `/v1/grainfiles/content`, executing tasks/processes, reporting `depends_on` completions, and serving tunneled local UIs (`hub/nodeui`).

### 2. Update `scripts/grain-interop.sh` and `hub/conformance`
- Update `scripts/grain-interop.sh` (`.grain/workflows/grain-interop.yml`) and `silo conformance` (`hub/conformance`) to verify the Grainfile subscription, trigger, and same-repo content proxy flows against real `bwsalmon/grain`.
- Record the real-Grain CUJ test run report under `docs/test-runs/`.
