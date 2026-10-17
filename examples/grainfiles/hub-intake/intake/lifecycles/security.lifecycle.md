---
kind: lifecycle
schema: grain/lifecycle@v1
title: Security-reviewed work
initial: proposed
states:
  proposed: {category: proposed}
  triage: {category: proposed}
  sec-approved: {category: ready}
  in-progress: {category: active}
  blocked: {category: waiting}
  shipped: {category: done}
  dropped: {category: cancelled}
transitions:
  - {from: proposed, to: [triage, dropped]}
  - {from: triage, to: [sec-approved, dropped]}
  - {from: sec-approved, to: [in-progress, dropped]}
  - {from: in-progress, to: [blocked, shipped, sec-approved]}
  - {from: blocked, to: [in-progress, dropped]}
aliases: {approved: sec-approved}
on_outcome:
  accepted: in-progress
  done: shipped
  failed: sec-approved
stalled_after: 72h
---
Every task needs the security team's approval before it is dispatchable.
