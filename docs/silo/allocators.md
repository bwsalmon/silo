# Silo allocators: quota, regions and address space

## Status

Normative draft, part of the Silo design ([`README.md`](README.md)),
written in task 1930. Marks such as "call 7" refer to
[Calls made in this draft](README.md#calls-made-in-this-draft).

Four allocators are handed down the silo tree:

| Allocator | What a parent hands a child | Scope | Fungible? |
| --- | --- | --- | --- |
| [Regional quota](#regional-quota) | An amount of each quota class in each region | Per region, plus a few [global counts](#global-counts) | Yes |
| [Allowed regions](#allowed-regions) | A subset of the parent's regions | Global policy | Not an amount |
| [Address space](#address-space) | One or more aligned CIDR blocks carved from the parent's | Global | No |
| [Model budget](#model-budget) | Dollars of model use per budget period, for its agents | Global | Yes |

The same rules hold for all four:

- **A child can never be looser than its parent.** Every change to a
  child's allocation is checked against what the parent has, and
  refused if it doesn't fit.
- **The parent decides.** A child asks for more through its parent.
  There is no request queue in v1; the parent's member, or a process
  in the parent silo, makes the call.
- **Every change is atomic and audited.** silod makes it in one
  serializable database transaction
  ([`state.md`](state.md#what-silod-relies-on-the-store-for)) and writes an audit record
  ([`fence-api.md`, Audit](fence-api.md#audit)).
- **Deleting a silo returns everything to its parent.** That includes
  its children's allocations, because deleting a silo deletes its
  subtree.

## Regional quota

GCE's own quotas are regional, so silo quota is per region too.

### Classes {#SV-27}

| Class | Unit | Charged by | Root capacity (from the host project) |
| --- | --- | --- | --- |
| `vcpu` | vCPU | each standard instance not `TERMINATED`: its machine type's `guestCpus` | `CPUS` in the region, minus the control plane's own use |
| `memory_mb` | MiB | each standard instance not `TERMINATED`: its machine type's `memoryMb` | unlimited, unless install is given `--root-memory` (call 4) |
| `spot_vcpu` | vCPU | each Spot instance not `TERMINATED` | `PREEMPTIBLE_CPUS` in the region (see [Open question 2](README.md#open-questions)) |
| `spot_memory_mb` | MiB | each Spot instance not `TERMINATED` | unlimited, unless `--root-memory` is given |
| `instances` | count | each instance, in any state | `INSTANCES` in the region |
| `standard_disk_gb` | GB | each `pd-standard` disk, for as long as it exists | `DISKS_TOTAL_GB` in the region |
| `ssd_disk_gb` | GB | each `pd-balanced`, `pd-ssd` or `pd-extreme` disk, for as long as it exists | `SSD_TOTAL_GB` in the region |
| `pd_extreme_iops` | IOPS | each `pd-extreme` disk's `provisionedIops`, for as long as it exists | `PD_EXTREME_TOTAL_PROVISIONED_IOPS` in the region |
| `hyperdisk_balanced_gb`, `hyperdisk_balanced_iops`, `hyperdisk_balanced_throughput` | GB, IOPS, MiB/s | each `hyperdisk-balanced` disk's size, `provisionedIops` and `provisionedThroughput`, for as long as it exists | `HDB_TOTAL_GB`, `HDB_TOTAL_IOPS`, `HDB_TOTAL_THROUGHPUT` in the region |
| `hyperdisk_extreme_gb`, `hyperdisk_extreme_iops` | GB, IOPS | each `hyperdisk-extreme` disk's size and `provisionedIops` | GCE's Hyperdisk Extreme capacity and IOPS quotas in the region |
| `hyperdisk_throughput_gb`, `hyperdisk_throughput_throughput` | GB, MiB/s | each `hyperdisk-throughput` disk's size and `provisionedThroughput` | GCE's Hyperdisk Throughput capacity and throughput quotas in the region |
| `hyperdisk_ml_gb`, `hyperdisk_ml_throughput` | GB, MiB/s | each `hyperdisk-ml` disk's size and `provisionedThroughput` | GCE's Hyperdisk ML capacity and throughput quotas in the region |
| `local_ssd_gb` | GB | each standard instance's local SSD, 375 GB per partition or what its machine type bundles, for as long as the instance exists | `LOCAL_SSD_TOTAL_GB` in the region |
| `spot_local_ssd_gb` | GB | the same, for each Spot instance | `PREEMPTIBLE_LOCAL_SSD_GB` in the region |
| one class per GPU type, and its Spot variant | GPU | see [GPU classes](#gpu-classes) | the GPU type's metric in the region |

A parent grants an amount per class per region, for example
`us-central1: vcpu=32, memory_mb=131072, ssd_disk_gb=1000`. A class
it doesn't name is zero.

`pd-extreme` capacity goes to `ssd_disk_gb` because GCE counts it in
`SSD_TOTAL_GB`. Each class follows the GCE metric that limits it, so a
charge silod accepts is one GCE has room for (call 22). Updating a
disk's provisioned IOPS or throughput charges or refunds the difference
([`gcp-backend.md`, Disks](gcp-backend.md#disks)).

The usage clock follows GCE's (call 6). A stopped instance holds no
vCPU or memory, so `instances.start` is checked like `insert` and can
fail with `QUOTA_EXCEEDED`. A suspended instance holds them, because
GCE keeps its memory.

### GPU classes {#SV-28}

GCE limits GPUs per region by accelerator type, with a separate
metric for Spot (preemptible) use. The classes mirror those metrics:
the metric's name in lower case, with `PREEMPTIBLE_` written `spot_`
like the other Spot classes.

| GCE metric | Class | Accelerator types |
| --- | --- | --- |
| `NVIDIA_T4_GPUS` / `PREEMPTIBLE_NVIDIA_T4_GPUS` | `nvidia_t4_gpus` / `spot_nvidia_t4_gpus` | `nvidia-tesla-t4` |
| `NVIDIA_L4_GPUS` / `PREEMPTIBLE_NVIDIA_L4_GPUS` | `nvidia_l4_gpus` / `spot_nvidia_l4_gpus` | `nvidia-l4` (G2 bundles it) |
| `NVIDIA_A100_GPUS` / `PREEMPTIBLE_NVIDIA_A100_GPUS` | `nvidia_a100_gpus` / `spot_nvidia_a100_gpus` | `nvidia-tesla-a100` (A2 bundles it) |
| `NVIDIA_A100_80GB_GPUS` / `PREEMPTIBLE_NVIDIA_A100_80GB_GPUS` | `nvidia_a100_80gb_gpus` / `spot_nvidia_a100_80gb_gpus` | `nvidia-a100-80gb` (A2 ultra) |
| … | … | V100, P100, P4, the `_VWS_` workstation variants, and newer families |

The mapping from accelerator type to metric is a table in silod's
config, since GCE's names for the two differ. A class exists only for
a metric the host project has in the region. Newer families that
`regions.get` doesn't report, such as A3's H100, are read
through the Cloud Quotas API instead ([Root capacity](#root-capacity)).

- **Charged by** each instance that isn't `TERMINATED`: the count of
  each `guestAccelerators[]` entry, or the GPUs its machine type
  bundles (A2, A3, G2). A Spot instance charges the `spot_` class.
- **Root capacity** is the metric's limit in the region. If the Spot
  metric is 0, GCE charges Spot GPUs to the standard metric, and v1
  gives the root no Spot GPU capacity, as for Spot vCPUs
  ([Open question 2](README.md#open-questions)).
- **Every GPU also counts once** in the global count `gpus_all_regions`
  ([Global counts](#global-counts)).

The facade's checks on a GPU instance (`onHostMaintenance`, the zone,
the machine type) are in [`gcp-backend.md`, GPUs](gcp-backend.md#gpus).

### Zones {#SV-29}

Zonal resources (instances and disks) charge the quota of their
zone's region. Zones need no allocation: a silo with quota in
`us-central1` may use any of its zones.

### Global counts {#SV-30}

Some GCE quotas are per project, not per region, and every silo in the
host project shares them. If nothing handed them down, one silo could
exhaust them for the whole tree. So they form a small global class
(call 5):

| Class | Charged by | Root capacity |
| --- | --- | --- |
| `subnetworks` | each subnet | `SUBNETWORKS`, minus install's own |
| `firewall_rules` | each tenant VPC firewall rule | `FIREWALLS`, minus install's own |
| `images` | each image | `IMAGES` |
| `snapshots` | each snapshot | `SNAPSHOTS` |
| `image_storage_gb` | each image's `archiveSizeBytes`, rounded up | unlimited unless set |
| `snapshot_storage_gb` | each snapshot's `storageBytes`, rounded up | unlimited unless set |
| `instance_templates` | each instance template, global or regional. Templates charge no regional quota | `INSTANCE_TEMPLATES` |
| `gpus_all_regions` | each GPU of an instance that isn't `TERMINATED`, in any region, standard or Spot | `GPUS_ALL_REGIONS` |
| `agent_sessions` | each agent that is `RUNNING`, `IDLE` or `WAITING` | `--agent-sessions` at install, default 0 |

`agent_sessions` isn't a GCE quota. It counts harness sessions on the
control VM, which is a resource the whole tree shares, so it is handed
down like one. The harnesses' CPU and memory aren't charged to any
quota class in v1 (call 49, [`agents.md`, Capacity](agents.md#10-capacity-of-the-control-vm)).

The fence's own firewall-policy rules are not charged to any silo.
They are bounded by the [block cap](#growth) and by tree depth.

### Hard partition {#SV-31}

Quota is a hard partition, not a set of limits (call 3). For each
class and region, silod keeps three numbers for every silo:

- `granted`: what its parent gave it, which is its capacity. For the
  root, it is the root capacity above.
- `delegated`: the sum of its children's `granted`.
- `used`: what its own objects are charged.

The invariant is `delegated + used ≤ granted` in every silo, class
and region, at all times. So:

- a facade call that would charge `n` succeeds only if
  `used + n ≤ granted − delegated` in the caller's silo. Ancestors need
  no check, because the child's grant was already reserved out of
  theirs;
- raising a child's grant by `n` succeeds only if the parent has `n`
  free (`granted − delegated − used ≥ n`);
- lowering a child's grant to `g` succeeds only if
  `g ≥ child.delegated + child.used`. silod never deletes objects to
  make room.

### Room for custom allocators {#SV-32}

Custom allocators, served by a service in a silo and charged through
this same ledger, are designed but come after v1
([`custom-allocators.md`](custom-allocators.md)). The regional quota
above is then one allocator kind, `gce`, owned by the root. So that
custom kinds can be added later without a migration, **v1's ledger
must already**:

- **key quota by data, not by a fixed enum.** A quota row is
  `(silo, kind, class, region)`, all strings. `region` is `global` for
  the [global counts](#global-counts). The classes in the tables above
  are rows of a class table (kind, name, unit, sort, scope), seeded
  with kind `gce`. Nothing switches on a fixed list of classes, and no
  table has a column per class;
- **record a payer separate from the home** on every charge, with
  `via` and the object charged for. In v1 the payer is always the home
  and `via` is `facade`. Refunds, and the [reconcile](gcp-backend.md#reconcile)'s
  recount of `used`, add charges up by payer;
- **keep `granted` as a sum of allocation edges** (`from`, `to`,
  kind, class, region, amount). In v1 the only edge into a silo is
  from its parent;
- **leave room for metered classes:** a `sort` and a `period` on each
  class, a `consumed` amount on each quota row (0 in v1), and amounts stored as integer millionths of the unit, or in a
  type that holds them;
- **keep the [model budget](#model-budget) in the same ledger,** as
  kind `model`, class `usd`, with the deployment's budget period,
  rather than in a table of its own.

The list, with the fence API's side of it, is in
[`custom-allocators.md`, What v1 must already allow](custom-allocators.md#what-v1-must-already-allow).

### Root capacity {#SV-33}

At install, and then every hour, silod reads the host project's
regional quotas (`regions.get` → `quotas`) and its project quotas
(`projects.get` → `quotas`) for the allowed regions. It sets the root's
`granted` to the limit, minus what the control plane's own objects use.

Most limits come from `regions.get`: `CPUS`, `SSD_TOTAL_GB`,
`LOCAL_SSD_TOTAL_GB`, `PREEMPTIBLE_LOCAL_SSD_GB`,
`PD_EXTREME_TOTAL_PROVISIONED_IOPS`, the `HDB_TOTAL_*` metrics, and
each GPU metric the region has. The rest come from the Cloud Quotas
API (`cloudquotas.googleapis.com`), which reports every Compute quota:
the GPU families `regions.get` leaves out, and the Hyperdisk Extreme,
Throughput and ML quotas. A metric that neither reports, or that
reports 0, gives the root no capacity in that class. silod discovers
the GPU metrics: a new family appearing in the project gets a class on
the next hourly read.

If GCE's limit drops below what the tree already holds, the root is
over-committed. silod records this, reports it in `describe` and the
audit log, and refuses new charges and grants in that class and region
until it is back under. It reclaims nothing.

A GCE quota refusal that silod didn't predict (a per-family quota like
`N2_CPUS`, or another project's usage counting against a shared limit)
is passed through to the tenant as GCE's own error. silod refunds the
charge.

### Charging and refunds {#SV-34}

A facade call that creates or grows an object charges the quota
before silod calls GCE, in the same transaction that checks it. If the
GCE operation fails, silod refunds the charge when it sees the failure.
The [reconcile](gcp-backend.md#reconcile) recomputes `used` from the
objects that exist, so a crash between the charge and the call costs
nothing for long.

### Preview {#SV-35}

`quota:preview` ([`fence-api.md`](fence-api.md#quota-preview)) takes a
facade request or an allocation change and returns, without doing
anything, what it would charge to which classes and regions, and the
free amount at each level from the caller up to the root.

## Model budget {#SV-36}

Agents spend money on model calls, so model use is an allocator too
([`agents.md`, Model budget and metering](agents.md#7-model-budget-and-metering)).

- **Unit**: US dollars per budget period, kept as micro-dollars
  (call 46). The period is one for the whole deployment, set by
  `silo install --budget-period` (`month` by default: calendar months
  in UTC; or `week`, or `day`).
- **Hard partition**, like quota (call 3). Each silo has `granted`,
  `delegated` (its children's `granted`) and `used`, and
  `delegated + used ≤ granted` always. Here `used` is the sum of its
  own agents' budgets: an agent's `budget.usd` is reserved when the
  agent is created or its budget raised, and returned when it is
  deleted.
- **Spend is per agent.** Each agent's `spent_usd` is metered against
  its own budget, and can't go past it by more than one subscription
  turn ([`agents.md`, Metering](agents.md#metering)). At the start of
  each period every `spent_usd` goes back to 0. Grants and
  reservations carry over.
- **The root's budget** is `--root-model-budget` at install, default
  0. Unlike quota, it isn't read from anywhere: the provider's own
  billing limits are separate, and an operator should set both.
- **Changes** follow the same rules as quota: raising a child's grant
  needs the parent's free amount, and lowering it below
  `delegated + used` fails with `FAILED_PRECONDITION`. Lowering never
  stops a running agent.

## Allowed regions {#SV-37}

A child's allowed regions are a policy that its parent hands down:
always a subset of the parent's regions, chosen for residency or cost.

- The root's regions are the ones `silo install --regions` was given.
- The facade refuses any call that names a region, or a zone in a
  region, outside the caller's set. It does this with a 403
  (`regionNotAllowed`) rather than GCE's 404, so that a tenant can
  tell "not allowed" from "doesn't exist". `regions.list` and
  `zones.list` show only the allowed set.
- Quota can only be granted in an allowed region. Removing a region
  from a child fails with `FAILED_PRECONDITION` if the child or any of
  its descendants still holds quota, or objects, in that region.
- Adding a region to a child grants it no quota. A child may be
  allowed a region and have no quota there.

Images and snapshots are global objects. Their storage location is
set by GCE from where the source disk is, unless `storageLocations` is
given. The facade allows `storageLocations` only when every location
named is an allowed region or the multi-region that contains one
(call 25: the multi-region rule, so that `us` works for a silo
allowed `us-central1`).

## Address space

### Blocks and subnets {#SV-38}

- **Blocks are global**, because a VPC's address space is global.
- **Subnets are regional**, as in GCE: one subnet spans every zone in
  its region, so traffic across zones needs no extra design. A silo
  carves subnets in different regions from its one block.
- **The root holds the shared VPC's range, minus the gateway's range
  and the control range.** `silo install --address-space` sets the range
  (default `10.0.0.0/8`). The gateway's range (default
  `10.255.255.0/24`) and the control range (default `10.255.254.0/24`,
  where the database's endpoint is: [`state.md`](state.md#the-control-subnet))
  are recorded as delegated to system entries, so that they are never
  handed out.
- **Each silo gets one or more CIDR blocks carved from its parent's.**
  It delegates slices onward to its children.
- **A silo's own subnets must fit inside its blocks, minus what it has
  delegated.** That includes their secondary (alias) ranges. The facade
  checks `ipCidrRange`, every `secondaryIpRanges[].ipCidrRange`, and
  the target of `expandIpCidrRange` against that.
- **Tenants see their blocks** through `describe`, and as the
  read-only field `siloAddressBlocks` on the facade's network.

### Every silo's space map {#SV-39}

For each of its blocks, silod keeps a map of the space within it. Each
range in the block is exactly one of:

- **free**;
- **delegated** to a named child: one of that child's blocks;
- **own**: a subnet or secondary range of this silo's, placed by the
  tenant.

Ranges never overlap. The **own space** of a silo is its blocks minus
what it has delegated. Origin identity works from it: a source IP
belongs to the silo whose own space contains it, and there is exactly
one such silo ([`gateway.md`, Origin identity](gateway.md#origin-identity)).

### Not fungible {#SV-40}

Address space is not fungible like quota. Two free /21s that aren't
buddies don't make a /20. So:

- **Blocks are aligned power-of-two prefixes.** A child asks for a
  **prefix length** ("a /20"), and silod picks the range. A parent may
  name an exact range instead (`--address 10.1.16.0/20`) when it needs
  to. silod checks that the range is aligned, inside one of the
  parent's blocks, and free.
- **The smallest block is a /28 (call 8).** The smallest subnet is a
  /29, which is GCE's smallest IPv4 primary range. GCE reserves four
  addresses in every primary range.
- **silod picks with a buddy allocator.** Among the free aligned slots
  of the requested size, it prefers, in order:
  1. a slot whose buddy (the other half of the next size up) is also
     free, so that the new block can later [widen in place](#growth);
  2. the slot inside the smallest free aligned region that holds it,
     so that large free regions stay whole;
  3. the lowest address, so that the result is the same every time.

  If no slot of the requested size is free, the request fails with
  `RESOURCE_EXHAUSTED` and names the largest prefix that would fit.

### Growth {#SV-41}

The chat left this choice open: either reserve headroom so a block can
widen in place, or allow several non-contiguous blocks. Each block adds
fence-rule entries, so few, larger blocks are better.

**Call 7: widen in place when the buddy is free, otherwise add a
block, up to a cap. Reserve nothing.**

- `silo grow <path> --to /19` asks to widen one of the child's blocks
  to a /19. silod tries first to widen in place: a /20 can become the
  /19 that contains it if its buddy /20 is free in the parent. Widening
  in place adds no fence rules, because the rules name the block,
  which just gets bigger.
- If the buddy is taken, `silo grow` fails with
  `FAILED_PRECONDITION`, unless `--add-block` is given. In that case
  silod allocates a new block of the requested size instead, wherever
  it fits.
- A silo may hold at most **4 blocks** by default. A parent may raise
  this per child, up to 16. The fence's cost is linear in the number
  of blocks ([`gcp-backend.md`, Rule budget](gcp-backend.md#rule-budget)).
- Shrinking: a block can be given back (`silo block release`) only if
  nothing in it is own or delegated. A block can't be narrowed in
  place in v1.

The buddy preference in placement gives most blocks room to widen at
no cost. Hard reservation was rejected because it compounds: a silo
that reserves 2× for each child, at each of four levels, uses one
sixteenth of its space.

### Freed blocks {#SV-42}

When a silo is deleted, its blocks become free in its parent's map. Any
of its own subnets are already deleted by then (deletion is
bottom-up). Free buddies merge back together. silod doesn't reuse a
freed block for **one hour** (call 24), so that a stale connection or
a DNS cache entry for the old silo doesn't land in a new one.

### Fence rules and links are written per block {#SV-43}

The fence ([`gcp-backend.md`, The fence](gcp-backend.md#the-fence))
names a silo's blocks, not its subnets. Making a subnet changes no
fence rule. Delegating a block changes the fence (the child's rules
are added), and so does adding a block. An L3 link between two silos
names every block of each.

### Checks the facade makes {#SV-44}

For a subnet the tenant asks for in region `r`:

1. `r` is an allowed region.
2. `ipCidrRange` is a valid IPv4 CIDR, /29 or larger.
3. It lies inside one of the silo's blocks.
4. It overlaps nothing delegated and no other own range of this silo.
   GCE also rejects overlap within a VPC, but silod checks first so that
   the error names the silo's map.
5. The same checks apply to each secondary range, and to the new range
   of `expandIpCidrRange`.

If any check fails, the facade returns GCE's own 400 shape
(`invalid`), with a message naming the silo's blocks.

### IPv6 (later)

GCE gives a VPC a ULA /48 and each dual-stack subnet a /64. This hands
down easily. Blocks become IPv6 prefixes between /48 and /64, carved
with the same buddy allocator. The space is large enough that every
silo can be given room to widen in place by default, so the growth
question goes away. The fence would need IPv6 rules beside the IPv4
ones.
