# Root of Grain "us"

The root of Grain `us`, a team Grain owned by the hub team's admins. Only
the Grain and its owners write here (`GRAINOWNERS`, docs/task-repos.md §3.5).

- [`grain.config.md`](grain.config.md) names Silo's placements for `us`
  and its own `backlog/` as lanes, strict, Silo's first; its own
  `triggers/` and Silo's assigned triggers as trigger roots; and its own
  `memory/`, which the memory agent creates when it first writes.
- The example shows a task re-placed here from Grain `eu` (§8.8): Silo
  moved its placement between the two Grains' folders in
  `hub/placements`, and this Grain's execution record names the fenced
  record in `eu` as `previous`. The task depends on a task that Grain
  `eu` completed, which this Grain read from `eu`'s execution record
  (§8.9).
- Silo assigned `us` the team's daily granule pull. The trigger entry for
  Silo's folder ignores it, and `triggers/granule-pull-weekly.trigger.md`
  is this Grain's own trigger in its place, referencing the same template
  and the original in `derived_from` (§6.2, §8.15).
