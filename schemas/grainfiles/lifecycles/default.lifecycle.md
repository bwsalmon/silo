---
kind: lifecycle
schema: grain/lifecycle@v1
title: grain/default, grain's task states (docs/grainfiles-spec.md §12.9)
initial: proposed
states:
  proposed: {category: proposed}
  queued: {category: ready}
  deferred: {category: waiting}
  active: {category: active}
  failed: {category: waiting}
  awaiting_submit: {category: waiting}
  awaiting_ack: {category: waiting}
  completed: {category: done}
  rejected: {category: cancelled}
  closed: {category: cancelled}
transitions:
  - {from: proposed, to: [queued, rejected, deferred, closed]}
  - {from: queued, to: [active, deferred, closed]}
  - {from: deferred, to: [queued, proposed, closed]}
  - {from: active, to: [queued, failed, awaiting_submit, awaiting_ack, completed, closed]}
  - {from: failed, to: [queued, closed]}
  - {from: awaiting_submit, to: [completed, active, closed]}
  - {from: awaiting_ack, to: [completed, active, closed]}
  - {from: completed, to: [queued]}
  - {from: closed, to: [queued]}
aliases: {approved: queued, cancelled: closed}
on_outcome:
  accepted: active
  done: completed
  nothing_needed: completed
  failed: queued
  failure_streak: failed
  interrupted: queued
  cancelled: closed
stalled_after: 72h
---
The built-in default lifecycle. Readers behave as if `grain/default`
resolved to this file. Run statuses (running, awaiting_reply, failed,
interrupted) are on run records, never here.
