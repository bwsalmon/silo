# Root of Grain "eu"

This repo is the root of Grain `eu`, owned by bsalmon@google.com
(docs/grainfiles.md §4.2). Silo hosts it, because the Grain runs behind
NAT, but **only the Grain and its owner write it** (`GRAINOWNERS`).

- [`grain.config.md`](grain.config.md) says what `eu` acts on: two backlog
  lanes (Silo's placements for `eu`, and its own `backlog/`), two trigger
  roots (its own `triggers/` and the hub team's) and two memory roots
  (its own `memory/` and the team's). Nothing else is active.
- `backlog/` is the owner's own lane: `*.ref.md` files ranking tasks in
  `tasks/`.
- `triggers/` holds `eu`'s own trigger, and `templates/` the template it
  fires, reached only from that trigger.
- `memory/` is written by `eu`'s memory agent, and given to its runs ahead
  of the team's shared memory.
- `capabilities/bases/` holds `eu`'s declarations of the base
  capabilities it implements, written from its provider code (§6.8).
  They are published for readers, and never a root.
- `runs/` holds one execution record (`<utc>.run.md`) per task this Grain
  took on, with its thread in `comments/`.

To unlink from Silo, the owner removes Silo's entries from
`grain.config.md`. This repo, and all of its history, stays the Grain's.
