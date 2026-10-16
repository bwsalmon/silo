# Running the hub's store on Cloud Spanner: an evaluation

Task 4319, 2026-10-16. The question was whether the hub can move from
Cloud SQL for PostgreSQL (task 4318, `deploy/terraform/hub`) to Cloud
Spanner's **PostgreSQL dialect**, reached through **PGAdapter**, with the
hub's existing pgx / `database/sql` code. Everything below was measured
against the Spanner emulator and PGAdapter 0.56.1, in a grain sandbox and
in grain CI. No real Spanner instance and no GCP credential were used.

**Recommendation: stay on Cloud SQL.** Revisit Spanner only if the hub
needs more than one region, more than 99.95% availability, or write
throughput that one Cloud SQL primary cannot take. See the last section.

## How it was tested

`gcr.io/cloud-spanner-pg-adapter/pgadapter-emulator` runs the emulator
and PGAdapter in one container. Started with `-r autoConfigEmulator=true`
(the image's default), PGAdapter creates the instance and any database a
client connects to. The hub's pgx driver connects to it as it would to
PostgreSQL:

    docker run -d -p 127.0.0.1:5433:5432 -p 127.0.0.1:9020:9020 \
      gcr.io/cloud-spanner-pg-adapter/pgadapter-emulator
    SILO_TEST_SPANNER=1 go test ./hub/directory ./hub/tasks ./hub/registry ./hub/lease

`SILO_TEST_SPANNER` makes `internal/pgtest.Start` hand each test a fresh
database on the emulator instead of starting PostgreSQL
(`internal/pgtest/spanner.go`). Each database is dropped when its test
ends, through the emulator's REST port 9020: an emulator instance holds
at most 100 databases, and the suite makes more than that.

This works in a grain CI runner. Docker is there, and both images pull
from gcr.io without credentials. The new `spanner` job in
`.grain/workflows/hub.yml` runs those four packages on every push. In
CI it took **1m03s**, including the container's start. Locally the tests
take about 16s, against 20–40s on PostgreSQL. The emulator is a
single-process model of Spanner. It does not show production latency,
throughput or TrueTime behaviour. It does reproduce Spanner's DDL
limits, its type and function surface, and its aborts of conflicting
transactions.

## Compatibility

What the hub uses, and what Spanner (PostgreSQL dialect, through
PGAdapter) made of it.

| Feature | Where the hub uses it | On Spanner | Status |
|---|---|---|---|
| `CREATE TABLE IF NOT EXISTS`, `TEXT`, `BIGINT`, `INTEGER`, composite primary keys | every store | works | ok |
| `REFERENCES ... ON DELETE CASCADE`, `CHECK (...)` | directory | works | ok |
| `CREATE INDEX IF NOT EXISTS`, `CREATE UNIQUE INDEX` | directory, tasks, live | works | ok |
| `ALTER TABLE ... ADD COLUMN ... NOT NULL DEFAULT` | directory, tasks (`dbaccess.AddColumns`) | works | ok |
| Inline column `UNIQUE` (`node_tokens.hash`) | directory | **rejected**: "`<UNIQUE>` constraint is not supported, create a unique index instead" | **fixed**: now a `CREATE UNIQUE INDEX`, on all three databases |
| Partial unique index `... (node) WHERE revoked_at IS NULL` (one live token per node) | directory | **rejected**: only `WHERE col IS NOT NULL` (a null-filtered index) is allowed | **worked around**: skipped on Spanner (`dbaccess.Spanner`). Its serializable transactions already keep two racing mints apart (see below). |
| `ON CONFLICT (...) DO UPDATE SET x = excluded.x` and `DO NOTHING` | directory, lease, registry, live | works | ok |
| `ON CONFLICT ... DO UPDATE SET epoch = hub_leases.epoch + 1 ... RETURNING` | lease (new) | works | ok |
| `$n` placeholders (`s.q` rewrites `?`), parameter types inferred by pgx | every store | works through PGAdapter's extended protocol | ok |
| `LIMIT`, `LIMIT ... OFFSET`, subqueries, `COALESCE`, `LIKE`, `\|\|` | tasks, live, directory | works | ok |
| `UPDATE` of a primary-key column (`team_members.principal`, the legacy-principal migration) | directory | **rejected**: "Cannot modify a primary key column with UPDATE" | Not needed: a Spanner hub starts empty and has no legacy rows. The test is skipped there. |
| `DROP TABLE` on a table that has indexes | tests only (old-schema fixtures) | **rejected** until its indexes are dropped | Those tests build the schema of old PostgreSQL hubs, so they are skipped on Spanner. |
| Transactions that conflict | directory (`tx`), lease (`Take`), any read-then-write | **aborted** with SQLSTATE 40001: in a probe, 13 of 20 concurrent `Take`s were aborted | **fixed**: `dbaccess.InTx` retries 40001/40P01 with backoff. This is portable; PostgreSQL raises the same codes. |
| Session advisory locks (`pg_try_advisory_lock`, `pg_advisory_unlock`) | lease: the dispatcher's lease, hub.md §2.2.1 | **missing**: "function pg_try_advisory_lock does not exist" | **gap**: needs a new lease (below) |
| `LISTEN` / `pg_notify` / `NOTIFY` | notify: the cross-pod bus, hub.md §2.2.4 | **missing**: "Unknown statement: LISTEN", "pg_notify is not supported" | **gap**: needs a new bus (below) |
| `CREATE ROLE`, `GRANT`, `ALTER DEFAULT PRIVILEGES`, `pg_roles`, `has_*_privilege` | dbaccess: the migrate role and the least-privileged runtime role (SA1-L8) | **missing**: "Statement is not supported" | **gap**: replace with IAM plus fine-grained access control (below) |
| `pg_dump` / `pg_restore` | the backup and restore test, the runbook | **missing** | **gap**: Spanner has its own backups and export (below) |
| `pg_database_size`, `pg_terminate_backend`, `pg_locks` | faulttest and lease tests only | missing | test-only |
| Isolation | every store | Spanner is always serializable (externally consistent). PostgreSQL defaults to READ COMMITTED. | Stricter is safe, but it means aborts, which `InTx` now retries. |
| Commit timestamps / TrueTime | none | `spanner.commit_timestamp()` exists | Not needed: lease epochs are a counter, and the hub's times are its own clock. |

### Test results on the emulator

| Package | Result on Spanner |
|---|---|
| `hub/directory` | **pass**. Three migration tests that build an old PostgreSQL schema are skipped. |
| `hub/tasks` (store, API, dispatcher with in-memory leases) | **pass**, unchanged |
| `hub/registry` | **pass**, unchanged |
| `hub/lease` | `Take`, `Holder` and the new `TestConcurrentTakes` **pass**. The two advisory-lock tests are skipped. |
| `hub/notify` | skipped: no LISTEN/NOTIFY |
| `hub/server` | **fails**: every multi-pod or dispatcher test waits on the lease or the bus. `TestMigrateThenServeLeastPrivileged` fails at `CREATE ROLE`; the backup test needs `pg_dump`. |
| `hub/faulttest` | not run: it needs `hub/server` |
| `hub/live` | no database tests |

The first run also failed all of `hub/directory`, at its first DDL
statement (the inline `UNIQUE`).

### A bug on PostgreSQL that this found

The new `TestConcurrentTakes` races ten `lease.Take`s. On PostgreSQL it
failed before this change: "10 Takes got 6 distinct epochs and left
epoch 6". `Take` read the epoch, then wrote epoch+1, and under READ
COMMITTED two `Take`s could read the same epoch. In production only the
advisory lock's holder calls `Take`, so the race could not happen. But
the epoch was correct only because of the lock. `Take` is now a single
`INSERT ... ON CONFLICT DO UPDATE SET epoch = hub_leases.epoch + 1
RETURNING epoch`, which is atomic on all three databases.

## What it would take to finish

The cheap, portable changes are done (above). This is what is left
before a hub could run on Spanner, with rough sizes for one engineer who
knows the code:

1. **The dispatcher's lease, without advisory locks: 2–3 days.** Replace
   the session lock with a row lease in `hub_leases`:
   `holder`, `epoch` and `expires_at`. It is taken by a conditional
   upsert (`... WHERE expires_at < now OR holder = me`), renewed every
   few seconds, and given up when a renewal fails or runs past the TTL.
   This works on PostgreSQL too, and could replace the advisory lock
   everywhere, keeping one implementation. What changes is the failure
   model. Today PostgreSQL frees the lock when the holder's TCP session
   dies, within about 16s of keepalive. A row lease is freed at its TTL,
   and a holder that stalls longer than the TTL without noticing can
   overlap its successor. The hub already fences with epochs. The
   dispatcher would have to check its epoch inside each write it makes,
   which `tasks.Store`'s versioned updates partly do already. This needs
   the fault matrix rerun, and a review of hub.md §2.2.1's argument.
2. **The cross-pod bus, without LISTEN/NOTIFY: 3–5 days.** Two options:
   - A `hub_notices` table that every pod polls, say every 250ms, by
     sequence number, with old rows pruned. Simple and portable, but it
     adds up to the poll interval to cross-pod fan-out, and a read per
     pod per poll.
   - A Spanner change stream on that table, or on `task_events`. Change
     streams are read through the Spanner API (`READ_<stream>`), not
     through PGAdapter's wire protocol in a way pgx can LISTEN to, so
     this means a Spanner client library in the hub and a
     Spanner-specific bus.

   Either way the load test's fan-out p99, now 2–7ms, needs measuring
   again.
3. **Roles and the least-privileged runtime identity (SA1-L8): 1–2
   days, plus terraform.** Spanner has no PostgreSQL roles. The
   equivalent is IAM: the migrate job's service account gets
   `roles/spanner.databaseAdmin` (DDL), and the hub's gets
   `roles/spanner.databaseUser`. That role still allows DDL, so a hub
   that may not change the schema needs fine-grained access control
   (`CREATE ROLE` / `GRANT` in Spanner's own syntax, with
   `roles/spanner.fineGrainedAccessUser` and a database role the
   connection names). `dbaccess.Grant` and `LeastPrivileged` would need
   a Spanner branch, and the emulator does not model either, so it
   could only be tested on a real instance.
4. **DDL as one batch per migration: 1 day.** Every DDL statement on
   Spanner is a schema change, which takes seconds on a real instance
   and validates against existing data. The hub sends about 20 DDL
   statements one by one on every start (`CREATE ... IF NOT EXISTS`).
   On Spanner that start-up cost is real: start-up should first check
   whether the schema is current, and the migration job should submit
   the statements as one batch (PGAdapter's `START BATCH DDL`). On the
   emulator each statement is instant, so this did not show up in the
   tests.
5. **Backups and restore, in the runbook and the tests: 1 day.**
   `pg_dump` is gone. Use Spanner's backup schedules, or export to
   Cloud Storage through Dataflow, and PITR through the database's
   `version_retention_period` (up to 7 days).
   `hub/server/restore_test.go` would need a Spanner equivalent against
   a real instance.
6. **Terraform and deploy: 2–3 days.** A `google_spanner_instance` and
   `google_spanner_database` with `database_dialect = "POSTGRESQL"`.
   PGAdapter as a sidecar in the hub's pod, and in the migrate Job.
   Workload Identity bindings, docs/deploy.md and docs/hub-operations.md.
7. **Primary-key hot spots: 0.5 day to review.** Spanner splits tables
   by key range, so a monotonically increasing key concentrates writes
   on one split. The hub's IDs are random hex, which suits it.
   `task_events (task, seq)` is keyed by random task first. Nothing
   needs changing, but the review should be done again once real load
   is known.

That is **about 2–3 weeks** in total, most of it in items 1 and 2. They
change two of the hub's subtlest pieces, and each needs its fault
scenarios rerun.

## Operations

| | Cloud SQL (today) | Spanner (PostgreSQL dialect) |
|---|---|---|
| Connection | Cloud SQL Auth Proxy sidecar (or the Go connector), IAM database auth | PGAdapter sidecar, a JVM of roughly 0.5–1 GiB of memory. Or the Go Spanner client, which means leaving pgx and the portable SQL path. |
| Identity | Workload Identity → Cloud SQL IAM user, plus PostgreSQL roles for least privilege | Workload Identity → IAM on the database (`databaseUser`/`databaseAdmin`). Least privilege needs fine-grained access control. |
| Schema changes | Transactional DDL, fast, at hub start or `silo hub migrate` | Long-running schema operations, seconds to minutes each, and best batched. No transactional DDL, and limits on what can be altered. |
| Backups / PITR | Automated daily backups, PITR from the WAL (7 days by default), `pg_dump` | Backup schedules (full and incremental), PITR via version retention (≤ 7 days), export through Dataflow. No `pg_dump`. |
| High availability | REGIONAL: a standby in a second zone, failover in about a minute, SLA 99.95% | Regional: synchronous replicas in three zones, no failover event, SLA 99.99%. Multi-region: 99.999%. |
| Scaling | Vertical. A larger tier means a restart, and connection limits grow with the tier. | Horizontal, in 100 processing-unit steps, online. Autoscaling is managed or open-source. No connection limit like PostgreSQL's. |
| Maintenance | Maintenance windows, with restarts | None visible |
| Local development and CI | PostgreSQL, or SQLite | Emulator plus PGAdapter, in Docker. SQLite still works for development. |
| Latency | ~1ms per statement in-zone, ~2–5ms per commit | A commit is a Paxos round across zones, typically 5–15ms, plus the PGAdapter hop |

The latency difference matters here. The load report
(`docs/test-runs/2026-10-16-hub-load.md`) found that adding 1ms each way
to the database moved dispatch p50 from 1ms to 12ms, because the
dispatcher makes serial round trips per task. Spanner's commit latency
would push that further, and items 1–2 add round trips of their own. It
is not a correctness problem, but the fault and load runs would need
redoing on a real instance.

## Cost

These are approximate list prices for us-central1, as published in 2026.
Check them against the pricing pages before relying on them.

| Configuration | Approximate monthly cost |
|---|---|
| Cloud SQL, `db-custom-1-3840` (1 vCPU, 3.75 GB), ZONAL, 10 GB SSD | ~$50 |
| Cloud SQL, `db-custom-1-3840`, **REGIONAL** (today's default, `deploy/terraform/hub`) | ~$100 |
| Spanner Standard edition, regional, **100 processing units** (the minimum), 10 GB | ~$70 |
| Spanner, 1,000 processing units (1 node), regional | ~$660 |
| Spanner, multi-region (e.g. nam6), 1 node | ~$2,700 |

What the hub needs, by the load report. At full size (500 Grains,
50 tasks/s intake, 41 stored events per 20-second task) the hub writes
about 2,000 events/s at steady state, plus task updates. Google's sizing
guidance puts a regional node at a few thousand writes/s, so 100 PU
(about 300–350 writes/s) would hold a small hub but not the full-size
load. That load needs roughly 600–1,000 PU, about $400–660 a month. The
same load ran on PostgreSQL in a 4-vCPU sandbox shared with everything
else. A `db-custom-2-7680` REGIONAL Cloud SQL instance, about $200 a
month, should carry it with room left.

So at today's sizes Spanner's minimum (~$70) costs about the same as the
Cloud SQL instance it would replace, and it is more available. Its cost
rises faster with write load, though: at the full-size load test it
costs 2–3 times as much. PGAdapter's memory adds a little to every hub
pod.

## Recommendation

**Don't move now. Keep the store Spanner-compatible, which is cheap, and
revisit if one of these becomes true:**

- **The hub must survive a region going down, or needs better than
  99.95% availability.** A multi-region Spanner instance is the simplest
  way to get both. Cloud SQL can only do this with a cross-region
  replica and a failover by hand.
- **Write load outgrows one Cloud SQL primary.** That is roughly 10× the
  full-size load test (tens of thousands of stored events/s), or a
  connection count that a sensible tier cannot hold.
- **Operations want no maintenance windows and no failover events**,
  enough to pay for it.

The work needed if one of these does arrive is listed above: about 2–3
weeks, mostly the lease and the bus. Today Spanner would cost that work
and give higher write latency, and in return offer availability the hub
has not yet needed. Cloud SQL REGIONAL at ~$100 a month is the better
fit.

To keep the option open:

- The `spanner` CI job keeps the directory, tasks, registry and lease
  stores working on Spanner's PostgreSQL dialect.
- New SQL should be written to pass it: unique indexes rather than
  inline `UNIQUE`, no partial indexes except `IS NOT NULL`, no updates
  to primary-key columns, and transactions through `dbaccess.InTx`.
- If the lease is ever reworked for another reason, a TTL row lease
  (item 1) works on both databases and would remove the largest
  blocker.
