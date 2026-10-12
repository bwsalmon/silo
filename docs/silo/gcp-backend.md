# Silo v1 GCP backend: subnet-backed, in one host project

## Status

Normative draft, part of the Silo design ([`README.md`](README.md)),
written in task 1930. The control policy (sealed silos) and `manage`
grants were added in task 1947. Marks such as "call 9" refer to
[Calls made in this draft](README.md#calls-made-in-this-draft).

This backend implements the `subnet` backing. `vpc` and `project` are
[later](README.md#5-backings-and-their-trade-offs).

## Layout {#SV-56}

Everything lives in one **host project** (for testing,
`bsalmon-fun-sandbox`), which `silo install` sets up
([`install.md`](install.md)). The host project should be dedicated to
Silo. Install warns if it already holds instances.

| Object | Name | What it is |
| --- | --- | --- |
| Shared VPC | `silo` | A custom-mode, global-routing VPC. `networkFirewallPolicyEnforcementOrder` is `BEFORE_CLASSIC_FIREWALL`, so the fence runs before tenant rules. |
| Gateway subnet | `silo-gateway` | The gateway's range (default `10.255.255.0/24`) in the home region (the first `--regions` entry). Private Google Access is on. |
| Control subnet | `silo-ctl` | The control range (default `10.255.254.0/24`) in the home region, held back from the root like the gateway range. It holds the database's PSC endpoint and no VMs, and no silo can reach it ([`state.md`](state.md#the-control-subnet)). |
| Cloud NAT | `silo-gateway-nat` | On a Cloud Router in the home region, for the gateway subnet only (call 17). |
| Fence policy | `silo-fence` | A global network firewall policy, associated with `silo` and owned by silod. |
| DNS zone | `silo-internal` | A Cloud DNS private zone for `silo.internal.`, visible to `silo`. Its A record points at the gateway's address. |
| Control VM | `silo-control` | Runs silod and the gateway. Its address in the gateway subnet is static. With `--control-replicas`, a regional MIG of such VMs. |
| Database | `silo-db-<suffix>` | Cloud SQL for PostgreSQL, silod's state, with no public IP ([`state.md`](state.md#the-cloud-sql-instance-call-33)). |
| Database endpoint | `silo-db-psc` | A PSC endpoint (address and forwarding rule) for the database, in `silo-ctl`, and the private DNS zone `silo-db` that names it. |
| Gateway MIG and ILB (optional) | `silo-gateway-*` | With `--gateway-replicas`, a MIG of gateways behind an internal passthrough NLB with global access. The NLB's address is then `silo.internal`. |
| Tenant subnets | `<id>-<rest>` | Regional subnets of `silo`, inside the owning silo's blocks. The tenant names them so ([Naming](#naming)). |
| Tenant objects | `<id>-<rest>` | Instances, disks, images, snapshots, firewall rules and instance templates, labelled `silo=<id>` where GCE has labels. |

Every object silod makes for a tenant carries the labels `silo=<id>` and
`silo-managed=true`, or, where GCE has no labels (subnets, firewall
rules, instance templates), the prefix `<id>-` and a description
beginning `silo:<id>`.

## The fence

### Shape {#SV-57}

The fence is the firewall policy `silo-fence`. silod owns it and
compiles it from the database. Since the VPC's enforcement order is
`BEFORE_CLASSIC_FIREWALL`, its rules are evaluated before the tenant's
VPC firewall rules. A rule's action is one of:

- `allow` or `deny`: final, and the tenant's rules are never consulted;
- `goto_next`: hand the packet on to the tenant's VPC firewall rules,
  and so on to GCE's implied rules (deny ingress, allow egress).

For each silo `n` with blocks `B(n)`, the fence has these rules
(call 10):

| Rule | Direction | Match | Action |
| --- | --- | --- | --- |
| `n-intra-in` | ingress | src ∈ `B(n)`, dst ∈ `B(n)` | `goto_next` |
| `n-intra-out` | egress | src ∈ `B(n)`, dst ∈ `B(n)` | `goto_next` |
| `n-gw-out` | egress | src ∈ `B(n)`, dst ∈ gateway range, TCP 80, 3128 and the export ports | `allow` |
| `n-api-out` | egress | src ∈ `B(n)`, dst ∈ gateway range, TCP 8080 (the API port). **Omitted when `n`'s effective control policy is `none`** | `allow` |
| `n-deny-in` | ingress | dst ∈ `B(n)` | `deny` |
| `n-deny-out` | egress | src ∈ `B(n)` | `deny` |

There are also these rules, which aren't per silo:

| Rule | Direction | Match | Action |
| --- | --- | --- | --- |
| `gw-in` | ingress | dst ∈ gateway range, src ∈ the root's range, TCP 80, 8080, 3128 and the export ports | `allow` |
| `gw-admin-in` | ingress | dst ∈ gateway range, src `35.235.240.0/20` (IAP), the front-door port | `allow` |
| `gw-health-in` | ingress | dst ∈ gateway range, src Google health-check ranges | `allow` (only with `--gateway-replicas`) |
| `gw-out` | egress | src ∈ gateway range | `goto_next` (GCE's implied allow-egress then applies). This is how silod reaches the database endpoint in the control range. |
| `ctl-deny-out` | egress | src ∈ the root's block (the address space minus the gateway and control ranges), dst ∈ control range, all protocols | `deny` |
| `catch-in` | ingress | any | `deny` |
| `catch-out` | egress | any | `deny` |

Grants add rules of their own, at the highest priorities:

| Rule | Direction | Match | Action |
| --- | --- | --- | --- |
| `export-<x>-in` | ingress | src ∈ gateway range, dst = the export's `ip`, its port | `allow` |
| `link-<l>-in` / `-out` | both | src ∈ `B(a)`, dst ∈ `B(b)`, the link's protocols and ports (and the reverse, for a two-way link) | `allow` |

### Ordering (call 9) {#SV-58}

A child's blocks lie inside its parent's, so the parent's `intra`
rules match the child's packets too. Rules are therefore ordered
**deepest silo first**. A packet that touches a child's space is
decided by the child's rules, whose `deny` rules stop it before it
reaches the parent's `goto_next`. Within one depth, silos' spaces are
disjoint, so order between them doesn't matter.

Priorities are assigned in bands:

| Band | Priority range | Holds |
| --- | --- | --- |
| Control | 100–999 | `ctl-deny-out` |
| Grants | 1000–9999 | `export-*`, `link-*` |
| Gateway | 10000–19999 | `gw-*` |
| Depth 16 … depth 0 | from 20000 + (16 − depth) × 10000 | each silo's six rules (five when sealed), in the order listed above. Each rule keeps its slot whether or not `n-api-out` is present, so sealing and unsealing move nothing else |
| Catch-all | 2147483000 and up | `catch-*` |

The gateway band comes before the silo bands because the gateway's
range lies inside the root's block, so the root's `deny` rules would
otherwise match traffic to it. The control band comes before
everything, so that no grant or gateway rule, however written, can
open the database's endpoint to a silo
([`state.md`, What the fence does](state.md#what-the-fence-does)). Tree depth is capped at 16, with the
root at depth 0. silod
recompiles the policy when a silo, block, grant or effective control
policy changes. It applies the difference rule by rule: adds before
deletes, and for a new silo its `deny` rules before its `intra` rules,
so that no intermediate state is looser than either end.

### Sealed silos {#SV-59}

The API port has a rule of its own, `n-api-out`, so that the
fence can take the control plane away from a silo and leave the rest
of the gateway alone ([`gateway.md`, Control policy](gateway.md#control-policy)).
For a silo whose effective control policy is `none`, silod leaves the
rule out. Its packets to TCP 8080 then match nothing until its own
`n-deny-out`, and are dropped. Ports 80, 3128 and the export ports are
still allowed by `n-gw-out`, so egress and exports work as before.

Rules are ordered deepest first, so a sealed silo's `n-deny-out` is
reached before its parent's `n-api-out`, whose source ranges cover the
sealed silo's blocks too. Every descendant of a sealed silo is sealed
as well, so no deeper rule reopens the port.

Sealing deletes one rule per silo in the subtree, and unsealing adds
them back. Either is a single step per rule, and the gateway's own
refusal (layer 1) is in place before the first rule changes, so no
intermediate state lets a sealed silo make a control call. A `read`
silo keeps `n-api-out`.

Reconcile treats a missing or extra `n-api-out` like any other drift
([Reconcile](#reconcile)): it repairs it and audits `fence_drift`.

### Why `goto_next` inside a silo

Inside a silo, connectivity is the tenant's to control
([principle 6](README.md#2-principles)). `goto_next` hands intra-silo
packets to the tenant's own VPC firewall rules, which the facade has
pinned to the silo ([Firewall rules](#firewall-rules)). With no tenant
rule, GCE's implied ingress deny applies, exactly as in a fresh GCE
network.

A grant is different. Both sides have agreed to it, so it is `allow`
at the fence, and neither tenant has to write a rule for it.

### Rule budget {#SV-60}

A global network firewall policy has a quota on its rules and on
their attributes (ranges, ports, tags). Each silo costs six rules (five
when sealed), plus roughly `5 × blocks` range attributes (`4 × blocks`
when sealed). Each link costs two to four
rules, and each export one. `silo install` reads the policy quota and
records it. silod refuses a change that would push the compiled policy
past 90% of it with `RESOURCE_EXHAUSTED`, naming the quota. This is
the hard limit on how big a subnet-backed tree can grow, and the
reason blocks are capped ([`allocators.md`, Growth](allocators.md#growth)).

### What the fence does not cover

- **The metadata server** (`169.254.169.254`) is not subject to
  firewall rules. Every VM can reach it. See
  [What leaks between silos](#what-leaks-between-silos).
- **Google APIs.** Tenant subnets have Private Google Access off (the
  facade refuses to turn it on), and `catch-out` denies their egress to
  Google's ranges anyway. Their VMs have no service account, so they
  have no token to use.
- **Spoofing** is stopped by GCE, not by the fence. A VM can send only
  from its own addresses and alias ranges unless `canIpForward` is
  set, and the facade refuses `canIpForward`.

## The `compute/v1` facade

### Address and shape {#SV-61}

```
http://silo.internal:8080/compute/v1/projects/<project>/...
```

The facade is served on the gateway's API port
([`gateway.md`, Address](gateway.md#address)).

The paths, query parameters, request bodies, responses and errors
follow GCE's `compute/v1` REST API. So:

- `gcloud` works with
  `gcloud config set api_endpoint_overrides/compute http://silo.internal:8080/compute/v1/`
  and `gcloud config set auth/disable_credentials true`;
- terraform's `google` provider works with
  `compute_custom_endpoint = "http://silo.internal:8080/compute/v1/"` and any
  `access_token` (the facade ignores `Authorization`).

From outside the cloud, the same paths are served through the front
door ([`gateway.md`](gateway.md#front-door)).

### Projects and the caller {#SV-62}

The `<project>` segment names a silo. It may be:

- `silo-<id>`, the silo's own facade project ID;
- the **host project's ID**, which always means the caller's own silo
  (call 2). Every VM's metadata server reports the host project ID, so
  tools that discover their project work unchanged.

A caller may read and write:

- its own silo;
- a descendant's `silo-<id>` (call 13, revised in task 1947: ancestors
  may write, not only read). The objects belong to the descendant and
  are charged to its quota. The audit record is the descendant's, with
  `via=ancestor:<id>`;
- the `silo-<id>` of the target of an active
  [`manage` grant](fence-api.md#manage-grants) it holds, or of one of
  the target's descendants, on the resource types the grant's scope
  names. A type outside the scope, or a write under `access: read`,
  gets 403 (`forbidden`, `silo: manage grant rollout does not cover
  disks`). The audit record is the target's, with
  `via=manage:<holder id>`.

Any other project gets GCE's 403 (`forbidden`, "Required permission ...
for project"), whether or not the silo exists, so that nothing is
learned about silos outside the caller's subtree.

Every call is then cut down by the **caller's** effective control
policy ([`fence-api.md`, Control policy](fence-api.md#control-policy)).
Under `read`, only `get`, `list`, `aggregatedList`, `getFromFamily`,
`getSerialPortOutput` and operation `get`, `list` and `wait` are
allowed, in any project the caller may read. Under `none`, nothing
is. The target's own policy doesn't matter: a sealed silo's facade is
written to by its ancestors and `manage` holders, which is the point
of sealing it.

`projects.get` returns a synthesized `Project`: `name` and `id` are
the silo's, `quotas` are the silo's [global counts](allocators.md#global-counts),
and `commonInstanceMetadata` is empty. `regions.get` returns the
region's silo quotas in GCE's `quotas` shape (`CPUS`,
`PREEMPTIBLE_CPUS`, `INSTANCES`, `DISKS_TOTAL_GB`, `SSD_TOTAL_GB`,
`LOCAL_SSD_TOTAL_GB`, `PREEMPTIBLE_LOCAL_SSD_GB`, each GPU metric such
as `NVIDIA_L4_GPUS` and `PREEMPTIBLE_NVIDIA_L4_GPUS`,
`PD_EXTREME_TOTAL_PROVISIONED_IOPS`, the Hyperdisk metrics such as
`HDB_TOTAL_GB`, plus `SILO_MEMORY_MB` and `SILO_PREEMPTIBLE_MEMORY_MB`;
[`allocators.md`, Classes](allocators.md#classes)), with
`limit = granted − delegated` and `usage = used`.

### Default-deny {#SV-63}

The facade is an allowlist at three levels:

1. **Methods.** A method not in the tables below returns 403
   (`forbidden`, "silo: method not allowed").
2. **Resource types.** Any type not below is unknown to the facade:
   404 for its paths.
3. **Fields.** Each allowed write method has an allowlist of request
   fields, by JSON path. A field outside it that is set to anything
   but its default returns 403 (`forbidden`), and the message names the
   field (`silo: field networkInterfaces[0].accessConfigs is not
   allowed`). A field GCE itself doesn't know returns GCE's 400.

Responses are allowlisted too. The facade builds each response from
the fields it knows, rewritten, so that nothing about the host project
leaks through a field it forgot.

### Covered {#SV-64}

Read-only, filtered to allowed regions:

| Resource | Methods |
| --- | --- |
| `regions`, `zones` | `get`, `list` |
| `machineTypes`, `diskTypes`, `acceleratorTypes` | `get`, `list`, `aggregatedList` |
| `images` in public image projects (`debian-cloud`, `ubuntu-os-cloud`, `cos-cloud`, `rocky-linux-cloud`, `fedora-coreos-cloud` and others on a list in silod's config) | `get`, `getFromFamily`, `list` |
| `zoneOperations`, `regionOperations`, `globalOperations` | `get`, `wait`, `list` (the caller's own) |

Read and write, in the caller's silo:

| Resource | Methods | Notes |
| --- | --- | --- |
| `instances` | `insert`, `get`, `list`, `aggregatedList`, `delete`, `start`, `stop`, `reset`, `suspend`, `resume`, `attachDisk`, `detachDisk`, `setLabels`, `setMetadata`, `setTags`, `setMachineType`, `setDeletionProtection`, `getSerialPortOutput` | See [Instances](#instances) |
| `disks` | `insert`, `get`, `list`, `aggregatedList`, `delete`, `resize`, `update`, `setLabels`, `createSnapshot` | See [Disks](#disks) (call 22) |
| `images` | `insert`, `get`, `getFromFamily`, `list`, `delete`, `setLabels`, `deprecate` | Sources: the silo's own disks, images and snapshots, or a public image. No `rawDisk`. |
| `snapshots` | `get`, `list`, `delete`, `setLabels` | If cheap enough to build in v1. They are made with `disks.createSnapshot`. |
| `networks` | `get`, `list` | One network, `default` (call 12) |
| `subnetworks` | `insert`, `get`, `list`, `aggregatedList`, `delete`, `patch`, `expandIpCidrRange` | See [Subnetworks](#subnetworks) |
| `firewalls` | `insert`, `get`, `list`, `delete`, `patch`, `update` | See [Firewall rules](#firewall-rules) |
| `instanceTemplates`, `regionInstanceTemplates` | `insert`, `get`, `list`, `delete` | See [Instance templates](#instance-templates) (call 22) |
| `projects` | `get` | Synthesized, as above |

### Refused {#SV-65}

These return 403 with a message naming the method or the field:

- routes, external addresses (`addresses` of type `EXTERNAL`),
  `accessConfigs` and `ipv6AccessConfigs` on interfaces, routers and
  NAT, network peering, VPN and interconnect, PSC, load balancers and
  forwarding rules (later);
- `canIpForward: true`;
- `serviceAccounts` on instances, unless empty;
- `projects.setCommonInstanceMetadata`, `projects.setUsageExportBucket`
  and every other `projects` write;
- OS Login: the instance metadata keys `enable-oslogin`,
  `enable-oslogin-2fa` and `enable-windows-ssh`;
- the instance metadata key `block-project-ssh-keys` set to anything but
  `true` (the facade sets it on every instance), and `silo-name-prefix`
  set to anything but the silo's prefix ([Naming](#naming));
- `networks.insert`, `delete`, `patch`, `addPeering`, `switchToCustomMode`;
- `subnetworks.setPrivateIpGoogleAccess`, and `privateIpGoogleAccess:
  true`;
- `reservationAffinity` other than `NO_RESERVATION`, and
  `reservations`. A reservation is project-wide, so one silo's instances
  could consume capacity that another silo reserved (later: reservations
  as objects a silo owns, used only by its own instances);
- sole tenancy: `scheduling.nodeAffinities`, `nodeGroups` and
  `nodeTemplates`. These are project-level and expensive (later);
- `machineImages`. A machine image is global and captures an
  instance's metadata and disks (later, with naming and visibility
  rules like [instance templates](#instance-templates)'s);
- Hyperdisk storage pools: `storagePools`, and `storagePool` on a
  disk. A pool is capacity shared at the project level (later);
- managed instance groups (`instanceGroupManagers`,
  `regionInstanceGroupManagers`) and autoscalers. A MIG creates and
  autoscales instances itself, outside the facade, so neither quota
  charging nor the instance checks would see them. Later, with a
  different mechanism: for example, silod watching the MIG's instances,
  and the facade capping `targetSize` × the template's cost against
  quota;
- `sourceInstance` and `sourceInstanceParams` on instance templates
  (later);
- `firewalls` with `targetServiceAccounts` or `sourceServiceAccounts`.

### Instances {#SV-66}

On `instances.insert`, the facade:

1. checks the zone's region is allowed;
2. resolves the machine type and charges `vcpu` and `memory_mb`
   (`spot_vcpu` and `spot_memory_mb` if
   `scheduling.provisioningModel` is `SPOT`), plus `instances`, plus the
   boot and attached disks' classes, plus any [GPUs](#gpus) and
   [local SSD](#local-ssd) ([`allocators.md`](allocators.md#regional-quota));
3. checks every `networkInterfaces[]`: `network` is `default` or
   omitted; `subnetwork` is one of the silo's (it is rewritten to the
   real one); `networkIP`, if given, is in that subnet; every
   `aliasIpRanges[]` is inside the subnet's ranges. There are no
   access configs;
4. checks every disk source: a new disk from an allowed image, or an
   existing disk of the silo's, in the same zone;
5. checks [GPUs](#gpus), [local SSD](#local-ssd) and the disk types'
   [machine-type compatibility](#disks);
6. allows `advancedMachineFeatures.enableNestedVirtualization`,
   `shieldedInstanceConfig`, `scheduling` (`SPOT`, `STANDARD`,
   `onHostMaintenance`, `automaticRestart`,
   `instanceTerminationAction`), `labels`, `tags`, `metadata` (except
   the refused keys), `description`, `minCpuPlatform` and
   `deletionProtection`, `guestAccelerators` and local SSD `disks[]`
   entries;
7. writes the real instance: the name is the tenant's, which carries
   the silo's prefix ([Naming](#naming)), as must each tag; the tags
   get `<id>` added, the labels get `silo=<id>` and
   `silo-managed=true`, the metadata gets `block-project-ssh-keys: true`
   and `silo-name-prefix: <id>-`, and `serviceAccounts` is empty.

The tenant never sees the `silo` labels or the `<id>` tag. The facade
strips them from responses, and refuses a tenant label key or tag that
starts with `silo`.

`setTags` and `setLabels` are rewritten the same way. `setMetadata`
applies the same key rules. `setMachineType` re-prices the instance,
including the GPUs and local SSD its machine type bundles, and checks
the new machine type against its GPUs and disks.

`instances.insert` with `sourceInstanceTemplate` is described under
[Instance templates](#instance-templates).

### GPUs {#SV-67}

`guestAccelerators` is allowed (call 22). Each entry charges its
accelerator type's GPU class, or the Spot variant if
`scheduling.provisioningModel` is `SPOT` or `scheduling.preemptible` is
true, for as long as the instance isn't `TERMINATED`
([`allocators.md`, GPU classes](allocators.md#gpu-classes)). A machine
type that bundles GPUs (A2, A3, G2 and their kin, whose
`machineTypes.get` lists `accelerators[]`) charges the same classes
from the machine type, with no `guestAccelerators` given.

Before charging anything, the facade checks what GCE would check, so
that the error comes back clean and GCE-shaped (400, `invalid`, naming
the field) rather than as a failed operation after the charge:

- `scheduling.onHostMaintenance` is `TERMINATE`;
- `acceleratorType` names a type that `acceleratorTypes.get` offers in
  the instance's zone, and the zone is in an allowed region (403,
  `regionNotAllowed`, as for any zone);
- the machine type is compatible: for attached GPUs, a machine series
  that takes them (N1), and a count GCE allows for that type with that
  machine type; for a machine type that bundles its GPUs, no
  `guestAccelerators` at all.

silod keeps GCE's documented compatibility table in its config. A
combination it misses gets GCE's own refusal, passed through, and the
charge refunded ([`allocators.md`, Root capacity](allocators.md#root-capacity)).

### Local SSD {#SV-68}

Local SSD is allowed (call 22): a `disks[]` entry of `type: SCRATCH`
with `initializeParams.diskType: local-ssd` and `interface` `NVME` or
`SCSI`, or a machine type that bundles it (the `-lssd` types, Z3 and
the like). It charges `local_ssd_gb` (`spot_local_ssd_gb` for a Spot
instance): 375 GB per attached partition, or what the machine type
bundles, for as long as the instance exists. `instances.stop` allows
`discardLocalSsd`.

Local SSD data is lost when the instance stops (with
`discardLocalSsd`), is preempted, or its host fails. That is the
tenant's concern, not the fence's: a local SSD is attached to one
instance of the silo's own and is never reachable from any other.

### Disks {#SV-69}

`disks.insert`, and a new disk in `instances.insert`, may use these
types (call 22):

| Type | Charges ([`allocators.md`, Classes](allocators.md#classes)) | Settable |
| --- | --- | --- |
| `pd-standard` | `standard_disk_gb` | |
| `pd-balanced`, `pd-ssd` | `ssd_disk_gb` | |
| `pd-extreme` | `ssd_disk_gb`, and `pd_extreme_iops` for `provisionedIops` | `provisionedIops` |
| `hyperdisk-balanced` | `hyperdisk_balanced_gb`, `hyperdisk_balanced_iops`, `hyperdisk_balanced_throughput` | `provisionedIops`, `provisionedThroughput` |
| `hyperdisk-extreme` | `hyperdisk_extreme_gb`, `hyperdisk_extreme_iops` | `provisionedIops` |
| `hyperdisk-throughput` | `hyperdisk_throughput_gb`, `hyperdisk_throughput_throughput` | `provisionedThroughput` |
| `hyperdisk-ml` | `hyperdisk_ml_gb`, `hyperdisk_ml_throughput` | `provisionedThroughput`, `accessMode` |

Where the tenant doesn't give `provisionedIops` or
`provisionedThroughput`, the facade charges GCE's default for the
type and size, and the response shows the value GCE set.

- **Machine-type compatibility.** Hyperdisk attaches only to the
  machine series GCE supports it on (C3, C4, N4, M3, A3 and others), and
  some of those series take no `pd-*` disks. `pd-extreme` attaches only
  to the machine types GCE lists for it. The facade checks every disk
  against the instance's machine type, on `instances.insert`,
  `attachDisk` and `setMachineType`, from the same table as GPUs, and
  refuses a mismatch with 400 (`invalid`).
- **`disks.update`** allows `provisionedIops`, `provisionedThroughput`
  and `accessMode`, with GCE's `paths` parameter, and nothing else. A
  change in IOPS or throughput charges the increase before calling
  GCE, and refunds a decrease when the operation succeeds, like any
  charge ([`allocators.md`, Charging and refunds](allocators.md#charging-and-refunds)).
  GCE's own limit on how often a disk's performance may change is
  passed through.
- **`disks.resize`** charges the GB difference to the type's capacity
  class. For a Hyperdisk, GCE may raise the provisioned IOPS or
  throughput to the new size's minimum; the facade charges that too.
- **Multi-writer and read-only-many** Hyperdisk (`accessMode`) attach
  only to the silo's own instances, like any disk.
- **`storagePool`** on a disk is refused: see [Refused](#refused).

### Instance templates {#SV-70}

Global `instanceTemplates` and regional `regionInstanceTemplates` are
allowed (call 22). Their names are prefixed and mapped back like any
other object's ([Naming](#naming)), and `list` returns only the silo's
own.

**At `insert`**, the template's `properties` go through every instance
rule above, as if they were an instance: refused fields and metadata
keys, `network` and `subnetwork` in the silo, regions allowed, disk
sources (images and disks) the silo's own or public, GPUs, local SSD and
disk types. So a template can't name another silo's subnetwork, image
or disk. For a regional template, its region must be allowed. For a
global one, every region its properties name (through `subnetwork` or a
disk source) must be. The real template is written rewritten as an
instance would be: names and tags checked for the silo's prefix, the
`<id>` tag, the `silo` labels in `properties.labels`,
`block-project-ssh-keys` and `silo-name-prefix`, and no service
accounts. The facade maps its links back on `get` and `list`.

**Templates charge no regional quota**: a template is not an instance.
Each counts once in the global count `instance_templates`, because GCE
caps them per project ([`allocators.md`, Global counts](allocators.md#global-counts)).

**At `instances.insert` with `sourceInstanceTemplate`**, the template
must be the silo's own. The facade merges the request body over the
template's `properties` as GCE does, then checks the *effective*
instance with every rule in [Instances](#instances), and charges its
quota. A template that was valid when made is checked again, because
the allowed regions or the silo's subnets may have changed since. silod
then calls GCE with the real template's URL and the whole effective
instance in the body, so that GCE's own merge can add nothing silod
didn't check.

Managed instance groups stay refused ([Refused](#refused)).

### Subnetworks {#SV-71}

The facade's network is `default`, a fiction over the silo's own
space in the shared VPC.

- `subnetworks.insert` runs the [address checks](allocators.md#checks-the-facade-makes),
  charges `subnetworks`, and creates the subnet, named `<id>-<rest>`
  by the tenant, in `silo`. It allows
  `name`, `description`, `region`, `ipCidrRange`, `secondaryIpRanges`,
  `logConfig`, `stackType: IPV4_ONLY` and `purpose: PRIVATE`.
- `patch` allows changing `secondaryIpRanges` (with the same checks),
  `logConfig` and `description`.
- `expandIpCidrRange` checks the new range.
- `get` and `list` return `network` as the facade's `default` and
  `region` as given. `gatewayAddress` is GCE's, which is inside the
  tenant's range.

`networks.get` on `default` returns a `Network` with
`autoCreateSubnetworks: false`, `routingConfig.routingMode: GLOBAL`,
`subnetworks` listing the silo's own, the silo's blocks in the
read-only field `siloAddressBlocks` (a GCE client ignores unknown
fields), and the same blocks in `description`.

### Firewall rules {#SV-72}

Tenant firewall rules become real VPC firewall rules in `silo`, pinned
to the silo (call 11). On `insert`, `patch` and `update`, the facade:

- refuses `targetServiceAccounts` and `sourceServiceAccounts`;
- refuses (400, `invalid`) a `targetTags` or `sourceTags` entry
  without the silo's prefix `<id>-`, as it does a name; a tenant tags
  its instances with prefixed tags too. If `targetTags` is empty, it
  sets `targetTags: [<id>]`, which every instance of the silo carries
  and which responses drop. So a rule can only ever target the silo's
  own instances;
- for `INGRESS`, intersects `sourceRanges` with the silo's own space,
  and refuses the rule (400, `invalid`) if that leaves nothing when
  something was asked for. `0.0.0.0/0` thus means "anywhere in this
  silo";
- for `EGRESS`, intersects `destinationRanges` the same way;
- keeps `priority`, `allowed`, `denied`, `disabled` and `logConfig`;
- charges `firewall_rules`.

The facade returns the rule as the tenant wrote it. It records the
original next to the real rule, so that `get` doesn't show the
intersected ranges; the tags it shows are the real rule's, less
`<id>`.

Since the fence runs first and passes only intra-silo packets on, a
tenant rule can't open anything across silos, even when it is wrong.
The pinning keeps it from changing what another silo's VMs receive.
Without it, a rule with no target would apply to every instance in
the VPC.

### Operations {#SV-73}

GCE operations are rewritten like any other object: name, `targetLink`,
`selfLink` and `user` (always `silo:<id>`, the id of the silo that made
the call, which is an ancestor's or a `manage` holder's when one of
them started it). `get` and `wait` on an operation the caller didn't
start return 404, except to callers who may read the facade it was
started in. silod watches the
operations it started, so that it can [refund](allocators.md#charging-and-refunds)
quota on failure and finish its own bookkeeping.

### Lists and filters {#SV-74}

`list` and `aggregatedList` ask GCE for the silo's objects only (by the
`silo=<id>` label, or by name prefix where GCE has no labels), then
apply the tenant's `filter`, `orderBy`, `maxResults` and `pageToken`
in the facade. silod implements the filter grammar `gcloud` and
terraform send (comparisons, `AND`, `OR`, `NOT`, `:` on lists and
labels, regular expressions with `eq` and `ne`). An expression outside
that grammar returns 400 (`invalid`). Page tokens are the facade's own
and opaque. Call 26: filtering in the facade, rather than rewriting
the tenant's filter and passing it to GCE, means a filter can never
reach another silo's objects.

### Errors {#SV-75}

Errors are GCE-shaped:

```json
{"error": {"code": 403, "message": "Quota 'CPUS' exceeded. Limit: 32.0 in region us-central1 (silo /team-a).",
  "errors": [{"domain": "usageLimits", "reason": "quotaExceeded", "message": "..."}]}}
```

| Case | HTTP | `reason` |
| --- | --- | --- |
| A quota class doesn't have room | 403 | `quotaExceeded`. An operation that fails later carries the code `QUOTA_EXCEEDED`. |
| Refused method or field | 403 | `forbidden` |
| The caller's control policy forbids the call (`none`, or a write under `read`) | 403 | `forbidden`, with the message `silo: control-plane calls from /prod are refused (control=none)` |
| A `manage` grant doesn't cover the resource type or the access | 403 | `forbidden`, naming the grant |
| Region or zone not allowed | 403 | `regionNotAllowed` |
| Project outside the caller's reach | 403 | `forbidden` |
| Range outside the silo's own space, name without the silo's prefix (the message names it) or one GCE refuses, filter not supported | 400 | `invalid` |
| Object not found (or another silo's) | 404 | `notFound` |
| Name already used in this silo | 409 | `alreadyExists` |
| silod unavailable | 503 | `backendError` |

### Naming {#SV-76}

A tenant names each object by its real name. Every object in the
collections the tenant names (instances, disks, images, snapshots,
subnetworks, firewall rules and instance templates) is named
`<id>-<rest>`, where `<id>` is the silo's id, for example
`k7f3qa-builder` (call 1, revised in task 3024). The name is used
unchanged everywhere: in requests and responses, in the host project,
as the guest's hostname, in GCE's internal DNS and on the metadata
server. There is one name per object.

The facade checks names. It doesn't rewrite them:

- A name without the silo's prefix, or with another silo's, is 400
  (`invalid`), and the message says what the prefix must be:
  `Invalid value for field 'name': 'builder'. silo: names in this
  silo must start with "k7f3qa-"`. That holds for a name in a request
  body, in a path, and in a link the request names an object by,
  whether bare, a partial path or a full URL. A full URL may use
  `silo.internal`, `www.googleapis.com` or `compute.googleapis.com`.
  A `get`, `delete` or other call naming an object by a bare name is
  refused the same way rather than given 404, so a caller that still
  holds a bare name learns the prefix.
- The limit is GCE's own: a name GCE refuses, longer than 63
  characters or not a match of
  `[a-z]([-a-z0-9]{0,61}[a-z0-9])?`, is 400 (`invalid`), with GCE's
  message. The prefix counts, since the whole name is the tenant's.
- Network tags on instances and in firewall rules' `targetTags` and
  `sourceTags` must carry the prefix too ([Firewall
  rules](#firewall-rules)).
- A caller writing another silo's facade, an ancestor or a `manage`
  holder ([Projects and the caller](#projects-and-the-caller)), names
  that silo's objects, so uses that silo's prefix: through
  `projects/silo-<target>`, the names must start with `<target>-`.
- Names in public image projects pass through unchecked.

The facade still maps projects and the network. The host project is
`silo-<id>` in every response, link and message; a link the facade
writes is on its own endpoint, for example
`http://silo.internal:8080/compute/v1/projects/silo-k7f3qa/zones/<z>/instances/k7f3qa-builder`;
and the shared VPC is `default` (call 12).

`list` and `aggregatedList` show only the silo's own objects, by its
label or name prefix ([Lists and filters](#lists-and-filters)), so
a filter such as `name eq k7f3qa-builder` is on the real name.

A tenant learns its prefix without guessing:

- from the 400 above;
- from the silo object, as `facade.name_prefix`
  ([`fence-api.md`, Silo](fence-api.md#silo)), which `silo describe`
  prints as `Name prefix:`;
- on any instance, from the metadata key `silo-name-prefix`, which the
  facade sets on every instance it makes or whose metadata it sets
  (a tenant may send the key, but only with that value);
- on an agent's tool VM, from `SILO_NAME_PREFIX` in the environment of
  the commands `silo-tools` runs, and from the agent's `compute` tool
  description ([`agents.md`, Tools](agents.md#tools)).

Before task 3024 the facade gave each name a hidden prefix: a tenant's
`builder` was the real `k7f3qa-builder`, and the facade mapped names
both ways. Nothing in GCE changes, since the real names were already
`<id>-…`; only the facade's view does. A caller that stored a bare
name gets the 400 with the prefix, and uses the real name from then
on. Records silod keeps by name, firewall originals and the space
map's subnet ranges, stay keyed by the name less the prefix, so those
an earlier release wrote are still found. An agent's tool VM is now
`<id>-agent-<name>`; an agent made before keeps its record of
`agent-<name>`, which silod prefixes when it reaches the VM.

### Where the real names show through

There is one name, so nothing shows through: the name the tenant gives
is the guest's hostname (`hostname` on `k7f3qa-builder` prints
`k7f3qa-builder`), its GCE internal DNS name
(`k7f3qa-builder.us-central1-a.c.<host-project>.internal`), the name
on the metadata server and in serial console and guest-agent logs.
What still shows through is not a name:

- **The metadata server** reports the host project ID and numeric
  project number, and `instance/zone` in the host project, where the
  facade says `silo-<id>`.
- **Numeric `id` fields** are GCE's. They are unique and harmless, but
  they are not stable across a delete and a re-create with the same
  name, which is also true in GCE.

### What leaks between silos

Firewall rules stop traffic between silos. They can't stop these:

- **Project-wide metadata.** Every VM can read the host project's
  `commonInstanceMetadata` from the metadata server. So the host
  project's metadata must hold nothing secret. Install checks this
  and warns, and silod never writes to it.
- **GCE internal DNS.** Every VM can resolve every other instance's
  real name to its internal IP (open question 6). The name and the
  address leak, but no traffic flows.
- **The database endpoint's name.** The PSC DNS name in the `silo-db`
  zone resolves to the control range from every VM. The fence denies
  traffic to it.
- **Quota exhaustion** of a GCE quota that isn't handed down
  ([`allocators.md`, Root capacity](allocators.md#root-capacity)).

## Reconcile {#SV-77}

silod treats its database as the truth for the tree, allocations,
grants and the fence. It treats GCE as the truth for tenant objects.

- **When a replica becomes leader, and every ten minutes**, the
  leader silod ([`state.md`](state.md#more-than-one-silod-and-the-leader-call-34)) lists every labelled or
  prefixed object in the host project. It recomputes each silo's
  `used` from what exists, and compiles the fence again and applies
  the difference.
- An object with a prefix or label for a silo that doesn't exist is an
  **orphan**. silod reports it in the audit log and deletes it after a
  grace period of one hour (call 27), so that a half-finished delete
  completes.
- An object in the VPC with neither label nor prefix, other than
  install's own, is **foreign**. silod reports it and leaves it alone.
- Any difference between the compiled fence and the real policy is
  repaired at once, and audited as `fence_drift`.

## silod's own placement

silod and the gateway run on `silo-control`
([`gateway.md`, Placement](gateway.md#placement),
[`install.md`](install.md)). silod's database is the Cloud SQL instance
`silo-db-<suffix>` in the home region, reached through the PSC endpoint
`silo-db-psc` in the control subnet, with IAM login as silod's service
account. It has daily backups kept for 14 days and point-in-time
recovery. This replaces the `silo-state` disk and its snapshot schedule
of call 16 ([`state.md`](state.md)).
