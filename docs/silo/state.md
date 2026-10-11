# silod's state: Cloud SQL for PostgreSQL, with SQLite for dev and tests

## Status

Normative draft, part of the Silo design ([`README.md`](README.md)).
This page is an addendum, written in task 1952. It replaces what call 16
said about state ("SQLite on its own persistent disk and a daily
snapshot schedule"). Marks such as "call 31" refer to
[Calls made in this draft](README.md#calls-made-in-this-draft).

bsalmon, in director chat task 1913 on 2026-10-04: "default silod to
use a cloud sql postgres db. this should be simple to set up and will
give us better scaling, backups, etc."

The chat decided:

- **The default store is Cloud SQL for PostgreSQL**, created by
  `silo install` in the host project.
- **SQLite stays**, behind the same store interface, for local
  development, the fake-GCE CI tests and single-binary demos.
- **Both backends pass the same tests.**

The rest of this page is the author's calls. Each is listed in the
README.

## Contents

1. [Two backends, one interface](#two-backends-one-interface)
2. [What silod relies on the store for](#what-silod-relies-on-the-store-for)
3. [The Cloud SQL instance](#the-cloud-sql-instance-call-33)
4. [Connectivity: a PSC endpoint in a control subnet](#connectivity-a-psc-endpoint-in-a-control-subnet-call-32)
5. [Authentication: IAM, no password](#authentication-iam-no-password)
6. [More than one silod, and the leader](#more-than-one-silod-and-the-leader-call-34)
7. [Schema migrations and upgrades](#schema-migrations-and-upgrades-call-35)
8. [Backups and recovery](#backups-and-recovery)
9. [Availability, cost and limits](#availability-cost-and-limits)
10. [Testing the store](#testing-the-store)

## Two backends, one interface {#SV-45}

| Backend | Selected by | Used for | Replicas |
| --- | --- | --- | --- |
| `postgres` | `--store=postgres` (the default). With `silo install` it is always a Cloud SQL instance that install created | Every real install | Any number |
| `sqlite` | `--store=sqlite:<path>` | Local development, the fake-GCE tests in CI, and single-binary demos against the fake backend | One. silod refuses to start a second process on the same file. |

`silo install` always creates Cloud SQL in v1. It has no flag for an
install on SQLite (call 31). An install on SQLite would bring back the
state disk, the snapshot schedule and the single control VM, and it
would be a second production path for a case nobody has asked for.
Whether a cheap throwaway install should be able to choose it is
[open question 17](README.md#open-questions).

All of silod's state goes through one Go package, `store`, and nothing
else in silod writes SQL. Its shape:

```go
type Store interface {
    // Tx runs fn in one transaction at serializable isolation. A
    // serialization failure is retried, up to a limit, with fn run
    // again from the start. So fn must not call GCE or do anything
    // else that can't be repeated.
    Tx(ctx context.Context, fn func(Tx) error) error

    // Changes returns the change log from seq onwards, then follows
    // it: each committed change, in commit order, with no gaps.
    Changes(ctx context.Context, since uint64) (<-chan Change, error)

    // Lead blocks until this process holds the named lease, or ctx
    // ends. Lost is closed the moment the lease can no longer be
    // trusted.
    Lead(ctx context.Context, name string) (lost <-chan struct{}, err error)

    // Schema reports the applied migration version.
    Schema(ctx context.Context) (int, error)

    // Shared reports whether several processes may use this store at
    // once: true for postgres, false for sqlite.
    Shared() bool
}
```

The schema is written once in the subset of SQL the two backends
share: text, integers, booleans, and timestamps as UTC integers.
Where the two need different DDL (types, `CREATE INDEX CONCURRENTLY`,
triggers), each backend has its own migration file with the same
version number. Queries don't use features that only one backend has,
such as Postgres arrays and JSONB operators. JSON is stored as text
and read in Go. grain's own model store works the same way
(`pkg/model/sqlite` and `pkg/model/postgres`, with a parity suite), and
the silo store should copy that pattern rather than invent another.

## What silod relies on the store for {#SV-46}

These are the semantics both backends have to provide. The
[store conformance tests](#testing-the-store) check each one.

1. **Atomic multi-row transactions.** A fence call or a facade write
   changes several rows: the silo, its parent's quota, the block map,
   the grant, and the audit record and change-log entry that go with
   them ([`allocators.md`](allocators.md)). All of it commits together
   or not at all. silod calls GCE only after the transaction that
   charged for the call has committed. A refund is a transaction of its
   own ([`allocators.md`, Charging and refunds](allocators.md#charging-and-refunds)).
2. **Concurrent allocations never over-allocate.** Two children
   carving blocks from one parent, or two VMs charging one silo's
   quota, must not both succeed past the limit.
   - **Postgres:** `SERIALIZABLE` isolation, with retry on
     serialization failure (`40001`). A transaction that changes an
     allocation also takes `SELECT ... FOR UPDATE` on the rows of the
     silo and each ancestor it touches, in a fixed order (root first,
     then by depth), so contention queues on the lock instead of being
     retried, and two transactions can't deadlock.
   - **SQLite:** one writer at a time (`BEGIN IMMEDIATE`, WAL mode),
     which is serializable by construction.
3. **A gap-free, ordered change log.** Every transaction that changes
   the origin table, a grant, an egress policy or anything the fence is
   compiled from appends a row to `changes`. The row's sequence number
   comes from a counter row updated in the same transaction. A Postgres
   `SEQUENCE` won't do, because its values aren't handed out in commit
   order. The counter serializes those writers, which silod's write rate
   allows. The sequence number is the version the gateway puts in
   `Silo-Origin` ([`gateway.md`, Origin identity](gateway.md#origin-identity)),
   so it has to be one number for the whole fleet.
4. **Watching the change log.** Each silod replica follows `changes` to
   keep its gateways' tables current, to close revoked connections, and
   to learn when the fence has been applied.
   - **Postgres:** the writing transaction runs `NOTIFY silo_changes,
     '<seq>'`, which Postgres delivers on commit. Each replica keeps a
     connection in `LISTEN silo_changes`. A notification only wakes the
     reader, which then reads `changes WHERE seq > last`, so a lost or
     merged notification loses nothing. As a backstop, the reader also
     polls once a second, and always reads the log first after
     reconnecting.
   - **SQLite:** an in-process wake-up, with the same poll.

   The one-second revocation promise
   ([`gateway.md`, Proxied exports](gateway.md#proxied-exports)) rests
   on `NOTIFY`, which takes milliseconds. The poll keeps the worst case
   near one second when a notification is lost.
5. **Leases.** Some work needs exactly one doer
   ([below](#more-than-one-silod-and-the-leader-call-34)). On Postgres a lease
   is a session advisory lock. On SQLite, where there is only one
   process, it is an in-process mutex.
6. **Idempotency.** `request_id` ([`fence-api.md`](fence-api.md)) has
   a unique index. A retried call finds its first result instead of
   doing the work twice, whichever replica it reaches.
7. **One clock.** The freed-block reuse delay, the orphan grace period
   and audit retention use the database's time (`now()`), not each
   replica's, so replicas whose clocks differ still agree.

## The Cloud SQL instance (call 33) {#SV-47}

Install creates one instance in the host project:

| Setting | Value | Flag |
| --- | --- | --- |
| Name | `silo-db-<4 random characters>`, found by the labels `silo-install` and `silo-role=db`. It is the one install object without a fixed name, because Cloud SQL doesn't let a deleted instance's name be reused for about a week. With a fixed name, reinstalling after `uninstall --delete-data` would fail. | |
| Engine | PostgreSQL 17 | `--db-version` |
| Edition | Enterprise | |
| Tier | `db-g1-small` (shared core, 1.7 GB) | `--db-tier` |
| Region | The home region | |
| Availability | Zonal | `--db-ha` makes it regional (HA) |
| Storage | SSD, 10 GB, automatic increase on | |
| Backups | Automated daily, kept for 14 | `--db-backup-retention` |
| Point-in-time recovery | On, 7 days of logs | |
| Deletion protection | On | |
| IP | No public IP, no private services access. Private Service Connect on, with the host project as the only allowed consumer | |
| Database flags | `cloudsql.iam_authentication=on` | |
| Maintenance window | Sunday 05:00 UTC, so it doesn't land in someone's working day | `--db-maintenance-window` |
| Labels | `silo-install=<install-id>`, `silo-role=db` | |

In it, install creates the database `silo` and one user,
`silod@<project>.iam`, of type `CLOUD_IAM_SERVICE_ACCOUNT`. That user
owns `silo` and runs the migrations. Install sets no password for the
built-in `postgres` user and creates no other users.

The tier default is a shared-core one because the control plane's
load is small: rows per silo, per grant and per audited call. A shared
core keeps the fixed cost near the bottom of Cloud SQL's range. Shared
cores have no Cloud SQL SLA, though. An operator who wants the SLA
passes `--db-tier db-custom-1-3840`, at roughly twice the cost. Whether
that should be the default is [open question 18](README.md#open-questions).

## Connectivity: a PSC endpoint in a control subnet (call 32)

Cloud SQL lives in Google's network, not in `silo`. silod reaches it
through an endpoint in `silo`, and that endpoint must be somewhere no
silo can reach.

### The control subnet {#SV-48}

Install makes a second reserved subnet next to the gateway's:

- **`silo-ctl`**: `--control-range`, by default `10.255.254.0/24`,
  in the home region, inside `--address-space`. Like the gateway range,
  it is **held back from the root's block**, so no silo can be given any
  of it, and no tenant subnet can overlap it.
- It holds only control-plane endpoints. In v1 that is the database's
  PSC endpoint. No VM runs in it.

The endpoint can't go in the gateway range. The fence lets every silo
reach the gateway range (`n-gw-out`, `gw-in`). Today that is on fixed
ports, none of them a database port, but a future port added to that
list, or a mistake in it, would put the database within reach of every
tenant. A range that the fence never opens to a silo doesn't depend on
a port list being right.

### Recommended: Private Service Connect {#SV-49}

Install enables PSC on the instance and creates, in `silo-ctl`:

- the static internal address `silo-db-psc`;
- the forwarding rule `silo-db-psc`, targeting the instance's PSC
  service attachment;
- the Cloud DNS private zone `silo-db`, visible to `silo`, with an A
  record from the instance's PSC DNS name to `silo-db-psc`. The Cloud
  SQL Go connector dials that name.

Why PSC rather than private services access:

- **No peering.** Private services access peers `silo` with Google's
  service-producer network and exchanges routes with it. That is one
  more path out of the VPC that the fence has to account for, and it
  takes a peering slot that the VPC backing may want later
  ([open question 10](README.md#open-questions)).
- **One address, in a range silod owns.** A PSC endpoint is one IP in a
  subnet install made. Private services access needs a whole allocated
  range (a /24 at least, and Google suggests more), and that range would
  have to be carved out of `--address-space` too.
- **One direction.** A PSC endpoint is reached by the consumer. The
  service can't open connections back into `silo`.
- **Simple fence.** The endpoint sits in a range that is already held
  back from every silo, so the rules that already deny everything
  outside each silo cover it, and one explicit deny makes sure
  ([below](#what-the-fence-does)).

What PSC costs:

- **Money.** A PSC endpoint is billed per hour and per GB processed: a
  few dollars a month at silod's traffic. Private services access is
  free.
- **Org policy.** In a project with an organization, the constraints
  `compute.disablePrivateServiceConnectCreationForConsumers` and
  `compute.restrictPrivateServiceConnectProducer` can forbid the
  endpoint. bsalmon's projects have no organization, so no policy
  applies to them, which is the path install expects. Install checks
  both constraints where there is an organization
  ([`install.md`, Prerequisite checks](install.md#prerequisite-checks)).

### The alternative: private services access

If PSC is blocked by org policy, the same design works with private
services access. Install would enable `servicenetworking`, allocate a
range (`--db-psa-range`, default `10.255.240.0/20`) held back from the
root's block like the control range, peer `silo` with
`servicenetworking.googleapis.com`, and create the instance with a
private IP in that range. The fence would deny the allocated range in
the same way as the control range. v1 doesn't build this. It is
recorded so that an organization that forbids PSC isn't a dead end.

### What the fence does {#SV-50}

The control range is outside every silo's block, so each silo's
`n-deny-out` and the final `catch-out` already deny traffic to it. On
top of that, install adds a rule in a new band ahead of the grants
([`gcp-backend.md`, The fence](gcp-backend.md#the-fence)):

| Rule | Direction | Match | Action |
| --- | --- | --- | --- |
| `ctl-deny-out` | egress | src ∈ the root's block (`--address-space` minus the gateway and control ranges), dst ∈ control range, all protocols | `deny` |

The deny rule comes first, so no grant rule, gateway rule or future
change to either can open the control range to a silo. Traffic to a
PSC endpoint is decided by the sender's egress rules, so an egress
deny is the rule that counts. The control VMs are in the gateway range,
not in the root's block, so their own egress (`gw-out`) still reaches
the endpoint.

Three more layers stand behind the fence:

- **The egress proxy** refuses any destination that resolves into the
  shared VPC's range ([`gateway.md`, Enforcement](gateway.md#enforcement)),
  which includes the control range.
- **An export target** must be inside the exporter's own space, which
  the control range never is.
- **Tenant VMs have no service account**, so even with a route they
  would have no IAM identity to log in with.

The PSC DNS name in `silo-db` resolves from every VM in `silo`. This is
one more name that leaks, like GCE internal DNS
([`gcp-backend.md`, What leaks between silos](gcp-backend.md#what-leaks-between-silos)).
The name and the address show, but no traffic flows to them.

## Authentication: IAM, no password {#SV-51}

- silod connects with the **Cloud SQL Go connector**
  (`cloud.google.com/go/cloudsqlconn`), using `WithIAMAuthN()` and the
  PSC dial option, through `pgx`. The connector gets silod's OAuth token
  from the metadata server and refreshes it, and the connection is
  TLS to the instance's server certificate.
- The database user is `silod@<project>.iam`, silod's own service account.
  There is no password anywhere: not in metadata, not in a secret,
  not in the image.
- The gateway container already has no route to the metadata server
  ([`gateway.md`, Placement](gateway.md#placement)), so it can't get a
  token and can't connect to the database.
- silod's custom role gains `cloudsql.instances.connect`,
  `cloudsql.instances.get` and `cloudsql.instances.login`, the last
  being what IAM database login checks
  ([`install.md`, silod's service account](install.md#silods-service-account)).
  silod gets no `cloudsql.instances.update`, no `cloudsql.users.*` and
  no `cloudsql.backupRuns.*`. It can use its database but can't
  reconfigure, back up, restore or delete it.
- **Install** creates the instance, the database and the IAM user, so
  install's credentials need `roles/cloudsql.admin`. silod's don't.
- **Ownership.** An IAM user starts with no rights in a database it
  didn't create. Install makes `silod@<project>.iam` the owner of `silo`
  through the Admin API, by giving the user the `cloudsqlsuperuser`
  database role when it creates it. This is silod's own instance, and
  holds nothing else, so that role gives silod nothing beyond its own
  data. If the Admin API can't grant that role, the fallback needs
  one SQL session as the built-in user, which means a password.
  [Open question 19](README.md#open-questions) asks how to do that
  without leaving one behind.

## More than one silod, and the leader (call 34) {#SV-52}

With state in Cloud SQL, silod no longer has to live on one machine.
This settles half of [open question 9](README.md#open-questions).

- **`--control-replicas N`** (default 1) runs `silo-control` as a
  regional MIG of `N` control VMs, each with silod and a gateway, behind
  the internal passthrough NLB that is then `silo.internal`
  ([`gateway.md`, Placement](gateway.md#placement)). With 1, it is the
  single VM it always was. The default stays 1 because a second VM
  doubles the control plane's compute cost, and a restart of one VM
  costs only a few seconds of facade and fence calls, while the
  gateways keep serving from cache.
- **Any replica** serves facade and fence calls, charges and refunds
  quota, writes audit, serves the front door, and watches the GCE
  operations it started. All of that is a transaction against shared
  state.
- **Only the leader**:
  - **compiles and applies the fence policy.** Two writers applying
    diffs to one policy at once could interleave adds and deletes and
    briefly leave it looser than either end. A replica that commits a
    fence-changing transaction waits until the leader records that it
    has applied that change's sequence number, and only then answers
    the caller, as [`fence-api.md`](fence-api.md) requires;
  - **runs the reconcile**: on taking the lease, then every ten
    minutes. That covers orphans, recomputing `used`, and repairing
    drift ([`gcp-backend.md`, Reconcile](gcp-backend.md#reconcile));
  - **adopts orphaned operations**: GCE operations whose starting
    replica died before seeing them finish. Each replica writes a
    heartbeat. The leader takes over the operations of any replica
    whose heartbeat is older than 30 seconds, so refunds still happen;
  - **issues gateway-replica certificates** through instance metadata;
  - **prunes audit records** older than the retention.
- **Leader election** is a session-level Postgres advisory lock,
  `pg_try_advisory_lock` on a fixed key, held on a connection of its
  own. Each replica tries every five seconds. Postgres releases the
  lock when the holder's session ends.
- **Stepping down.** The leader pings its lock connection every two
  seconds and stops all leader work as soon as a ping fails. The
  connection's TCP keepalive is set longer than that (about ten
  seconds), so the old leader stops before Postgres can free the lock
  for someone else. Two more guards cover a leader that stalls instead
  of failing:
  - every leader-only write to the database checks the leader epoch, a
    counter the new leader bumps when it takes the lease;
  - every fence apply sends the firewall policy's `fingerprint`, so
    GCE refuses a patch computed from a policy that someone else has
    changed since.
- **On SQLite**, `Shared()` is false. silod refuses
  `--control-replicas` above 1, and its one process is always leader.

What still has no answer is the other half of open question 9: whether
the gateways should keep serving facade calls from a cache while no
silod can reach the database.

## Schema migrations and upgrades (call 35) {#SV-53}

- **Versioned.** Migrations are numbered SQL files embedded in the
  silod binary, one directory per backend, with the same numbers in
  both. The applied version is recorded in `schema_migrations`.
  Migrations only go forward.
- **Run by install and upgrade, not at start-up.** `silod serve` never
  migrates. On start it reads the schema version and refuses to serve
  (its health check says `schema_mismatch`) unless the version is
  within the range its release supports. Two replicas starting at once
  therefore never race to migrate. On SQLite, dev and test builds may
  pass `--migrate` to `silod serve` for convenience. It is refused on
  Postgres.
- **Where it runs.** The instance has no public IP, so the installer's
  machine can't reach it. Install's `migrate` step creates a short-lived
  VM, `silo-migrate`: Container-Optimized OS, the silod service account,
  an ephemeral address in the gateway subnet, no external IP. The VM
  runs `silod migrate --to <version>` from the image being installed,
  reports the result as a guest attribute, and install deletes it. This
  takes about a minute. It is the same on a first install, where no
  silod is running yet, and on an upgrade, where the fleet keeps
  running. `silod migrate` takes an advisory lock of its own, so two
  installs can't migrate at once.
- **Expand, then contract.** A migration shipped in release N+1 must
  leave the schema usable by release N. It may add tables, nullable or
  defaulted columns, and indexes (built concurrently). It may not
  rename, drop, or tighten a constraint that N writes against. Drops
  and renames ship as the contract half, one release later, in N+2.
  Each release declares the oldest and newest schema it runs against.

**How a running fleet upgrades** to N+1 (`silo install --version N+1`):

1. `image` copies N+1's image.
2. `migrate` applies N+1's migrations. These are N+1's expand steps
   and N's contract steps. The replicas still running N keep working,
   because each expand step is compatible with N. `silod migrate`
   refuses a contract step while any replica's heartbeat reports a
   release older than that step allows.
3. `control` rolls the control MIG one VM at a time (surge 1,
   unavailable 0). With one control VM, it restarts that VM, and
   facade and fence calls wait for a few seconds. Leadership moves when
   the leader's VM is replaced.

**Skipping a release.** Install applies migrations in order. If a
release in between has a contract step that needs the whole fleet past
a version it isn't at yet, install stops and says so. `--allow-downtime`
then stops every replica, migrates all the way and starts N+1.

**Rolling back.** Run install with the old `--version`. After the
expand steps, the schema is still one the old release can use. Data
damage is recovered from a backup, not from a down-migration.

## Backups and recovery {#SV-54}

This resolves [open question 7](README.md#open-questions).

- **Automated backups**, daily and kept for 14 days by default,
  replace the `silo-state-daily` snapshot schedule.
- **Point-in-time recovery** keeps 7 days of transaction logs. Any
  moment in that window can be restored, not just the last daily
  backup.
- **Restoring** makes a new instance (a clone at a point in time, or a
  backup restored into a new instance). `silo install --db-instance
  <name>` then adopts it: it labels it, repoints the PSC endpoint and
  DNS record, and restarts silod.
- **A restored silod starts with reconcile in report-only mode**
  (call 36).
  Objects that were created in GCE after the restore point look like
  orphans to the restored database, and the normal reconcile would
  delete them an hour later (call 27). After a restore, silod only
  reports orphans until an operator runs `silo reconcile --apply` or
  re-creates the missing rows. The orphan list is the record of what
  happened after the restore point.
- **Deletion protection** stops the instance being deleted by accident,
  by install's own credentials or anyone else's.
  `silo uninstall --delete-data` is the explicit way to lift it
  ([`install.md`, `silo uninstall`](install.md#silo-uninstall)).
  Uninstall asks Cloud SQL for a final backup, kept for 30 days, before
  it deletes the instance.

The labels on cloud objects still let silod rebuild the tree's shape if
every backup is lost. Grants, policies and audit can't be rebuilt from
them, which is why backups matter.

## Availability, cost and limits

**Zonal or HA.**

- **Zonal (the default):** one primary in one zone. A zone outage or
  maintenance takes the database down: maintenance usually costs under
  a minute, once in a while, inside the maintenance window. A zone
  outage lasts until the zone recovers or the database is restored
  elsewhere.
- **`--db-ha`:** a regional instance with a standby in another zone,
  and automatic failover in about a minute. The standby is a failover
  replica only. It serves no reads, so HA doesn't add read capacity, and
  silod needs none. It roughly doubles the instance and storage cost.

**Cost** (us-central1, list prices, rough; check current pricing):

| Configuration | Instance and storage, per month |
| --- | --- |
| `db-g1-small`, zonal (the default) | about $25–30 |
| `db-f1-micro`, zonal (`--db-tier`, throwaway installs) | about $10 |
| `db-custom-1-3840`, zonal (has an SLA) | about $50 |
| Any of these with `--db-ha` | about twice the zonal figure |

Backups, PITR logs and the PSC endpoint add a few dollars a month. All
of this is fixed cost. It is paid whether or not any silo exists, on top
of the control VM.

**Limits.**

- **Install takes longer.** Creating a Cloud SQL instance takes several
  minutes, often five to ten, and the plan says so.
- **A regional dependency.** The control plane depends on one Cloud SQL
  instance in the home region. If it is unreachable, facade and fence
  calls fail with `UNAVAILABLE` (503). The gateways keep serving
  exports and egress from their cached tables. No grant can change
  while the database is down, so a cache can't be serving a grant that
  was revoked after the outage began.
- **Connections.** A `db-g1-small` allows tens of connections. Each
  replica uses a pool of at most 10, plus one each for its lease and its
  `LISTEN`. That fits a handful of replicas. More would need a bigger
  tier.
- **Storage grows but never shrinks.** Automatic increase can't be
  undone. Audit retention keeps the database small.

## Testing the store {#SV-55}

- **One suite for both backends**, `store/storetest`. Every backend
  runs it. It covers the semantics in
  [What silod relies on the store for](#what-silod-relies-on-the-store-for):
  - a failed `fn` rolls back everything;
  - many goroutines carving blocks and charging quota under one parent
    never over-allocate, and never hand out one block twice;
  - serialization failures are retried, and `fn` sees no state from an
    attempt that failed;
  - the change log has no gaps and is in commit order under concurrent
    writers, and `Changes` delivers each entry once, including across a
    dropped connection;
  - at most one holder of a lease at a time; the lease moves when its
    holder's connection is killed; and `lost` fires before anyone else
    can take it;
  - `request_id` replays;
  - database time is used for every delay;
  - migrations from every released version to the newest, and each
    release's binary against the schema of the next (expand
    compatibility).
- **In CI.** The fake-GCE tests run silod on SQLite in the `go` job, with
  no service. The store suite runs there on SQLite too, and also against
  Postgres in this repository's `postgres` job
  (`.grain/workflows/tests.yml`), modelled on grain's own. That job installs Debian's
  `postgresql` package and starts a throwaway server per test from its
  binaries. No container is needed, and it costs well under a minute.
  The Postgres tests fail rather than skip when that job sets
  `SILO_REQUIRE_POSTGRES`, as grain's do with `GRAIN_REQUIRE_POSTGRES`.
- **Against real Cloud SQL.** The conformance run against
  `bsalmon-fun-sandbox` installs with Cloud SQL, PSC and IAM login, the
  same as production, and includes the case that no silo VM can reach
  the database ([`testing.md`](testing.md#conformance-suite)).
