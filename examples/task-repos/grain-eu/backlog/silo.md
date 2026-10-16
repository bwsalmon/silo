---
kind: include
schema: grain/include@v1
target: "placements:grains/eu/backlog/@main"
ref_mode: track
kinds: [ref]
added_by: "user:bsalmon@google.com"
added_at: "2026-10-16T09:00:00Z"
---

The link to Silo (docs/task-repos.md §8.12). Silo's placements for this
Grain, read live from `main`. `kinds: [ref]` is the trust grant: whatever
else that folder ever holds, this include brings in backlog references
and nothing else, and each one is still held to the Grain's local policy
and its attestation (§11.7). Deleting this file unlinks the Grain.
