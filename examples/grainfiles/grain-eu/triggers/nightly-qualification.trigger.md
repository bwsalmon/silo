---
title: Nightly release qualification
"on":
  schedule: {cron: "11 2 * * *", timezone: Europe/Dublin}
fires:
  template: ../templates/release-qualification/release-qualification.template.md
  inputs: {branch: candidate}
settings:
  team: release
---
Qualify the release candidate every night at 02:11 Dublin time.
