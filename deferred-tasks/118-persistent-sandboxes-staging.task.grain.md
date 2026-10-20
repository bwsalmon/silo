---
grainfile: v1
title: "[HOLD for grain refactor] pkg/orchestrator, pkg/tunnel, pkg/ui: support persistent sandboxes with auto-restart and host-port / UI sub-path routing"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2026-10-25T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 118
x_state_at_dump: "deferred, closed"
x_closed_at: "2026-10-19"
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-18T23:32:30Z"
x_deferred_until: "2026-10-25T00:00:00Z"
x_capabilities:
  - grain-ci
---
**HOLD UNTIL `bwsalmon/grain` REFACTORING LANDS.**

Implement **Persistent Sandboxes / Staging Environments** in `bwsalmon/grain` (`pkg/orchestrator`, `pkg/staterepo`, `pkg/model`, `pkg/tunnel`, `pkg/ui`, `ui/`) so a Grain deployment can run standing staging environments and long-lived services inside managed sandboxes:

### 1. Persistent Sandbox Specification & State (`pkg/model`, `pkg/staterepo`, `pkg/repodecl`)
- Allow defining persistent sandboxes in the deployment state (`tables/persistent_sandboxes.json`) and/or repository declarations:
  - **Repository & ref:** Target repository (`owner/name`) and branch/ref (cloned into the sandbox workspace on startup, with optional auto-update/fetch on restart).
  - **Predefined startup command (`command`):** The shell command executed inside the sandbox workspace once the repository is checked out.
  - **Resources & capabilities:** VM sizing (`cpus`, `memory_mb`, `disk_gb`), network access, and attached capabilities/secrets.
  - **Exposure mode:**
    - **Host port binding (`host_port` -> `sandbox_port`):** Bind a port on the Grain host and forward TCP/HTTP traffic through Grain to the sandbox's listening port, OR
    - **Grain UI sub-path (`ui_subpath`, e.g. `/staging/<name>` -> `sandbox_port`):** Expose an HTTP service inside the sandbox under a sub-path of the Grain UI.

### 2. Supervised Lifecycle & Automatic Restart (`pkg/orchestrator`, `pkg/daemon`)
- **Start on deployment startup:** When the Grain daemon starts (including after a deployment restart or upgrade), reconcile all enabled persistent sandboxes and boot/start them automatically.
- **Restart on command or VM exit:** Supervise the predefined command and sandbox VM continuously:
  - If the predefined command exits (whether cleanly or with a non-zero exit code) or the underlying sandbox VM terminates unexpectedly, automatically restart it (with bounded exponential backoff on rapid crash loops, resetting backoff after sustained uptime) and record exit events and live stdout/stderr ring-buffer logs.
  - Ensure persistent sandboxes do not consume the task-runner concurrency slots (`max_concurrent_runs`) used for normal task attempts, or account for their VM resources explicitly in `read_deployment_health`.

### 3. Host-Port Forwarding & Grain UI Sub-Path HTTP Reverse Proxy (`pkg/tunnel`, `pkg/ui`)
- **Host port binding (via Grain):**
  - Open a listener on the configured host port (validating no collision with Grain's own ports or other persistent sandboxes) and proxy connections into the sandbox's target port across restarts.
- **Grain UI sub-path reverse proxy (`/staging/<name>/...`):**
  - Mount an HTTP reverse proxy (supporting HTTP/1.1, SSE streaming, and WebSocket `101` upgrades) under the Grain UI mux (e.g. `/staging/<name>/...`), stripping/rewriting the prefix as configured.
  - Because it is served under `pkg/ui`, the staging environment is automatically reachable locally, through `grain-relay`, and through the Grain Hub's `hub/nodeui` reverse tunnel (`https://<node>.<node-ui-domain>/staging/<name>/...`), inheriting the deployment's authentication and role-based access control.

### 4. Grain UI Management View (`pkg/ui`, `ui/`)
- Add a **Persistent Sandboxes / Staging** view (or section in Sandboxes) in the Grain UI showing each persistent sandbox's state (`starting`, `running`, `restarting`, `stopped`), repository/branch, command, uptime, restart count, bound host port or clickable `/staging/<name>/` UI link, live stdout/stderr log tail, and controls to **Restart**, **Stop/Start**, or **Rebuild Sandbox** (fresh VM + fresh clone).

### Verification
- Add unit and integration tests covering persistent sandbox creation, automatic restart when the predefined command exits, automatic startup when the Grain daemon restarts, host-port forwarding, and HTTP/WebSocket proxying under a Grain UI sub-path.
