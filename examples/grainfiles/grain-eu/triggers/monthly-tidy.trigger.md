---
kind: trigger
schema: grain/trigger@v1
title: Tidy memory every month
"on":
  schedule: {monthly: {day: 1, at: "09:00"}, timezone: UTC, overlap: skip}
fires:
  template: ../templates/tidy.template.md
fires_in: deployment
---
