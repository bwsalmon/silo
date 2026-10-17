# Hub team placements

Silo's own repo, and Silo is its only writer (docs/grainfiles.md §8.3).
It holds one folder per Grain that serves the hub team:

- `grains/<grain>/backlog/` is that Grain's **placements**: one
  `*.ref.md` per task Silo placed there, with the rank, `spec_rev` and
  the attestation. Placement adds one, and close, cancel and
  re-placement remove it. Re-placement is one commit here: the reference
  leaves one Grain's folder and arrives in another's (§10.7).
- `grains/<grain>/triggers/` holds the team triggers assigned to that
  Grain, as `*.trigger.md` files Silo writes.

**Nothing here is active by itself.** A Grain acts on its folder only
because its owner named it in the Grain's `grain.config.md`: as a backlog
lane, and, to take assigned triggers, as a trigger root. Linking a Grain
to Silo is adding those entries, and unlinking is removing them (§10.9).
People who reorder a Grain's placements do it through Silo's UI or API,
and Silo commits with `ranked_by` set to them.

Read access is per repo, so the team's Grains can read each other's
placements here. A deployment that wants Grains isolated from each other
gives each Grain's folder a repo of its own; nothing else changes,
because a root entry names any folder (§12.4).
