# Silo gateway and front door

## Status

Normative draft, part of the Silo design ([`README.md`](README.md)),
written in task 1930. The control policy (sealed silos) and `manage`
grants were added in task 1947. Marks such as "call 15" refer to
[Calls made in this draft](README.md#calls-made-in-this-draft).

## What the gateway does

The gateway is the one address every silo can reach. It:

1. serves the per-silo **facade** and the **fence API** to callers inside
   the cloud, with origin identity, on its API port, and applies
   each silo's [control policy](#control-policy);
2. carries **proxied exports** between silos;
3. is the **egress proxy**;
4. carries **agent tool channels**, from each agent's tool VM to its
   harness on the control VM ([below](#agent-tool-channels)).

It isn't how an agent's harness reaches silod. A harness runs on the
control VM and calls silod on its own session socket
([`agents.md`, The session socket](agents.md#5-the-session-socket-how-a-harness-reaches-silod)).
The model proxy is served only on those sockets, never by the
gateway.

It holds no cloud credentials. It forwards facade and fence calls to
silod, with the caller's silo attached.

## Address {#SV-78}

- **`silo.internal`**: an A record in the Cloud DNS private zone
  `silo.internal.`, visible to the shared VPC. Every VM resolves it
  through the metadata server's resolver, with no configuration.
- **One internal IP**: the control VM's static address in the gateway
  subnet, or, with `--gateway-replicas`, the internal passthrough NLB's
  address. The NLB has global access, so VMs in every allowed region
  reach it.
- **The same everywhere** ([principle 5](README.md#2-principles)). The
  name and the IP are the same from every silo. What a caller can do
  depends on where it comes from, not on the address it used.

| Port | Serves |
| --- | --- |
| 8080 | **The API port**: `/compute/v1/...` (the facade) and `/silo/v1/...` (the fence API) |
| 80 | `/x/<exporter-id>/<name>/...` (HTTP exports), and the agent tool endpoints `/silo/v1/agents/-/tools:attach` and `/silo/v1/agents/-/tools/binary` ([`agents.md`, The tool channel](agents.md#the-tool-channel)) |
| 3128 | The egress proxy (`CONNECT`, and plain-HTTP absolute-URI requests) |
| 20000–29999 | TCP exports, one port per accepted import |
| 8443 | The front door (IAP only; the fence denies it from inside) |

The control APIs have a port of their own (call 53) so that the fence
can take them away from a silo whose [control policy](#control-policy)
is `none` while leaving its exports and egress alone. The port 80
listener has no facade or fence handler mounted at all: a request
there for `/compute/v1/` or `/silo/v1/` gets 404 with a body naming
port 8080, so no routing mistake on port 80 can reach the control
plane. The two exceptions are the agent tool endpoints, which are
served by their own handlers and do nothing but attach a tool server
and hand out its binary ([Agent tool channels](#agent-tool-channels)).
They stay on port 80 so that an agent in a sealed silo keeps its tools
(call 53).

## Origin identity {#SV-79}

The gateway identifies every inside caller by the source IP of its TCP
connection, and nothing else:

1. **Source IP → silo.** silod keeps a longest-prefix-match table of
   every silo's own space: its blocks minus what it has delegated
   ([`allocators.md`, Every silo's space map](allocators.md#every-silos-space-map)).
   Each IP is in at most one silo's own space. The gateway holds a copy
   of the table, pushed by silod whenever it changes, with a version
   number. The version is the database's change sequence, one number
   across every silod replica
   ([`state.md`](state.md#what-silod-relies-on-the-store-for)).
2. **The subnet check.** The IP must also lie in a subnet silod made for
   that silo (a primary or secondary range). An IP in a silo's block
   but in no subnet of it can't be a real VM, and gets 403.
3. **The result** is passed to silod as the header `Silo-Origin: <id>;v=<table
   version>`, over the local connection. silod refuses the request if the
   version is older than the newest change that could affect it, for
   example when the caller's block was released a moment ago.

The header is set by the gateway and removed from any inbound request.
Nothing in the request itself (no `Authorization`, no cookie, no
metadata value) affects identity.

Spoofing is prevented by GCE: a VM can only send from its own
addresses and alias ranges, because the facade refuses `canIpForward`.
The source IP survives because the gateway sees the connection
directly. A passthrough load balancer doesn't rewrite it (call 16).

## Control policy

Every silo has a `control` policy: `full`, `read` or `none` (sealed)
([`fence-api.md`, Control policy](fence-api.md#control-policy)). It
limits what any principal acting *as* that silo may do on the control
port. It doesn't touch the other ports. Enforcement has two layers,
and each one holds without the other.

### Layer 1: the gateway and silod refuse {#SV-80}

silod pushes each silo's **effective** control policy to the gateway
with the origin table, under the same version number. On the control
port, after it has resolved the caller's silo, the gateway:

- for `none`, refuses every request;
- for `read`, refuses every request that isn't a read. A read is
  classified by the facade method or fence operation, not by the HTTP
  verb: `zoneOperations.wait` and `quota:preview` are `POST`s and are
  reads; every facade method that creates, changes or deletes an
  object is not. The table is in silod's code and shared with the
  gateway, so the two can't disagree about a method;
- refuses unknown paths the same way, so that a method added to the
  facade later is a mutation until the table says otherwise.

A refusal on the facade is GCE-shaped: 403, `reason: forbidden`, and
a message such as `silo: control-plane calls from /prod are refused
(control=none)`. On the fence API it is `CONTROL_DENIED`
([`fence-api.md`, Errors](fence-api.md#errors)). silod checks the same
policy again on every call it receives, from the gateway or from the
front door, so a gateway that lets a call through still gets it
refused.

The policy also applies to **front-door members** mapped to the silo,
since they act as the silo, and to **an agent's harness** in the silo,
whose calls arrive on its session socket (`via=harness`,
[`agents.md`, §5](agents.md#5-the-session-socket-how-a-harness-reaches-silod)).
Neither the front door nor a session socket is behind the fence, so
for those callers silod's check is the only layer. What a harness keeps
in a sealed silo is listed in
[`fence-api.md`, Control policy](fence-api.md#control-policy).

When a silo's effective policy becomes stricter, the gateway closes
its open connections on the API port from that silo's space at
once, as it does for a revoked export.

### Layer 2: the fence drops the API port {#SV-81}

For a silo whose effective policy is `none`, the fence omits the rule
that lets its blocks reach the API port (`n-api-out`,
[`gcp-backend.md`, The fence](gcp-backend.md#the-fence)). Its packets
to port 8080 then fall through to its `n-deny-out` and are dropped
before they reach the gateway. A bug in the gateway's check still
hits a wall.

A `read` silo keeps the API port, since it needs it for reads. Its
only layer is the first one.

A sealed VM's `gcloud` therefore times out rather than getting a 403,
because its SYN is dropped. That is the price of the second layer.
`silo` inside the cloud names the cause when it sees a timeout on the
API port and a working egress proxy.

### What a sealed silo keeps {#SV-82}

Nothing outside the API port changes. Egress through port 3128,
proxied exports in both directions (port 80 and the TCP export
ports), L3 links and agent tool channels (port 80) keep working. So
do an agent's model access and its control surface (messages and
status), which its harness reaches on its session socket. A sealed
cell runs normally. It just can't create, resize, re-firewall or
delete its own infrastructure, or change its grants.

## Proxied exports {#SV-83}

An export is a grant ([`fence-api.md`, Grants](fence-api.md#grants-exports-and-links)).
Once it is accepted:

- **HTTP exports** are served at
  `http://silo.internal/x/<exporter-id>/<name>/...` (call 23). The
  gateway checks that the caller's silo holds an accepted import of the
  export, strips the prefix, and forwards the request to the export's
  `ip:port`. It adds `Silo-Caller: <importer-id>` and
  `Silo-Caller-Path: /team-a/ci`, so the exporter knows who is calling
  per call. Any inbound `Silo-Caller*` header is removed.
- **TCP exports** get a gateway port when the import is accepted. A
  connection to `silo.internal:<port>` from the importer's own space is
  spliced to the export's `ip:port`. From anywhere else it is refused.
  The exporter sees the gateway's address as the source. It learns the
  caller from the audit log, or from a PROXY protocol v2 header if the
  export asked for one (`proxy_protocol: true`).

For both kinds:

- the fence opens only gateway → export `ip:port` (`export-<x>-in`),
  so the importer never has a direct path;
- each call (HTTP) or connection (TCP) is audited: time, importer,
  export, and for HTTP the method, path, status and bytes;
- **revocation closes connections.** When either side withdraws, or
  either side's ancestors' policy stops allowing exports, silod tells
  the gateway. The gateway refuses new calls and closes open
  connections for that grant within one second.

## Agent tool channels {#SV-84}

An agent's tool VM runs the MCP tool server `silo-tools`, and the
agent's harness runs on the control VM
([`agents.md`, §3](agents.md#3-v1-placement-harnesses-on-the-control-vm-tools-remote)).
They meet at the gateway (call 42):

- `silo-tools` dials out to `http://silo.internal/silo/v1/agents/-/tools:attach`
  and holds the connection open. It needs no fence rule beyond
  `n-gw-out`, and nothing listens in the tool VM. The attach is on
  port 80, not the API port, so a sealed silo's agents keep their
  tools ([Control policy](#control-policy)).
- The gateway identifies the caller by origin, as for any call. silod
  then finds the agent whose tool VM has that IP, and checks that the
  GCE instance behind the IP is the one it created. The facade refuses
  writes to a tool VM (call 40), so a tenant can't put another VM at
  its address. With no matching agent, the gateway answers 403.
- The channel is a multiplexed stream. silod sends the harness's tool
  calls down it and reads the results back. Each tool call is
  recorded in the agent's session, not as an `export.call`.
- The channel ends on the control VM that hosts the agent's session.
  Any other gateway, a replica or another control VM's, answers the
  attach with a redirect to the host's gateway address
  ([`agents.md`, The tool channel](agents.md#the-tool-channel)). When
  the channel drops, `silo-tools` reconnects with backoff.

## Served resources (after v1)

Custom allocators are after v1
([`custom-allocators.md`](custom-allocators.md)). When they are built,
the gateway carries two more things, both made of what is above:

- **A served resource is an export owned by its handle.** It is
  offered to the paying silo only, and it is withdrawn when the handle
  ends ([`custom-allocators.md`, Serve](custom-allocators.md#serve)).
  HTTP endpoints are served at
  `http://silo.internal/r/<handle>/<endpoint>/...`. The gateway adds
  `Silo-Handle: <handle>` beside `Silo-Caller`, so that one allocator
  target can serve many handles. TCP endpoints get a port, as other
  TCP exports do. Revocation, auditing and the one-second close are
  the same.
- **silod's calls to an allocator** leave from the gateway range, to
  the target the owner registered, through one fence rule per kind
  (`alloc-<kind-id>-in`). No tenant can import that endpoint.

Nothing in v1 needs to change for this, except that `/r/` on port 80
is reserved.

## Egress proxy {#SV-85}

No VM in any silo has an external IP, and no tenant subnet has NAT.
The only way out is the gateway's proxy, used as an explicit proxy:

```sh
export HTTPS_PROXY=http://silo.internal:3128 HTTP_PROXY=http://silo.internal:3128 NO_PROXY=silo.internal
```

Nothing sets this for the tenant. It goes in the tenant's image or
startup script, as it would behind any corporate proxy.

### Policy {#SV-86}

A silo's **effective egress policy** is the intersection of its own
policy and every ancestor's policy for its subtree. A host is allowed
only if every level allows it. Each policy is:

```json
{"allow": ["*.debian.org", "proxy.golang.org", "github.com:443"], "deny": ["*.example.com"]}
```

- An entry is a hostname or a `*.` wildcard, with an optional port
  (443 by default for `CONNECT`, 80 for plain HTTP).
- At one level, `deny` beats `allow`. A host matching neither is
  denied.
- The root's policy is set at install (`--egress-allow`) and is empty
  (deny all) by default.
- A parent sets a child's policy through the fence API. A silo may
  also narrow its own, but never widen it.

### Enforcement {#SV-87}

- `CONNECT host:port`: the gateway checks the policy on the hostname
  the client gave. It resolves the name itself and refuses if any
  resolved address is internal (the shared VPC's range,
  `169.254.0.0/16`, or RFC 1918, ULA or loopback ranges), so that a
  DNS name can't be used to reach another silo.
- It doesn't terminate TLS in v1. It can't see whether the SNI differs
  from the `CONNECT` host, so policy is on the `CONNECT` host only.
  A tenant who controls a permitted hostname's DNS can therefore
  tunnel to wherever that name points. This is the usual trade-off of
  an explicit proxy, and it is stated as a limitation.
- Plain HTTP with an absolute URI is forwarded if `Host` is allowed.
- Every connection is audited: silo, host, port, allowed or denied,
  bytes each way.

A transparent path, with a next-hop route to the gateway and SNI
inspection, is [later](README.md#9-later).

## Front door

People and outside callers reach the same APIs from outside the cloud
through the front door. Identity there is the caller's Google identity,
and each identity is mapped to a silo
([`fence-api.md`, Members](fence-api.md#members)).

### v1: IAP TCP forwarding plus an ID token (call 15) {#SV-88}

- The fence rule `gw-admin-in` allows IAP's range
  (`35.235.240.0/20`) to reach the control VM's port 8443. Install
  grants the IAM role `roles/iap.tunnelResourceAccessor`, on the
  control VM only, to the members of the root and to anyone else install
  is told to (`--front-door-member`). IAP decides who may connect at
  all.
- `silo proxy` (and every `silo` command run outside the cloud) opens
  an IAP tunnel to port 8443 and sends each request with
  `Authorization: Bearer <Google ID token>`, audience
  `silo:<host-project>`.
- silod verifies the token's signature, audience and expiry, takes its
  `email`, and looks the caller up in the member table. An unmapped
  identity gets 403. A mapped one acts as its silo, with the same
  authority as a VM inside it, and under the same
  [control policy](#control-policy).
- `silo proxy --listen localhost:8642` serves the facade and fence API
  on localhost, so that `gcloud` and terraform work from a laptop.

This needs no organization, no domain and no certificate. It works in
a standalone project.

### Later: HTTPS load balancer with IAP

With a domain, an external Application Load Balancer with IAP in front
of port 8443 lets browsers and tools reach silod without a tunnel. silod
then verifies `x-goog-iap-jwt-assertion` instead of an ID token. The
member table and the authority rules are the same.

## Placement {#SV-89}

In v1 (call 16):

- **One VM, `silo-control`**, in the home region, runs silod and the
  gateway as two containers. The gateway talks to silod over a Unix
  socket. Only silod uses the VM's service account. The gateway's
  container gets no metadata-server route, through an `iptables` rule
  in its network namespace, so it can't fetch the account's token.
  This is process separation on one trusted VM, not a credential
  boundary.
- **silod's state is in Cloud SQL**, not on the VM. The control VM
  has no state disk. silod reaches the database through a PSC endpoint
  in the control subnet, `silo-ctl`, logging in with IAM as its
  service account. The gateway container, cut off from the metadata
  server, has no token and can't log in
  ([`state.md`](state.md)).
- **`--control-replicas N`** (default 1) makes `silo-control` a
  regional MIG of `N` control VMs, each running silod and a gateway,
  behind an internal passthrough NLB with global access, whose address
  is then `silo.internal`. Each gateway talks to its own VM's silod. Any
  silod serves any call. One of them, the leader, writes the fence and
  runs the reconcile
  ([`state.md`, More than one silod](state.md#more-than-one-silod-and-the-leader-call-34)).
- **Each control VM also runs agents' harnesses**, one container per
  session, and holds the key to the agent credentials kept in the
  database ([`agents.md`, Security](agents.md#11-security)). Session
  containers have no metadata-server route either.
- **`--gateway-replicas N`** adds a regional MIG of `N` gateway-only
  VMs behind an internal passthrough NLB with global access. They have
  no service account. They reach silod on a control VM over mTLS, with
  certificates silod issues at boot through the instance's startup
  metadata, which only silod can write. With several control VMs, they
  reach any of them through the control MIG's NLB. They cache the
  origin table, control policies, grants and egress policy, so they
  keep serving exports and egress while silod restarts or the database
  is unreachable. Facade and fence calls wait for silod, and fail with
  503 after 30 seconds.
- **The facade is plain HTTP inside the VPC.** Google encrypts VPC
  traffic between VMs, and identity doesn't depend on anything in the
  request ([Open question 8](README.md#open-questions)).
