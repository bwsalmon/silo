---
kind: lifecycle
schema: grain/lifecycle@v1
title: L
initial: new
states:
  new: {category: proposed}
  go: {category: ready}
  ok: {category: done}
  no: {category: cancelled}
transitions:
  - {from: new, to: [go, no]}
  - {from: go, to: [ok]}
aliases: {go: ok}
---
