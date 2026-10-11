# `silo install` and `silo uninstall`

## Status

Normative draft, part of the Silo design ([`README.md`](README.md)),
written in task 1930. Marks such as "call 18" refer to
[Calls made in this draft](README.md#calls-made-in-this-draft).

bsalmon, in task 1913: "we will need a silo install command that sets
everything up given root project credentials. i think this will be
significant code."

## Synopsis

```sh
silo install --project bsalmon-fun-sandbox --regions us-central1,europe-west4 \
    --admin user:bsalmon@example.com [flags]
silo install ... --dry-run
silo uninstall --project bsalmon-fun-sandbox [--force] [--delete-data] [--dry-run]
```

## Input {#SV-90}

| Flag | Default | Meaning |
| --- | --- | --- |
| `--project` | required | The host project. It should be dedicated to Silo. |
| `--regions` | required | The root's allowed regions. The first is the **home region**, where the control VM, gateway subnet and NAT live. |
| `--admin` | required on first install | The first root member (`user:` or `serviceAccount:`). It can be repeated. |
| `--credentials` | ADC | A service-account key file. Without it, Application Default Credentials. |
| `--address-space` | `10.0.0.0/8` | The shared VPC's range, which becomes the root's block. |
| `--gateway-range` | `10.255.255.0/24` | The gateway subnet's range, inside `--address-space`, held back from the root. |
| `--control-range` | `10.255.254.0/24` | The control subnet's range, for the database's PSC endpoint. Inside `--address-space`, held back from the root, and never reachable from a silo ([`state.md`](state.md#the-control-subnet)). |
| `--root-memory` | unlimited | The root's `memory_mb` and `spot_memory_mb` per region (GCE has no memory quota). |
| `--egress-allow` | empty | The root's egress allowlist. |
| `--root-grant-policy` | `exports=allow,links=deny` | The root's grant policy (call 14). |
| `--version` | the CLI's own | The silod release to deploy. |
| `--image` | the release's | A silod container image to deploy instead, by digest. For development. |
| `--control-machine-type` | `e2-standard-2` | The control VM's machine type. With `--agent-sessions`, pick a larger one ([`agents.md`, Capacity](agents.md#10-capacity-of-the-control-vm)). |
| `--control-replicas` | 1 | Control VMs (silod and a gateway each). Above 1, a regional MIG behind an internal passthrough NLB ([`state.md`](state.md#more-than-one-silod-and-the-leader-call-34)). |
| `--agent-sessions` | 0 | How many agent harness sessions the control VM may run at once. It is the root's `agent_sessions` count. 0 turns agents off. |
| `--root-model-budget` | 0 | The root's model budget, in US dollars per period ([`allocators.md`, Model budget](allocators.md#model-budget)). |
| `--budget-period` | `month` | The model budget period for the whole tree: `month`, `week` or `day`, in UTC. |
| `--gateway-replicas` | 0 | Extra gateway VMs behind an internal passthrough NLB. |
| `--db-tier` | `db-g1-small` | The Cloud SQL machine tier (call 33). `db-custom-1-3840` for one with an SLA. |
| `--db-version` | `POSTGRES_17` | The Cloud SQL database version. |
| `--db-ha` | off | A regional (HA) instance with a standby in a second zone, instead of a zonal one. It roughly doubles the database's cost. |
| `--db-backup-retention` | 14 | How many daily backups Cloud SQL keeps. PITR keeps 7 days of logs either way. |
| `--db-maintenance-window` | `sun:05:00` (UTC) | When Cloud SQL may restart the instance for maintenance. |
| `--db-instance` | none | Adopt an existing instance (for example one restored from a backup) instead of creating one ([`state.md`, Backups and recovery](state.md#backups-and-recovery)). |
| `--front-door-member` | none | Extra identities granted the IAP tunnel to the front door. Root members always are. |
| `--audit-sink` | off | Also stream audit records to Cloud Logging. |
| `--dry-run` | off | Print the plan and change nothing. |
| `--yes` | off | Don't ask for confirmation before applying. |

The credentials need, on the host project, `roles/owner`, or this
set: `roles/serviceusage.serviceUsageAdmin`,
`roles/compute.admin`, `roles/dns.admin`, `roles/iam.serviceAccountAdmin`,
`roles/iam.roleAdmin`, `roles/resourcemanager.projectIamAdmin`,
`roles/artifactregistry.admin`, `roles/iap.admin`, `roles/cloudsql.admin`,
and `roles/browser`. `roles/cloudsql.admin` is for creating the
database instance, its database and its IAM user. silod's own role
can only connect to the database ([below](#silods-service-account)).
They also need `billing.resourceAssociations.list` to check billing.
Install checks this with `testIamPermissions` before it does anything
else. These credentials are used only by install. silod never sees
them.

## Approach: Go, not terraform (call 18) {#SV-91}

grain deploys itself with terraform (`deploy/terraform/gcp`). For
`silo install`, Go with the GCP client libraries was chosen instead:

- **It reuses silod's own code.** The fence compiler, the address
  allocator, the quota reader and the reconcile loop exist in silod
  anyway. Install has to make the first fence, compute the root's
  space and read the root's quota. In terraform each of these would be
  written a second time, in HCL or an external data source, and the
  two copies would drift.
- **The root silo is a database row, not a cloud object.** Install
  writes silod's first state (the root, its capacity, the first admin).
  Terraform has no natural way to do that.
- **Prerequisite checks with clear messages.** Billing, quota headroom
  and the firewall and VPC limits need reading and arithmetic before
  anything is created. In Go they are ordinary code with ordinary
  error messages, not `precondition` blocks.
- **No state backend to bootstrap.** Terraform needs a state bucket,
  which has to exist before the first apply, and which an operator can
  lose. Install finds its objects by name instead (call 21).
- **One binary.** `silo` is already a Go CLI. Shipping terraform and a
  module with it would add a tool and a version to manage for every
  user.

The cost is that Go has no `plan` for free. Install has to build its
own plan and diff, which is the "significant code" bsalmon expected.
Every step is written as a small reconciler with three parts:

```go
type Step interface {
    Name() string
    Observe(ctx) (Observed, error)   // read the cloud; never changes anything
    Plan(Observed) []Change          // create, update, ok, delete; pure
    Apply(ctx, []Change) error       // make the changes; safe to repeat
}
```

`--dry-run` runs `Observe` and `Plan` for every step and prints the
result. silod's reconcile loop is built from the same kind of step, so
install and silod share code for the shared VPC, the fence and the
quota reader.

## What install sets up {#SV-92}

Each step finds its object by fixed name. Everything it creates has the
label `silo-install=<install-id>`, or the description prefix
`silo-install:` where GCE has no labels. Install runs the steps in this
order:

| # | Step | Creates or ensures |
| --- | --- | --- |
| 1 | `prereqs` | Nothing. Runs the [checks](#prerequisite-checks). |
| 2 | `apis` | Enables `compute`, `dns`, `iam`, `iap`, `artifactregistry`, `cloudresourcemanager`, `serviceusage`, `logging`, `sqladmin`, `cloudquotas`, and `cloudbilling` (read only). |
| 2a | `db-instance` | Starts creating the Cloud SQL instance `silo-db-<suffix>` ([settings](state.md#the-cloud-sql-instance-call-33)): no public IP, PSC on, IAM authentication on, backups, PITR and deletion protection on. It is found by its labels, not by name. This step doesn't wait. Creation takes several minutes, so it runs early and overlaps the steps after it. |
| 3 | `service-account` | The service account `silod@<project>.iam.gserviceaccount.com`. |
| 4 | `role` | The custom role `projects/<project>/roles/silod` ([below](#silods-service-account)), bound to `silod`. Also `roles/logging.logWriter` and `roles/artifactregistry.reader`. |
| 5 | `network` | The custom-mode VPC `silo`, with global routing and `networkFirewallPolicyEnforcementOrder: BEFORE_CLASSIC_FIREWALL`. |
| 6 | `gateway-subnet` | `silo-gateway`: `--gateway-range` in the home region, with Private Google Access on. |
| 6a | `control-subnet` | `silo-ctl`: `--control-range` in the home region, with Private Google Access off. |
| 7 | `nat` | The Cloud Router `silo-router` and Cloud NAT `silo-gateway-nat`, for `silo-gateway` only. |
| 8 | `fence` | The firewall policy `silo-fence` with its gateway and catch-all rules, associated with `silo`. It holds no silo rules yet: the root's are added in step 13. Until then, `catch-*` denies everything outside the gateway. |
| 9 | `address` | A static internal address `silo-gateway-ip` in `silo-gateway`. |
| 10 | `dns` | The private zone `silo-internal` for `silo.internal.`, visible to `silo`, with the A record `silo.internal.` → `silo-gateway-ip`. |
| 11 | `image` | The Artifact Registry repository `silo` in the home region, and the silod image copied into it by digest (call 19). |
| 11a | `db` | Waits for the instance from step 2a. Creates the database `silo` and the IAM user `silod@<project>.iam`, and makes that user the database's owner ([`state.md`, Authentication](state.md#authentication-iam-no-password)). |
| 11b | `db-endpoint` | The static address `silo-db-psc` in `silo-ctl`, the PSC forwarding rule `silo-db-psc` to the instance's service attachment, and the private DNS zone `silo-db` with the instance's PSC DNS name pointing at that address. |
| 11c | `migrate` | Runs the image's schema migrations: a short-lived VM `silo-migrate` in the gateway subnet, with the `silod` service account, runs `silod migrate` and reports through a guest attribute. Install then deletes the VM ([`state.md`, Schema migrations](state.md#schema-migrations-and-upgrades-call-35)). |
| 12 | `control` | The VM `silo-control`: Container-Optimized OS, the `silod` service account, no external IP, at `silo-gateway-ip`, with the image digest in its metadata. It has no state disk. With `--control-replicas` above 1, an instance template and a regional MIG of control VMs instead, with a health check, behind an internal passthrough NLB with global access. Then, with `--gateway-replicas`, the gateway instance template, MIG, health check and internal passthrough NLB with global access. With either NLB, the DNS record moves to its address. |
| 13 | `root` | Waits for silod to be healthy, then calls silod's local bootstrap endpoint (over the IAP tunnel, with the installer's identity) to create the root silo. Its block is `--address-space` with `--gateway-range` and `--control-range` held back. Its regions are `--regions`. Its capacity is [read from the project's quotas](allocators.md#root-capacity). The bootstrap endpoint accepts this call only while there is no root. |
| 14 | `admin` | Maps each `--admin` to the root, and grants them `roles/iap.tunnelResourceAccessor` on `silo-control`. |
| 15 | `verify` | From the installer, through the front door: `describe /`, and a facade `regions.list`. It prints the root's regions, blocks and quota. |

Every step is **idempotent**: if the object exists and matches, the
step is `ok`; if it exists and differs in a field install owns, it is
updated; if it is missing, it is created. Every step is **resumable**:
an interrupted install is finished by running it again with the same
flags. A run that changes a flag that can't be changed in place
(`--address-space`, `--gateway-range` or `--control-range` once the
root exists, or the home region) stops at `prereqs` and says so.
`--db-tier`, `--db-ha`, `--db-backup-retention` and
`--db-maintenance-window` are changed in place. A tier or HA change
restarts the instance, and the plan says so. `--db-version` only goes
up, as a Cloud SQL major-version upgrade.

With `--agent-sessions`, step 12 also gives the control VM what
harnesses need: the control image carries the pinned harness CLIs
(`claude-code` and `codex`), and the control VMs' metadata gets a
generated key, `silo-credential-key`, for the agent credentials kept in
the database. Session state and transcripts are kept in the database
too, so storage auto-increase covers them. Agent credentials aren't
install's: root members add them afterwards with
`silo credential add` and `silo credential login`
([`agents.md`, Credentials](agents.md#6-credentials)).

Re-running install with a newer `--version` is how silod is upgraded.
Step 11 copies the new image. Step 11c applies the new release's
migrations while the old release keeps running. Migrations are
expand-then-contract, so the old release still works on the new
schema. Step 12 then updates the control VMs' metadata and restarts
their containers one VM at a time, and rolls the gateway MIG. silod
never migrates at start-up. It refuses to serve a schema outside the
range its release supports
([`state.md`, How a running fleet upgrades](state.md#schema-migrations-and-upgrades-call-35)). Running agent
sessions on each control VM are stopped by its restart and resumed
from their saved state
([`agents.md`, silod restarts](agents.md#silod-restarts)).

### silod's service account {#SV-93}

The custom role `silod` (call 20) holds only what silod's facade,
fence and reconcile need:

- instances: `compute.instances.{create,delete,get,list,start,stop,reset,suspend,resume,attachDisk,detachDisk,setLabels,setMetadata,setTags,setMachineType,setDeletionProtection,getSerialPortOutput,setScheduling}`;
- disks: `compute.disks.{create,delete,get,list,resize,update,setLabels,use,useReadOnly,createSnapshot}`;
- instance templates: `compute.instanceTemplates.{create,delete,get,list,useReadOnly}`, `compute.regionInstanceTemplates.{create,delete,get,list,useReadOnly}`;
- images and snapshots: `compute.images.{create,delete,get,list,getFromFamily,setLabels,deprecate,useReadOnly}`, `compute.snapshots.{create,delete,get,list,setLabels,useReadOnly}`;
- networking: `compute.subnetworks.{create,delete,get,list,update,expandIpCidrRange,use}`, `compute.networks.{get,updatePolicy,use}`, `compute.firewalls.{create,delete,get,list,update}`, `compute.firewallPolicies.{get,update}` for the `silo-fence` policy;
- operations and reads: `compute.{zoneOperations,regionOperations,globalOperations}.{get,list}`, `compute.{regions,zones,machineTypes,diskTypes,acceleratorTypes}.{get,list}`, `compute.projects.get`, and `cloudquotas.quotas.{get,list}` for the quotas `regions.get` doesn't report;
- its own control VM: `compute.instances.setMetadata` (already above), to hand certificates to gateway replicas;
- its database: `cloudsql.instances.connect`, `cloudsql.instances.get` and `cloudsql.instances.login`, for the Cloud SQL Go connector and IAM database login. Install also makes the account the IAM database user `silod@<project>.iam` ([`state.md`](state.md#authentication-iam-no-password)).

It has no IAM, DNS, service-account or project-metadata permissions.
It has no Cloud SQL administration permissions either: it can't
change, back up, restore or delete its database instance.
`compute.instances.setServiceAccount` is missing on purpose, and
creating an instance with no service account needs no `actAs`.

## Prerequisite checks {#SV-94}

`prereqs` runs every check and reports them all, not just the first
failure. Each failure prints what is wrong, why install needs it, and
the command or console page that fixes it.

| Check | Fails when | Message says |
| --- | --- | --- |
| Credentials | `testIamPermissions` lacks a permission from [Input](#input) | which permissions are missing, and the role that has them |
| Billing | `cloudbilling` reports `billingEnabled: false` | to link a billing account, with the console URL |
| Regions | A region doesn't exist, or doesn't offer the control machine type in any zone | which region, and which machine types it does offer |
| Quota headroom | The home region's `CPUS`, `INSTANCES`, `STATIC_ADDRESSES` or `SSD_TOTAL_GB` can't fit the control plane (the control VMs and gateway replicas), or a region has `CPUS` of 0 | the quota, its limit and usage, what install needs, and the quota page to raise it |
| Agent capacity | `--agent-sessions` is more than `--control-replicas` VMs of `--control-machine-type` can carry (0.2 vCPU and 0.5 GB per session, plus 1 vCPU and 2 GB per VM for silod and the gateway). A warning, not a failure, if they couldn't carry it with one VM down | the machine type or replica count that would carry it |
| VPC limits | `NETWORKS` has no room for `silo`; `SUBNETWORKS` or `FIREWALLS` has less than 10% left; `ROUTERS` has no room | the limit and what install and the tree will use |
| Firewall policy limits | The project's network firewall policy quota has no room, or the per-policy rule-attribute quota is below what a 100-silo tree needs (500 rules) | the quota, and the size of tree it allows |
| Address space | `--address-space` overlaps an existing subnet in another VPC of the project, `--gateway-range` or `--control-range` isn't inside it, the two overlap, or any is badly formed | the overlapping subnet |
| Cloud SQL | `sqladmin` can't be enabled; the home region doesn't offer `--db-tier` or `--db-version`; or the project's Cloud SQL instance quota has no room | what is missing, and the tiers and versions the region does offer |
| PSC | The project has a parent organization, and `compute.disablePrivateServiceConnectCreationForConsumers` or `compute.restrictPrivateServiceConnectProducer` would forbid the database's endpoint. A project with no organization, like bsalmon's, has no such policy and passes | which constraint. Private services access is the documented alternative, but v1 doesn't build it ([open question 16](README.md#open-questions)) |
| Existing install | Objects with `silo-install` labels from a different `--address-space` or home region | which flag differs, and that `silo uninstall` comes first |
| Dedicated project | The project already has instances outside silo | a warning, not a failure: they share the project's quota |
| Project metadata | `commonInstanceMetadata` has keys other than GCE's own | a warning: every silo's VMs can read project metadata ([gcp-backend.md](gcp-backend.md#what-leaks-between-silos)) |
| Org policy | The project has a parent organization whose constraints would block a step: for example `iam.disableServiceAccountCreation`, or `gcp.resourceLocations` excluding one of `--regions` | which constraint, and which step it blocks |

The limits install reads come from the Compute and Service Usage
quota APIs, where Google exposes them. Where it doesn't, install uses
the documented default and says so in the plan.

The `root` step reads the root's capacity the same way silod does every
hour ([`allocators.md`, Root capacity](allocators.md#root-capacity)):
`regions.get` for the classes it reports, including local SSD,
`pd-extreme` IOPS, Hyperdisk Balanced and the GPU metrics it lists, and
the Cloud Quotas API for the rest (newer GPU families, and Hyperdisk
Extreme, Throughput and ML). So the `apis` step also enables
`cloudquotas.googleapis.com`. A class whose limit is 0, or that
neither API reports, gives the root no capacity, and the plan says so
for each GPU and Hyperdisk class rather than leaving it out. GPU and
Hyperdisk limits aren't a prerequisite: a project with none still
installs, and the tree just can't use them until the operator raises
the quota.

## Plan output {#SV-95}

`--dry-run` (and, before asking for confirmation, every install)
prints the plan:

```
silo install: project bsalmon-fun-sandbox, home region us-central1

  prereqs           ok      billing on; CPUS us-central1 24/96 used; NETWORKS 1/15
  apis              enable  compute.googleapis.com, dns.googleapis.com, iap.googleapis.com,
                            sqladmin.googleapis.com, cloudquotas.googleapis.com
  db-instance       create  silo-db-x4kq POSTGRES_17 db-g1-small zonal us-central1
                            (takes several minutes; install continues meanwhile)
  service-account   create  silod@bsalmon-fun-sandbox.iam.gserviceaccount.com
  role              create  projects/bsalmon-fun-sandbox/roles/silod (41 permissions)
  network           create  silo (custom, global routing, firewall policy first)
  gateway-subnet    create  silo-gateway 10.255.255.0/24 us-central1
  control-subnet    create  silo-ctl 10.255.254.0/24 us-central1
  ...
  db                create  database silo, IAM user silod@bsalmon-fun-sandbox.iam
  db-endpoint       create  silo-db-psc in silo-ctl, zone silo-db
  migrate           apply   schema 0 -> 7
  ...
  root              create  / blocks 10.0.0.0/8 (minus 10.255.254.0/23)
                            us-central1 vcpu 94, ssd_disk_gb 2000, local_ssd_gb 6000,
                            nvidia_l4_gpus 0 (no quota), ...; europe-west4 vcpu 24, ...
  admin             create  user:bsalmon@example.com -> /

  20 steps: 17 to create or apply, 0 to update, 3 ok
  Estimated time: about 10 minutes, most of it waiting for Cloud SQL.
```

`--dry-run --output json` prints the same as JSON, one object per
step.

## `silo uninstall` {#SV-96}

Uninstall removes, in reverse order, everything install created:

1. **Tenant objects.** If any silo other than the root exists, or the
   root has facade objects or agents, uninstall stops and lists them,
   unless `--force` is given. With `--force`, it asks silod to delete the
   root's subtree (the same as `silo delete --force` on each child),
   waits for that, then deletes the root's own objects.
2. **Orphans.** Any object with a silo label or prefix that silod
   didn't know about, with `--force`.
3. **Agent credentials.** silod deletes every credential from the
   database, and install removes `silo-credential-key` from the control
   VMs' metadata, before anything below. A kept database, or a final
   backup, then holds no credentials, and older backups hold them only
   encrypted with a key that is gone.
4. **Install's objects**, by their `silo-install` label, in reverse step
   order: the admin IAM bindings, the gateway MIG and NLB, the control
   VMs, the PSC endpoint and the `silo-db` zone, the image repository,
   the DNS zone, the static address, the fence policy,
   NAT and router, the control and gateway subnets, the VPC, the role
   binding, the role and the service account.
5. **The database.** By default uninstall **keeps** the Cloud SQL
   instance, with its deletion protection and backups, and says so. It
   is all that remains of the tree's grants, policies and audit. With
   `--delete-data`, uninstall lifts deletion protection on that
   instance only, then deletes it with a final backup kept for 30
   days. It names the instance in its confirmation prompt (`--yes`
   skips the prompt, as CI needs). Without `--delete-data` it never
   touches deletion protection. The database's IAM user goes with the
   instance. A later install finds a kept instance by its labels and
   stops at `prereqs`. It asks for either `--db-instance <name>`, to
   adopt the instance and its data, or `silo uninstall --delete-data`
   first.

It is idempotent and resumable like install. `--dry-run` prints its
plan. It doesn't disable APIs (open question 11), and it says which
APIs it leaves on.

## Testing install {#SV-97}

Install is tested two ways ([`testing.md`](testing.md)):

- **Unit tests** of each step's `Plan` against recorded `Observe`
  results, including every partly-installed state, so that
  "interrupted after step `n`, re-run" is covered for every `n`.
- **A real install and uninstall** in the conformance run, against
  `bsalmon-fun-sandbox`. The run installs, runs the conformance suite,
  uninstalls, and checks that no labelled object is left.
