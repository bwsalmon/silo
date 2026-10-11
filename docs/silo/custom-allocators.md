# Custom allocators: `allocator/v1`

## Status

**Designed now, built after v1.** This is an addendum to the Silo design
([`README.md`](README.md)), written in task 1958. bsalmon decided in
director chat task 1913, on 2026-10-04: "we don't need it in v1 but we
should design the custom allocators." Warm pools aren't needed at the
start.

Nothing on this page is part of v1, except
[What v1 must already allow](#what-v1-must-already-allow). That
section is normative for v1: v1's ledger and fence API must leave room
for this design, so that adding it later is an addition and not a
migration. Implementation is on hold, as for the rest of silo.

Calls made on this page are marked **(call A1)**, **(call A2)** and so
on, with their own numbering. That keeps them apart from the
[calls in the README](README.md#calls-made-in-this-draft), which other
addenda are adding to at the same time. They are listed in
[Calls made on this page](#calls-made-on-this-page), and what is still
undecided is under [Open questions](#open-questions).

## Contents

1. [Why](#why)
2. [The model](#the-model)
3. [The ledger](#the-ledger)
4. [Grants of a custom class](#grants-of-a-custom-class)
5. [The `allocator/v1` contract](#the-allocatorv1-contract)
6. [Shapes: where the created resource lives](#shapes-where-the-created-resource-lives)
7. [Fence API additions](#fence-api-additions)
8. [Failure handling](#failure-handling)
9. [Trust](#trust)
10. [Audit](#audit)
11. [Worked example](#worked-example)
12. [What v1 must already allow](#what-v1-must-already-allow)
13. [Conformance cases](#conformance-cases)
14. [Calls made on this page](#calls-made-on-this-page)
15. [Open questions](#open-questions)

## Why

Shibu let any namespace implement an allocator, and "holding an
allocator, or a grant of one, is the permission"
([grain's `docs/shibu.md`, Component allocators](https://github.com/bwsalmon/grain/blob/main/docs/shibu.md#component-allocators)).
The silo draft dropped this: its only allocators are the ones silod
implements over GCE ([`allocators.md`](allocators.md)). Without it,
every shared service needs a bespoke way to say who may use how much
of it. A CI pool, a Postgres service and a model proxy would each need
their own, and none of them would follow the tree.

This page restores the idea in Silo terms. A service in one silo hands
out something it makes, and silod keeps the books and does the wiring.

## The model

### A custom allocator

A **custom allocator** is a service running in a silo. It serves an
**`allocator/v1`** endpoint inside its own network, and **registers**
that endpoint with silod as an **allocator kind** owned by that silo.

- **Verbs.** The endpoint serves `create`, `delete`, `usage`, `list`
  and `describe` ([The contract](#the-allocatorv1-contract)).
- **Classes and units.** The allocator declares these itself through
  `describe`. For example:

  | Kind | Classes |
  | --- | --- |
  | `ci-pool` | `runner_minutes` |
  | `pg` | `databases`, `storage_gb` |
  | `model` | `usd` |

- **Quota.** The owning silo holds the root of its kind's quota. It sets
  the capacity of each class itself, and can raise or lower it, within
  the rules of [the ledger](#the-ledger).

**Naming (call A1).** A kind's name is a DNS label chosen by its
owner, unique within that silo. Its durable name is `<name>@<owner-id>`
(`ci-pool@q4ci7a`), because ids are stable and paths aren't
([README, Silo](README.md#silo)). `describe` shows the owner's path
beside it for people. A class is named `<kind>/<class>`
(`ci-pool@q4ci7a/runner_minutes`) wherever it appears beside the
built-in classes.

Two silos can each run a `pg` kind. A consumer names the one it means
by owner, so neither can pass for the other.

### silod keeps the ledger

silod keeps the books for a custom class exactly as it does for the
built-in classes. Per silo, per class, and per region for a regional
class, it keeps `granted`, `delegated` and `used`, with the invariant
`delegated + used ≤ granted`
([`allocators.md`, Hard partition](allocators.md#hard-partition)).
Quota is handed down the tree and can be sub-granted. Created
resources carry leases.

- **Charge before forwarding.** silod checks headroom and records the
  charge **before** it forwards a `create` to the allocator, in the
  same transaction as the check.
- **Refund on delete or failure.** silod refunds the charge on
  `delete`, or if the `create` fails.

So an allocator never has to trust its callers about quota. A create
that doesn't fit is refused before the allocator sees it. And a
consumer never has to trust the allocator's accounting of its grant:
the grant's headroom is silod's number, not the allocator's.

### Access is a grant

Using a custom allocator needs a **grant** of one of its classes, with
headroom. As with exports, both sides agree: the owner offers a grant,
and the consumer accepts it ([Grants](#grants-of-a-custom-class)).

**Holding a grant with headroom is the permission.** There is no
separate ACL. A silo with no grant of `pg@h2pg5m/databases`, or none
left, can't create a database there. A silo with headroom can, and the
allocator can't refuse it for being the wrong caller. It can still
refuse a spec it can't serve.

### The built-in classes are the root allocator

The GCE classes (`vcpu`, `memory_mb`, the disk classes, and the GPU
classes that task 1956 adds) are one allocator kind, **`gce`**. It is
owned by the root silo and implemented by silod itself
(**call A2**).

- Its capacity comes from the host project's quotas
  ([`allocators.md`, Root capacity](allocators.md#root-capacity)).
  A custom kind's capacity is whatever its owner says.
- Its `create` is the facade: a much richer API than `allocator/v1`,
  but charged through the same ledger.
- Its `describe` is the class table in `allocators.md`.
- A future backend's root kind (`docker`, `k8s`) would sit beside it.

Nothing about the built-in classes changes in v1. The point is that
the ledger, the grant rules and `describe` treat `gce` as data, as one
kind among several, so that a custom kind slots in beside it
([What v1 must already allow](#what-v1-must-already-allow)).

**The model budget (task 1945).** Task 1945 made the model budget a
fourth allocator: dollars per budget period, kept as micro-dollars, a
hard partition handed down like quota
([`allocators.md`, Model budget](allocators.md#model-budget)). In this
page's terms it is a **metered class, `usd`, with a period, of a kind
`model` served by the model proxy**. It needs no special case
(**call A3**):

| [`agents.md`](agents.md#7-model-budget-and-metering) | This page |
| --- | --- |
| An agent | A handle of kind `model` |
| `budget.usd`, reserved when the agent is created | The handle's reserve |
| `spent_usd`, metered by the proxy | The handle's consumption |
| `--budget-period` | The class's `period` |
| The `model_budget` field in `describe` | A view of the `model/usd` quota rows |

- **In v1,** nothing in task 1945's design changes, except where the
  numbers are kept. The proxy stays inside silod. The budget is stored
  in the same ledger as quota, as kind `model`, class `usd`, owned by
  the root ([What v1 must already allow](#what-v1-must-already-allow),
  item 5). It doesn't get a table of its own.
- **After v1,** the proxy can serve `allocator/v1` from a silo, and
  the `model` kind can move to it. Existing grants and charges keep
  their rows.

There is one real difference. In task 1945, deleting an agent returns
its whole reservation, including what it has spent. Under this page's
rule for metered classes, what was spent stays charged until the
period ends ([call A5](#calls-made-on-this-page)). Otherwise a silo
could spend its budget, delete the agent, and spend it again in the
same period. Which rule the model budget should follow is
[Open question 6](#open-questions). This page doesn't change
`agents.md`.

## The ledger

### Held and metered classes

A class is one of two sorts, declared in `describe`:

- **Held**: charged while the resource exists, and refunded when it is
  gone. Examples: `databases`, `storage_gb`, a GPU job slot. This is
  how every built-in class works.
- **Metered**: consumed over time and not given back. Examples:
  `runner_minutes`, `usd`.

A metered class can't be charged its final amount at `create`, because
nobody knows it yet. So it works as a **reserve (call A4)**:

1. `create` names a `reserve` for each metered class, for example
   `runner_minutes: 60`. silod charges the reserve up front, like a held
   amount.
2. The allocator reports cumulative consumption per handle through
   `usage`. Consumption runs up to the reserve and never past it. When
   it reaches the reserve, the allocator must stop the resource. The
   consumer may `extend` the reserve first, which is checked and
   charged like a `create`.
3. On `delete`, the handle **settles**. Its last reported consumption
   becomes a permanent charge, and the rest of the reserve is refunded.

So for a metered class, `used` is the reserves of live handles plus
everything already consumed. Consumption stays spent:

- lowering a grant still needs `g ≥ delegated + used`, consumption
  included;
- when a silo is deleted, its quota goes back to its parent as usual,
  **but its consumption stays charged to the parent (call A5).**
  Otherwise deleting and re-creating a child would refill its budget.

**Periods.** A metered class may declare a `period` (`day`, `week` or
`month`, in UTC), as the model budget's `--budget-period` does. At the
start of each period, silod sets every silo's `consumed` for the class
back to 0, and every live handle's consumption with it. Grants and
reserves carry over. A class with no period is a lifetime budget,
which the owner renews by raising grants.

Amounts are non-negative decimals with up to six places after the
point, stored as integer millionths, so `usd` can hold `0.000250`
(**call A6**).

### Scope

A class is `global` (one counter per silo) or `regional` (one per silo
per region, within the silo's allowed regions), as the allocator
declares. Most custom classes are global. A GPU job slot pool that
runs in two regions would declare a regional class.

### Payer and home

Every charge records two silos:

- **payer**: whose quota it is charged to;
- **home**: where the resource lives.

For every built-in charge in v1 they are the same silo. They differ in
the two [shapes](#shapes-where-the-created-resource-lives):

| | Payer of the custom class | Home | Payer of the cloud resources behind it |
| --- | --- | --- | --- |
| Built-in (`gce`) | the caller | the caller | the caller |
| Serve | the consumer | the allocator's silo | the allocator's silo |
| Lend | the consumer | the consumer | the allocator's silo |

Nothing is counted twice. Take a CI runner that the `ci-pool` kind
serves. It costs the consumer `runner_minutes`, in the pool's own
unit. The VM behind it costs the pool's silo `vcpu`, in GCE's unit. The
pool decides how many minutes to ask for a vCPU.

### Leases

Every handle (a resource created through an allocator) has an optional
lease: `lease_seconds` at `create`, renewed with `:renew`. When a lease
runs out, silod deletes the handle, as if the consumer had called
`delete`. An allocator may require a lease and cap its length in
`describe` (`max_lease_seconds`). A CI runner, for example, should
never outlive a forgotten job.

Grants of a custom class may also expire. See below.

## Grants of a custom class

### From the owner, across the tree

The owner of a kind offers a grant to any silo, the way an export is
offered ([`fence-api.md`, Exports](fence-api.md#exports-proxied)):

- The grant names the classes and amounts:
  `{"to": "/team-a", "amounts": {"runner_minutes": 6000}}`.
- It is `PENDING` until the receiver accepts it. On acceptance, the
  amounts are added to the receiver's `granted` for those classes, and
  to the owner's `delegated`.
- A grant may carry `expires`. At that time it stops allowing new
  creates. Outstanding handles live on until they are deleted or their
  own lease runs out, and then the amount goes back to the owner.
- The owner may raise a grant at any time, without asking. Lowering it
  must fit, as for any allocation: `g ≥ receiver.delegated +
  receiver.used`.
- The owner may `revoke` with `kill: true`. That deletes every handle
  the receiver's subtree holds of that kind, then takes the grant away.
  Without `kill`, revoking works like lowering to zero, and fails while
  anything is in use.

### Down the tree

A receiver hands its custom classes down to its own descendants with
the ordinary `PATCH` on `quota` that it uses for `vcpu`
([`fence-api.md`, Changing allocations](fence-api.md#changing-allocations)).
No consent is needed inside the tree, any more than it is for `vcpu`.

So `granted` has more than one source. In v1 the only source is the
parent. Here it is the parent plus any grants accepted from owners.
The ledger keeps each of these as an **allocation edge**
(`from`, `to`, class, region, amount), and `granted` is their sum.
v1 has to keep edges this way too
([What v1 must already allow](#what-v1-must-already-allow)).

**No re-granting across the tree (call A7).** Only the owner offers
grants outside the tree. A holder can hand its share down to its own
descendants, but can't offer it to a sibling or a stranger. That keeps
"who may use `pg`" answerable from the owner's list of grants and the
tree under each. Re-granting is [Open question 3](#open-questions).

### Ancestors' policy

The **grant policy** gains one key, `allocators`
([`fence-api.md`, Grants](fence-api.md#grants-exports-and-links)).
Like the others, the effective value is the AND over the silo and its
ancestors:

- **On the consumer's side,** `allocators: deny` stops a subtree
  accepting grants of any custom kind. This restores the control that
  Shibu's F33 asked for: an ancestor can keep a subtree off allocators
  it hasn't vetted.
- **On the owner's side,** it stops the subtree registering a kind at
  all.
- If either side's policy stops allowing it, the grant is `SUSPENDED`,
  as an export is: new creates are refused, and existing handles keep
  running.

In silos that predate this feature, the `allocators` key is absent, and
absent means `deny`. Install sets the root's value with a flag. So
turning the feature on is a decision someone makes, not a side effect
of an upgrade (**call A8**). Allowing only named kinds, rather than
all or none, is [Open question 4](#open-questions).

## The `allocator/v1` contract

`allocator/v1` is served by the allocator, and called only by silod. It
is JSON over HTTP, with the same conventions as the fence API.

### Reaching the endpoint

At registration, the owner names a target in its own space, as for an
export: an `ip:port` or an instance and port. silod opens one fence rule
from the gateway range to that target (`alloc-<kind-id>-in`), and calls
it from there.

- The endpoint isn't exported to anyone, so no other silo can reach it.
- VMs in the owner's own silo can reach it, but they have the owner's
  authority anyway ([README, Identity](README.md#identity)).
- As for every caller identity in Silo, nothing in the request proves
  the call came from silod. The proof is where it comes from.

### Verbs

Every verb names a resource by its **handle**: an id silod makes before
it calls `create`. This makes every verb idempotent: a repeated
`create` with the same handle returns the first result.

**`describe`**: `GET /allocator/v1/describe`

```json
{"classes": [
   {"name": "databases", "unit": "count", "sort": "held", "scope": "global"},
   {"name": "storage_gb", "unit": "GB", "sort": "held", "scope": "global"}],
 "shapes": ["serve"],
 "spec_schema": {"type": "object", "properties": {"version": {"enum": ["16", "17"]}}},
 "max_lease_seconds": null,
 "create_timeout_seconds": 120}
```

A metered class has `"sort": "metered"`, and a `period` if it renews
([Periods](#held-and-metered-classes)).

silod calls `describe` at registration and on `:refresh`. A class may
be added at any time. A class that has outstanding charges or grants
can't be removed until they are gone. `spec_schema` is optional. If it
is given, silod checks `create` specs against it before charging, so
that a malformed request costs nothing.

**`create`**: `POST /allocator/v1/resources`

```json
{"handle": "h-7d2kq9x4",
 "consumer": {"id": "k7f3qa", "path": "/team-a"},
 "amounts": {"databases": 1, "storage_gb": 20},
 "spec": {"version": "17"},
 "lease_expires": null}
```

The response is a resource, served by the allocator:

```json
{"handle": "h-7d2kq9x4", "state": "READY",
 "endpoints": [{"name": "pg", "target": "10.4.0.12:5432", "protocol": "tcp"}],
 "credentials": {"user": "app", "password": "…"},
 "attributes": {"database": "team_a_17"}}
```

- `state` is `READY`, or `CREATING` for a slow create. While it is
  `CREATING`, silod polls `list` for the handle, up to
  `create_timeout_seconds` (at most 600).
- Each `endpoint` must be in the allocator's own space. silod checks
  this as it does an export target.
- `credentials` are passed to the consumer once, in the response to
  its `create`. silod doesn't store or audit them (**call A9**).
  Credentials kept behind a proxy, so they never enter the consumer, is
  [later](README.md#9-later).
- `attributes` are stored on the handle and shown in `describe`, so
  they must not be secret.

**`delete`**: `DELETE /allocator/v1/resources/{handle}`. Idempotent:
a `404` counts as deleted.

**`usage`**: `GET /allocator/v1/usage?handle=…` (repeatable), or with
no handle for all of them.

```json
{"usage": [{"handle": "h-ci-31", "consumed": {"runner_minutes": 37}, "as_of": "2026-10-04T18:02:00Z"}]}
```

`consumed` is cumulative for the life of the handle. silod asks every
minute for live metered handles, and once more just before it settles
a handle. For held classes, `usage` may report what the resource
really uses (a database's actual GB, say). That figure is shown as
**reported** and never changes the ledger.

**`list`**: `GET /allocator/v1/resources?handle=…&page_token=…`
returns the handles the allocator holds, with each one's `state`. silod
uses it to poll a slow `create`, and to
[reconcile](#reconcile).

### Reconciling usage reports

silod reads a usage report like this:

- **Held classes:** the ledger charges what `create` declared, and
  nothing else. A report is shown beside it, marked as reported, and
  that is all.
- **Metered classes:** a report moves the handle's consumption, within
  limits:
  - consumption only goes up. A report lower than the last one is
    ignored and audited (`allocator.report_rejected`);
  - it is clamped to the reserve. A report above the reserve is
    recorded as the reserve, and audited as an overrun;
  - a report for a handle the allocator doesn't hold, or that belongs
    to another kind, is ignored.
- **Settling:** on `delete`, silod asks for usage once more and
  settles at what comes back. If the allocator doesn't answer within
  the [timeout](#the-allocator-is-down-or-slow), it settles at the last
  report it accepted.

So the allocator is trusted to say how much of a reserve was used, and
for nothing else ([Trust](#trust)).

### Reconcile

Every ten minutes, and when the allocator comes back after being
unreachable, the leader compares silod's handles of each kind with the
allocator's `list`:

- **The allocator holds a handle that silod has deleted, or never
  made:** silod calls `delete` on it. That is the allocator's own
  leftover, paid from its own GCE quota.
- **silod holds a live handle that the allocator doesn't list:** the
  handle becomes `LOST`. silod tears down its export, refunds the held
  amounts, settles the metered ones at the last report, and audits
  `allocator.handle_lost`.

silod is the source of truth for **what exists and who pays**. The
allocator is the source of truth for **what it is running**. Where the
two disagree, silod's books win for charges, and silod tells the
allocator to clean up.

## Shapes: where the created resource lives

| Shape | Where the resource lives | Status |
| --- | --- | --- |
| [Serve](#serve) | The allocator's silo | Primary. Specified here in full. |
| [Lend](#lend) | The consumer's silo | Designed, not detailed. |
| [Warm pools and adopt](#warm-pools-and-adopt) | Moved from the allocator's silo into the consumer's | Later. Noted only. |

An allocator lists the shapes it serves in `describe`. The consumer
asks for one at `create`, and the default is `serve`.

### Serve

The resource lives in the **allocator's** silo: a database, a CI
runner, a slot on a GPU job queue. The consumer reaches it through a
proxied export that silod sets up for it.

**`create`, step by step:**

1. **The consumer asks**, or an ancestor or `manage` holder asks for
   it. `POST /silo/v1/self/resources`
   `{"kind": "pg@h2pg5m", "amounts": {...}, "spec": {...}, "lease_seconds": 86400}`.
2. **silod checks**, in one transaction:
   - the consumer's effective grant policy allows `allocators` and
     `exports`, and so does the owner's;
   - the kind is `ACTIVE`, the spec matches `spec_schema`, and every
     class named is the kind's;
   - every amount fits: `used + n ≤ granted − delegated` in the
     consumer's silo, for each class.
3. **silod charges and records the handle**, in the same transaction:
   state `CREATING`, payer the consumer, home the allocator's silo.
4. **silod forwards `create`** to the allocator, after the commit
   ([`state.md`](state.md#what-silod-relies-on-the-store-for), item 1).
5. **silod wires the export.** For each endpoint in the response, it
   creates an export from the allocator's silo, offered to the consumer
   only and already accepted by it. It is named `r-<handle>-<endpoint>`
   and owned by the handle, not by either tenant. The consumer's
   `create` is its consent, and the allocator's registration is the
   allocator's.
6. **silod answers** with the handle, its endpoints, at the proxied
   addresses ([`gateway.md`, Served resources](gateway.md#served-resources-after-v1)),
   the credentials, once, and the attributes.

**The fence is untouched.** No rule opens between the two silos. The
only new path is gateway → endpoint, exactly as for any export. The
consumer never has a direct route to the allocator's network.

**Tearing down.** All of these tear down both the export and the
resource:

- `DELETE /silo/v1/self/resources/{handle}` by the payer, or by one of
  its ancestors;
- the handle's lease running out;
- deleting the consumer's silo, or the ancestor whose grant it was
  drawing on. silod does it as part of the delete, before the quota is
  returned;
- a revoke with `kill: true`.

In each case, silod first withdraws the export, which closes open
connections within a second
([`gateway.md`, Proxied exports](gateway.md#proxied-exports)). Then it
settles the charge and calls `delete` on the allocator. The consumer
loses access at once, whether or not the allocator answers.

A served resource's export can't be withdrawn on its own by either
side, because it belongs to the handle. If the consumer withdraws the
import, that counts as a `delete` of the handle.

**Who can reach it.** Only the payer silo. Exports are offered to one
silo, not to a subtree. A child that needs its own database creates its
own handle, from quota its parent handed down.

### Lend

The resource lives in the **consumer's** silo (its home), but the cloud
resources behind it are charged to the **allocator's** quota (the
payer). An example is a "GPU box" kind that places a VM directly on
the consumer's subnet, paid for from the pool's `gpu` quota. This is
designed here, not detailed.

- **A placement right comes with the request.** When a consumer asks
  for a lent resource, silod gives the allocator's silo a **placement
  right** in the consumer's silo, for that handle alone. It is a
  narrower kind of `manage` grant
  ([`fence-api.md`, Manage grants](fence-api.md#manage-grants)), made
  by silod for the handle rather than offered by an ancestor. It allows
  facade writes in the consumer's silo that:
  - create objects of the shapes the consumer's `create` allowed (for
    example one instance, of these machine types, on this subnet,
    from these images);
  - are labelled with the handle, and are charged to the allocator's
    silo, the payer;
  - change or delete only objects that carry that label.

  It doesn't allow reads or writes of anything else in the consumer's
  silo, and no fence calls. It ends when the handle does, and silod then
  deletes whatever still carries the label. A v1 `manage` grant can
  only be scoped to resource types, not to labels. A placement right
  needs the per-object scope that the sealed-silo addendum left open.
- **The facade takes the right as a header.** The allocator calls the
  consumer's facade with `Silo-Placement: <handle>`. Its identity still
  comes from its origin. The header only says which of its rights it
  is using, and silod checks that the allocator's silo holds it. As with
  a `manage` holder, the allocator's own control policy still binds
  it.
- **A placement right never exceeds its requester.** Whoever made the
  `create` must itself be able to make those objects in the home: the
  home silo under a `full` control policy, a proper ancestor, or a
  `manage` holder whose scope covers those resource types. A request
  can't use an allocator to do in a silo what the requester couldn't
  do there itself.
- **A lent VM acts as the consumer's silo.** Inside a silo there is no
  per-VM identity ([README §8](README.md#8-limitations)). So the
  allocator's code, running in a VM placed in the consumer's silo, has
  whatever authority the consumer's control policy gives its VMs. It
  can reach everything on the consumer's network, and the consumer's
  exports, links and egress. Lending is only safe where the consumer
  trusts the allocator's image that far. The usual pattern is for the
  consumer to make a child silo just to receive lent resources.

**Sealed consumers (task 1947).** A sealed silo (`control: none`) makes
no control-plane calls of its own, and is changed only from outside, by
an ancestor or a `manage` holder
([`fence-api.md`, Control policy](fence-api.md#control-policy)). So:

- **A sealed silo can't ask for anything itself,** in either shape. It
  can't accept an allocator grant or call `create`. As with exports,
  an ancestor, or a `manage` holder whose scope includes `grants`,
  makes those calls on its behalf at `/silo/v1/silos/{silo}/...`. The
  sealed silo is still the payer, and its grant policy still has to
  allow `allocators` ([`fence-api.md`, Grant calls on a descendant's behalf](fence-api.md#grant-calls-on-a-descendants-behalf)).
- **Serve needs nothing more.** Nothing enters the sealed silo. It
  reaches the resource through an export, and exports keep working
  under a seal.
- **Lend into a sealed silo is refused unless the requester is a proper
  ancestor of it (call A10).** A seal says that change comes from
  outside, and only on the authority of those above the cell. Lending
  places an object chosen by a third party, the allocator, inside the
  cell. So only an ancestor can let that happen, not a `manage`
  holder. The seal still binds the lent VM. Its control-plane calls
  are refused like any VM's in the cell, so it can't change the cell's
  infrastructure. It does get the cell's network.
- **An existing handle outlives a seal.** Sealing a silo later doesn't
  end the handles it pays for, just as it leaves `ACTIVE` exports in
  place. Its ancestors can still delete them.

Left to detail when lend is built: the shape language for placement
rights, what happens to a lent VM's disks at the end, and how the
consumer limits which images may be lent in.

### Warm pools and adopt

These are later, and noted here only. A warm pool hands out a
pre-booted VM instead of booting one. With the subnet backing, moving
a VM into another silo means moving its NIC to the consumer's subnet,
and GCE allows that only while the VM is stopped. That loses most of
what was warm.

The alternative is to pre-warm **inside** the consumer's silo: a lend
allocator keeps a few stopped or suspended VMs on the consumer's
subnet, paid for by the pool, and starts one on `create`. That needs
nothing new beyond lend, but each consumer has its own pool, so it
only pays off for consumers that use a lot.

## Fence API additions

All of these are after v1. The paths follow
[`fence-api.md`](fence-api.md#transport-and-conventions).

| Call | Who | What it does |
| --- | --- | --- |
| `POST /silo/v1/self/allocators` `{"name", "endpoint": {"target" or "instance", "port"}, "capacity": {...}}` | the owner | Registers a kind. silod calls `describe`, records its classes, sets the owner's `granted` to `capacity`, and opens `alloc-<kind-id>-in`. |
| `PATCH /silo/v1/self/allocators/{name}` `{"capacity": {...}}` | the owner | Raises or lowers the owner's root capacity. Lowering must fit `delegated + used`. |
| `POST /silo/v1/self/allocators/{name}:refresh` | the owner | Re-reads `describe`. |
| `DELETE /silo/v1/self/allocators/{name}?force=true` | the owner, or an ancestor | Unregisters the kind. Without `force`, fails with `FAILED_PRECONDITION` while any handle or grant is outstanding. With it, works as [deleting the owner](#the-allocators-silo-is-deleted) does. |
| `POST /silo/v1/self/allocators/{name}/grants` `{"to", "amounts", "expires"}` | the owner | Offers a grant. |
| `PATCH`, `DELETE …/grants/{id}?kill=true` | the owner | Raises, lowers or revokes it. |
| `GET /silo/v1/self/allocator-offers`, `POST …/allocator-offers/{id}:accept`, `DELETE /silo/v1/self/allocator-grants/{id}` | the receiver | Lists offers, accepts one, gives a grant back. Giving back must fit, as lowering does. |
| `PATCH /silo/v1/silos/{silo}` with `quota: {"global": {"pg@h2pg5m/databases": 2}}` | an ancestor | Hands a custom class down the tree, exactly as for `vcpu`. |
| `POST /silo/v1/self/resources`; `GET`, `DELETE …/resources/{handle}`; `…:renew`; `…:extend` | the payer (and its ancestors, for `GET` and `DELETE`) | Creates, reads, deletes, renews and tops up handles. |
| The same receiver and resource calls at `/silo/v1/silos/{silo}/...` | a proper ancestor of `{silo}`, or a `manage` holder whose scope includes `grants` | Acts as if `{silo}` had made the call, as for [exports](fence-api.md#grant-calls-on-a-descendants-behalf). It is how a sealed silo gets a grant or a resource. |
| `GET /silo/v1/allocators` | any silo | The kinds the caller owns or holds a grant of, each with its owner's path, classes, units, sorts and scope. |

**`describe` lists custom classes.** The silo object's `quota` map
holds custom classes beside the built-in ones, under `global` or a
region, keyed `<kind>/<class>`. A new field, `allocators`, lists the
kinds the silo owns, with their state (`ACTIVE`, `UNREACHABLE`,
`DRAINING`), and the grants it holds, with their source:

```json
"quota": {
  "us-central1": {"vcpu": {"granted": 16, "delegated": 4, "used": 8}},
  "global": {
    "ci-pool@q4ci7a/runner_minutes": {"granted": 6000, "delegated": 2000, "used": 1240, "consumed": 1180}
  }
},
"allocators": {
  "owned": [],
  "held": [{"kind": "ci-pool@q4ci7a", "owner_path": "/platform/ci-pool", "grant": "g-41", "classes": ["runner_minutes"]}]
}
```

`consumed` appears only for metered classes. It is the part of `used`
that is already spent and won't come back.

**`quota:preview`** takes `{"resource_request": {...}}` as well, and
returns its charges in the same list form as for a facade request.

**Errors.** No new codes. An allocator that times out gives
`UNAVAILABLE`, with `details.allocator` naming the kind. A spec that
doesn't match `spec_schema` gives `INVALID_ARGUMENT`. The allocator's
own refusal (it can't serve the spec) gives `FAILED_PRECONDITION`, with
the allocator's message in `details.allocator_message`.

## Failure handling

### The allocator is down or slow

- **Timeouts.** `describe`, `delete`, `usage` and `list` get 10
  seconds. `create` gets 30 seconds to answer, and
  `create_timeout_seconds` (at most 600) to reach `READY`.
- **A `create` that fails or times out is refunded.** silod calls
  `delete` on the handle, because the allocator may have made
  something before it failed. Then it refunds the whole charge, whether
  or not that `delete` succeeds, and returns `UNAVAILABLE` or the
  allocator's refusal. The consumer never pays for an allocator's
  failure. Anything the allocator left behind is found by
  [reconcile](#reconcile).
- **A `delete` that fails** still takes effect for the consumer at
  once: the export is withdrawn, held amounts are refunded, and metered
  amounts settle at the last report. The handle stays `DELETING` on
  the allocator's side, and silod retries with backoff, and in every
  reconcile, until the allocator confirms.
- **An unreachable allocator** is one that fails three calls in a row.
  Its kind is marked `UNREACHABLE` in `describe`, and new creates fail
  fast with `UNAVAILABLE` without being charged. Existing exports keep
  working if the resource behind them still answers: the gateway
  doesn't need the allocator to carry traffic. Usage reports stop, so
  metered handles can't consume past their reserve, and that is the
  limit.
- **Concurrency.** silod makes at most 16 calls at once to one kind,
  and queues the rest, so one slow allocator can't tie up silod's
  workers (**call A11**).

### The allocator's silo is deleted

Deleting the owner's silo, or unregistering with `force`, **drains**
the kind:

1. The kind becomes `DRAINING`. New creates and new grants are refused.
2. Every handle of the kind is torn down as for a `delete`: its export
   is withdrawn, its charge settled, and `delete` called on the
   allocator, with the usual timeout. Consumers see their handles end
   in state `TERMINATED`, with the reason `allocator_deleted`.
3. Every grant of the kind, and every allocation of its classes down
   every tree, is removed. The classes disappear from every silo's
   `quota`.
4. Then the silo delete carries on as usual
   ([`fence-api.md`, delete](fence-api.md#delete)). Deleting the silo
   deletes the VMs and disks that ran the resources, whether or not
   the allocator answered step 2.

Without `force`, deleting a silo that owns a kind with outstanding
handles fails with `FAILED_PRECONDITION`, as deleting a non-empty silo
does. An ancestor of the owner can delete it with `force`, even though
consumers outside the ancestor's subtree lose their resources. The
owner's ancestors are the ones who gave it the means to run the
service. The audit record names every consumer affected.

### A malicious allocator

An allocator can only affect its own resources, and its own kind's
usage reports.

- **It can't spend anyone's quota.** Every charge is decided by silod
  from the consumer's request, before the allocator sees it.
- **It can't overspend a reserve.** Reports only move a handle's
  consumption up, and only as far as its reserve. The most a dishonest
  allocator can take from a consumer is the reserves that consumer
  chose to set.
- **It can't point a consumer at someone else.** Endpoints must be in
  the allocator's own space, so it can't use the consumer's trust in
  silod's wiring to send it to a third silo's service.
- **It can't reach the consumer.** In the serve shape nothing is
  opened toward the consumer: the export runs from the consumer to the
  allocator only. In the lend shape its placement right covers only
  the handle's own objects.
- **It can't mint quota of other kinds,** including `gce`. Whatever it
  runs is charged to its own silo's grants.
- **It can't stall silod.** Timeouts, the concurrency cap, and a 1 MiB
  limit on each response.
- **What it can do:** serve a consumer badly, refuse creates, keep
  running resources after `delete` (at its own cost), and see each
  consumer's id and path in `create`. Choosing to accept a grant from
  it is choosing to trust it with those.

## Trust

| Who | Is trusted for | Is not trusted for |
| --- | --- | --- |
| silod | The ledger: grants, headroom, charges, refunds, and what exists. The export wiring. | |
| The allocator | Running what it was asked to, and saying how much of a reserve was used. | Quota, held charges, who may call, where endpoints point, anything outside its own silo. |
| The consumer | Its own requests: which kind, how much, what reserve. | The allocator's books. It doesn't have to trust them. |
| The owner's ancestors | Whether the kind exists at all: they can delete the owner. | |
| The consumer's ancestors | Whether the subtree may use custom allocators (`allocators` in grant policy), and how much of a class each child gets. | |

## Audit

Every charge, refund and settlement is recorded with `payer`, `home`
and `via`. For a custom allocator, `via` is `allocator:<kind>`. For
example:

```json
{"kind": "allocator.create", "silo": "k7f3qa", "actor": "k7f3qa",
 "payer": "k7f3qa", "home": "h2pg5m", "via": "allocator:pg@h2pg5m",
 "handle": "h-7d2kq9x4", "charge": {"pg@h2pg5m/databases": 1, "pg@h2pg5m/storage_gb": 20},
 "result": "OK"}
```

The new record kinds are:

- `allocator.register`, `allocator.refresh`, `allocator.unregister`,
  `allocator.unreachable`, `allocator.reachable`;
- `allocator.grant_offer`, `allocator.grant_accept`,
  `allocator.grant_change`, `allocator.grant_revoke`;
- `allocator.create`, `allocator.delete`, `allocator.renew`,
  `allocator.extend`, `allocator.lease_expired`, `allocator.settle`;
- `allocator.report_rejected`, `allocator.handle_lost`,
  `allocator.orphan_deleted`.

The records go to both the payer's audit and the home's, so each side
sees the handles that touch it. The served export's
`export.call` records carry the handle as well.

## Worked example

The tree:

```
/                       root, owns kind gce
├── /platform           k1pl8t
│   ├── /platform/ci-pool   q4ci7a  owns kind ci-pool  (runner_minutes, metered)
│   └── /platform/pg        h2pg5m  owns kind pg       (databases, storage_gb, held)
├── /team-a             k7f3qa
│   └── /team-a/ci          p2m8xd
└── /team-b             m3tb6c
```

The root's member allows `allocators` in the root's grant policy, and
`/platform` gets the `vcpu`, disk and address space it needs to run
both services.

**The CI pool.** `/platform/ci-pool` runs a small scheduler VM serving
`allocator/v1` on `10.4.8.2:7000`.

```sh
# in /platform/ci-pool
silo allocator register ci-pool --target 10.4.8.2:7000 --capacity runner_minutes=100000
silo allocator grant ci-pool --to /team-a runner_minutes=6000
silo allocator grant ci-pool --to /team-b runner_minutes=3000

# in /team-a
silo allocator accept ci-pool@q4ci7a
silo quota set /team-a/ci ci-pool@q4ci7a/runner_minutes=2000
```

A job in `/team-a/ci` asks for a runner:

```sh
silo resource create ci-pool@q4ci7a --reserve runner_minutes=60 --lease 2h \
  --spec '{"labels": ["linux", "x64"]}'
```

1. silod sees that `/team-a/ci` has 2000 minutes granted and 0 used.
   60 fits. It charges 60 and records handle `h-ci-31`, with payer
   `p2m8xd`, home `q4ci7a`, and `via=allocator:ci-pool@q4ci7a`.
2. It forwards `create`. The pool boots a runner VM in its own silo.
   That VM's `vcpu` is charged to `/platform/ci-pool`. The pool answers
   `READY`, with the endpoint `10.4.8.40:8080` (the runner's job API,
   HTTP).
3. silod exports it to `/team-a/ci` only, and answers with
   `http://silo.internal/r/h-ci-31/runner/`.
4. The job runs for 37 minutes. The pool's `usage` reports 37, and the
   pool stops the runner once the job is done. The job calls
   `silo resource delete h-ci-31`.
5. silod withdraws the export, asks for usage (37), settles 37 as
   consumed, and refunds 23. `/team-a/ci` now shows `used: 37,
   consumed: 37`. The pool deletes its VM, and its `vcpu` comes back.

If the job had asked for 3000 minutes, step 1 would have refused it
with `QUOTA_EXCEEDED` (2000 granted), and the pool would never have
heard of it.

**Postgres.** `/platform/pg` registers kind `pg` with
`databases=50, storage_gb=2000`, and grants `/team-b`
`databases=2, storage_gb=100`. `/team-b` creates one:

```sh
silo resource create pg@h2pg5m databases=1 storage_gb=20 --spec '{"version": "17"}'
```

silod charges the two held classes, and the service creates a database
on its own Postgres VM. silod exports `10.4.16.5:5432` (TCP) to
`/team-b` only, and returns `silo.internal:20931` and the credentials,
once. `/team-a`, which holds no `pg` grant, can't create one. Even if
it learned the port, the gateway refuses its connections, because it
holds no import of that export. When `/team-b` deletes the handle,
or `/team-b` is deleted, the port closes within a second, and the
service drops the database.

## What v1 must already allow {#SV-123}

This section is **normative for v1**. Each item is cheap now and
expensive to retrofit, because it fixes the shape of a table or of the
API.

**The ledger** ([`allocators.md`, Room for custom allocators](allocators.md#room-for-custom-allocators)):

1. **Kind and class are data.** A quota row is keyed by
   `(silo, kind, class, region)`, all strings, with `region` set to
   `global` for a global class. Classes are rows in a class table
   (kind, name, unit, sort, scope), which v1 seeds with kind `gce` and
   its classes. There is no enum of classes, and no column per class.
   The facade's charge code looks a class up rather than switching on
   it.
2. **Charges carry a payer separate from the home.** Every charge row
   records `payer`, `home`, `via` and the object or handle it is for.
   v1 always writes `payer = home` and `via = facade`. Refunds and the
   reconcile's recount of `used` group charges **by payer**, never by
   where the objects live.
3. **`granted` is a sum of allocation edges.** Each allocation is an
   edge (`from`, `to`, kind, class, region, amount). In v1 `from` is
   always the parent. `granted` is computed from, or kept in step with,
   the edges, so that a second source (an owner's grant) is a new edge
   and not a schema change.
4. **Room for metered classes.** The class table has `sort` and
   `period` columns, and a quota row can hold a `consumed` amount
   (always 0 in v1). Amounts are stored as
   integer millionths of the class's unit, or in a type that can hold
   them.
5. **The model budget is a class in the same ledger**: kind `model`,
   class `usd`, metered, with the deployment's budget period
   ([`allocators.md`, Model budget](allocators.md#model-budget)). It
   doesn't get a table of its own, and the fence API's `model_budget`
   field is a view of those rows ([call A3](#calls-made-on-this-page)).

**The fence API** ([`fence-api.md`, Room for custom allocators](fence-api.md#room-for-custom-allocators)):

6. **Class names are open strings.** `quota` maps, in `describe`,
   `create` and `PATCH`, are keyed by class name. A client ignores a
   class it doesn't know. The server refuses an unknown class with
   `INVALID_ARGUMENT` (looked up in the class table), not with a parse
   error. The names `<kind>/<class>` and anything containing `@` or `/`
   are reserved.
7. **`grant_policy` is an open map** of grant kind to `allow` or
   `deny`. v1 knows `exports` and `links`. A key v1 doesn't know is
   kept, inherited and shown, not refused, and an absent key means
   `deny`.
8. **Charges in API responses say who pays.** `quota:preview` returns
   a list of `{payer, kind, class, region, amount}`, rather than a map
   that assumes the caller pays.
9. **Audit records for charges** carry `payer`, `home` and `via` from
   v1 onward ([`fence-api.md`, Audit](fence-api.md#audit)).
10. **Exports can be owned by something other than a tenant.** The
   export record has an owner field (`tenant` in v1), so that an export
   owned by a handle can't be withdrawn by its tenant later.
11. **`/silo/v1/self/allocators`, `/allocator-offers`,
    `/allocator-grants` and `/resources`, and their
    `/silo/v1/silos/{silo}/...` forms, are reserved paths.** v1 returns
    `UNIMPLEMENTED` for them.

## Conformance cases

These are for when it is built. They join the
[conformance suite](testing.md#conformance-suite), with a test
allocator: a small `allocator/v1` server in its own silo that can be
told to fail, stall, or lie in its reports.

**Quota**

- A `create` over quota is refused with `QUOTA_EXCEEDED`, and the test
  allocator receives no call.
- Two concurrent creates that each fit, but not together: exactly one
  succeeds.
- A `create` from a silo with no grant of the kind is refused before
  the allocator is called. So is one from a silo whose effective grant
  policy denies `allocators`.

**Refunds**

- The test allocator returns 500 on `create`: the charge is refunded,
  and `delete` is called on the handle.
- The test allocator stalls past the timeout: the consumer gets
  `UNAVAILABLE`, the charge is refunded, and the handle doesn't
  appear in the consumer's list.
- `delete` while the allocator is down: the export is gone at once, the
  held charge is refunded, and the allocator gets `delete` when it
  comes back.
- A metered handle settles at its last report. A report above the
  reserve settles at the reserve. A falling report is ignored.

**Reachability**

- A served resource is reachable by the consumer through its export
  address.
- It is unreachable from a sibling of the consumer, from the
  consumer's child, from the consumer's parent, and from a third silo
  that holds a grant of the same kind, on TCP, UDP and ICMP, both at
  the endpoint's own address and through the gateway.
- An allocator whose `create` names an endpoint outside its own space
  (another silo's address) is refused, the charge is refunded, and no
  export is made.

**Teardown**

- `delete` tears down the export: open connections close within a
  second, and new ones are refused.
- So does lease expiry, and so does deleting the consumer's silo.
- Deleting the allocator's silo with `force` ends every handle, closes
  every export, and removes its classes from every silo's `quota`.

## Calls made on this page

- **A1. A kind's durable name is `<name>@<owner-id>`,** and a class's
  is `<kind>/<class>`. Ids are stable where paths aren't, and the
  owner is part of the name, so two silos can't pass for each other's
  kind.
- **A2. The built-in classes are kind `gce`, owned by the root and
  implemented by silod.** One ledger and one set of grant rules is
  simpler than two, and it is what makes a custom kind an addition.
- **A3. The model budget of task 1945 is a metered class with a
  period, kind `model`, served by the model proxy.** In v1 it is kept
  in the same ledger as quota, owned by the root, and task 1945's API
  is unchanged. After v1 the kind can move to an allocator in a silo.
- **A4. Metered classes work through reserves.** The final amount
  isn't known at `create`, and charging after the fact would let a
  consumer overspend. A reserve bounds both the consumer and a
  dishonest allocator.
- **A5. Consumption of a metered class stays spent until its period
  ends,** even when the handle or the silo is deleted. A silo's is
  charged to its parent. So a budget can't be refilled by deleting a
  handle or a child.
- **A6. Amounts are decimals with six places**, stored as integer
  millionths, so that `usd` and fractional GB fit without floating
  point.
- **A7. Only the owner grants across the tree.** Holders hand down
  only within their own subtree. It keeps the set of users of a kind
  easy to state, and leaves re-granting for later.
- **A8. A new grant-policy key, `allocators`, absent means deny.**
  Turning custom allocators on for a subtree is a decision, and an
  upgrade doesn't make it.
- **A9. Credentials from `create` pass through silod once and are not
  stored.** silod isn't a secret store. Credentials behind a proxy are
  later.
- **A10. Lending into a sealed silo needs a proper ancestor as the
  requester.** A `manage` holder can create served resources for a
  sealed cell, but can't lend into it. Lending puts an object chosen
  by the allocator inside the cell, and a seal reserves that for those
  above it.
- **A11. At most 16 calls at once to one kind**, with timeouts of 10
  seconds (30 seconds for `create`, up to 600 seconds to `READY`).

## Open questions

1. **Per-resource exports and the rule budget.** Every distinct
   endpoint target adds a fence rule
   ([`gcp-backend.md`, Rule budget](gcp-backend.md#rule-budget)). A
   pool that gives each runner its own address could use up the
   budget. Should the design require an allocator to serve every
   handle from a few shared targets, with the gateway routing by
   handle? Or should silod open one rule per allocator silo, covering
   its whole space, for the gateway only?
2. **Periods per kind or per deployment.** The model budget has one
   period for the whole deployment. This page lets each metered class
   choose its own. Should a grant be able to choose instead, so that
   one team gets minutes per week and another per month?
3. **Re-granting across the tree.** Should a holder be able to offer
   part of its grant to a silo outside its subtree, with the owner's
   leave (`regrant: true` on the grant)?
4. **Allowing named kinds only.** `allocators` in grant policy is
   all-or-nothing. Should an ancestor be able to allow
   `pg@h2pg5m` and nothing else?
5. **Charging the allocator's own costs to the consumer.** In the
   serve shape, the pool pays its `vcpu` and charges the consumer
   minutes, at a rate it chooses. Should silod show both sides together,
   so a consumer can see what its minutes cost in GCE units?
6. **The model budget on delete.** Task 1945 returns an agent's whole
   reservation when it is deleted, spent or not. This page keeps what
   was spent charged until the period ends (call A5). Should
   `agents.md` adopt that rule, so that deleting an agent can't refill
   its silo's budget? And should `agents.md` and `allocators.md` say in
   their own text that the model budget is kind `model` (call A3), or
   is this page's note enough?
7. **Lend placement rights.** What is the shape language for what an
   allocator may place, and may the consumer narrow it (images,
   machine types) per request?
