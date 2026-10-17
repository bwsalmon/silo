# Org-wide capability definitions

Capability objects (`kind: capability`) the platform team maintains for
the whole org, released as tags (`v1`). Each one is a base capability
narrowed by reducers, and each reducer only narrows
(docs/grainfiles.md §6.8).

**Nothing here grants anything.** A team narrows these further in its own
shared repo; a task reaches a definition only by reference; and a Grain
honours a definition from here only if its own local policy lists this
repo as a source, for that base. A task gets a
capability only by requesting it in `context:` with `mode: use`.

- [`gcp/acme-widgets.capability.md`](gcp/acme-widgets.capability.md): the GCP service-account
  base, restricted to the `acme-widgets` project.
