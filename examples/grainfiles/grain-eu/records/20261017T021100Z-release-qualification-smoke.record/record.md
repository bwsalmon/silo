---
title: Smoke-test candidate on 2 platforms
definition: "git::https://github.com/acme/grain-eu.git//templates/release-qualification/smoke.task.md?ref=8d2e4f6a0b1c3d5e7f9a1b2c3d4e5f6a7b8c9d0e"
inputs: {branch: candidate, platforms: [linux-amd64, linux-arm64]}
trigger: "git::https://github.com/acme/grain-eu.git//triggers/nightly-qualification.trigger.md?ref=8d2e4f6a0b1c3d5e7f9a1b2c3d4e5f6a7b8c9d0e"
outcome: failed
deployment: "@acme-grain-eu"
started_at: "2026-10-17T02:11:00Z"
finished_at: "2026-10-17T02:30:45Z"
links:
  branch: grain/eu/qualify-candidate-20261017
---
The smoke suite failed on `linux-arm64`: the runner image lacks `qemu`.
`linux-amd64` passed.
