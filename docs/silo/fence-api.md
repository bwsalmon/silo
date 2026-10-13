# The `silo/v1` fence API

## Status

Normative draft, part of the Silo design ([`README.md`](README.md)),
written in task 1930. The control policy (sealed silos) and `manage`
grants were added in task 1947. The `isolation` and `ingress` policies
and the `local` backing and region were added in task 3408, for the
[local backend](local-backend.md), and [repos](#repos) and the `repos`
policy in task 3422 ([`local-git.md`](local-git.md)), and
[image repos](#image-repos) in task 3450
([`local-registry.md`](local-registry.md)). Marks such as "call 14" refer to
[Calls made in this draft](README.md#calls-made-in-this-draft).

The fence API is what every backend shares. It manages the tree, the
allocators, the grants between silos, egress policy, members and audit.
It says nothing about what is inside a silo; that is the backend's
facade ([`gcp-backend.md`](gcp-backend.md#the-computev1-facade)).

## Transport and conventions {#SV-1}

- **Base URL.** Inside the cloud: `http://silo.internal:8080/silo/v1/`,
  on the gateway's API port
  ([`gateway.md`, Address](gateway.md#address)). Outside: the same
  paths through the front door
  ([`gateway.md`](gateway.md#front-door)).
- **The caller** is a silo, from origin identity inside, from the
  member table at the front door, or, for an agent's harness on the
  control VM, from the session socket it called on
  ([`agents.md`, The session socket](agents.md#5-the-session-socket-how-a-harness-reaches-silod)).
  Every operation below says which silos the caller may name.
- **JSON** request and response bodies, with `snake_case` fields.
- **Silo references.** A silo is named by its path (`/team-a/ci`) or
  its id (`k7f3qa`). In a URL, a path is written with its leading `/`
  dropped and its `/` kept: `/silo/v1/silos/team-a/ci`. A segment that
  begins with `-` is an id: `/silo/v1/silos/-k7f3qa`. `/silo/v1/self`
  is the caller's own silo.
- **Verbs** are HTTP methods on resources, and `:verb` suffixes for
  actions, as in Google's API style (`POST .../ci:grow`).
- **Idempotency.** Every mutating call takes an optional `request_id`
  (a UUID). A repeat with the same id within 24 hours returns the
  first call's result.
- **Dry run.** Every mutating call takes `?validate_only=true`. It
  runs every check, then returns what would happen without doing it.
- **Pagination.** `page_size` and `page_token`, as in the facade.
- **Versioning.** `v1` changes only by adding things. A field a
  client doesn't know is to be ignored.

### Errors {#SV-2}

```json
{"error": {"code": "QUOTA_EXCEEDED", "message": "us-central1 vcpu: asked 16, /team-a has 8 free",
  "details": {"class": "vcpu", "region": "us-central1", "asked": 16, "free": 8}}}
```

| Code | HTTP | When |
| --- | --- | --- |
| `INVALID_ARGUMENT` | 400 | A malformed field, an unaligned range, a bad name |
| `PERMISSION_DENIED` | 403 | The caller may not name that silo, or may not do that to it, or a `manage` grant doesn't cover the call (`details` names the grant and what is missing) |
| `CONTROL_DENIED` | 403 | The caller's own effective `control` policy forbids the call: any call under `none`, a mutating call under `read`. `details` gives the policy and the silo it comes from |
| `LOOSER_THAN_PARENT` | 403 | The change would give a child more than its parent has: a region, an egress host, a grant policy, a `control` policy, an isolation class, `ingress: host` or `repos.mirror` that the parent lacks |
| `QUOTA_EXCEEDED` | 403 | Not enough free quota in the parent (or, for a facade call, in the silo) |
| `RESOURCE_EXHAUSTED` | 429 | No free aligned slot of that size, the block cap, the depth cap, or the fence's rule budget |
| `NOT_FOUND` | 404 | No such silo, grant, repo or record visible to the caller |
| `ALREADY_EXISTS` | 409 | A sibling with that name, or a grant or repo with that name |
| `FAILED_PRECONDITION` | 409 | The change would strand something: lowering quota below usage, removing a region still in use, releasing a non-empty block, deleting a non-empty silo without `force` |
| `UNIMPLEMENTED` | 501 | A backing this deployment doesn't support (`vpc` and `project` in v1, and a child's backing other than its parent's) |
| `UNAVAILABLE` | 503 | silod is restarting, its database is unreachable, or the cloud is refusing calls |

A silo that the caller may not see returns `NOT_FOUND`, never
`PERMISSION_DENIED`, so that the tree outside the caller's subtree
isn't revealed.

## Who may do what {#SV-3}

| Caller relative to the target silo | May |
| --- | --- |
| The silo itself | `describe`, read its audit, narrow its own egress, grant and agent policy, offer and accept grants, release a free block, create, change and delete its own [agents](#agents) |
| A proper ancestor | Everything above, plus: create and delete children, change allocations and policies (including `control`), map members, read audit, offer and revoke `manage` grants, and make grant calls on the target's behalf |
| The holder of an active `manage` grant on the target, or on one of its ancestors | What the grant's scope allows, within the limits in [Manage grants](#manage-grants). Never `control`, members or `manage` itself |
| Anyone else | Nothing. The target is `NOT_FOUND`. |

Changing allocations and policies is done by an ancestor, never by the
silo itself, except to narrow its own. Creating a child is done by the
child's parent, or by any ancestor of the parent.

An agent acts as its silo, so it has the silo's row above. Its own
session may not renew its own lease, raise its own budget, or answer
its own requests (call 38).

Every row is then cut down by the **caller's own** effective control
policy ([Control policy](#control-policy)). A caller whose policy is
`read` may make only the reads in its row, and a caller whose policy is
`none` may make no call at all. This applies to the caller whatever
the target is: a sealed ancestor can't change its descendants either.

## Objects

### Silo {#SV-4}

```json
{
  "id": "p2m8xd",
  "name": "ci",
  "path": "/team-a/ci",
  "parent": "k7f3qa",
  "depth": 2,
  "backing": {"kind": "subnet"},
  "facade": {"kind": "gce", "project": "silo-p2m8xd", "endpoint": "http://silo.internal:8080/compute/v1/", "name_prefix": "p2m8xd-"},
  "regions": ["us-central1"],
  "quota": {
    "us-central1": {
      "vcpu": {"granted": 16, "delegated": 4, "used": 8},
      "spot_vcpu": {"granted": 64, "delegated": 0, "used": 0}
    },
    "global": {
      "subnetworks": {"granted": 10, "delegated": 0, "used": 1},
      "agent_sessions": {"granted": 4, "delegated": 1, "used": 1},
      "repos": {"granted": 10, "delegated": 0, "used": 2}
    }
  },
  "model_budget": {"period": "month", "granted_usd": "200.00", "delegated_usd": "40.00", "used_usd": "40.00"},
  "address": {
    "blocks": ["10.1.16.0/20"],
    "max_blocks": 4,
    "delegated": [{"range": "10.1.20.0/22", "child": "/team-a/ci/job-412"}],
    "own_ranges": [{"range": "10.1.16.0/24", "subnet": "subnetworks/us-central1/p2m8xd-build", "region": "us-central1"}]
  },
  "egress": {"own": {"allow": [], "deny": []}, "effective": {"allow": ["*.debian.org"], "deny": []}},
  "grant_policy": {"own": {"exports": "allow", "links": "deny"}, "effective": {"exports": "allow", "links": "deny"}},
  "control": {"own": "full", "effective": "full", "effective_from": "/team-a/ci"},
  "isolation": {"own": {"allowed": ["vm", "sandbox"], "default": "vm"}, "effective": {"allowed": ["vm"], "default": "vm"}},
  "ingress": {"own": "none", "effective": "none"},
  "repos": {"own": {"mirror": true}, "effective": {"mirror": false}, "effective_from": "/team-a"},
  "agent_policy": {"own": {}, "effective": {"credentials": ["anthropic-api"], "adapters": ["claude-code"],
    "models": ["claude-*"], "max_lease": "24h"}},
  "owner_agent": null,
  "state": "ACTIVE",
  "over_committed": [],
  "create_time": "2026-10-04T16:00:00Z",
  "labels": {"team": "a"}
}
```

`state` is `ACTIVE`, `DELETING` or `DELETE_FAILED`. `over_committed`
lists classes and regions where the silo holds more than its grant,
which happens only at the root when GCE's quota drops
([`allocators.md`, Root capacity](allocators.md#root-capacity)).

### Backing {#SV-5}

`{"kind": "subnet"}` (the GCP backend) or `{"kind": "local"}` (the
[local backend](local-backend.md)) in v1. `{"kind": "vpc"}` and
`{"kind": "project"}` are reserved, with any options they turn out to
need, and are refused with `UNIMPLEMENTED` until a backend supports
them.

- The root's backing is set at install. A tree has the root's backing
  throughout: a child's defaults to its parent's, and a different one
  is refused with `UNIMPLEMENTED`. Mixed nesting, a local child of a
  GCE silo, comes later
  ([`local-backend.md`, open question 10](local-backend.md)).
- A `local` tree has one region, `local`, which every silo in it has
  ([`local-backend.md`, "Classes"](local-backend.md#classes-call-l19)).
  A `local` silo can't have a GCE region, and a `subnet` silo can't
  have `local`.

### Isolation and ingress policies

Two policies the [local backend](local-backend.md) enforces. Every
silo has both, whatever its backing; the GCP backend ignores them.

**`isolation`** is the classes the silo's containers may run in, and
the one a container gets when it names none
([`local-backend.md`, "In a silo's policy"](local-backend.md#in-a-silos-policy)):

```json
"isolation": {"allowed": ["vm", "sandbox"], "default": "vm"}
```

- The classes are `vm` and `sandbox`, ordered by strength: `vm` is
  stronger. `allowed` is always written strongest first.
- `allowed` can't be empty, and `default` must be in it. A `default`
  left out is the strongest class allowed.
- The **effective** `allowed` is the intersection of the silo's own
  and every ancestor's. The effective `default` is the silo's own if
  that is still allowed, and otherwise the strongest class that is.
- A child's `allowed` is a subset of its parent's effective one, or
  `LOOSER_THAN_PARENT`. A parent that allows only `vm` makes its whole
  subtree `vm`-only: that is how a parent requires `vm`.
- The root's is set at install: every class the host offers, unless
  the operator narrowed it.

**`ingress`** is `host` or `none`: whether the silo's published ports
also listen on the host's loopback
([`local-backend.md`, "Published ports"](local-backend.md#published-ports)).

- The effective value is `host` only if the silo's own and every
  ancestor's is `host`.
- A child's can be `host` only if its parent's effective value is, or
  `LOOSER_THAN_PARENT`.
- The root's is `host`, set at install. A child's defaults to `none`.

### The repos policy

**`repos`** is `{"mirror": true|false}`: whether the silo's
[repos](#repos) are mirrored into the uber repo
([`local-git.md`, "What egress allowlists mean here"](local-git.md#what-egress-allowlists-mean-here),
call L54).

- The effective `mirror` is `true` only if the silo's own and every
  ancestor's is. `effective_from` is the nearest silo up the tree that
  turned it off, or the root.
- A child's can be `true` only if its parent's effective value is, or
  `LOOSER_THAN_PARENT`. A child's defaults to its parent's effective
  value. A silo may turn its own off, but only an ancestor turns it
  back on.
- The root's is `true` unless bootstrap is given another, and the root
  may change its own either way with `PATCH`.

## Operations

### describe {#SV-6}

`GET /silo/v1/self`, `GET /silo/v1/silos/{silo}`

Returns the silo object above. `?view=subtree` adds `children` and
recurses (ancestors only).

`GET /silo/v1/silos/{silo}/children` lists the direct children.

### create {#SV-7}

`POST /silo/v1/silos/{parent}/children`

```json
{
  "name": "ci",
  "backing": {"kind": "subnet"},
  "regions": ["us-central1"],
  "quota": {"us-central1": {"vcpu": 16, "spot_vcpu": 64, "memory_mb": 65536}, "global": {"agent_sessions": 2}},
  "model_budget_usd": "50.00",
  "address": {"prefix_length": 20},
  "max_blocks": 4,
  "egress": {"allow": ["*.debian.org"]},
  "grant_policy": {"exports": "allow", "links": "deny"},
  "control": "full",
  "agent_policy": {"credentials": ["anthropic-api"], "max_lease": "8h"},
  "isolation": {"allowed": ["vm"]},
  "ingress": "none",
  "labels": {"team": "a"}
}
```

- `address` is `{"prefix_length": n}` (silod picks the slot) or
  `{"range": "10.1.16.0/20"}` (the parent picks it). It is required.
- `regions` defaults to the parent's. `quota` and `model_budget_usd`
  default to nothing. `egress` defaults to deny all. `grant_policy`,
  `agent_policy`, `control` and `isolation` default to the parent's
  effective policy (call 54 for `control`). An `isolation` with only a
  `default` allows the parent's effective classes. `ingress` defaults
  to `none`. `backing` defaults to the parent's.
- Checks: depth ≤ 16; the backing is the parent's; regions ⊆ the
  parent's; quota fits in the parent's free; the range is aligned and
  free in the parent; egress, grant, agent, control, isolation and
  ingress policy are no wider than the parent's effective ones; the
  model budget fits in the parent's free budget.
- Effects: one transaction that records the silo and moves the quota
  and the block, then a fence update. The call returns once the fence
  rules are in place, because until then the child's addresses are
  still the parent's.

### delete {#SV-8}

`DELETE /silo/v1/silos/{silo}?force=true`

- Without `force`, it fails with `FAILED_PRECONDITION` if the silo has
  children, facade objects or grants.
- With `force`, it deletes the subtree bottom-up. For each silo, it
  withdraws its grants (closing proxied connections), deletes its
  facade objects (instances first, then disks, images, snapshots,
  firewall rules, subnets), removes its fence rules, and returns its
  blocks and quota to its parent.
- It returns a long-running operation:
  `GET /silo/v1/operations/{op}`. `state` is `DELETING` until it
  finishes. If something can't be deleted, the state becomes
  `DELETE_FAILED`, with the reason in the operation, and a repeat call
  resumes.
- The root can't be deleted through the API. That is `silo uninstall`.

### Changing allocations {#SV-9}

`PATCH /silo/v1/silos/{silo}` with any of `regions`, `quota`,
`model_budget_usd`, `max_blocks`, `egress`, `grant_policy`,
`agent_policy`, `control`, `isolation`, `ingress`, `labels`. Called by
an ancestor, or by the silo itself to narrow its own `egress`,
`grant_policy`, `agent_policy`, `isolation` and `ingress` if its
control policy is `full`. `control` is set only by an ancestor
([Control policy](#control-policy)). The backing never changes, and
the root's `isolation` and `ingress` are set at install, not with
PATCH.

- `quota` is replaced per region and class as given. A class not
  given is unchanged. To return a class to zero, give `0`.
- Raising checks the parent's free amount. Lowering checks that the
  silo's `delegated + used` still fits. `model_budget_usd` is checked
  the same way ([`allocators.md`, Model budget](allocators.md#model-budget)).
- Narrowing `egress` or `grant_policy` applies to the subtree at once:
  the gateway closes proxied connections and denies hosts that the
  change no longer allows.
- `isolation` replaces the fields it gives. A new `allowed` that drops
  the `default` moves it to the strongest class kept. Like `control`,
  an `isolation` or `ingress` the call leaves alone isn't held to the
  parent's: an ancestor narrowing keeps its descendants' own settings,
  narrows their effective ones, and gives them back when it widens
  again.

**Address changes** are actions:

- `POST /silo/v1/silos/{silo}:grow` `{"block": "10.1.16.0/20", "to_prefix_length": 19, "add_block": false}`.
  This widens in place, or, with `add_block`, adds a new block of that
  size if widening fails ([`allocators.md`, Growth](allocators.md#growth)).
- `POST /silo/v1/silos/{silo}:addBlock` `{"prefix_length": 22}` or
  `{"range": ...}`.
- `POST /silo/v1/silos/{silo}:releaseBlock` `{"block": "10.1.32.0/22"}`.
  The block must hold no own range and no delegated range.

### Quota preview {#SV-10}

`POST /silo/v1/quota:preview`

```json
{"facade_request": {"method": "POST", "path": "/compute/v1/projects/silo-k7f3qa/zones/us-central1-a/instances",
  "body": {"machineType": "zones/us-central1-a/machineTypes/n2-standard-8"}}}
```

or `{"allocation": {"silo": "/team-a/ci", "quota": {...}}}`.

It returns the charge by class and region, and, for each silo from the
one charged up to the root, the free amount before and after, and
whether the request fits. Nothing changes.

### Room for custom allocators {#SV-11}

Custom allocators come after v1. With them, a silo registers an
allocator kind, grants its classes to other silos, and hands them down
the tree, and `describe` lists custom classes beside the built-in ones
([`custom-allocators.md`, Fence API additions](custom-allocators.md#fence-api-additions)).
So that this is only an addition, **v1's fence API must already**:

- **treat class names as open strings.** The `quota` maps in
  `describe`, `create` and `PATCH` are keyed by class name. Clients
  ignore classes they don't know, under the versioning rule above.
  silod refuses an unknown class with `INVALID_ARGUMENT`, found by
  looking it up in the class table. Names containing `@` or `/` are
  reserved for custom classes (`ci-pool@q4ci7a/runner_minutes`);
- **treat `grant_policy` as an open map** of grant kind to `allow` or
  `deny`. A key v1 doesn't know, such as `allocators`, is kept,
  inherited and shown, not refused. An absent key means `deny`;
- **say who pays** in `quota:preview`: charges come back as a list of
  `{payer, kind, class, region, amount}`;
- **carry `payer`, `home` and `via`** on every audit record that
  charges or refunds quota ([Audit](#audit));
- **give exports an owner** field, `tenant` in v1, so that an export
  owned by a handle can't later be withdrawn by a tenant;
- **reserve the paths** `/silo/v1/self/allocators`, `allocator-offers`,
  `allocator-grants`, `resources`, their `/silo/v1/silos/{silo}/...`
  forms, and `/silo/v1/allocators`. v1 returns `UNIMPLEMENTED` for
  them.

## Control policy {#SV-12}

Every silo has a `control` policy, decided in task 1913. It says what
principals acting **as** the silo may do through the control APIs, the
fence API and the facade:

| `control` | The silo's own calls on the fence API and the facade |
| --- | --- |
| `full` | Whatever the [Who may do what](#who-may-do-what) table allows. The default today. |
| `read` | Reads only: `describe`, listing children, reading audit, listing grants, `quota:preview`, and the facade's `get`, `list`, `aggregatedList`, `getFromFamily`, `getSerialPortOutput` and operation `get`, `list` and `wait`. Every other call is refused with `CONTROL_DENIED`, or with the facade's 403. |
| `none` | **Sealed.** Nothing. Every call is refused, `describe` of itself included. |

**Who it binds.** Every principal that acts as the silo: its VMs,
identified by origin (an agent's tool VM included); front-door members
mapped to it; and its agents' harnesses, on their session sockets
(`via=harness`,
[`agents.md`, §5](agents.md#5-the-session-socket-how-a-harness-reaches-silod)).
It doesn't bind anyone acting as another silo: an ancestor writing into
the silo, or the holder of a `manage` grant on it, acts as itself and
is bound by its own policy. Nor does it bind what silod does to carry
out such a caller's request: an ancestor creating an agent in a sealed
silo gets its tool VM made, although the tool VM is made "as the
agent's silo" ([`agents.md`, Creation](agents.md#creation)).

**What it doesn't touch.** It is about the control plane only. Egress,
proxied exports in both directions, L3 links, agent tool channels and
model access keep working. Grants that are already `ACTIVE` stay
active; the silo just can't offer, accept or withdraw grants itself.

**Agents in a `read` or sealed silo.** An agent there can operate its
service but can't change its infrastructure. Its harness, on its
session socket, keeps:

- the tool VM's tools (`exec`, `read_file` and the rest), so it can
  read logs, restart a process and edit configuration on its VM;
- the model proxy;
- its own `ask` and `request_approval`;
- the [agent control surface](#agents) for the silo's own agents: the
  agent objects (status), sessions, `GET` and `POST` on messages, and
  answering requests (still not its own, call 38).

Everything else on the socket is a fence or facade call and follows
the table above: under `none` the fence tools (`silo_describe`,
`silo_create_child`, `agent_create` and the rest) and the `compute`
tool are refused; under `read` only their reads work. Creating,
deleting, stopping, resuming, renewing and `PATCH`ing agents are
mutations, so an ancestor or a `manage` holder does them. A sealed
silo's VMs, its tool VMs included, can't reach the control surface at
all, since the API port is dropped; the exemption is for the harness
and for front-door members (call 52).

**Who sets it.** Only a proper ancestor whose own effective policy is
`full`, on create or with `PATCH`:

```sh
silo create /prod --control none --address /20 ...     # as /prod's parent, or an ancestor of it
silo patch /prod --control read                         # as an ancestor of /prod
silo describe /prod                                     # control: own none, effective none (from /prod)
```

The silo itself can never change it, not even to tighten it: a `PATCH`
of its own `control` is `PERMISSION_DENIED` (and a sealed silo can't
reach the API to try). A `manage` grant never carries it
([Manage grants](#manage-grants)). The root's policy is always
`full`, because it has no ancestor to set it.

**Inheritance.** A child can't be looser than its parent, with
`none` < `read` < `full`:

- the **effective** policy is the strictest of the silo's own and every
  ancestor's, and `describe` shows `own`, `effective` and
  `effective_from` (the silo whose setting wins);
- `create` or `PATCH` that sets a child's `control` looser than its
  parent's effective policy fails with `LOOSER_THAN_PARENT`;
- tightening a silo tightens its whole subtree's effective policy at
  once. The descendants' own settings are kept, so loosening it again
  restores them (call 54). This matches how egress and grant policy
  behave.

So the children of a `read` silo are at most `read`. A `read` or
sealed silo can't create children itself, since `create` is a mutation,
but an ancestor above it can, and the new child is no looser.

**Enforcement** has two layers, either of which stops the call alone
([`gateway.md`, Control policy](gateway.md#control-policy)):

1. the gateway and silod refuse the call, by the method's class rather
   than its HTTP verb;
2. for `none`, the fence drops the gateway's API port from the
   silo's blocks, so its packets never reach the gateway
   ([`gcp-backend.md`, The fence](gcp-backend.md#the-fence)).

A change of `control` returns once both layers are in place: the
gateway has the new table, open API-port connections from the
subtree are closed, and the fence rules are updated. It is audited as
`silo.set_control`.

## Grants: exports and links {#SV-13}

A grant between two silos exists only when both sides have agreed.
Before then it is `PENDING`. Each side's **effective grant policy**
must allow that kind of grant (call 14): it is the AND of the silo's
own policy and every ancestor's policy for it. If either side's policy
stops allowing it, the grant is `SUSPENDED` and its traffic stops. It
resumes if the policy allows it again.

### Exports (proxied) {#SV-14}

Offering, by the exporter:

`POST /silo/v1/self/exports`

```json
{"name": "pkgcache", "target": "10.1.0.5:8080", "protocol": "http",
 "to": ["/team-a/ci"], "proxy_protocol": false}
```

- `target` must be in the exporter's own space. It can also be an
  instance name and port (`{"instance": "cache", "zone": "us-central1-a", "port": 8080}`),
  which silod resolves to the instance's primary IP, and follows if it
  changes.
- `protocol` is `http` or `tcp`.
- `to` lists the silos the export is offered to. A path in `to` needn't
  be visible to the exporter, and nothing is revealed about whether it
  exists.

Accepting, by an importer:

`POST /silo/v1/self/imports` `{"export": "/team-a/pkgcache"}`. The
export is named by the exporter's path and the export name. This
returns the address: `http://silo.internal/x/k7f3qa/pkgcache/` for
HTTP, or `silo.internal:20417` for TCP.

`GET /silo/v1/self/exports`, `GET /silo/v1/self/imports` and
`GET /silo/v1/self/offers` (exports offered to this silo and not yet
accepted) list them.

Withdrawing: `DELETE /silo/v1/self/exports/{name}` (the exporter, for
everyone), `DELETE /silo/v1/self/exports/{name}/to/{silo}` (the
exporter, for one importer), or `DELETE /silo/v1/self/imports/{id}`
(the importer). Open connections close within a second
([`gateway.md`, Proxied exports](gateway.md#proxied-exports)).

### Links (L3) {#SV-15}

`POST /silo/v1/self/links`

```json
{"name": "db", "peer": "/team-b", "direction": "outbound",
 "rules": [{"protocol": "tcp", "ports": ["5432"]}]}
```

- `direction` is `outbound` (connections start in this silo), `inbound`
  or `both`. The peer must propose the mirror image: the same `rules`,
  with the opposite direction (`both` matches `both`).
- When the two match, the link becomes `ACTIVE`, and silod writes
  `allow` rules between every block of each side, for those protocols
  and ports.
- `rules` name protocols and, for TCP and UDP, ports. `{"protocol":
  "all"}` is allowed, and both sides have to agree to it like any
  other rule.
- Either side may `DELETE /silo/v1/self/links/{name}`. silod removes the
  rules. Established connections then fail at their next packet,
  because GCE's connection tracking doesn't outlive a deny.
- A link names blocks, not subnets. If either side adds a block, silod
  extends the link's rules to cover it.

A link between a silo and its own ancestor or descendant is allowed.
Their spaces are disjoint (one's own space excludes what it has
delegated), so the rules name each side's own space, and the fence
writes them as ranges with the descendant's blocks carved out where
needed.

### Grant calls on a descendant's behalf {#SV-16}

A sealed silo can't offer or accept anything, so its grants are made
for it. Every `/silo/v1/self/...` grant call above also exists as
`/silo/v1/silos/{silo}/...` (`exports`, `imports`, `offers`, `links`),
callable by a proper ancestor of `{silo}`, or by a `manage` holder
whose scope includes `grants`. It acts exactly as if `{silo}` had made
the call: `{silo}`'s own grant policy and its ancestors' still have to
allow it, and the other side still has to agree. The ancestor's
consent stands in for the descendant's (call 55). The audit record
names `{silo}` and the real caller ([Audit](#audit)).

## Manage grants {#SV-17}

A **`manage` grant** lets one silo, the **holder**, act on another silo's
facade and fence API, the **target**, as if it were an ancestor of the
target, without being one. It is how a sealed production cell is
changed by a deploy pipeline that isn't above it in the tree. It was
decided in task 1913; the details below are calls (call 56).

### What a holder may do {#SV-18}

On the target **and its descendants**, within the grant's scope:

- **facade**: every read and write a proper ancestor could make, on
  the resource types the scope names. Objects belong to the target and
  are charged to its quota, as if the target had made them;
- **fence** (each an opt-in group in the scope):
  - `read`: `describe`, children, audit, grant lists. Always included;
  - `grants`: grant calls on the target's behalf
    ([above](#grant-calls-on-a-descendants-behalf));
  - `children`: create and delete the target's descendants, and
    change their allocations and policies, drawn from the target's own;
  - `blocks`: `grow`, `addBlock` and `releaseBlock` on the target's
    descendants, and `releaseBlock` on the target;
  - `agents`: create, delete, stop, resume, renew and `PATCH` agents in
    the target and its descendants, and use their control surface.
    Model budget for them comes from the target's own, as when the
    target creates them; a holder can't raise it.

A holder may **never**:

- change any silo's `control` policy. Only a real ancestor may
  (recommendation in task 1913, taken here). Otherwise a pipeline,
  or whoever compromises it, could unseal the cell it deploys to;
- change the **target's own** allocations, egress or grant policy, or
  delete the target. Those come out of the target's parent, which the
  holder has no authority over;
- map members to the target or its subtree, since a member acts as the
  silo it is mapped to;
- offer, revoke or pass on `manage` grants. There is no re-delegation
  in v1.

Every call is still bound by the **holder's** own effective control
policy. A `read` holder gets reads only, and a sealed holder gets
nothing, since its VMs can't reach the API port.

### Scope {#SV-19}

```json
{
  "name": "rollout",
  "holder": "/deploy",
  "scope": {
    "facade": {"resources": ["instances", "disks", "subnetworks", "firewalls"], "access": "write"},
    "fence": ["read", "grants"]
  },
  "lease": {"ttl": "86400s", "renew_until": "2027-01-01T00:00:00Z"}
}
```

- `facade.resources` names facade resource types from
  [`gcp-backend.md`, Covered](gcp-backend.md#covered), `repos`
  ([below](#repos)) and `registry` ([below](#image-repos)). `["*"]` means every type, including types added
  later. A call is
  charged to the resource in its path: a boot disk that
  `instances.insert` creates is covered by `instances`. `access` is
  `read` or `write`.
- `fence` lists the groups above.
- An empty scope covers `read` on the fence only.

So `manage` **can** be scoped to resource types (decided here, call
35). It can't be scoped to single objects, names or labels in v1
([Open questions](README.md#open-questions)).

### Lease {#SV-20}

- `lease.ttl` is how long the grant lasts from acceptance or from the
  last renewal, at most 30 days. It is required.
- `lease.renew_until` is the latest time the holder may renew to.
  Without it, the grant can't be renewed.
- When the lease runs out, the grant becomes `EXPIRED`, and it is
  treated like a revoked one.

### Operations {#SV-21}

Offering, by a proper ancestor of the target whose control policy is
`full`:

`POST /silo/v1/silos/{target}/manageGrants` with the body above.
Each grant has a `name`, unique per target, and an `id` that silod
assigns.

- The holder must lie outside the target's subtree. A grant to the
  target itself or to one of its descendants is `INVALID_ARGUMENT`, so
  a cell can never be given the means to change itself. A grant to an
  ancestor of the target is refused too, as it adds nothing.
- `holder` needn't be visible to the offerer, and nothing is revealed
  about whether it exists, as for export offers.

Accepting, renewing and giving up, by the holder:

- `GET /silo/v1/self/manageGrants` lists the grants offered to and held
  by the caller, with each target's path;
- `POST /silo/v1/self/manageGrants/{id}:accept` makes it `ACTIVE`. Until
  then it is `PENDING` and gives nothing, as for every grant;
- `POST /silo/v1/self/manageGrants/{id}:renew` restarts the `ttl`, up to
  `renew_until`;
- `DELETE /silo/v1/self/manageGrants/{id}` gives it up.

Revoking, by any proper ancestor of the target whose control policy is
`full`:

`DELETE /silo/v1/silos/{target}/manageGrants/{name}`.

Listing, by an ancestor of the target: `GET
/silo/v1/silos/{target}/manageGrants`.

When a grant ends (revoked, given up or expired), silod tells the
gateway at once. The next call that relies on it is refused, and the
gateway closes the holder's open API-port connections. A GCE
operation the holder had already started runs to completion, because
GCE can't cancel it, and silod finishes its bookkeeping for it as
usual. Deleting the target or the holder ends the grant too.

While a grant is `ACTIVE`, the holder can see the target: `describe`,
the facade project `silo-<target id>`, and the target's path in the
holder's grant list. Without one, the target is `NOT_FOUND` to the
holder, as before.

### Audit {#SV-22}

Each call the holder makes is recorded under the target, with
`silo=<target>`, `actor=<holder>` and `via=manage:<holder id>`, so the
target's audit, which its ancestors read, shows every change made to
it and by whom. The holder's own audit shows the same records.

## Agents

An agent is an object in a silo: a harness session on the control VM
and a tool VM in the silo. The design is in [`agents.md`](agents.md);
this section is its API.

### Agent policy {#SV-23}

Every silo has an agent policy, handed down like egress (call 51):

```json
{"credentials": ["anthropic-api"], "adapters": ["claude-code", "codex"], "models": ["claude-*"], "max_lease": "24h"}
```

- `credentials`, `adapters` and `models` are allowlists. A `models`
  entry may end in `*`. A missing list allows whatever the parent's
  effective policy allows; an empty one allows nothing.
- `max_lease` is the longest an agent in the silo may be leased from
  now, at create or renewal.
- The **effective** policy is the intersection of the silo's own and
  every ancestor's. The root's allows every credential, adapter and
  priced model, with a `max_lease` of 7 days. A child's can't be
  looser (`LOOSER_THAN_PARENT`). Narrowing it doesn't stop running
  agents; it applies at their next create, resume or renewal.

### Operations {#SV-24}

| Call | Does |
| --- | --- |
| `POST /silo/v1/silos/{silo}/agents` | Create an agent: the [agent object](agents.md#2-the-agent-object)'s `name`, `harness`, `model`, `budget.usd`, `tool_vm`, `lease`, `prompt` and `labels`. Returns a long-running operation that finishes when the tool server has connected. |
| `GET /silo/v1/silos/{silo}/agents`, `GET .../agents/{agent}` | List, or get one. `GET /silo/v1/agents/-{id}` gets one by id. |
| `PATCH .../agents/{agent}` | Change `budget.usd`, `labels` |
| `DELETE .../agents/{agent}` | Stop the session, delete the tool VM and what the agent owns, return its budget. A long-running operation. |
| `POST .../agents/{agent}:stop`, `:resume` | Stop the harness and keep the tool VM; start it again from its saved session |
| `POST .../agents/{agent}:renew` | `{"expire_time": ...}` or `{"ttl": "4h"}` |
| `GET .../agents/{agent}/session` | The transcript, as events; `?follow=true` streams them |
| `GET`, `POST .../agents/{agent}/messages` | Read the conversation; send the agent a message |
| `GET .../agents/{agent}/requests`, `POST .../requests/{id}:answer` | Its open questions and approval requests; answer one |

Checks on create: the adapter, credential and model are in the silo's
effective agent policy, and the model has a price; the lease is within
`max_lease`; `budget.usd` fits in the silo's free model budget; the silo
has an `agent_sessions` slot free; the tool VM passes the facade's
checks and fits the silo's quota; `tool_vm.subnet` is one of the
silo's subnets, by its name, which starts with the silo's id
([`gcp-backend.md`, Naming](gcp-backend.md#naming)).
Failures use the codes above: `QUOTA_EXCEEDED` (with `class`
`model_usd`, `agent_sessions` or a quota class), `PERMISSION_DENIED`
for a policy refusal, `INVALID_ARGUMENT` for an unpriced model, and
`RESOURCE_EXHAUSTED` when the control VM has no session slot left.

Deleting a silo with `force` deletes its agents first.

## Repos

A repo is a git repo in a silo: a silo object, created explicitly,
named per silo, and kept on the silo's backend. The design is in
[`local-git.md`](local-git.md); this section is its API. Only the local
backend keeps repos so far: elsewhere creating one is
`FAILED_PRECONDITION`.

```json
{
  "name": "foo",
  "id": "r-8k2m4q",
  "silo": "/team-a/x",
  "default_branch": "main",
  "clone_url": "http://silo.internal/git/team-a/x/foo.git",
  "clone_url_by_id": "http://silo.internal/git/-k7f3qa/foo.git",
  "size_bytes": 1843200,
  "create_time": "2026-10-12T18:54:00Z",
  "mirror": {"state": "UNCONFIGURED", "external_prefix": "refs/heads/silo/team-a/x/foo.git/", "last_error": null}
}
```

- A `name` is 1 to 63 characters of `[a-z0-9._-]`, starting with a
  letter or digit, with no `..`, and not ending in `.git` or `.lock`.
  Two silos can each have the same name.
- `default_branch` is the repo's `HEAD`: `main` unless create names
  another (or, for a seeded repo, the seed's), and changed only by
  `PATCH`.
- `mirror.state` is `OFF` where the silo's effective
  [repos policy](#the-repos-policy) keeps it out of the uber repo
  (`mirror.from` names the silo that does), and `UNCONFIGURED` until
  the uber repo is set up.

| Call | Does |
| --- | --- |
| `POST /silo/v1/silos/{silo}/repos` | Create. Body: `name`, optional `default_branch`, optional `seed` |
| `GET /silo/v1/silos/{silo}/repos` | List. `?recursive=true` lists the descendants' repos too, each with its silo's path |
| `GET .../repos/{repo}` | Get one |
| `PATCH .../repos/{repo}` | Change `default_branch` |
| `DELETE .../repos/{repo}` | Delete. A long-running operation of kind `repo.delete`, done once the repo is gone from disk |

- **Who may call them** ([`local-git.md` §4](local-git.md#4-the-fence-on-repos),
  call L43): the silo itself and its ancestors, any of them; the holder
  of an active `manage` grant on the silo or an ancestor whose
  `facade.resources` covers `repos`, list and get at `read` and all of
  them at `write`; anyone else, `NOT_FOUND`. The caller's own control
  policy then applies: under `read`, list and get only.
- **Seeding.** `"seed": {"repo": "/team-a/x/foo"}` copies a repo's
  branches and tags into the new one, server-side. The seed is a path
  to a repo the **caller** may read, relative to the caller's own silo
  unless it starts with `/`. One it can't read is `NOT_FOUND`, as one
  that doesn't exist. The copy is a snapshot; nothing links the two
  afterwards.
- **Quota.** Each repo holds one of its silo's `repos` (a global count
  class) while it exists, so a create past it is `QUOTA_EXCEEDED`. The
  root's is unlimited unless install names one; a child's is what its
  parent grants, as for any class.
- Creates, `PATCH`es and deletes are audited under the silo as
  `repo.create`, `repo.patch` and `repo.delete`. Deleting a silo with
  `force` deletes its repos.

## Image repos

An image repo is a repository of the host-local OCI registry at
`registry.silo.internal`: a silo object, named per silo, and created by
its first push rather than by a call. The design is in
[`local-registry.md`](local-registry.md); this section is its fence API.
The registry service that pushes and pulls is a later step, so for now
nothing outside silod makes one.

```json
{
  "name": "app",
  "id": "ir-3fq8zk",
  "silo": "/team-a/x",
  "state": "READY",
  "ref": "registry.silo.internal/team-a/x/app",
  "ref_by_id": "registry.silo.internal/id_k7f3qa/app",
  "tags": 4,
  "manifests": 6,
  "charged_bytes": 412090368,
  "create_time": "2026-10-13T09:12:00Z",
  "last_push_time": "2026-10-13T10:40:51Z"
}
```

- A `name` is one component of 1 to 63 characters matching
  `[a-z0-9]+((_|__|-+)[a-z0-9]+)*`, not starting with `id_`: the OCI
  name grammar without its `.`, so that no repo name looks like an
  upstream registry.
- `charged_bytes` is the stored size of every distinct blob and
  manifest the repo's manifests reference, each once
  ([`local-registry.md`, Charging](local-registry.md#charging-call-l61)).

| Call | Does |
| --- | --- |
| `GET /silo/v1/silos/{silo}/imageRepos` | List. `?recursive=true` lists the descendants' too, each with its silo's path |
| `GET .../imageRepos/{repo}` | Get one |
| `GET .../imageRepos/{repo}/tags` | `tags`, each with the digest it points at, and `manifests`, every manifest the repo holds, untagged ones included, each with its `charged_bytes`, its `tags` and `referenced` (another manifest of the repo, an index, references it) |
| `DELETE .../imageRepos/{repo}/tags/{tag}` | Untag. The manifest stays, untagged and charged. Returns the repo |
| `DELETE .../imageRepos/{repo}/manifests/{digest}` | Delete a manifest and every tag on it, and an index's children that nothing else in the repo tags or references. Returns the repo |
| `DELETE .../imageRepos/{repo}` | Delete the repo and everything in it. An operation of kind `registry.delete` |

- **Who may call them** ([`local-registry.md` §5](local-registry.md#who-may-do-what-call-l63)):
  the silo itself and its ancestors, any of them; the holder of an
  active `manage` grant on the silo or an ancestor whose
  `facade.resources` covers `registry`, list, get and tags at `read`
  and all of them at `write`; anyone else, `NOT_FOUND`, whether the
  repo exists or not. The caller's own control policy then applies:
  under `read`, list, get and tags only.
- **Quota.** Each image repo holds one of its silo's `image_repos` (a
  global count class), so a first push past it is refused. The root's
  is unlimited unless install names one.
- **Creating** one by a push is a control-plane call: a caller whose
  own control policy isn't `full` can't, though it keeps pushing to
  the repos it has.
- Untags, manifest deletes and repo deletes are audited under the silo
  as `registry.delete`, with `refunded_bytes`. Deleting a silo without
  `force` is `FAILED_PRECONDITION` while it has image repos; with
  `force` it deletes them.

## Members {#SV-25}

`GET`, `PUT`, `DELETE /silo/v1/silos/{silo}/members/{identity}`

- `identity` is `user:alice@example.com` or
  `serviceAccount:ci@other-project.iam.gserviceaccount.com`. It is
  checked against the front door's verified email.
- An identity maps to exactly one silo. Mapping it again moves it.
  Only an ancestor of both the old silo and the new one may move it.
- Called by an ancestor of `{silo}`. The root's members are set by
  `silo install --admin` and by other root members.

## Audit {#SV-26}

`GET /silo/v1/silos/{silo}/audit?since=...&kind=...&subtree=true`

Every record has `time`, `silo` (the id and the path at the time),
`actor` (a silo id, plus the member identity for front-door calls),
`via` (how the actor reached `silo`, when it isn't `silo` itself),
`kind`, `request_id`, `result`, and fields that depend on the kind.
`via` is one of:

- `ancestor:<id>`: a proper ancestor acting on its descendant;
- `manage:<holder id>`: the holder of a [`manage` grant](#manage-grants);
- `harness`: a call an agent's harness made on its session socket,
  which also carries `agent: <agent id>`.

A call from an agent's tool VM has `from_tool_vm: <agent id>`
([`agents.md`, The session socket](agents.md#5-the-session-socket-how-a-harness-reaches-silod)).

Kinds:

- `silo.create`, `silo.delete`, `silo.patch`, `silo.set_control`,
  `silo.grow`, `silo.add_block`, `silo.release_block`;
- `manage.offer`, `manage.accept`, `manage.renew`, `manage.revoke`,
  `manage.expire`;
- `control.denied`: a call refused by the caller's control policy, at
  the gateway or in silod. A dropped packet at the fence leaves no
  record;
- `member.put`, `member.delete`;
- `export.offer`, `export.accept`, `export.withdraw`,
  `export.call` (one per HTTP call or TCP connection), `link.propose`,
  `link.activate`, `link.delete`, `grant.suspend`, `grant.resume`;
- `egress.connect` (allowed and denied);
- `agent.create`, `agent.delete`, `agent.expire`, `agent.stop`,
  `agent.resume`, `agent.renew`, `agent.budget_exhausted`,
  `agent.message`, `agent.request`, `agent.answer`,
  `agent.session_restart`;
- `model.call`: every model call an agent makes, with the model, the
  credential, tokens by kind, the dollars charged, and whether it was
  proxied or reported by the harness ([`agents.md`, Metering](agents.md#metering));
- `credential.add`, `credential.remove`;
- `facade.call`: every facade write, with the method, the object's
  name (the real one, carrying the silo's prefix), the charge (with its `payer`, `home` and
  `via`: [Room for custom allocators](#room-for-custom-allocators)) and
  the result. Reads are not
  audited by default (call 28: they are many, and they reveal
  nothing outside the silo);
- `quota.root_resync`, `quota.over_committed`, `fence_drift`,
  `orphan.found`, `orphan.deleted`.

The silo itself and its ancestors may read its records.
`subtree=true` includes descendants, for ancestors. Records are kept
for 90 days in silod's database ([`state.md`](state.md)). Older ones are dropped, unless install was
given `--audit-sink`, which also streams every record to Cloud Logging
as it is written.
