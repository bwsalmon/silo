# Hub team shared definitions

This repo holds templates, triggers, memory, capability definitions and
design docs for the hub team. Settings are in `.grainconfig`, and who may
write what in `GRAINOWNERS`.

**What is active, and what is only referenced** (docs/task-repos.md §3.1):

- `triggers/` is a **trigger root** for any Grain whose `grain.config.md`
  names it: two scheduled triggers, and `plan-designs.trigger.md`, the
  planner as a `design-change` trigger. A `fires_in: silo` trigger fires
  in Silo and lands in the hub intake; a `fires_in: grain` one fires on
  each Grain that names this folder. `triggers/archive/` holds a retired
  trigger, which no root lists (§3.8).
- `memory/` is a **memory root** for Grains that name it, read-only:
  shared guidance, given to runs after each Grain's own memory.
- `templates/` is **only referenced**: triggers and tasks name a template,
  and nothing makes a template active. `templates/partials/` holds
  include-only pieces, and `templates/ghcr/` the GHCR ones.
  `templates/prune-packages.template.moved.md` is the tombstone the move to
  `ghcr/` left.
- `designs/` holds design docs (`*.design.md`) with `{#HS-n}` requirement
  anchors, referenced by tasks' `implements:` and watched by
  `triggers/plan-designs.trigger.md`.
- `capabilities/` holds the team's capability objects
  (`*.capability.md`), **only referenced** from tasks' `context:`.
  `widgets-deployer.capability.md` narrows the org's
  `org-caps:gcp/acme-widgets.capability.md`, pinned at `v1`, by role and
  token lifetime, and with a per-use Anselm reducer. A Grain honours it
  only if its local policy sanctions this folder as a source (§5.7).
