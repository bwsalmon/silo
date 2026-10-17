---
kind: repo
schema: grain/repo@v1
role: shared
title: Org-wide capability definitions
owner:
  team: platform
  silo: https://silo.example.com
host: silo
default_ref: main
ref_policy:
  protected: [main]
  fast_forward_only: true
  review_prefix: review/
writers: ["team:platform#maintainer"]
---

# Org-wide capability definitions

Capability objects (`kind: capability`) the platform team maintains for
the whole org, released as tags (`v1`). Each one is a base capability
narrowed by reducers, and each reducer only narrows
(docs/task-repos.md §5.7).

**Nothing here grants anything.** A team narrows these further in its own
shared repo; a Grain that includes a team's folder can offer what it
holds; and a Grain honours a definition from here only if its own local
policy lists this repo as a source, for that base. A task gets a
capability only by requesting it in `context:` with `mode: use`.

- [`gcp/acme-widgets.md`](gcp/acme-widgets.md): the GCP service-account
  base, restricted to the `acme-widgets` project.
