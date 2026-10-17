---
kind: gate
schema: grain/gate@v1
title: Changes to .grain/ need a human
match: {paths: [".grain/**"]}
requires:
  human: {when: always}
sensitive: true
---
