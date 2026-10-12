# Testing Silo

## Status

Normative draft, part of the Silo design ([`README.md`](README.md)),
written in task 1930. The control policy (sealed silos) and `manage`
grants were added in task 1947.

In the subnet backing, isolation is firewall-only
([`README.md`, Limitations](README.md#8-limitations)), so most of the
weight is on tests. There are two layers:

| Layer | Runs against | Runs when | Proves |
| --- | --- | --- | --- |
| [Fake GCE backend](#fake-gce-backend) | An in-process fake of the `compute/v1` subset | Every grain CI run, no credentials | The facade, the fence compiler, the allocators and quota pricing, install's planning |
| [Store conformance](#store-conformance) | SQLite, and a throwaway local PostgreSQL server | Every grain CI run, no credentials | That both state backends give silod the same semantics: transactions, concurrent allocation, the change log, leases, migrations |
| [Conformance suite](#conformance-suite) | Real GCP, `bsalmon-fun-sandbox` | On a schedule, or by hand, with the `gcp-key` capability | That the fence really isolates: real packets, real firewall evaluation, real GCE behaviour |

## Fake GCE backend {#SV-119}

The fake is a Go package, `pkg/silo/backend/gcp/gce/fake`, that serves
the `compute/v1` resources silod calls (instances, disks, images,
snapshots, instance templates and
regional instance templates, subnetworks, firewalls, firewall policies,
networks, operations, regions, zones, machine types, disk types,
accelerator types and quotas). It keeps them in memory and behaves the way
GCE does where silod depends on it:

- names unique per scope, the 63-character limit, `alreadyExists`;
- subnet ranges that can't overlap within a network, and /29 as the
  smallest;
- operations that finish asynchronously, and can be made to fail on
  request (quota, a stockout, an internal error) so that refunds are
  tested;
- regional quota limits that can be set per test, for every class in
  [`allocators.md`, Classes](allocators.md#classes): the GPU metrics
  and their `PREEMPTIBLE_` variants, `LOCAL_SSD_TOTAL_GB` and
  `PREEMPTIBLE_LOCAL_SSD_GB`, `PD_EXTREME_TOTAL_PROVISIONED_IOPS`, the
  Hyperdisk capacity, IOPS and throughput metrics, `INSTANCE_TEMPLATES`
  and `GPUS_ALL_REGIONS`, some reported by `regions.get` and some only
  by a fake of the Cloud Quotas API, as in GCE;
- accelerator types offered per zone; machine types that bundle GPUs
  (A2, A3, G2) or local SSD (`-lssd`); GCE's GPU, Hyperdisk and
  `pd-extreme` machine-type compatibility, refusing what GCE refuses;
  and `onHostMaintenance: TERMINATE` required with GPUs;
- `pd-extreme` and Hyperdisk disks with provisioned IOPS and
  throughput, GCE's defaults when none is given, and `disks.update`;
- instance templates, and `instances.insert` with
  `sourceInstanceTemplate`, merging the body over the template's
  properties as GCE does;
- label and name-prefix filters on `list`.

silod runs on top of it with a real SQLite database (`--store=sqlite`,
[`state.md`](state.md)). Tests drive it
through the gateway's code with a configurable source IP, so origin
identity is part of every test.

The fake doesn't evaluate packets. Instead, a **fence evaluator** in
the same package takes the compiled firewall policy and the tenant
VPC rules and answers "is a packet from A to B on port P allowed?" by
GCE's documented rules: priority order, `goto_next`, the
`BEFORE_CLASSIC_FIREWALL` order, and the implied rules. Property tests
generate random trees, blocks, grants and tenant rules, and check:

- no packet crosses silos without an active grant;
- every packet within a silo is decided by that silo's tenant rules;
- every silo can reach the gateway's ports, and nothing else outside;
- no silo can reach the control range, on any protocol or port,
  whatever grants and tenant rules exist;
- a silo whose effective control policy is `none`, and every silo
  below it, can't reach the API port (8080), and can still reach
  ports 80, 3128 and the export ports;
- every intermediate state of a fence update is no looser than both
  its start and its end.

The same evaluator runs against the real policy in the conformance
suite, which checks that the evaluator agrees with GCE.

What CI covers with the fake:

- **The facade.** Every allowed method and field, and every refused
  one (403, with the field named). Name mapping both ways, including
  full URLs. Filters and paging. GCE-shaped errors. Projects that the
  caller can't see.
- **Allocators.** Buddy placement, growing in place and adding blocks,
  the block cap, freed blocks merging, the reuse delay. Quota
  `granted`/`delegated`/`used` across a tree under concurrent calls,
  over-commitment at the root, and refunds.
- **Quota pricing.** Each machine type, Spot, each disk type, stop and
  start, and `setMachineType`. GPUs attached and bundled, standard and
  Spot, and `gpus_all_regions`; local SSD attached and bundled;
  `pd-extreme` and Hyperdisk capacity, IOPS and throughput, with
  `disks.update` and `resize` charging and refunding the difference;
  a template charging only `instance_templates`, and an instance from a
  template charging what the effective instance costs.
- **Template validation.** Every instance rule applied to a template's
  `properties` at create, and again to the effective instance made from
  it, including after the silo's allowed regions or subnets change.
- **The fence API.** Every operation, every error code, who may call
  what, `validate_only`, `request_id`.
- **Control policy and `manage`.** The gateway's and silod's method
  classification (every facade method and fence operation is a read
  or a mutation, and an unknown one is a mutation), each layer of
  refusal on its own, inheritance, and every limit on a `manage`
  holder, with origin identity from the source IP as everywhere
  else.
- **Install.** Each step's `Plan` against every partly-installed state
  ([`install.md`, Testing install](install.md#testing-install)).

## Store conformance {#SV-120}

silod's state store has two backends, Cloud SQL for PostgreSQL and
SQLite, behind one interface. One suite, `store/storetest`, runs
against both. What it checks is listed in
[`state.md`, Testing the store](state.md#testing-the-store): rollback,
concurrent allocations that never over-allocate, serialization
retries, a gap-free change log and its watch, lease exclusivity and
hand-off, `request_id` replays, database time, and migrations.

- **SQLite** runs in the `go` job, with the rest of silod's tests.
- **PostgreSQL** runs in this repository's `postgres` job in
  `.grain/workflows/tests.yml`, modelled on grain's own. It installs Debian's `postgresql`
  package (it isn't in the sandbox image), and each test starts its own
  server on a free port from those binaries, as `pkg/model/postgres`
  does in grain. That costs well under a minute, needs no Docker, and has a
  precedent in grain's repository. A Postgres container isn't needed. With
  `SILO_REQUIRE_POSTGRES` set, a server that doesn't start fails the
  job instead of skipping, so the job can't go green over nothing.
- The fake-GCE tests above use SQLite only. Running them on Postgres
  too would add time and little coverage, because the store suite
  already pins down what silod relies on.

## Conformance suite

The `silo/v1` conformance suite runs against real GCP. It is meant for
every backend: the same cases, written against the fence API, with
backend-specific helpers to make a VM and send a packet. v1 has only
the GCP helpers.

### Where and how it runs {#SV-121}

- **Project:** `bsalmon-fun-sandbox`, with the `gcp-key` capability.
  The key is at `/home/debian/.gcp-service-account.json` in the
  sandbox.
- **When:** on a schedule (proposed: nightly, as a `.grain/workflows`
  workflow with `on.schedule`) and by hand. Not on every pull request:
  a run creates VMs and takes tens of minutes.
- **Shape:** `silo install` into a fresh `--address-space`, the suite,
  then `silo uninstall --force --delete-data --yes` and a check that no
  labelled object is left. A run that fails part-way still uninstalls.
  Install uses Cloud SQL, with the PSC endpoint and IAM login, exactly
  as in production, so the run also tests install's database steps and
  the migration VM.
- **Cost:** `e2-micro` VMs, Spot where the case allows, and a hard cap
  on how many VMs exist at once. The run installs with
  `--db-tier db-f1-micro`, the cheapest tier, which exists for an hour
  or so per run, and with `--control-replicas 2`, so that leader
  failover is tested. Creating it adds five to ten minutes. Whether to keep
  a standing instance instead is
  [open question 20](README.md#open-questions).

A scheduled run gets no write-scoped capabilities and no live provider
secrets on this deployment today. Running it on a schedule may need a
change to the deployment's CI capabilities in grain/config, so that
the schedule is granted `gcp-key`. That is for the implementation task
to sort out, not this draft.

### Cases {#SV-122}

Each case sets up a small tree (`/a`, `/a/c`, `/b`), with a probe VM in
each, and asserts what is refused and what is reachable. "Refused"
means the facade returns the GCE-shaped error and no GCE object
appears. "Unreachable" means a TCP connect and an ICMP echo both fail
from a probe VM.

**Attaching outside the silo**

- An instance in `/a` naming a subnet of `/b`, by name, by full URL,
  and by the real prefixed name.
- `attachDisk` of `/b`'s disk to `/a`'s instance, and the other way.
- An image or snapshot of `/a` used as a source in `/b`.
- An alias IP range, or a `networkIP`, outside the instance's subnet.

**Subnets**

- A subnet outside `/a`'s blocks; one overlapping a block `/a`
  delegated to `/a/c`; one overlapping `/b`'s.
- A secondary range outside the blocks, and `expandIpCidrRange` past
  them.
- A subnet in a region outside `/a`'s allowed set, and an instance in
  a zone outside it.

**Origin spoofing**

- A facade call from `/a`'s probe with a forged `Silo-Origin` header,
  and with `Authorization` set to a real Google token for the host
  project: it still acts as `/a`.
- A VM that tries to send from `/b`'s address (raw socket). The
  packet never arrives, and no `canIpForward` VM can be created.
- A call from an IP in `/a`'s block but in no subnet of it.
- A call just after `/a/c` is deleted, from an IP that was `/a/c`'s.

**Crossing silos without consent**

- `/a` → `/b`, `/a` → `/a/c` and `/a/c` → `/a`, on TCP, UDP and ICMP,
  with tenant rules on both sides that allow everything: unreachable.
- An export offered but not accepted, and accepted but not offered:
  unreachable through the gateway.
- A link proposed by one side only: unreachable. Both sides: reachable
  on the named port only. Deleted: unreachable again.
- Policy: `/a` sets `links: deny` on `/a/c`, and a link both sides
  want is refused with `LOOSER_THAN_PARENT` or suspended.
- Revoking an export closes an open connection within a second.

**List and get leaks**

- `list` and `aggregatedList` of every resource type from `/a` show
  none of `/b`'s or `/a/c`'s objects, with and without filters
  (including a filter on the real prefixed name).
- `get` of `/b`'s object by every name form returns 404.
- `projects/silo-<b's id>` returns 403, and so does a made-up id.
- Responses carry no `silo` labels, no `<id>` tags and no host-project
  links.
- `/a` can read and write `/a/c`'s facade. The objects are `/a/c`'s,
  charged to its quota, and audited with `via=ancestor:<a's id>`.
  `/a/c` can't write to `/a`'s.
- Operations: `/a` can't `get` or `wait` `/b`'s.

**Name collisions**

- `/a` names its objects `<a's id>-…`; a name without the prefix, or
  with `/b`'s, is refused (400) with a message naming `/a`'s prefix.
- A 63-character name, prefix included, works, and a 64-character one
  is refused.
- A tenant tag or label starting with `silo` is refused.

**Refused fields and methods**

- Each item in [`gcp-backend.md`, Refused](gcp-backend.md#refused),
  one case each.
- A field the facade doesn't know but GCE does (taken from a recent
  `compute/v1` discovery document) returns 403. That checks
  default-deny against API drift.

**Egress and the gateway**

- An allowed host through `HTTPS_PROXY` works. A disallowed host, a
  host that resolves to an internal address, and a direct connection
  without the proxy all fail.
- Narrowing an ancestor's egress closes the child's open `CONNECT`.

**The database**

- From a probe VM in the root, in `/a` and in `/a/c`: a TCP connect to
  the database's PSC endpoint, on 5432, 3307 and a sample of other
  ports, and an ICMP echo, all fail. This holds with tenant egress
  rules that allow everything, and with an active link and an accepted
  export in the tree.
- The endpoint's DNS name resolves from the probe, as expected, and a
  connect to the name fails the same way.
- `CONNECT` through the egress proxy to the endpoint's address or DNS
  name is refused, even with `*` in the egress policy.
- An export whose target is the endpoint's address is refused.
- A tenant subnet overlapping the control range is refused.
- Control: silod, on the control VM, is connected and healthy
  throughout, so the case can't pass because the database is down.
- Killing the leader's VM (with `--control-replicas 2`) moves the
  lease to the other replica, and a fence change made just after is
  applied exactly once.

**Sealed and `read` silos**

The tree gets `/s` (sealed) and `/r` (`read`), each with a probe VM.

- From `/s`'s probe, a TCP connect to `silo.internal:8080` times out:
  the fence drops it. `silo.internal:80`, `:3128` and an accepted
  export's port still work, as do an L3 link and egress to an allowed
  host.
- With the fence rule forced back in (a test-only switch compiled into
  the conformance build of silod, `SILO_TEST_KEEP_CONTROL_PORT`), the same
  probe reaches the gateway and gets 403 `forbidden` from the facade
  and `CONTROL_DENIED` from the fence API, for reads and writes alike.
  That proves layer 1 without layer 2. This switch and the one below
  exist only in the conformance build, never in a release image. A front-door member mapped to
  `/s` gets the same refusals.
- With the gateway's check turned off (`SILO_TEST_SKIP_CONTROL_GATE`,
  the same build), silod still refuses. That proves silod's own check.
- From `/r`'s probe, every facade read and `zoneOperations.wait`
  works. Every mutating facade method in
  [`gcp-backend.md`, Covered](gcp-backend.md#covered), and every
  mutating fence operation (`create`, `PATCH`, `grow`, export offer and
  accept, link propose and delete), is refused, and no GCE object or
  database row appears.
- `/s`'s and `/r`'s ancestor writes into their facades and changes
  their allocations; both work, and the probes keep running.
- `/r` tries to `PATCH` its own `control` to `full`, and to `none`:
  both are `PERMISSION_DENIED`.

**Control inheritance**

- Creating `/r/c` with `control: full` is `LOOSER_THAN_PARENT`. So is
  `PATCH`ing an existing `read` child of `/r` to `full`.
- `/a/c` is `full` under a `full` `/a`. Sealing `/a` makes `/a/c`'s
  effective policy `none`: `/a/c`'s probe loses port 8080 within the
  fence update, and its open API-port connection is closed.
  Unsealing `/a` restores `/a/c`'s own `full`.
- `describe` of `/a/c` from `/` shows `own`, `effective` and
  `effective_from`.

**`manage` grants**

`/` offers `/p` a grant on `/s` with `facade.resources:
["instances", "disks"]`, `access: write`, `fence: ["read"]`, and a
short `ttl`.

- Before `/p` accepts, `/p` sees `/s` as `NOT_FOUND`, and its facade
  as 403.
- After accepting, `/p` creates and deletes an instance in `silo-<s>`.
  The instance is `/s`'s, charged to `/s`'s quota, and the audit
  record is `/s`'s with `via=manage:<p's id>`.
- Out of scope: a subnet or a firewall rule in `/s` (403 naming the
  grant), a `PATCH` of `/s`'s quota, a child of `/s`, a member mapped
  to `/s`, and a `PATCH` of `/s`'s `control` (`PERMISSION_DENIED`),
  and a `manage` offer on `/s` made by `/p`.
- Offering `/s` a grant on itself, or one of its descendants a grant
  on it, is `INVALID_ARGUMENT`.
- Revoking: an in-flight `terraform apply` from `/p` gets 403 on its
  next call after the revoke, and nothing it hadn't started appears.
  Expiry behaves the same, and a renewal past `renew_until` is
  refused.
- A sealed holder: sealing `/p` stops its calls on `/s` as well.

**Agents in a sealed silo**

`/` creates an agent in `/s` (sealed), with a tool VM.

- The create works, and the tool VM's tool server attaches on port 80.
- The harness's `exec` runs on the tool VM; the model proxy answers;
  `ask` puts the agent in `WAITING`, and a person answers it. A
  message sent to the agent from `/` starts a turn.
- The harness's `silo_create_child`, `agent_create` and `compute`
  (an `instances.insert`) are refused, as is `silo_describe`, and
  nothing is created. Under `read` instead of `none`, `silo_describe`
  and a `compute` `list` work and the rest are refused.
- An `exec` of `gcloud compute instances list` against
  `silo.internal:8080` times out.
- A `manage` holder with the `agents` scope renews and stops the agent;
  one without it gets `PERMISSION_DENIED`.

**Quota**

- An instance that exceeds the silo's `vcpu` returns `quotaExceeded`,
  as do a stopped instance's `start` and a disk past `ssd_disk_gb`.
- A child's grant can't exceed the parent's free amount.
- A GPU instance charges its GPU class (and the Spot variant for a
  Spot instance), as `quota:preview` and `describe` show, and one more
  past the silo's grant in that class returns `quotaExceeded`. A GPU
  instance without `onHostMaintenance: TERMINATE`, or with an
  accelerator type the zone doesn't offer, is refused before anything
  is charged. Creating a real GPU instance needs GPU quota in
  `bsalmon-fun-sandbox`. Without it, the case checks the charge with
  `quota:preview` and the refusal for real, and reports the rest as
  skipped, not passed.
- A `hyperdisk-balanced` disk's `disks.update` raising
  `provisionedIops` charges the increase to `hyperdisk_balanced_iops`,
  is refused past the grant, and lowering it refunds the difference.

**Instance templates**

- A template in `/a` naming `/b`'s subnetwork, image or disk, by any
  name form, is refused at create, and so is one with a refused field.
- An instance made from `/a`'s template is checked again and charged:
  after the template's region is removed from `/a`'s allowed set, or
  its subnet deleted, the insert is refused; and an insert whose body
  adds a refused field over a valid template is refused.
- `/b` can't use `/a`'s template as `sourceInstanceTemplate`, and
  doesn't see it in `list`.

**Still refused**

- A disk in a Hyperdisk storage pool (`storagePool`), and
  `storagePools.insert`.
- `reservationAffinity` other than `NO_RESERVATION`, and
  `reservations.insert`.
- `scheduling.nodeAffinities`, and `nodeGroups.insert`.
- `machineImages.insert` and `instanceGroupManagers.insert`.

**The fence matches the evaluator**

- For a generated tree, the conformance run probes a sample of
  (source, destination, port) triples on real VMs, and checks that
  each result matches what the fake's fence evaluator predicts.

The suite reports each case by name, with a pass rate per case kept
in grain's test history, so a flaky case is visible.
