# Silo: nested virtual clouds

A silo is a nested virtual cloud. Silo is the system that makes them.

## Status

First draft, docs only, no code. It records the decisions bsalmon made
in director chat task 1913 on 2026-10-04 and was written up in task
1930. That task replaces 1924 and 1928. Task 1945 added
[`agents.md`](agents.md), on running agents as silo resources, from
the same chat. **Implementation is on hold:** nobody files
implementation tasks from this document until bsalmon says so.

Silo replaces Shibu ([grain's `docs/shibu.md`](https://github.com/bwsalmon/grain/blob/main/docs/shibu.md),
[`docs/shibu-spec/`](https://github.com/bwsalmon/grain/blob/main/docs/shibu-spec/README.md)) as the current direction.
Those documents are kept for reference. Where an idea carries over,
this document restates it in Silo terms instead of linking to it.

The sealed-silo addendum (the control policy, and `manage` grants for
production cells) was decided in the same chat on 2026-10-04 and
written up in task 1947.

Where the chat decided something, this document says so plainly. Where
it did not, the author made a call. Each call is marked **(call n)** in
the text and listed in [Calls made in this draft](#calls-made-in-this-draft),
so a reviewer can check them in one place. Questions that are still
open are under [Open questions](#open-questions).

## Naming

- **Silo** is the system: the control plane `silod`, the `silo` CLI
  and the `silo/v1` fence API. **A silo** is the object, one nested
  virtual cloud, and a **child silo** is one made inside another.
- Silos are isolated unless both sides agree to a link or an export
  ([Grants between silos](#grants-between-silos)).
- The project was called NVC until task 2004 renamed it, as bsalmon
  decided in director chat task 1913 on 2026-10-05.
- Other things already use the name: Authentic8's "Silo"
  browser-isolation product, Silo AI, and the `loopholelabs/silo` Go
  storage library. None of them is in our category, but **a trademark
  check is needed before any public release**.

## Documents

| Document | What it covers |
| --- | --- |
| This file | Motivation, the model, the backing trade-offs, a walk-through, limitations, calls and open questions |
| [`fence-api.md`](fence-api.md) | The `silo/v1` fence API: objects, operations, errors, who may call what, the control policy and `manage` grants |
| [`allocators.md`](allocators.md) | Regional quota, allowed regions and address space: how each is handed down the tree, checked and returned |
| [`gcp-backend.md`](gcp-backend.md) | The v1 GCP backend: subnet-backed silos in one host project, the fence firewall policy, and the `compute/v1` facade |
| [`facades.md`](facades.md) | Where facades and backends live in the code, the request pipeline every facade shares, and when its shared core is extracted |
| [`gateway.md`](gateway.md) | The gateway: origin identity, `silo.internal`, proxied exports, the egress proxy and the front door |
| [`install.md`](install.md) | `silo install` and `silo uninstall` |
| [`state.md`](state.md) | silod's state: Cloud SQL for PostgreSQL by default, SQLite for dev and tests, the store interface, leadership, migrations and backups |
| [`agents.md`](agents.md) | Agents as silo resources: the agent object, harnesses on the control VM, tool VMs and the tool server, credentials, model budget, the control surface, and grain's mapping onto it |
| [`testing.md`](testing.md) | The fake GCE backend and the `silo/v1` conformance suite |
| [`custom-allocators.md`](custom-allocators.md) | Custom allocators (`allocator/v1`), in the serve and lend shapes: designed now, built after v1. Also what v1's ledger and fence API must already allow. Its calls and open questions are listed on the page. |

The spec pages are normative. If a spec page and this file disagree,
this file wins and the spec page is wrong.

## Contents

1. [Motivation](#1-motivation)
2. [Principles](#2-principles)
3. [The model](#3-the-model)
4. [Components](#4-components)
5. [Backings and their trade-offs](#5-backings-and-their-trade-offs)
6. [The v1 GCP backend, in brief](#6-the-v1-gcp-backend-in-brief)
7. [Walk-through](#7-walk-through)
8. [Limitations](#8-limitations)
9. [Later](#9-later)
10. [Calls made in this draft](#calls-made-in-this-draft)
11. [Open questions](#open-questions)

## 1. Motivation

Agents, CI runners and people want to make machines: a VM to build in,
a disk to keep a cache on, a small network to test a cluster in. Today
that means one of two things. They get the cloud project's
credentials, which can do anything to everything. Or they go through a
narrow, purpose-built API (grain's sandboxes, Shibu's VM kind), which
stops working the moment someone needs something its authors didn't
think of.

Shibu tried to fix this with namespaces, grants and allocators around
a kernel of its own. The kernel's API was new, so every tool had to
learn it, and every cloud feature had to be designed into it before
anyone could use it.

Silo takes the other road. **Inside a silo you get a cloud**, and the
API is that cloud's own: `gcloud compute instances create` and the
terraform `google` provider work against a silo by pointing the
compute endpoint at `silo.internal`. What a silo adds is a **fence**:
everything you make attaches only to your own network, uses only your
own quota, lives only in your allowed regions and addresses, and
cannot reach another silo or the internet unless someone with the
authority to allow it has done so.

Silos nest. A team gets a silo; it makes a child silo for CI and another
for a demo; CI makes a child per job. Each one is a cloud to whoever
is inside it, and each one is fenced by everything above it.

## 2. Principles

These were decided in task 1913.

1. **The silo is the unit.** It replaces Shibu's namespace. Silos form a
   **tree**: allocators flow down it, and **grants** work between
   silos. A child can never be looser than its parent.
2. **Inside a silo you get a cloud.** You can create VMs, volumes and
   networking, provided they attach **only to that silo's own network**.
3. **Only silod holds cloud credentials.** It is the only thing that
   creates objects, and it enforces rule 2.
4. **Native API, faithfully emulated.**
   - Inside a silo, the API is the underlying cloud's own, emulated as
     faithfully as possible, with coverage growing over time.
   - Backends share **one common fence** between silos: the `silo/v1`
     fence API. The API inside a silo is per backend.
   - The facade is a **default-deny allowlist** of methods, resource
     types and fields. It is not a pass-through.
5. **The silo address is the same everywhere.** Every silo reaches silod
   at the same name, `silo.internal`. Callers are identified by origin
   (source IP → subnet → silo). Nothing is handed into a VM for
   identity: no token, no key, no metadata entry.
6. **Networking.**
   - Inside a silo, connectivity and firewalls are under the tenant's
     control, within the fence.
   - Between silos, traffic is denied unless both sides agree.
   - External traffic is denied unless ancestors' policy allows it.

## 3. The model

### Silo

A silo is a node in the tree. It has:

- an **id**: six lowercase base-32 characters, generated, stable and
  globally unique (`k7f3qa`) **(call 1)**. It is what resource-name
  prefixes, facade project IDs and audit records use;
- a **name**: a DNS label unique among its siblings, and so a **path**
  from the root (`/team-a/ci/job-412`). Paths are for people;
  everything durable uses the id;
- a **parent** (none for the root) and zero or more **children**;
- a **backing**: `subnet` in v1; `vpc` and `project` later
  ([§5](#5-backings-and-their-trade-offs));
- its **allocations**: regional quota, allowed regions and address
  blocks, all handed down by its parent
  ([`allocators.md`](allocators.md));
- its **policies**, also handed down: egress hostnames, whether it
  may take part in exports and L3 links, and its **control** policy
  ([below](#control-policy-and-sealed-silos));
- its **members**: outside identities (people, service accounts
  calling through the front door) mapped to it.

The root silo is created by `silo install`. Its quota is the host
project's real regional GCE quota, its regions are the ones install
was given, and its address space is the shared VPC's range minus the
gateway's range and the control range ([`state.md`](state.md#the-control-subnet)).

### The tree and "never looser"

A parent hands a child a subset of what it has: a slice of its quota
in each region, a subset of its regions, aligned blocks carved from its
own blocks, an egress policy no wider than its own, and cross-silo
permissions no wider than its own. silod checks each of these when the
child is created and every time an allocation changes, and refuses
the change if it would make the child looser than its parent
([`fence-api.md`, Errors](fence-api.md#errors)).

Quota is a **hard partition** **(call 3)**. What a parent grants to its
children is reserved out of its own capacity, so a child can always
use what it was given. Oversubscription is an open question.

### Grants between silos

A grant between silos is an agreement that both of them have made.
In v1 there are two kinds, and both need both sides' consent:

- a **proxied export**: silo A offers a service (an `ip:port` inside
  its own network) to silo B, and B accepts it. B's callers reach it
  through the gateway. The gateway knows who each caller is, can
  record each call, and closes connections when the grant is revoked.
  This is the default.
- an **L3 link**: A and B both agree to open the fence between their
  blocks for given protocols and ports. Traffic flows directly and
  transparently, and silod sees none of it.

Each side's ancestors must allow it to take part in that kind of grant
([`fence-api.md`, Grants](fence-api.md#grants-exports-and-links)).
Shibu's richer grants (narrowing, leases, endpoints on arbitrary
components) are [later](#9-later).

### Identity

Inside the cloud, identity is by origin. A packet's source IP lies in
exactly one silo's own address space, because blocks are disjoint and
GCE's anti-spoofing drops packets a VM sends from an address that
isn't its own (the facade refuses `canIpForward`). The gateway maps
the source IP to the silo that owns it and passes that silo to silod
with the request ([`gateway.md`, Origin identity](gateway.md#origin-identity)).

Outside the cloud, people and outside callers authenticate at the
**front door** (IAP), and each identity is mapped to a silo. A caller
from the front door acts as the silo it is mapped to, with exactly the
authority a VM inside that silo has.

Inside a silo everyone is the silo. There is no per-VM identity and no
per-VM grant ([§8](#8-limitations)).

### Agents

An **agent** is a resource in a silo, created and deleted through the
fence API: a harness and model with a budget and a lease, and a
**tool VM** in the silo where everything it runs runs. Code the agent
runs gets that silo's egress and grants. Code it didn't write goes in a
child silo, a sandbox, and a helper agent goes in a child silo of its
own. In v1, harnesses run on the control VM, which alone holds the
model credentials, and call the tool VM's MCP tool server through the
gateway. See [`agents.md`](agents.md).

### Authority

A silo may:

- use its own facade: create, change and delete its own objects;
- manage its descendants through the fence API: create and delete
  children, hand down and take back allocations, set their policies
  (including `control`), read their audit, and map identities to them;
- read and write its descendants' facades **(call 13, revised by call
  55)**, and make grant calls on their behalf;
- offer and accept grants for itself, within its ancestors' policy;
- act on another silo's facade and fence through a `manage` grant that
  one of that silo's ancestors gave it
  ([below](#manage-grants-changing-a-sealed-cell)).

It may not touch its ancestors, its siblings or their subtrees,
except through a grant they have agreed to. Everything in this list
is then cut down by the silo's own control policy.

### Control policy and sealed silos

Decided in task 1913: "we also probably need to have the option to
not give access to silod for a silo. this way we can run 'production'
cells that can't modify themselves."

Every silo has a **`control`** policy:

- `full`: the default, as above;
- `read`: `describe`, list and get only, on the fence API and the
  facade;
- `none`: **sealed**. No control-plane call at all.

It binds every principal acting **as** the silo: its VMs (by origin),
front-door members mapped to it, and its agents' harnesses on their
session sockets (`via=harness`, [`agents.md`](agents.md)). It is set only by the silo's parent or
another ancestor, never by the silo itself, and a child can't be
looser than its parent, so the children of a `read` silo are at most
`read` (call 54).

It covers the **control plane only**. Egress, proxied exports, L3
links, agents' tools, model access and the agent control surface
(messages and status) keep working, so a sealed cell runs normally. It just can't
create, resize, re-firewall or delete its own infrastructure, or
change its grants.

It is enforced twice
([`gateway.md`, Control policy](gateway.md#control-policy)):

1. the gateway and silod refuse the calls: all of them for `none`, the
   mutating ones for `read`, with a GCE-shaped 403 on the facade and
   `CONTROL_DENIED` on the fence API;
2. the facade and fence API are served on an **API port** of their
   own (8080), separate from exports and egress (call 53), and the
   fence firewall drops that port from a sealed silo's blocks
   ([`gcp-backend.md`, Sealed silos](gcp-backend.md#sealed-silos)). A
   bug in the gateway still hits a wall.

### Manage grants: changing a sealed cell

A sealed cell is changed from outside. **Ancestors** may write into a
descendant's facade and fence (call 55, which settles what was open
question 4). For a cell that should be changed by something that isn't
above it, such as a deploy pipeline, an ancestor of the cell offers a
**`manage` grant** to that other silo. The holder then acts on the
cell's facade and fence as if it were an ancestor, without being one:

- within the grant's **scope**: facade resource types (`instances`,
  `disks`, ...) with `read` or `write` access, and fence groups
  (`read`, `grants`, `children`, `blocks`, `agents`);
- within its **lease**, which the holder may renew up to a limit the
  offerer set;
- **never** for `control`, members, the cell's own allocations, or
  further `manage` grants. Only real ancestors may unseal a cell
  (call 56);
- audited under the cell as `silo=<target>, via=manage:<holder>`;
- revoked by any ancestor of the cell, after which the next call that
  relies on it is refused.

The full rules are in [`fence-api.md`, Manage grants](fence-api.md#manage-grants).

## 4. Components

### silod, the control plane

silod holds the tree, the allocators, the grants and the audit log in
a PostgreSQL database: by default a Cloud SQL instance that `silo
install` creates in the host project. SQLite, behind the same store
interface, is for local development, the fake-GCE tests and demos
([`state.md`](state.md)). It is the only component with cloud credentials
([principle 3](#2-principles)), and the only one that creates, changes
or deletes cloud objects. It serves two APIs:

- the **fence API**, `silo/v1`: create and delete child silos, hand down
  allocations, cross-silo exports and links, egress policy, audit,
  quota preview, and `describe` (a silo's blocks, regions and quotas).
  It is the same for every backend. See [`fence-api.md`](fence-api.md).
- the **per-silo facade**: the backend's own API, a default-deny
  allowlisted subset, rewritten and checked so that everything a call
  makes stays inside the caller's silo. For GCP this is a `compute/v1`
  subset. See [`gcp-backend.md`](gcp-backend.md#the-computev1-facade).

silod also keeps the cloud in step with its database: it writes the
fence's firewall rules, and on start it reconciles what the cloud has
against what it recorded ([`gcp-backend.md`, Reconcile](gcp-backend.md#reconcile)).

Because its state is shared, silod can run as several replicas. Any
replica serves calls. One leader, chosen with a Postgres advisory
lock, writes the fence and runs the reconcile
([`state.md`, More than one silod](state.md#more-than-one-silod-and-the-leader-call-34)).

### silod's state: cost and limits

Cloud SQL was chosen for scaling, backups and recovery
([`state.md`](state.md)). What it costs:

- **A fixed monthly cost**, paid whether or not any silo exists: about
  $25–30 a month for the default zonal `db-g1-small`, roughly double
  with `--db-ha`, plus a few dollars for backups and the PSC endpoint.
- **Minutes at install.** Creating the instance takes several minutes,
  often five to ten.
- **A regional dependency.** The control plane depends on one Cloud SQL
  instance in the home region. While it is unreachable, facade and
  fence calls fail with 503. Gateways keep serving exports and egress
  from their caches.
- **A reachable database inside the VPC.** It sits behind a PSC
  endpoint in a control subnet that the fence denies to every silo
  (call 32).

### The gateway

The gateway is reachable from every silo at one internal IP and the
private DNS name `silo.internal`. It:

- serves the facade and the fence API to callers inside the cloud,
  with origin identity;
- carries proxied cross-silo exports;
- is the egress proxy (`HTTPS_PROXY`), enforcing per-hostname policy.

It holds no cloud credentials. See [`gateway.md`](gateway.md).

### The front door

People and outside callers reach silod through IAP. silod verifies the
caller's Google identity and maps it to a silo
([`gateway.md`, Front door](gateway.md#front-door)).

### Agent harnesses (v1)

The control VM also runs agent harnesses, one container per session,
and holds the model credentials they use. silod launches each session,
meters its model use against its budget, and serves it a session
socket on which its calls are made as the agent's silo
([`agents.md`, §3](agents.md#3-v1-placement-harnesses-on-the-control-vm-tools-remote)).

### `silo`, the CLI

`silo` is the command-line client of the fence API, and it carries
`silo install` and `silo uninstall` ([`install.md`](install.md)). Inside
the cloud it calls `http://silo.internal:8080/silo/v1/` and needs no
credentials. Outside, it goes through the front door.

## 5. Backings and their trade-offs

Every silo has a **backing**, which says what cloud construct its
fence is built from. Each step up gives stronger isolation and more
faithful networking. The fence API allows all three, and a parent may
choose a backing per child. v1 implements only `subnet`.

| | Subnet per silo (v1) | VPC per silo (later) | Project per silo (later) |
| --- | --- | --- | --- |
| **Isolation** | Firewall only: one global network firewall policy, owned by silod, evaluated before tenant rules. A bug in the rules is a breach. | A routing boundary: there is no route between VPCs unless silod peers them or connects them through PSC. | A project boundary: IAM, quotas, APIs, metadata, DNS and logs are all separate. Org policies and hierarchical firewall policies are enforced by Google. |
| **Facade fidelity** | Partial. One virtual network, which is a fiction over a slice of the shared VPC. CIDRs come from allocated blocks. Names are prefixed. No routes. | Good. The tenant gets a real VPC: its own routes and firewall rules, with CIDRs from its blocks. Names inside it need no prefix. | Near pass-through. The tenant's project is real, so almost every `compute/v1` call can be forwarded with a check rather than a rewrite. |
| **Gateway reach** | Direct: the gateway is in the same VPC. | Through Private Service Connect or VPC peering to the gateway's VPC. | The same as VPC, across projects. |
| **Speed to create** | Seconds: a database row and a few firewall rules. | Seconds to a minute. | Minutes. A pool of pre-made projects would help. |
| **Limits** | Firewall-policy rule size, and subnets per network. Every block adds rule entries ([`allocators.md`](allocators.md#growth)). | 15 VPC networks per project by default, so it needs a quota increase or nesting VPCs across projects. | Needs an organization (folders mirror the tree) and per-project quotas, which have to be requested per project. |
| **Billing and teardown** | Labels only. Teardown is silod deleting what it recorded. | Labels only. Teardown is deleting the VPC and its contents. | Per-project billing. Teardown is deleting the project. |
| **Needs** | One standalone project. | One standalone project. | An organization and folders. |

**Subnet per silo was chosen for v1** because bsalmon has only
standalone projects, with no organization or folders. That rules out
project backing for now, and the 15-VPC default makes VPC backing a
poor default for a tree that is meant to grow.

What this choice costs is stated in [§8](#8-limitations). The main
costs are that isolation rests entirely on silod's firewall rules, and
that some host-project facts show through (names, project-wide
metadata, internal DNS). This is why the [conformance suite](testing.md)
carries the weight.

### Mixing backings

The fence API carries a backing on every silo from the start, so
adding the others later changes no client. When they come, the rule
is **(call 30)**: a child's blocks are always carved from its parent's
blocks, whatever its backing. Every silo's addresses are then unique
across the whole tree, and links and the gateway work across backings
without NAT. A `subnet` child lives in its parent's network, so it is
a subnet of the shared VPC, of its parent's own VPC, or of a VPC in
its parent's project. A `vpc` or `project` child gets a new network
of its own.

## 6. The v1 GCP backend, in brief

The full specification is [`gcp-backend.md`](gcp-backend.md). In
outline:

- **One host project, one shared custom-mode VPC.** Each silo's
  subnets are regional subnets of that VPC, inside the silo's blocks.
  silod, the gateway and an optional internal load balancer sit in the
  same VPC, in a gateway subnet outside every silo's space, so no PSC
  or peering is needed.
- **The fence** is a global network firewall policy owned by silod,
  attached to the VPC and evaluated before the tenant's VPC firewall
  rules. For each silo it hands intra-silo traffic on to the tenant's
  rules, allows traffic to the gateway (minus the API port, for a
  sealed silo), and denies everything else.
- **The facade** is a faithful `compute/v1` subset at
  `http://silo.internal:8080/compute/v1/projects/<project>/...`. It covers
  instances (with GPUs and local SSD), disks (with pd-extreme and
  Hyperdisk), images, snapshots (if cheap), instance templates,
  subnetworks, firewalls, operations, and the read-only regions, zones,
  machine types, disk types and accelerator types. It refuses routes,
  external IPs, NAT, peering, VPN, `canIpForward`, service accounts on
  instances, project-wide metadata, OS Login, reservations, sole
  tenancy, machine images, storage pools and managed instance groups.
  Errors are GCE-shaped.
- **Egress** goes only through the gateway, as an explicit
  `HTTPS_PROXY`, with per-hostname policy set by ancestors. No VM in
  any silo has an external IP, and no tenant subnet has NAT.
- **Ingress and public services** are out of scope for this draft.
- **silod and the gateway** run on one VM (or a small MIG) in the host
  project, with silod's own service account.
- **silod's state** is in Cloud SQL for PostgreSQL, reached through a
  Private Service Connect endpoint in the control subnet, which no silo
  can reach. silod logs in with IAM, with no password
  ([`state.md`](state.md)).

## 7. Walk-through

alice is the admin `silo install` wrote. The host project is
`bsalmon-fun-sandbox` and the allowed regions are `us-central1` and
`europe-west4`. Install made the shared VPC `silo`, gave the root silo
`10.0.0.0/8` with the gateway's `10.255.255.0/24` and the control
subnet's `10.255.254.0/24` held back, and read
the root's quota from the project: 96 vCPU and 4096 SSD GB in each
region.

**1. alice makes a team silo.** From her laptop, through the front
door:

```sh
silo create /team-a --regions us-central1 \
    --quota us-central1:vcpu=32,memory=128GiB,ssd-gb=1000 \
    --address /16 --egress-allow 'proxy.golang.org,*.debian.org'
```

silod checks that the root has 32 vCPU free in us-central1, that
`us-central1` is one of the root's regions, and that the egress list
is within the root's. It carves an aligned /16 from the root's free
space (say `10.1.0.0/16`), records `/team-a` (id `k7f3qa`), and adds the
team's fence rules to the firewall policy. No GCE object exists yet
for the team, apart from those rules.

**2. A team member works inside it.** bob is mapped to `/team-a`.
`silo proxy` opens the front door's IAP tunnel and serves the API on
localhost, attaching his Google identity to each call
([`gateway.md`, Front door](gateway.md#front-door)). He makes a subnet
and a VM:

```sh
silo proxy --listen localhost:8642 &
gcloud config set api_endpoint_overrides/compute http://localhost:8642/compute/v1/
gcloud config set project silo-k7f3qa
gcloud compute networks subnets create build --network default \
    --region us-central1 --range 10.1.0.0/24
gcloud compute instances create builder --zone us-central1-a \
    --machine-type n2-standard-8 --subnet build \
    --image-family debian-12 --image-project debian-cloud \
    --enable-nested-virtualization --no-address
```

The facade checks that `10.1.0.0/24` lies inside `/team-a`'s blocks
and isn't delegated to a child, that `us-central1` is allowed, and
that 8 vCPU and 32 GiB fit in the team's us-central1 quota. It creates
the real subnet `k7f3qa-build` and the real instance `k7f3qa-builder`,
labelled `silo=k7f3qa`, with no service account, no external IP and
`block-project-ssh-keys` set. It answers with GCE-shaped objects whose
names, selfLinks and project are the team's.

If bob had left out `--no-address`, the facade would have refused
with a 403 naming `networkInterfaces[0].accessConfigs`.

**3. The VM calls the cloud with no credentials.** On `builder`,
`gcloud` is configured with `auth/disable_credentials` and the
endpoint `http://silo.internal:8080/compute/v1/`. Its calls reach the
gateway from `10.1.0.2`. The gateway maps that address to `/team-a`
and passes the call to silod as `/team-a`'s. The VM can make more VMs
in the team's subnets and nowhere else. `apt-get` goes through
`HTTPS_PROXY=http://silo.internal:3128`, and the proxy lets through
`*.debian.org` and refuses `pypi.org`.

**4. The team makes a CI child.**

```sh
silo create /team-a/ci --quota us-central1:vcpu=16,spot-vcpu=64 --address /20
```

This is the same call as step 1, one level down. It is allowed
because the caller is `/team-a`, which is `/team-a/ci`'s parent. The
16 vCPU come out of the team's 32. The /20 (`10.1.16.0/20`) comes out
of the team's /16, chosen so that it doesn't overlap the team's
`build` subnet. The CI silo's facade project is `silo-p2m8xd`. A VM in
`10.1.16.0/20` that calls `silo.internal` is `/team-a/ci`, not
`/team-a`.

**5. CI reaches the team's cache, with consent.** The team runs a
package cache on `10.1.0.5:8080` and offers it:

```sh
silo export create pkgcache --target 10.1.0.5:8080 --protocol http --to /team-a/ci   # as /team-a
silo export accept /team-a/pkgcache                                               # as /team-a/ci
```

CI's VMs now reach `http://silo.internal/x/k7f3qa/pkgcache/`. Each
call is audited with the caller's silo. If either side withdraws, the
gateway closes the open connections. An L3 link instead would open
the fence between the two blocks for TCP 8080, and silod would see
nothing per call.

**6. Teardown.** `silo delete /team-a/ci --force` deletes CI's
instances, disks, images, snapshots, subnets and firewall rules,
removes its fence rules and grants, returns its /20 to `/team-a`'s
free space, and returns its quota to `/team-a`. The audit log keeps
the record.

### A production cell

`/team-a` runs a web service it doesn't want anything inside it to be
able to change, including an agent that operates it. A deploy
pipeline, which isn't above the service in the tree, rolls it out.

**1. alice makes the sealed cell and the pipeline.** As the root,
`/team-a`'s ancestor, she first widens the team's egress so that the
children's lists fit inside it:

```sh
silo patch /team-a --egress-allow 'proxy.golang.org,*.debian.org,us-docker.pkg.dev,github.com:443'
silo create /team-a/prod --control none --quota us-central1:vcpu=16,memory=64GiB \
    --address /22 --egress-allow 'us-docker.pkg.dev'
silo create /team-a/deploy --quota us-central1:vcpu=4 --address /24 \
    --egress-allow 'us-docker.pkg.dev,github.com:443'
silo manage offer /team-a/prod --to /team-a/deploy --name rollout \
    --facade instances,disks,subnetworks,firewalls --facade-access write \
    --ttl 24h --renew-until 2027-01-01
```

`/team-a/prod` (id `w4n2hc`) gets no fence rule for the API port.
`describe` from `/team-a` shows `control: own none, effective none`.

**2. The pipeline accepts and builds the cell.** On the pipeline's
runner VM in `/team-a/deploy`:

```sh
silo manage accept /team-a/prod/rollout
gcloud config set api_endpoint_overrides/compute http://silo.internal:8080/compute/v1/
terraform apply -var project=silo-w4n2hc -var release=v41
```

The terraform module makes a subnet, a firewall rule and the instance
`web-v41`. Its instance template is the module's own
`google_compute_instance` definition: a public Debian image, the
`release` in metadata, and a startup script that pulls that release
from `us-docker.pkg.dev` through the egress proxy. The facade allows
instance templates, but has no managed instance groups in v1 to roll
them out (call 22, call 57). Every object is
`/team-a/prod`'s, charged to its quota, and audited under it with
`via=manage:<deploy's id>`.

**3. The cell runs, and can't change itself.** `web-v41` serves, pulls
from its allowed registry, and is offered to the rest of the team as a
proxied export, which `/team-a` set up on its behalf (call 55). An
agent in `/team-a/prod` reads its logs on the VM, restarts the
service, answers messages and reports status (task 1945). If it runs
`gcloud compute instances delete web-v41` from the VM, the connection
to `silo.internal:8080` times out: the fence drops it. If its harness
makes the call for it (`via=harness`), silod answers 403 `forbidden`,
"control-plane calls from /team-a/prod are refused (control=none)",
and audits `control.denied`.

**4. The pipeline rolls out v42.** The pipeline changes the template's
`release` to `v42` and applies: terraform creates `web-v42`, waits for
it to pass its health check, and deletes `web-v41`. If the pipeline
tried `silo patch /team-a/prod --control full`, it would get
`PERMISSION_DENIED`: a `manage` grant never carries `control`. If its
module tried to add an image, it would get 403 naming the grant,
because `images` isn't in its scope.

**5. Revoking.** When `/team-a` moves to another pipeline, it runs
`silo manage revoke /team-a/prod/rollout`. The old pipeline's next call
gets 403, and the cell keeps running untouched. If a person has to fix
the cell by hand, they do it as `/team-a`, which writes into the
cell's facade directly as its ancestor. The cell stays sealed
throughout.

## 8. Limitations

These are true of v1 by design.

- **Isolation is firewall-only in the subnet backing.** One VPC is
  shared by every silo, and the only thing between two silos is silod's
  firewall policy. A wrong rule is a breach. The
  [conformance suite](testing.md#conformance-suite) exists for this
  reason.
- **No per-VM identity or grants inside a silo.** Every VM in a silo,
  and every member mapped to it, has the silo's full authority, cut down
  only by the silo's control policy. To give a job less, give it its own
  child silo, or seal it.
- **A sealed VM's control-plane calls time out rather than fail.** The
  fence drops its packets to the API port, so `gcloud` waits for
  its connect timeout instead of getting a 403
  ([`gateway.md`, Layer 2](gateway.md#layer-2-the-fence-drops-the-api-port)).
- **`read` has one layer of enforcement.** A `read` silo needs the
  API port for its reads, so only the gateway's and silod's checks
  stop its writes. The fence can't tell a read from a write.
- **Front-door callers have one layer too.** The front door isn't
  behind the fence, so a member mapped to a sealed silo is stopped only
  by silod.
- **silod sees nothing that stays inside a silo.** Traffic between two
  VMs of one silo never touches silod or the gateway. The same is true
  of traffic over an L3 link. Only facade calls, fence calls, proxied
  exports and egress are audited.
- **Code written against one backend's facade isn't portable to
  another.** The fence API is common, but the API inside a silo is
  the backend's own. A terraform module written for the GCP facade
  won't run on a future Docker or AWS backend.
- **Facade fidelity is partial.** CIDRs must come from allocated
  blocks. There are no routes, external IPs or NAT, and there is only
  one network. Names are rewritten, and the real names show through
  in some places: guest hostnames, internal DNS, the metadata server
  and the 63-character limit
  ([`gcp-backend.md`, Where the real names show through](gcp-backend.md#where-the-real-names-show-through)).
- **Host-project facts reach every VM.** GCE's metadata server tells
  any VM the host project's ID and its project-wide metadata, and GCE
  internal DNS resolves instance names across the whole VPC. No
  firewall can stop either. So the host project's metadata must hold
  nothing secret, and instance names and internal IPs are visible
  across silos, though no traffic is
  ([`gcp-backend.md`, What leaks between silos](gcp-backend.md#what-leaks-between-silos)).
- **The control VM is the crown jewel.** It holds the cloud
  credentials and the key to every agent's model credentials, and runs
  the harnesses (with `--control-replicas`, every control VM does). A compromise of it, or of a harness that escapes its
  container, is a compromise of all of them
  ([`agents.md`, Security](agents.md#11-security)).
- **The control plane depends on one Cloud SQL instance.** silod can
  run as several replicas, but they share one database in the home
  region, which is zonal unless install was given `--db-ha`. While it
  is down, the gateways keep serving proxied traffic and egress from
  cache, but facade and fence calls fail
  ([`state.md`, Availability](state.md#availability-cost-and-limits)).
- **silod and the gateway share a VM in v1**, and so share its
  service account. "Only silod holds credentials" holds at the
  process level only as far as the VM is trusted
  ([`gateway.md`, Placement](gateway.md#placement)).
- **No ingress.** Nothing outside the cloud can reach a tenant VM.
  Tenants can't use IAP SSH to reach their own VMs either; they reach
  them from inside the silo.

## 9. Later

- VPC-backed and project-backed silos ([§5](#5-backings-and-their-trade-offs)).
- **IPv6.** A ULA /48 per VPC and a /64 per subnet. This hands down
  easily: blocks become IPv6 prefixes carved the same way, and the
  space is big enough that every silo gets room to grow in place
  ([`allocators.md`, IPv6](allocators.md#ipv6-later)).
- **A docker+granule backend**: a Docker Engine API subset, one bridge
  per silo with anti-spoofing.
- **A k8s backend**: the Kubernetes API, roughly what vcluster does.
- **An AWS backend**: an EC2/VPC subset.
- **Mixed nesting**: a GCE silo hosting a nested-virt VM that runs a
  granule child silo.
- Load balancers, NAT and managed services in the facade.
- More of GCE's compute features in the facade (call 22):
  - **managed instance groups**, with a mechanism of their own, since
    a MIG creates and autoscales instances outside the facade: for
    example, silod watching the MIG's instances, and the facade capping
    `targetSize` × the template's cost against quota;
  - **reservations** as objects a silo owns, used only by its own
    instances;
  - **sole tenancy** (node groups and `nodeAffinities`);
  - **machine images**, with naming and visibility rules like
    instance templates';
  - **Hyperdisk storage pools**, which would need pool capacity handed
    down as an allocator of its own.
- A transparent egress path: a next-hop route to the gateway instead of
  an explicit proxy.
- Ingress and public services.
- From Shibu, restated in Silo terms:
  - **grants with narrowing and leases**: a grant that the receiving
    silo can pass on to its children only narrowed, and that expires
    unless renewed. `manage` grants already have a scope and a lease,
    but can't be passed on in v1;
  - **secrets behind proxies**: credentials held by a proxy endpoint
    in one silo and offered to others as an export, so the credential
    itself never enters the receiving silo;
  - **runners**: a silo that makes child silos with agents in them on
    request. Agents themselves are now specified in
    [`agents.md`](agents.md), and their own later steps (the harness
    in the agent's VM, granule µVMs) are in
    [`agents.md`, Later](agents.md#13-later).
- **Custom allocators** (Shibu's component allocators, restated). A
  service in a silo serves `allocator/v1` and registers a kind with
  its own classes. silod keeps the ledger and wires each served
  resource to its consumer as an export. Holding a grant with headroom
  is the permission. This is designed in
  [`custom-allocators.md`](custom-allocators.md), and v1's ledger
  must already leave room for it
  ([`allocators.md`, Room for custom allocators](allocators.md#room-for-custom-allocators)).

## Calls made in this draft

The chat did not decide these. The author made each call, for the
reason given. A reviewer should check each one.

1. **Silo ids** are six generated base-32 characters. Real resource
   names are prefixed with `<id>-`, so a tenant name may be up to 56
   characters, and the facade project ID is `silo-<id>`
   ([`gcp-backend.md`, Naming](gcp-backend.md#naming)). A short prefix
   leaves room under GCE's 63-character limit, and a stable id keeps
   names valid when a silo is renamed.
2. **The facade accepts the host project's ID as an alias for the
   caller's own silo.** The metadata server tells every VM the host
   project ID, so tools that detect their project work unchanged
   ([`gcp-backend.md`, Projects](gcp-backend.md#projects-and-the-caller)).
3. **Quota is a hard partition**, with no oversubscription
   ([`allocators.md`](allocators.md#regional-quota)).
4. **Memory is a silo quota class even though GCE has no memory
   quota.** The root's memory is unlimited unless install is given
   `--root-memory`. GCE's per-family CPU quotas (`N2_CPUS` and so on)
   are not handed down. A GCE refusal on one of them reaches the
   tenant as GCE's own `QUOTA_EXCEEDED`.
5. **Project-wide counts are a small global quota class**: subnets,
   firewall rules, images, snapshots, and snapshot and image storage
   GB. They are shared by the whole tree, so one silo could otherwise
   exhaust them for all ([`allocators.md`](allocators.md#global-counts)).
6. **Instances charge vCPU and memory while they are not
   `TERMINATED`**, as GCE does, so `start` can fail with
   `QUOTA_EXCEEDED`. Disks charge while they exist.
7. **Address growth: widen in place when the buddy is free, otherwise
   add a block, up to 4 blocks per silo.** silod places blocks so that
   their buddy stays free when it can (soft headroom), but reserves
   nothing. Hard-reserved headroom wastes space at every level of the
   tree ([`allocators.md`, Growth](allocators.md#growth)).
8. **The smallest block is a /28 and the smallest subnet a /29**, GCE's
   own smallest primary range.
9. **The fence orders rules deepest silo first**, so that a child's
   deny rules are matched before its parent's intra-silo rule, which
   covers the child's addresses too. That caps tree depth at 16
   ([`gcp-backend.md`, The fence](gcp-backend.md#the-fence)).
10. **Intra-silo traffic is `goto_next`, and grants are `allow`.** The
    tenant's own firewall rules decide traffic inside a silo. A link
    or export, once both sides have agreed, is opened at the fence
    without the tenant having to write rules for it.
11. **Tenant firewall rules are pinned with network tags.** Every
    instance gets the tag `<id>`. A rule with no target gets
    `targetTags: [<id>]`, tags are prefixed, and source and destination
    ranges are intersected with the silo's own space. Rules naming
    service accounts are refused.
12. **The facade has one network, named `default`**, and refuses
    `networks.insert`. Many tools assume `default`. The silo's blocks
    are shown on it as the read-only field `siloAddressBlocks`, and in
    its description.
13. **Ancestors may read their descendants' facades**, list and get
    only. Writes inside a descendant go through the descendant (or
    deleting it). **Revised by call 55:** ancestors may write too.
14. **Each side's ancestors gate grants.** Every silo has a grant
    policy, `exports` and `links`, each `allow` or `deny`, and a child's
    can't be looser. Both sides' effective policies must allow a grant.
    The root allows exports and denies links by default.
15. **The front door in v1 is IAP TCP forwarding to silod's front-door
    port, plus a Google-signed ID token that silod verifies.** It needs
    no organization, domain or certificate. An external HTTPS load
    balancer with IAP is the later path, once there is a domain
    ([`gateway.md`, Front door](gateway.md#front-door)).
16. **silod and the gateway run on one VM in v1**, `silo-control`. Its
    state is in Cloud SQL (task 1952; this replaces "SQLite on its own
    persistent disk and a daily snapshot schedule"), so
    `--control-replicas` can make it a MIG (call 34).
    `--gateway-replicas` puts extra gateways in a MIG behind an
    internal passthrough load balancer with global access. A
    passthrough load balancer is needed because a proxy one would hide
    the source IP.
17. **The gateway subnet gets Cloud NAT, and only the gateway subnet.**
    The egress proxy has to reach the internet somehow. Tenant subnets
    have no NAT, and the fence denies their egress anyway.
18. **`silo install` is Go with the GCP client libraries, not
    terraform** ([`install.md`, Approach](install.md#approach-go-not-terraform-call-18)).
19. **The control image is a container on Container-Optimized OS**,
    copied by digest into an Artifact Registry repository in the host
    project. Install does the copy with go-containerregistry, so it
    needs no Docker.
20. **silod gets a custom role**, `silod`, with an enumerated permission
    list, rather than predefined roles. The predefined roles are each
    wider than silod needs ([`install.md`, silod's service account](install.md#silods-service-account)).
21. **Install keeps no state file.** Every object it makes has a fixed
    name and the label `silo-install`. Each step finds its object by
    name, which is what makes install idempotent and resumable.
22. **v1 allows GPUs, local SSD, pd-extreme, Hyperdisk and instance
    templates, each charged to quota classes that mirror GCE's own
    metrics** (bsalmon, director chat task 1913, 2026-10-04; added in
    task 1956). None of them affects isolation: each attaches only to
    the silo's own instances and touches no network. GPUs charge one
    class per accelerator type and a Spot variant of each, from the
    instance or from a machine type that bundles them; local SSD
    charges `local_ssd_gb`; pd-extreme charges `ssd_disk_gb` and
    `pd_extreme_iops`; each Hyperdisk type charges its capacity, IOPS
    and throughput classes, and `disks.update` re-charges the
    difference ([`allocators.md`, Classes](allocators.md#classes)).
    Instance templates charge only the global count
    `instance_templates`. They are checked against every instance rule
    when made, and the effective instance is checked and charged again
    when an instance is made from one
    ([`gcp-backend.md`, Instance templates](gcp-backend.md#instance-templates)).
    **v1 still refuses** reservations (project-wide, so one silo could
    consume another's), sole tenancy (project-level and expensive),
    machine images (global, and they capture an instance's metadata and
    disks), Hyperdisk storage pools (shared project-level capacity) and
    managed instance groups (they create instances outside the facade,
    where neither quota nor the checks reach). All are
    [later](#9-later).
23. **Exports are reached by URL path for HTTP**
    (`http://silo.internal/x/<exporter-id>/<name>/`) **and by a gateway
    port for TCP**, allocated when the import is accepted.
24. **A freed block isn't reused for an hour**, so that stale
    connections and DNS answers for a deleted silo don't reach a new one
    ([`allocators.md`, Freed blocks](allocators.md#freed-blocks)).
25. **Image and snapshot `storageLocations` may name an allowed region
    or the multi-region containing one** (`us` for `us-central1`).
26. **The facade applies `filter`, `orderBy` and paging itself**, after
    asking GCE for the silo's objects only. It doesn't rewrite the
    tenant's filter for GCE, so a filter can never reach another silo's
    objects ([`gcp-backend.md`, Lists and filters](gcp-backend.md#lists-and-filters)).
27. **Orphans are deleted after an hour.** An orphan is a labelled
    object whose silo no longer exists. Foreign objects are only
    reported ([`gcp-backend.md`, Reconcile](gcp-backend.md#reconcile)).
28. **Facade reads are not audited by default.** There are many of
    them, and they reveal nothing outside the silo. Writes, fence calls,
    export calls and egress connections are all audited.
29. **TCP exports show the gateway as the source.** The exporter learns
    the caller from the audit log, or from a PROXY protocol v2 header if
    it asks for one.
30. **A child's blocks are always carved from its parent's, whatever
    its backing**, so that addresses are unique across the tree, and
    links and the gateway work across backings without NAT
    ([§5, Mixing backings](#mixing-backings)).

The calls below were made in task 1952, the Cloud SQL addendum
([`state.md`](state.md)). The decision itself, Cloud SQL for
PostgreSQL by default with SQLite kept for dev and tests, is
bsalmon's, from task 1913.

31. **`silo install` always creates Cloud SQL.** SQLite is selected
    only by running silod with `--store=sqlite:<path>`, for development,
    the fake-GCE tests and demos. An install on SQLite would be a
    second production path, with its own disk, snapshots and
    single-VM limit ([`state.md`](state.md#two-backends-one-interface)).
32. **The database is reached through a Private Service Connect
    endpoint in a control subnet** (`--control-range`, default
    `10.255.254.0/24`), held back from the root's block like the
    gateway range. It isn't in the gateway range, which every silo may
    reach. A new top-priority fence rule, `ctl-deny-out`, denies every
    silo block egress to it. PSC was chosen over private services
    access because it needs no VPC peering and no allocated range, and
    the service can't connect back into `silo`
    ([`state.md`, Connectivity](state.md#connectivity-a-psc-endpoint-in-a-control-subnet-call-32)).
33. **The instance defaults to PostgreSQL 17, Enterprise edition,
    `db-g1-small`, zonal, in the home region**, with storage
    auto-increase, daily backups kept 14 days, 7 days of PITR, and
    deletion protection. Its name has a random suffix, found by label,
    because Cloud SQL doesn't let a deleted instance's name be reused
    for about a week ([`state.md`, The Cloud SQL instance](state.md#the-cloud-sql-instance-call-33)).
34. **Any silod replica serves calls. One leader writes the fence**,
    runs the reconcile, adopts dead replicas' GCE operations, issues
    gateway certificates and prunes audit. The leader is chosen with a
    session advisory lock in Postgres, and guarded by an epoch and by
    the firewall policy's fingerprint. `--control-replicas` defaults to
    1 ([`state.md`, More than one silod](state.md#more-than-one-silod-and-the-leader-call-34)).
35. **Migrations are versioned and expand-then-contract, run by
    install** from a short-lived `silo-migrate` VM inside the VPC,
    never by `silod serve` at start-up. A running fleet upgrades by
    migrating first, then rolling the control VMs
    ([`state.md`, Schema migrations](state.md#schema-migrations-and-upgrades-call-35)).
36. **After a restore, reconcile only reports orphans** until an
    operator says otherwise. Objects created after the restore point
    would otherwise be deleted as orphans an hour later
    ([`state.md`, Backups and recovery](state.md#backups-and-recovery)).

The calls below were made in task 1945, the agents addendum
([`agents.md`](agents.md)). Placing harnesses on the control VM and
keeping tools remote is bsalmon's decision, from task 1913.

37. **An agent that needs bounds of its own gets a silo of its own.**
    An agent acts as its silo, and inside a silo everyone is the silo.
    Several agents in one silo are allowed, for agents meant to share
    everything ([`agents.md`, §1](agents.md#the-authority-of-an-agent)).
38. **An agent's own session may not renew its own lease, raise its
    own budget, or answer its own requests.** Otherwise leases and
    approvals bind nothing.
39. **v1 ships the `claude-code` and `codex` adapters**, the harnesses
    grain runs today, pinned in the control image.
40. **The facade refuses writes to a tool VM.** It is listed and
    charged like any instance, but managed through its agent, so that
    nobody can put another VM at its address.
41. **The tool server's `exec` sets the egress proxy variables**,
    because an agent's commands expect the internet. Tenant VMs are
    still left alone.
42. **The tool server dials out to the gateway**, and the channel is
    bound to the agent by origin and instance id. No fence rule is
    added and nothing listens in the tool VM
    ([`gateway.md`, Agent tool channels](gateway.md#agent-tool-channels)).
43. **An agent owns what it creates.** Child silos and agents its
    session creates are deleted with it, when its lease runs out or it
    is deleted.
44. **A harness reaches silod on a Unix socket per session**, and the
    socket is its identity. No token. In-process works only for a
    harness built into silod.
45. **Credentials are encrypted in silod's database with a key in the
    control VMs' metadata**, which no container on a control VM can
    reach.
46. **Model budget is dollars per period**, one period for the whole
    tree, kept as micro-dollars, a hard partition like quota
    ([`allocators.md`, Model budget](allocators.md#model-budget)).
47. **Subscription use is charged at API list price**, from the usage
    the harness reports after each turn, so a budget means the same
    thing whatever credential an agent uses.
48. **A harness doesn't outlive silod; its session does.** On start,
    silod relaunches every live agent with its adapter's resume.
49. **Harness CPU and memory aren't charged to silo quota in v1**, but
    concurrent sessions are a global count class, `agent_sessions`.
50. **Each harness session runs in its own container**: its own user,
    no capabilities, read-only root, only its session socket mounted,
    no network for API-key sessions, no metadata server.
51. **Every silo has an agent policy**, handed down like egress:
    allowed credentials, adapters and models, and a maximum lease
    ([`fence-api.md`, Agent policy](fence-api.md#agent-policy)).

These calls were made in task 1947, for the sealed-silo addendum:

52. **`read` is classified by method, not by HTTP verb.** `wait` and
    `quota:preview` are `POST`s and are reads. A method or path the
    shared classification table doesn't know counts as a mutation, so
    a method added to the facade later is refused under `read` until
    someone classifies it. A sealed silo can't even `describe` itself,
    and no silo can change its own `control`, not even to tighten it.
    An agent's harness keeps its tools, the model proxy, `ask`,
    `request_approval` and the control surface of its silo's agents
    (status, sessions, messages, answers), because those operate a
    service rather than change infrastructure
    ([`fence-api.md`, Control policy](fence-api.md#control-policy)).
53. **One API port, 8080, carries both the facade and the fence
    API.** Two ports would buy nothing, because both have reads that a
    `read` silo needs and the fence can't tell reads from writes. HTTP
    exports keep port 80 and their URLs (call 23). The control APIs
    moved rather than the exports because every client of the control
    APIs already sets an endpoint explicitly, while export URLs are
    handed around and should stay short
    ([`gateway.md`, Address](gateway.md#address)). The port 80 listener
    has no control handler mounted at all. It is called the API port,
    not the control port, so that it isn't confused with the control
    range and control VMs of call 32.
54. **`control` defaults to the parent's effective policy, and the
    effective policy is the strictest along the path.** Tightening an
    silo tightens its subtree at once, and keeps each descendant's own
    setting so that loosening restores it, as egress and grant policy
    do. Setting a child looser than its parent is `LOOSER_THAN_PARENT`.
    The root is always `full`.
55. **Ancestors may write into their descendants' facades and fence
    API, and make grant calls on their behalf.** This settles open
    question 4 and revises call 13. Without it, a sealed cell could be
    changed only by deleting it. The objects belong to, and are charged
    to, the descendant. The ancestor's consent stands in for the
    descendant's in grants, and the descendant's grant policy still
    applies. It is audited as `via=ancestor:<id>`.
56. **What a `manage` grant may carry.**
    - It is offered by a proper ancestor of the target and accepted by
      the holder, like any grant.
    - It covers the target and its descendants. Its scope names facade
      resource types, with `read` or `write` access, and fence groups
      (`read`, `grants`, `children`, `blocks`, `agents`). So it **can** be scoped
      to resource types, but not to single objects.
    - It never carries `control` (as recommended in task 1913), member
      mappings, changes to the target's own allocations and policies,
      deleting the target, or further `manage` grants.
    - The holder must be outside the target's subtree, and only the
      holder silo itself may use the grant: not its descendants, and not
      its ancestors.
    - A lease is required, at most 30 days, renewable by the holder up
      to a time the offerer set.
    - The holder's own control policy still applies to its calls.
57. **The production-cell walk-through keeps call 22.** Managed
    instance groups stay out of the v1 facade. Instance templates are
    allowed, but a template alone rolls nothing out, so a pipeline's
    "instance template" is its own terraform definition, and a rollout
    replaces instances one by one
    ([§7, A production cell](#a-production-cell)).

## Open questions

1. **Oversubscription.** Should a parent be able to grant its children
   more than it has, in total, on the bet that they won't all use it?
   v1 says no ([call 3](#calls-made-in-this-draft)).
2. **Spot capacity when `PREEMPTIBLE_CPUS` is 0.** GCE then charges
   Spot to `CPUS`. v1 gives the root no Spot capacity in that case.
   Should install instead let the operator move standard capacity into
   the Spot class?
3. **Per-family CPU quotas.** Should `N2_CPUS` and the like be handed
   down as classes of their own, or is passing GCE's refusal through
   enough?
4. ~~**Ancestor writes into descendants' facades.**~~ Settled in task
   1913 and written up in task 1947: ancestors may write (call 55).
5. **Guest hostnames.** Should the facade set a custom hostname on
   each instance (`builder.k7f3qa.silo.internal`, with a DNS record),
   so that the guest doesn't see its prefixed name? It costs a DNS
   record per instance and leaves GCE internal DNS unchanged.
6. **Cross-silo name visibility through GCE internal DNS.** Can a DNS
   policy or response policy hide other silos' instances from
   `*.internal` lookups without breaking the tenant's own? If not,
   should the docs simply tell tenants not to put secrets in names?
7. **Recovering from a lost database.** *Resolved in task 1952:*
   Cloud SQL's automated backups and point-in-time recovery, with
   reconcile in report-only mode after a restore (call 36,
   [`state.md`, Backups and recovery](state.md#backups-and-recovery)).
8. **TLS inside the VPC.** The facade and fence API are plain HTTP at
   `silo.internal`. VPC traffic is already encrypted by Google's
   network, and identity comes from the source IP, not from anything
   in the request. Is a private CA and HTTPS still worth the setup in
   every VM?
9. **Gateway replicas and silod failover.** *Partly resolved in task
   1952:* silod can run on several machines, because its state is
   shared, with a leader for the fence and the reconcile (call 34).
   Still open: should gateways serve facade reads from a cache while no
   silod can reach the database?
10. **The VPC backing's gateway reach**: PSC or peering? Peering is
    simpler but non-transitive and capped. PSC preserves no source IP
    without the PROXY protocol, which origin identity depends on.
11. **Uninstall and enabled APIs.** v1 leaves the APIs install
    enabled. Should uninstall record which ones it enabled and turn
    those off?
12. **Disk-type classes.** *Resolved in task 1956:* every class
    follows the GCE metric that limits it. `pd-extreme` capacity stays
    in `ssd_disk_gb` as GCE counts it, with its provisioned IOPS in
    `pd_extreme_iops`, and each Hyperdisk type gets its own capacity,
    IOPS and throughput classes (call 22,
    [`allocators.md`, Classes](allocators.md#classes)).
13. **How tenants reach their own VMs from outside.** With no ingress
    and no IAP SSH to tenant VMs, people inside a silo need a way in.
    It could be an SSH proxy at the gateway, an export to the front
    door, or IAP TCP with a per-silo firewall rule.
14. **The scheduled conformance run's credentials.** This deployment
    gives scheduled CI runs no live provider secrets. A nightly run
    against `bsalmon-fun-sandbox` needs `gcp-key` granted to that
    schedule. That is a grain/config change, not a change to this
    repository ([`testing.md`](testing.md#where-and-how-it-runs)).
15. **SNI versus the `CONNECT` host.** The egress proxy doesn't
    terminate TLS, so a tenant who controls an allowed name's DNS can
    tunnel anywhere that name points. Is that acceptable for v1, or
    should the proxy peek at the ClientHello and require that the SNI
    matches?
16. **The PSC alternative under org policy.** If an organization
    forbids PSC endpoints, should install fall back to private services
    access by itself, or only with a flag? v1 builds PSC only, and fails
    the prerequisite check with a message naming the constraint
    ([`state.md`, The alternative](state.md#the-alternative-private-services-access)).
17. **A SQLite install for throwaway use.** Should `silo install
    --store=sqlite` exist for a cheap demo install, to save Cloud SQL's
    fixed cost and its minutes at install? It would bring back the state
    disk and the single control VM (call 31).
18. **The default tier.** `db-g1-small` is shared-core and has no Cloud
    SQL SLA. `db-custom-1-3840` has one, at roughly twice the cost.
    Which should be the default? Implementation should also confirm
    that PSC and IAM login work on the shared-core tiers.
19. **Making silod's IAM user own its database without a password.**
    v1 grants `cloudsqlsuperuser` through the Admin API when it creates
    the user. If the Admin API can't do that, the fallback is one
    session as the built-in `postgres` user, with a password that
    install generates, uses from the `silo-migrate` VM, and then
    replaces with a random one it throws away. Is that acceptable, or
    is there a cleaner way?
20. **Should the conformance run reuse a standing Cloud SQL
    instance?** A fresh instance adds five to ten minutes and a little
    cost to every nightly run. A standing one tests less of install
    ([`testing.md`](testing.md#where-and-how-it-runs)).
21. **Hardening the control VM for harnesses.** Is a container per
    session enough, or should sessions run under gVisor, or on a
    separate harness VM without the cloud credentials?
22. **Where agent credentials are kept.** v1 encrypts them in the
    database, with the key in the control VMs' metadata (call 45). Secret Manager or Cloud KMS would take the key off
    the VM, at the cost of new IAM for silod.
23. **Subscription logins and their terms.** May one person's login be
    used by agents in silos they don't own, or by several sessions at
    once?
24. **Subscription rate limits as an allocator.** A login's rate
    windows are shared by every session on it, and v1 doesn't hand
    them down.
25. **Lifetime budgets.** v1 budgets are per period; a short task might
    want a total.
26. **Charging harness resources to silo quota** (call 49), if sessions
    grow.
27. **A tool VM image family with `silo-tools` in it**, so that agents
    start without fetching it at boot.
28. **Handing over owned objects.** Should an agent be able to give a
    child silo to its silo, so that it outlives the agent (call 43)?
29. **A timeout instead of a 403 for sealed VMs.** Is that acceptable,
    or should the fence send the API port to a gateway listener
    that only ever answers 403, at the cost of the separation that
    makes it a second layer?
30. **Object-level `manage` scope.** Should a grant be able to name
    objects by label or name prefix (only instances labelled
    `app=web`), so that one pipeline can't touch another service's
    objects in the same cell?
31. **Grants shared with a pipeline's children.** CI pipelines often
    run each job in a child silo, which can't use its parent's `manage`
    grant. Should a holder be able to pass a grant on to its own
    children, narrowed and with a shorter lease, as Shibu's grants
    could ([§9](#9-later))?
32. **MIGs in the facade.** A pipeline holding `manage` would then do
    a native rolling update instead of replacing instances itself.
    Instance templates are allowed (call 22). MIGs need a way to charge
    and check the instances a MIG makes outside the facade
    ([§9](#9-later)).
33. **Unsealing as break-glass.** Any ancestor can unseal a cell with
    one `PATCH`. Should unsealing, or loosening `control` at all, need a
    second ancestor's approval, or a delay, and should it notify the
    cell's other ancestors?
34. **A sealed silo reading its own state.** A sealed cell can't
    `describe` itself, so it can't read its own quota or blocks. Should
    `none` still allow `GET /silo/v1/self`, at the cost of opening the
    API port to it and losing the second layer?
