---
kind: capability
schema: grain/capability@v1
title: Deploy widgets previews to Cloud Run (acme-widgets), 1-hour tokens
base:
  capability: "org-caps:gcp/acme-widgets.capability.md@v1"
reducers:
  - language: builtin
    id: roles
    type: subset
    dimension: roles
    allow: [roles/run.developer, roles/iam.serviceAccountUser]
  - language: builtin
    id: lifetime
    type: limit
    dimension: token_lifetime
    max: 1h
  - language: anselm
    id: preview-services-only
    "on": use
    version: "0"
    note: >-
      A placeholder: Anselm's syntax is not settled, so this body is
      opaque and illustrative. Intended meaning, in plain words: allow a
      Cloud Run call only on services whose name starts with "preview-",
      in region europe-west1.
    body: |
      <opaque Anselm source: not interpreted until Anselm is adopted>
owners: ["team:hub#maintainer"]
created_at: "2026-10-16T11:30:00Z"
---

The hub team's narrowing of the org's `gcp/acme-widgets.capability.md`, pinned at
its `v1` tag, for agents that deploy preview builds of the widgets
service. Each layer only tightens (docs/task-repos.md §5.7):

1. the org's base keeps project `acme-widgets` only;
2. **roles**: keep only `roles/run.developer` and
   `roles/iam.serviceAccountUser`, enforced on the credential: the Grain
   mints tokens only for an account whose roles there are within these;
3. **lifetime**: tokens live at most an hour, on the credential;
4. **preview-services-only**: a per-use check at the Grain's Google API
   proxy, written in Anselm. Until a Grain can evaluate Anselm, it
   refuses any request for this capability, naming this reducer. It never
   skips it, because skipping a reducer would widen the capability.

A task reaches this definition only by reference, from a `context:`
entry with `mode: use`. Nothing makes it active, and it grants nothing
by itself (docs/task-repos.md §5.7).
