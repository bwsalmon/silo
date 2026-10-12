# Silo facades and backends: package structure

## Status

A design note on where code lives, written in task 3122 when the GCE
packages moved into the layout below. It changes no behaviour and no
requirement: what a facade does is specified per service, for GCE in
[`gcp-backend.md`, The `compute/v1` facade](gcp-backend.md#the-computev1-facade)
(SV-61 to SV-76). This page says how the code for that, and for the
facades after it, is arranged.

## Facades and backends

silod stands between two APIs that often have the same name, and the
code keeps them apart by direction:

- A **facade** is inbound. It is the cloud API a tenant calls inside
  its silo, served by silod at `silo.internal` and through the front
  door ([`gateway.md`](gateway.md)). It is default-deny, fenced to the
  caller's silo, and rewrites everything it returns. It lives in
  `pkg/facade/<cloud>/<service>`.
- A **backend** is outbound. It is silod's client of the real cloud:
  an interface with only the calls silod makes, the real
  implementation, an in-memory fake that CI runs silod against
  ([`testing.md`, Fake GCE backend](testing.md#fake-gce-backend)), and
  behaviour tests both must pass. It lives in
  `pkg/backend/<cloud>/<service>`. Only backends hold or use cloud
  credentials (principle 3 in the [README](README.md#2-principles)).

A facade calls its backend, never the cloud directly. Other parts of
silod (install, the fence compiler, reconcile, the allocators' root
capacity read) use the backend without going through any facade.

Today there is one of each, for GCE:

| Package | What it is |
| --- | --- |
| `pkg/facade/gcp/gce` | The per-silo `compute/v1` facade (package `gce`) |
| `pkg/facade/gcp/gce/filter` | The facade's list-filter grammar (SV-74) |
| `pkg/backend/gcp/gce` | The GCE client interface silod calls, `gce.Client`, with `Await` and the error helpers (package `gce`) |
| `pkg/backend/gcp/gce/gceclient` | The real client, over the generated `compute/v1` client and Cloud Quotas' REST API |
| `pkg/backend/gcp/gce/fake` | The in-memory fake, in process and over HTTP; `fake/fenceeval` evaluates the fence firewall policy |
| `pkg/backend/gcp/gce/gcetest` | Behaviour tests, run against the fake directly and against `gceclient` pointed at the fake's HTTP server |

The two packages share the name `gce` because each is named after the
service it is a facade or a client of. The facade imports the backend
as `gceapi`.

## Where new ones go

A new service gets a directory under its cloud on each side it needs:
`facade/<cloud>/<service>` and `backend/<cloud>/<service>`. Cloud SQL
or GCS for GCP would be `facade/gcp/sql` and `backend/gcp/sql`; a
second cloud gets a sibling of `gcp`. A backend's real client goes in
a sub-package named for what it wraps, not for the cloud, which the
path already names (`gceclient`, not `gcp`). Its fake and behaviour
tests sit beside it as `fake` and `<service>test`, as GCE's do.

A backend without a facade is fine: silod may need a service (Cloud
Quotas, Cloud SQL at install) that tenants never call. A facade
without its own backend is not: whatever it serves, it reaches through
a backend package, so that it can be tested against a fake.

## The pipeline every facade shares

Every facade answers a call the same way, and only the details are
the service's. In the GCE facade the order is the one written at the
top of `facade.go`:

1. **Resolve the project to a silo.** The project in the path names a
   silo: its own facade project (`silo-<id>`), or the host project
   meaning the caller's own silo. The caller reaches its own silo, its
   descendants (as `via=ancestor:<id>`), and the targets of active
   `manage` grants it holds and their descendants, for the resource
   types the grant's scope names (as `via=manage:<holder>`). Anything
   else is refused the same way whether or not it exists (SV-62).
2. **Hold the caller to its control policy**, by
   `pkg/control`'s classification of the method as reading or
   mutating (SV-62, [`fence-api.md`, Control policy](fence-api.md#control-policy)).
3. **Look the method up in the method table.** A method not in it is
   refused; a resource type the facade does not know is not found
   (SV-63).
4. **Check the region or zone** against the silo's allowed regions
   ([`allocators.md`, Allowed regions](allocators.md#allowed-regions)).
5. **Check the body against the allowlists.** A field outside the
   method's allowlist that is set to anything but its default is
   refused, and so is a label or tag in silo's reserved namespace
   (SV-63, SV-76).
6. **Call the backend** for the silo's own objects only, and **build
   the response** from allowlisted fields, with links mapped back to
   the tenant's project. Names are checked, not mapped: the tenant uses
   the real ones (SV-76).
7. **Audit.** Every write, accepted or refused once its silo is known,
   is a `facade.call` record in the target silo's audit log, with the
   via, the method, the object's name, and the charges.

Steps 1, 2 and 7 are the same for any service: they depend on the
silo tree, grants and the audit log, not on the API. Steps 3 to 6 have
the same shape everywhere and take their data from the service: its
method table, its idea of location, its allowlists and its naming.

## What stays per service

- **Handlers**: the code for each method, including quota charges and
  what it asks the backend for.
- **Allowlist tables**: resource types, methods, request fields and
  response fields (`tables.go` for GCE). They are data, and the only
  place a method or field is listed.
- **Operations**: how the service reports long-running work, and how
  silod watches it to refund quota and finish its bookkeeping (SV-73).
- **Lists and filters**: the service's own filter grammar and paging
  (GCE's is `filter/`, SV-74).
- **Error shapes**: each facade answers in its service's own error
  format, with its own reasons (SV-75).
- **Path parsing and naming**: how the service's URLs name a project,
  a location and an object, and how names map to real ones.

## When the shared core is extracted

Not now. The shared steps are in `pkg/facade/gcp/gce` today, in
GCE's terms: the caller and target resolution, the control check and
the audit write are methods on the GCE `Server`, over GCE's parsed
path.

**Decision:** the shared core moves into `pkg/facade` when the
second facade lands, as part of that work, and is shaped by the two
real users rather than guessed from one. Guessing the seams from GCE
alone would bake GCE's notions (zones, `compute/v1` paths, its
operation and error types) into an interface the second service then
has to bend around. With two implementations side by side, the common
part is whatever both actually call.

What is expected to move, so that the second facade's author knows
where to look:

- project → silo resolution, with descendants and `manage` grants;
- the control-policy check;
- the `facade.call` audit record and its via;
- the method-table and body-allowlist mechanics, with each service's
  tables staying in its own package.

Until then, a change to one of these in the GCE facade should keep it
free of GCE-only assumptions where that costs nothing, and say so in
the code when it cannot.
