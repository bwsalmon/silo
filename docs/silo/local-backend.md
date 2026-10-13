# Silo local backend: one machine, granule VMs or gVisor sandboxes, behind a Docker Engine API facade

## Status

Design written in task 3350 (filed from task 3024). **Implementation
has started**: task 3402 asked for the local backend to be built as
this page describes, and the [staged plan](#15-staged-implementation-plan)
is being built in its order. Step 1, the backend interfaces under
silod (`pkg/backend`, with GCP's in `pkg/backend/gcp`), is task 3404.

The direction in [§1](#1-settled-direction) was set by bsalmon when the
task was filed, and this page states it without reopening it. Two
comments on the task added to it: **agent CLIs run on the local machine
with ambient permissions**, and **they are already installed on the
host** ([§10](#10-agents-on-the-local-backend)).

Calls made on this page are marked **(call L1)**, **(call L2)** and so
on. They have their own numbering, as
[`custom-allocators.md`](custom-allocators.md) does, and are listed in
[Calls made on this page](#calls-made-on-this-page). What is still
undecided is under [Open questions](#open-questions).

**Part 2** of this design is
[`local-rootless-storage.md`](local-rootless-storage.md) (task 3356).
It records two later decisions by bsalmon: silod runs **fully
rootless**, and storage is laid out under **one data directory** so that
backups can be added later without migrating anything. Where the two
pages disagree, part 2 wins. The paragraphs of this page it replaces
(§2's outline, §9's socket note, §11's storage layout, and §12's state,
privileges, install and restart sections) say so. Its calls are
L26–L40, its open questions are 13–20, and its staged-plan steps are
items 14–18 [below](#15-staged-implementation-plan).

**Part 3** is [`local-git.md`](local-git.md) (task 3360). It adds git
repositories that silos host for their agents, served at
`http://silo.internal/git/` and mirrored outbound into one external
"uber repo" as namespaced branches, with a submodule index on its
`main`. It adds to the first two parts without replacing anything in
them. Its calls are L41–L55, its open questions are 21–30, and its
staged-plan steps are items 19–24 [below](#15-staged-implementation-plan).

**Part 4** is [`local-registry.md`](local-registry.md) (task 3449). It
adds a host-local OCI registry that silod serves at
`registry.silo.internal`, with repos scoped per silo under git's fence
model, and a pull-through cache for Docker Hub, gcr.io, ghcr.io and
other configured upstreams, held to each silo's own egress policy. It
changes the facade: `push` to the local registry, `save`, `load`,
`commit` for `sandbox` and a validate-only `/auth` are served, and
`build` stays refused, with builds done by kaniko or BuildKit in a
container. It moves containerd's root out of `cache/`, and answers
open questions 6 and 7. Its calls are L56–L76, its open questions are
31–40, and its staged-plan steps are items 25–36
[below](#15-staged-implementation-plan).

### What was checked, and against what

- **granule**: gke-labs/granule at `9cd65eb` (2026-10-12): its README,
  `pkg/guestwrap`, `cmd/granule-container`, `pkg/execwire`,
  `pkg/agent`, `pkg/config`, `pkg/hypervisor`, `pkg/guestboot`,
  `pkg/cli` and `cmd/granule-shim`. Each granule claim below names
  where it comes from. The ones the source did not settle are marked
  *unverified*.
- **gVisor**: the documentation at gvisor.dev, fetched on 2026-10-12:
  Networking, Changing Platforms, Filesystem, Compatibility (with the
  Linux/amd64 syscall table), Checkpoint/Restore, and the containerd
  quick start. Nothing on this page was run against `runsc`.
- **QEMU's security policy** on TCG could not be fetched from the
  sandbox (the request timed out). [§3](#tcg-is-not-a-fence) states it
  from QEMU's documentation as the author knows it, and the
  implementation task has to cite it from the source.
- **Silo**: this repository at `4aaeb68`, in particular
  `pkg/backend/gcp/gce`, `pkg/reconcile`, `pkg/fence`,
  `pkg/ledger/rootcap`, `pkg/fenceapi` and `pkg/harness`.

## Contents

1. [Settled direction](#1-settled-direction)
2. [The shape on one host](#2-the-shape-on-one-host)
3. [Isolation classes](#3-isolation-classes)
4. [The Docker facade](#4-the-docker-facade)
5. [Compose and testcontainers](#5-compose-and-testcontainers)
6. [Runtime drivers](#6-runtime-drivers)
7. [The backend interface under silod](#7-the-backend-interface-under-silod)
8. [Extracting the facade core](#8-extracting-the-facade-core)
9. [The local network fence](#9-the-local-network-fence)
10. [Agents on the local backend](#10-agents-on-the-local-backend)
11. [Resources and quota](#11-resources-and-quota)
12. [State, install and operations](#12-state-install-and-operations)
13. [Testing and the conformance split](#13-testing-and-the-conformance-split)
14. [Limitations](#14-limitations)
15. [Staged implementation plan](#15-staged-implementation-plan)
16. [Calls made on this page](#calls-made-on-this-page)
17. [Open questions](#open-questions)
18. Part 2: [rootless operation and the data directory](local-rootless-storage.md)
19. Part 3: [git repositories for agents, mirrored into one external repo](local-git.md)
20. Part 4: [a host-local OCI registry with a pull-through cache](local-registry.md)

## 1. Settled direction

- **Everything runs on one machine.** That covers silod, its state, the
  gateway, the agents' harnesses and every tenant workload. Tenant
  workloads are **granule microVMs** where the host has KVM, and
  **gVisor sandboxes** where it doesn't (for example a cloud VM
  without nested virtualisation). Fences are local Linux networking.
- **The local backend sits beside the GCP backend in one codebase.**
  A deployment's silod has one configured backend, `gcp` or `local`.
  Mixed nesting, where a GCE silo hosts a local child silo in a
  nested-virt VM, is out of scope and stays in
  [README §9](README.md#9-later).
- **Silo's own API is identical across backends.** The `silo/v1` fence
  API, the tree, blocks and the ledger, grants, links, exports, the
  gateway, egress, agents and the model proxy are Silo's concepts. A
  parent manages a local silo exactly as it manages a GCE one. Only
  the layer that makes machines differs.
- **The facade inside a local silo is a subset of the Docker Engine
  API.** bsalmon weighed three options:
  1. *Emulate `compute/v1` over the local runtime.* Rejected: zones,
     machine types, GCE images, disks, LROs, the metadata server and
     tag-based firewalls would all be fictions to maintain.
  2. *A bespoke native API.* Rejected: no client speaks it.
  3. *The Docker Engine API.* **Chosen**, so that existing clients work:
     the docker CLI, the SDKs, compose and testcontainers. This is the
     sketch in [README §9](README.md#9-later): "a Docker Engine API
     subset, one bridge per silo with anti-spoofing".

  Principle 4 still holds: the facade is a **default-deny allowlist**
  of endpoints and fields, not a pass-through
  ([README §2](README.md#2-principles)).
- **There are two isolation classes behind the one facade**, `vm` and
  `sandbox` ([§3](#3-isolation-classes)). The class is part of a
  silo's policy, and **never-looser** applies to it.
- **QEMU without KVM (TCG) is not a fence.** It is refused by default
  ([§3](#tcg-is-not-a-fence), call L3).
- **This is the second facade**, so the shared facade core that
  [`facades.md`](facades.md#when-the-shared-core-is-extracted) holds
  back for it is extracted here ([§8](#8-extracting-the-facade-core)).
- **Agent CLIs run on the host, with ambient permissions, and are
  already installed there** (bsalmon, on the task;
  [§10](#10-agents-on-the-local-backend)).

## 2. The shape on one host

```
 host (one Linux machine, root netns)
┌──────────────────────────────────────────────────────────────────────────┐
│ silod (root)  ── SQLite state, ledger, fence compiler, Docker facade,     │
│   │             fence API, model proxy, harness runner                    │
│   │ unix sockets: /run/silo/silo.sock  (fence API, front door)            │
│   │               /run/silo/docker/<id>.sock  (Docker facade per silo)    │
│   ▼                                                                       │
│ silo-containerd (private instance, /run/silo/containerd)                  │
│   ├─ runc shim ── granule VM container ── cloud-hypervisor ── guest       │
│   └─ runsc shim ─ gVisor sandbox (systrap, netstack)                      │
│                                                                           │
│ harnesses: claude / codex from the host's PATH, as the agent user         │
│                                                                           │
│ ┌──────────────── netns "silo-net" (no link to the root netns) ─────────┐ │
│ │ gw0: 10.255.255.1 silo.internal, 10.255.255.2 docker.silo.internal     │ │
│ │      (gateway + DNS sockets, opened here by silod)                     │ │
│ │ br-<net>  one bridge per Docker network; veth per container           │ │
│ │ nftables: bridge silo (anti-spoofing), inet silo (the fence)          │ │
│ └────────────────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────────┘
```

The diagram shows this page's first, rootful draft. In the rootless
design that replaces it
([part 2](local-rootless-storage.md#1-rootless-operation)), the same
pieces run as one unprivileged user. A namespace holder, `silo-nsd`,
owns a user namespace with `silo-net` in it. containerd runs in the
holder's namespaces, and silod stays in the host's. The sockets move to
`$XDG_RUNTIME_DIR/silo/` (or `/run/silo/` for the system install), and
the state moves under one data directory.

In outline:

- **silod runs rootless**, under systemd user units, with **SQLite** as
  its store ([part 2](local-rootless-storage.md), replacing "silod
  runs as root" in [§12](#12-state-install-and-operations)).
- **A private containerd** that silod alone can reach runs both
  isolation classes and keeps the image store they share
  ([§6](#6-runtime-drivers)).
- **Every tenant network lives in one network namespace, `silo-net`.**
  No interface joins it to the host's own namespace. The gateway's
  listening sockets are opened inside it, and the egress proxy dials
  out from the host's namespace. Those sockets are the only path
  between the two ([§9](#9-the-local-network-fence)).
- **The front door is a Unix socket.** The caller's uid, read with
  `SO_PEERCRED`, is mapped to a silo, just as an IAP identity is in the
  GCP backend.
- **Harnesses are the host's own `claude` and `codex`**, run as the
  agent user ([§10](#10-agents-on-the-local-backend)).

## 3. Isolation classes

| Class | What runs a container | Needs | Boundary |
| --- | --- | --- | --- |
| `vm` | A granule microVM: cloud-hypervisor and a guest kernel, inside an unprivileged runc container | `/dev/kvm`, `/dev/net/tun` | Hardware virtualisation (KVM). The stronger boundary. |
| `sandbox` | A gVisor sandbox: `runsc`, systrap platform, netstack | Nothing beyond a Linux host with seccomp | A user-space kernel (the Sentry) behind seccomp filters, with the filesystem behind a gofer. A real, widely deployed boundary (Cloud Run, GKE Sandbox), but weaker than a VM. |

### Detection

silod probes the host at start and records the classes it can offer.
It re-probes when the host changes, for example after a reboot. A class
counts as available only if a real workload of that class boots:

- **`vm`**: granule's own `granule doctor -only kvm,tun`. It opens
  `/dev/kvm`, checks the KVM module and nested virtualisation, and
  checks `/dev/net/tun` (granule README, "Checking a new host").
  silod then boots one `silo-probe` VM from the pinned granule image,
  because a `/dev/kvm` that opens but cannot run a VM has been seen
  under nested virtualisation.
- **`sandbox`**: one `runsc` sandbox with the pinned version and
  `--platform=systrap`, running `true`.

`silo describe` on the root reports the host's classes under
`host.isolation_classes`. A class silod could not probe is absent, not
`false`.

### In a silo's policy

Every silo gets a new policy, `isolation`, handed down like egress
(call L1):

```json
"isolation": { "allowed": ["vm", "sandbox"], "default": "vm" }
```

- The classes are **ordered by strength**: `vm` is stronger than
  `sandbox`. "Never looser" means a child's `allowed` is a subset of its
  parent's. A parent that allows only `vm` makes every descendant
  `vm`-only. A child can't add `sandbox` back, and the fence API refuses
  that with `LOOSER_THAN_PARENT`.
- `default` must be in `allowed`.
- The root's `allowed` is set at install. It is every class the host
  offers, unless the operator narrowed it. `default` is `vm` when the
  host offers it, and `sandbox` otherwise.
- A parent can require `vm` for a child, and that is the whole
  mechanism. There is no separate "minimum class".

### How a tenant asks for a class through the Docker API

- **`HostConfig.Runtime`** (`docker run --runtime=…`) names it:
  - `silo-vm` gives the `vm` class. `kata` and `granule` are accepted
    as aliases.
  - `silo-sandbox` gives the `sandbox` class. `runsc` is accepted as
    an alias.
  - `runc`, empty, and the name `/info` reports as `DefaultRuntime`
    all mean the silo's `default`.
  - Any other name is refused with 400.

  Accepting `runc` as the default is deliberate: compose files and test
  libraries often name it, and **nothing in a silo ever runs under
  plain runc** (call L2).
- **`GET /info`** reports `Runtimes` with only the classes this silo is
  allowed **and** the host can provide. `DefaultRuntime` is the silo's
  default.
- **The class is fixed at create.** Each container's inspect output
  shows it in `HostConfig.Runtime`, always under its canonical name.

### When the host can't provide a class

- **A class the silo's policy doesn't allow:** `403`, message
  `silo: isolation class "sandbox" is not allowed in this silo
  (allowed: vm)`.
- **A class the policy allows but the host can't run** (for example
  `vm` on a host without KVM): `409`, message
  `silo: this host cannot run isolation class "vm" (no usable /dev/kvm)`.
  silod **never falls back** to a weaker class, and never to a
  stronger one either, because the caller asked for something specific.
- **A silo whose `default` the host can't run** fails every container
  create that doesn't name a class, with the same 409. The fence API
  warns when a parent sets such a default.
- **A host that loses KVM after a reboot** keeps its existing `vm`
  containers in the database, stopped, and can't start them. Each
  container's `State.Error` names the reason. They are not converted.

### TCG is not a fence

QEMU's security documentation draws the line between its
"virtualization use case", with a hardware accelerator such as KVM,
and its "non-virtualization use case", which includes TCG. Only bugs
in the first are treated as security bugs. The second is not a
security boundary. (*Unverified in this run*: the page didn't load from
the sandbox. The implementation task must quote it.)

So **silod refuses TCG by default (call L3).** No class is backed by
it, and nothing falls back to it. If one is ever added, it would be a
third class, `emulated`, with these properties:

- It is off unless install is given `--allow-unfenced-emulation`.
- It is reported in `describe` and `/info` as **unfenced**.
- It is never in `allowed` unless an ancestor puts it there by name.
  Every child of a silo that has it inherits the label "unfenced".
- It is ordered **below** `sandbox`.

v1 doesn't build it. It is in [Open questions](#open-questions) for
anyone who wants a compatibility mode on hosts with neither KVM nor a
working gVisor.

## 4. The Docker facade

### Address, and how a request names its silo

The Docker API puts no project in the path, so the facade needs
another way to know a request's **target** (the silo it acts on) and
its **caller** (the silo acting). The options were a per-silo
endpoint, an auth token, or a host name per silo. This page takes
**endpoints per silo, with the caller from origin. There is no token**
(call L4), which keeps principle 5: nothing in a request can change
who the caller is.

| From | Endpoint | Caller | Target |
| --- | --- | --- | --- |
| Inside a silo | `tcp://docker.silo.internal:2375` | The source IP's silo (origin identity, as in [`gateway.md`](gateway.md#origin-identity)) | The caller's own silo |
| Inside a silo, acting on a descendant or a `manage` target | `tcp://<silo id>.docker.silo.internal:2375` | As above | The silo named in the `Host` header |
| The host | `unix:///run/silo/docker/<silo id>.sock` | The peer uid (`SO_PEERCRED`), mapped to a silo as a front-door member | The silo whose socket it is |
| An agent's harness | The session socket, at `/docker/` | The agent's silo (`via=harness`) | The caller's own silo |

- The **`Host` header plays the part of GCE's project in the path.**
  `docker -H tcp://k7f3qa.docker.silo.internal:2375 ps` from `/a`
  lists `/a/c`'s containers when `k7f3qa` is `/a/c`. The usual rules
  then apply: own silo, descendants (`via=ancestor:<id>`), and active
  `manage` grants (`via=manage:<holder>`). Anything else gets the same
  404 for every container, whether or not it exists.
- **The host sockets** are root-owned in a root-owned directory, mode
  `0666`. Authority comes from the uid check, not from the file mode,
  so any local user can connect, and a uid mapped to no silo gets 403
  on everything but `/_ping`. `silo docker-env /team-a/ci` prints
  `DOCKER_HOST=unix:///run/silo/docker/<id>.sock` for a path, and
  `silo install` adds a docker context per silo the installing user
  can reach.
- **`docker.silo.internal` is its own gateway address** (10.255.255.2,
  [§9](#9-the-local-network-fence)), not `silo.internal`. Every other
  port on that address is a **published port** of the caller's silo
  ([Published ports](#published-ports)). Clients that reach a
  published port at `DOCKER_HOST`'s host name, as testcontainers does,
  then work without configuration.
- **Port 2375 is in the API-port class.** The fence drops it for a
  sealed silo, as it drops 8080 ([§9](#sealed-silos-and-never-looser)).
  The gateway's TLS port, 2376, is not served: traffic in `silo-net`
  never leaves the host. TLS inside the network is the GCP backend's
  open question 8, which doesn't arise here.

### API version

The facade serves **Engine API versions 1.41 to 1.47** (call L5).

- `/_ping` answers with `API-Version: 1.47`.
- `/version` reports `ApiVersion: 1.47` and `MinAPIVersion: 1.41`.
- A request under `/v1.NN/` outside that range gets 400, with Docker's
  own wording ("client version 1.NN is too old/new").
- An unversioned path is read as 1.47.

The docker CLI, compose v2, the Go and Python SDKs and testcontainers
all negotiate down from `/_ping`. 1.41 is Docker 20.10, the oldest
release still widely deployed. 1.47 is Docker 27, whose request and
response shapes every current client accepts.

*Unverified:* the exact minimum server version that docker CLI 28 and
29, docker-py 7 and testcontainers-java accept. The facade task checks
each against the range and widens the top if a client refuses 1.47.
Serving a newer version only adds fields to the allowlists.

### Endpoints served

Every endpoint not listed here, and every route Docker doesn't have,
gets 404 `page not found`, as Docker answers an unknown route. The
refusals in the next table are routes Docker has and the facade
recognises.

| Group | Endpoints | Notes |
| --- | --- | --- |
| System | `GET`/`HEAD /_ping`, `GET /version`, `GET /info`, `GET /events`, `GET /system/df` | `/info` and `/system/df` describe **the silo**, not the host: see [What `/info` says](#what-info-says) |
| Containers | `POST /containers/create`, `GET /containers/json`, `GET /containers/{id}/json`, `POST /containers/{id}/start`, `/stop`, `/restart`, `/kill`, `/wait`, `/pause`, `/unpause`, `/rename`, `/resize`, `DELETE /containers/{id}`, `GET /containers/{id}/logs`, `GET /containers/{id}/stats`, `GET /containers/{id}/top`, `POST /containers/prune` | `top` lists processes through the driver. `stats` reads the container's cgroup. |
| Attach | `POST /containers/{id}/attach` (hijacked stream) | Both classes ([Exec, logs and attach](#exec-logs-and-attach)). `GET /containers/{id}/attach/ws` is refused. |
| Exec | `POST /containers/{id}/exec`, `POST /exec/{id}/start`, `POST /exec/{id}/resize`, `GET /exec/{id}/json` | |
| Archive | `HEAD`/`GET`/`PUT /containers/{id}/archive` (`docker cp`) | testcontainers copies files with it. See [Archive](#archive-docker-cp). |
| Images | `POST /images/create` (pull: `fromImage` and `tag` only), `GET /images/json`, `GET /images/{name}/json`, `GET /images/{name}/history`, `POST /images/{name}/tag`, `DELETE /images/{name}`, `POST /images/prune` | Pulls go through the egress policy ([Images](#images)). *Part 4* adds `POST /images/{name}/push` to the local registry, `GET /images/get` and `/images/{name}/get`, `POST /images/load`, `POST /commit` (`sandbox` only) and `POST /auth` ([`local-registry.md` §7](local-registry.md#7-the-docker-facade)) |
| Networks | `GET /networks`, `GET /networks/{id}`, `POST /networks/create`, `DELETE /networks/{id}`, `POST /networks/{id}/connect`, `/disconnect`, `POST /networks/prune` | `bridge` driver only ([Networks](#networks)) |
| Volumes | `GET /volumes`, `POST /volumes/create`, `GET /volumes/{name}`, `DELETE /volumes/{name}`, `POST /volumes/prune` | `local` driver, no driver options ([Volumes](#volumes)) |

Reads (`GET`, `HEAD`, and `/wait`, `/logs`, `/events`, `/attach` on a
container that is only read) are `read` under
[`pkg/control`](fence-api.md#control-policy), classified by endpoint,
not by verb (call 52). `POST /containers/{id}/attach` with `stdin=1`
and `POST /exec/…` mutate.

### Refused

| What | How it's asked for | Answer |
| --- | --- | --- |
| Bind mounts of host paths | `HostConfig.Binds` with a path source, `Mounts[].Type=bind` | 403 `silo: bind mounts of host paths are not allowed; use a volume` |
| Privilege | `HostConfig.Privileged`, `CapAdd` (anything), `SecurityOpt` other than `no-new-privileges`, `seccomp=unconfined` / `apparmor=unconfined` | 403 `silo: … is not allowed in a silo` |
| Devices | `HostConfig.Devices`, `DeviceRequests` (GPUs), `DeviceCgroupRules` | 403 |
| Host namespaces | `NetworkMode: host`, `PidMode: host`, `IpcMode: host`, `UTSMode: host`, `UsernsMode: host`, `CgroupnsMode: host` | 403 |
| Another container's namespaces | `NetworkMode`/`PidMode`/`IpcMode: container:<id>` | 403 in v1. A shared namespace doesn't exist across two VMs. |
| Kernel tuning | `Sysctls`, `HostConfig.Ulimits` above the class's limits, `OomScoreAdj` < 0, `CpusetCpus`, `CpusetMems` | 403 |
| Other mounts | `Mounts[].Type=npipe`/`cluster`, `VolumeDriver` other than `local`, volume `DriverOpts` (which can name a host device or an NFS server) | 403 |
| Raw host ports | `PortBindings` with a `HostIp` | 403. A published port has no host address ([Published ports](#published-ports)). |
| Logging drivers | `LogConfig.Type` other than `json-file` or `local` | 403. Logs are silod's. |
| Build | `POST /build`, `POST /build/prune`, `/session` | 501. *Part 4* changes the message to name the supported path, kaniko or BuildKit in a container and a push to `registry.silo.internal` ([`local-registry.md` §9](local-registry.md#9-building-images)) |
| Image import/export | `POST /images/create?fromSrc=…`, `GET /containers/{id}/export`, `GET /containers/{id}/changes`, `GET /images/search`, `POST /commit` of a `vm` container, and `POST /images/{name}/push` to any registry but `registry.silo.internal` | 501. *Part 4* serves `load`, `save`, push to the local registry and `sandbox` commit ([`local-registry.md` §7](local-registry.md#what-changes-call-l68)) |
| Registry login | `POST /auth` | *Part 4:* served, validate-only. Credentials still travel per pull in `X-Registry-Auth` and are never stored ([`local-registry.md`, `docker login`](local-registry.md#docker-login)). |
| Resource updates | `POST /containers/{id}/update` | 501 in v1 |
| Swarm | `/swarm*`, `/nodes*`, `/services*`, `/tasks*`, `/secrets*`, `/configs*` | 503 `This node is not a swarm manager`, Docker's own answer for a node outside a swarm, so clients handle it |
| Plugins | `/plugins*` | 501 |
| Distribution | `GET /distribution/{name}/json` | 501 |
| Checkpoints | `/containers/{id}/checkpoints*` | 501 ([§6](#snapshot-and-checkpoint)) |

A field outside an endpoint's allowlist that is set to anything but
its default gets 400, naming the field. The allowlists are the request
and response fields of the endpoints above. They live as data in
`pkg/facade/local/docker/tables.go`, as GCE's do in `tables.go`.

**Labels in Silo's namespace are refused:** any label key starting
with `dev.silo.` or `silo.`. silod puts its own there, and responses
strip them. Docker's and compose's own label namespaces
(`com.docker.*`) are the tenant's.

### Containers in each class

A Docker container is one of these:

- **`sandbox`: one gVisor sandbox** running the image. The OCI runtime
  spec is built from the image config: `Entrypoint`, `Cmd`, `Env`,
  `WorkingDir`, `User` and `StopSignal`, overridden by the create
  request as Docker does. It runs under `runsc` through containerd's
  `io.containerd.runsc.v1` shim, with `--platform=systrap`,
  `--network=sandbox` (netstack), `--overlay2=root:self` (the default,
  per gVisor's Filesystem guide) and directfs on (the default). The
  container's processes are the sandbox's. A Docker container never
  shares a sandbox with another.
- **`vm`: one granule microVM** running the image's ENTRYPOINT and CMD.
  The image is first **wrapped** (`granule wrap`, `pkg/guestwrap`):
  - Its filesystem is placed whole at `containerspec.RootfsDir` in a
    granule guest.
  - Its OCI config becomes the spec that `granule-container` runs in
    the guest. That covers entrypoint, cmd, env, workdir, user and
    stop signal.
  - At boot, `granule-container` mounts the pseudo-filesystems, chroots
    into that rootfs, drops to the image's user and execs the
    entrypoint.
  - The VM powers off when that process exits (`Spec.HaltOnExit`).
  - Nothing from the wrapped image runs at wrap time: it appears only
    as `COPY --from` (`pkg/guestwrap` package comment).

  The wrapped image runs as a runc container in `silo-containerd`. Its
  privileges are the ones granule asks of Docker (granule README,
  "Using a container CLI other than `docker`"): `/dev/kvm` and
  `/dev/net/tun` as devices, all capabilities dropped except
  `NET_ADMIN` (scoped to the container's own netns), and
  `no-new-privileges`. Its supervisor is PID 1 and runs
  cloud-hypervisor.

  Per-request overrides of entrypoint, cmd, env, workdir and user are
  passed to `granule-container` through the spec file. *Unverified:*
  whether `granule-container` reads its spec at boot from somewhere a
  per-container override can reach without rewrapping. Its package
  comment says wrap writes the spec into the image at
  `containerspec.ConfigPath`. If the override can't reach it, the
  granule driver needs a granule change to take the spec from the
  boot parameters or the control link. That change is plan item 9.

Both classes get the same defaults, so that a container behaves the
same whichever class it lands in:

- the class's resource defaults ([§11](#11-resources-and-quota));
- no capabilities beyond Docker's default set;
- `no-new-privileges`;
- a `/etc/hosts` and `/etc/resolv.conf` written by silod
  ([DNS](#dns)).

In a sandbox these are enforced inside gVisor's Sentry. In a VM they
are enforced inside the guest, which is the tenant's own kernel.

### Exec, logs and attach

| | `sandbox` | `vm` |
| --- | --- | --- |
| **exec** | containerd's `Exec`, which `runsc exec` serves: a new process in the sandbox, with stdin, TTY, resize and exit code | granule's guest agent over vsock (`pkg/execwire`: stdin in, stdout and stderr separately, terminal resizes, an exit frame). **Gap:** `granule exec` runs in the guest OS, not in the wrapped container's chroot (`pkg/agent` has no chroot), so a `docker exec` there would see granule's guest filesystem, not the image's. Docker's semantics need the agent to enter `RootfsDir` with the image's env and user: plan item 9. |
| **logs** | The shim's stdout and stderr, written by containerd to a per-container log file that silod owns, in `json-file` framing | **Gap:** `docker logs` of a granule container streams the guest's serial console, kernel lines included (granule README, "Console Logging"). `granule-container` pipes the workload's output to its own streams, "where the unit sends it" (`cmd/granule-container/main.go`). The facade needs the workload's streams alone, kept apart from the console: plan item 9. |
| **attach** | containerd's task I/O (FIFOs), stdin included when `OpenStdin` was set | The same streams as logs, plus stdin. *Unverified:* granule has no documented path for stdin to a wrapped workload, so `vm` attach is output-only in v1, and `OpenStdin`/`-i` on a `vm` container is refused with 400 until granule has one. |
| **wait / exit code** | The task's exit status | **Gap:** the VM container's exit status is cloud-hypervisor's (`cmd/granule-shim/main.go` exits with the VMM's code). The wrapped process's code, which `granule-container` computes with the 128+N signal convention, is not visibly passed out. Plan item 9 asks granule to report it, for example through the agent or a file the shim reads at power-off. Until it does, `vm` containers report 0 for a clean power-off and 137 for a kill. |
| **stop / kill** | `SIGTERM` (or `StopSignal`), then `SIGKILL` after the timeout | granule maps a stop to an ACPI power button (README, "Graceful Termination"). The guest's `granule-container` forwards SIGTERM/SIGINT to the workload as its stop signal. `kill -s KILL` destroys the VM. Other signals go through the agent: *unverified*. |
| **pause** | `runsc pause` through containerd | cloud-hypervisor's `Pause`/`Resume` (`pkg/hypervisor`). Not wired to a granule command yet. |
| **top** | `runsc ps` | exec of `ps` in the guest. The image may lack it, so `top` on a `vm` container can return 501. |

### Archive (`docker cp`)

- **Before start:** silod writes into the container's root before it
  first starts. For `sandbox`, that is the containerd snapshot's upper
  directory. For `vm`, it is the overlay that granule creates
  (`granule cp` exists, `pkg/guestcp`; *unverified* for a stopped VM).
- **After start:** for `sandbox`, an exec of the runtime's own tar
  helper, gVisor's `runsc` filesystem access through the gofer
  (*unverified* which `runsc` subcommand fits; the fallback is an exec
  of `tar` in the image, refused with 501 when the image has none).
  For `vm`, `granule cp` into the guest's `RootfsDir`.

The archive is parsed by silod, on the host, from a tenant. It is
size-capped and charged to `disk_gb`. Paths are resolved inside the
container's own root and never followed on the host, using the same
`O_NOFOLLOW` discipline as `pkg/harness`'s `nofollow_unix.go`.

### Images

- **Pulls run in silod, on the host, so they would bypass the fence**
  if nothing checked them. A pull is therefore held to the **silo's
  egress policy** (call L6): the registry host, and every host the
  registry redirects a blob to, must be allowed by the silo's
  `egress`. The pull is made through the gateway's egress proxy
  as the silo, so a pull and an in-silo `curl` are held to the same
  rules and audited the same way.
- **Registry credentials** come per request in `X-Registry-Auth`, are
  used for that pull, and are never stored or logged.
- **One content store, records per silo.** Blobs are stored once, by
  digest, in `silo-containerd`'s content store, and an image another
  silo pulled isn't fetched again. Image **records** (names, tags) are
  per silo, in containerd's namespace `silo-<id>` (call L7). A silo
  lists only what it pulled or tagged.
- **Charging.** A pull charges the silo the image's full unpacked size
  in `disk_gb`, whether or not the blobs were already on disk. The
  sharing is silod's saving, not a silo's discount, so the hard
  partition stays exact.
- **The cost of sharing:** a pull that finishes too fast shows that
  another silo pulled the same image. That is a timing channel between
  silos ([§14](#14-limitations)), and turning sharing off for a
  subtree is [open question 7](#open-questions).
- **Wrapped images for `vm`** are made at first use of an image in the
  `vm` class. They are keyed by the image's digest and the pinned
  granule version, shared the same way, and charged the same way.
- **`/images/json` shows only the tenant's image**, never the wrapped
  one.
- *Part 4:* every pull now goes through the pull-through cache's
  fetcher, which applies the egress rule above to cache hits as well
  as fetches, and revalidates private images with the caller's
  credentials. Pulls from `registry.silo.internal` skip egress and need
  read access instead ([`local-registry.md` §8](local-registry.md#8-the-pull-through-cache)).

### Networks

- **A Docker network is a subnet of the silo's blocks** (allocator
  rules: [`allocators.md`, Blocks and subnets](allocators.md#blocks-and-subnets)).
- **Each network is its own Linux bridge** in `silo-net`
  ([§9](#9-the-local-network-fence), call L8).
- **`Driver` is `bridge`, or empty.** `overlay`, `macvlan`, `ipvlan`,
  `host`, `none` and plugins are refused, except `none` as a
  container's `NetworkMode`, which is served: a container with only
  its loopback.
- **IPAM:** `Subnet` must lie in the silo's free blocks (400
  otherwise, naming the blocks). When it is absent, silod carves the
  smallest free `/24`, or the largest free block if no `/24` is free.
  `Gateway` is always the subnet's first address, and silod refuses
  any other (400): the bridge holds it, as the hop to the gateway.
  `IPRange`, `AuxiliaryAddresses`, `EnableIPv6`, `Internal: false`
  with options, and every `Options` key are refused. IPv6 is
  [later](README.md#9-later).
- **`Internal: true`** is served: that network gets no route to
  `silo.internal` either, so its containers reach only one another.
- **The default network.** A silo has a network named `bridge` from
  its first container create that needs one, which is Docker's default.
  It can't be deleted while containers use it, and is made again when
  needed.
- **Between networks of one silo, traffic is dropped,** as Docker
  drops it between user-defined networks. A container on two networks
  talks on each. That is the tenant's policy, evaluated after the
  fence, as GCE's `goto_next` hands intra-silo traffic to the tenant's
  rules (call 10).
- **`connect` with `IPAMConfig.IPv4Address`** must lie in the
  network's subnet and be free. MAC addresses are silod's choice, and
  `MacAddress` is refused, because anti-spoofing pins it.

### Volumes

- **A volume is a directory** on silod's data filesystem,
  `/var/lib/silo/volumes/<silo id>/<volume id>/`, with a project quota
  (XFS or ext4 project quotas) at the size the silo is charged (call
  L9).
- **`DriverOpts` are refused**, except `size` (silo-specific, default
  10 GiB, charged to `disk_gb` at create).
- **In a sandbox,** the volume is a bind mount in the OCI spec, served
  through gVisor's gofer. The sandbox never sees the host path, only
  the tree the gofer exposes (gVisor Filesystem guide, "Directfs").
- **In a VM,** the volume is a virtio-fs share. silod runs one
  `virtiofsd` per mounted volume, sandboxed (`--sandbox=namespace`,
  seccomp on), and passes granule its socket in `CHV_VOLUMES`. granule
  has a `Filesystem` (tag, socket, shared directory, read-only) in
  `pkg/config`, but doesn't start `virtiofsd` itself.

  *Unverified:* whether a wrapped container sees the share inside its
  chroot. `granule-container` would have to mount the tag under
  `RootfsDir` at the volume's container path (plan item 9).

  The alternative was a raw disk image per volume through granule's
  `CHV_EXTRA_DISKS`. That isn't shareable with a sandbox, because
  gVisor can't mount block filesystems (Compatibility guide), and not
  between two running VMs either.
- **Anonymous volumes** from an image's `VOLUME` are made per
  container, as Docker makes them. `docker rm -v` removes them.
- **`tmpfs` mounts** are served, sized against the container's memory.

### Published ports

`-p 8080:80`, `-P` and compose `ports:` don't open a host port (call
L10). A published port is:

- **Inside the silo:** a listener on `docker.silo.internal:<HostPort>`,
  answered only for callers from **this** silo. The gateway knows the
  origin and forwards to the container's address and port. Two silos
  can both publish 8080, because the listener tells them apart by
  origin. `HostPort` empty or `0` gets a free port from
  `32768–60999` in the silo's own port space, as Docker does.
  `HostPort` 2375 or 53 is refused, as those are the facade and DNS.
- **On the host, only if the silo's `ingress` policy allows it.**
  `ingress` is a new policy, never-looser, with values `host` or
  `none`. The root's is `host` and a child's defaults to `none`. With
  `host`, the gateway also listens on `127.0.0.1:<HostPort>` in the
  host's namespace, as a **proxied export to the front door**: it is
  audited, closed when the container stops, and refused (409) if the
  host port is taken. It is never bound to anything but loopback.
- **To other silos:** an ordinary export, through the fence API.
  Publishing never makes one.

Inspect reports the port as Docker would: `HostIp: "0.0.0.0"` and the
`HostPort`. A client that connects to its `DOCKER_HOST` host at that
port reaches the container from inside the silo. From the host, it
needs `ingress: host`.

### DNS

- **The gateway answers DNS** at `silo.internal`'s address, port 53,
  in `silo-net`. Every container's `/etc/resolv.conf` names it. For
  `vm`, the address is passed to granule's `-dns` (`pkg/cli/vm.go`),
  which puts it on the guest's boot parameters.
- **It answers for:**
  - `silo.internal`, `docker.silo.internal` and
    `<silo id>.docker.silo.internal`;
  - the caller's own containers, by name, network alias and
    compose service alias, on the networks the caller's container
    shares with them. This is Docker's embedded DNS, which compose
    relies on.
- **Everything else is `NXDOMAIN`.** That includes other silos' names
  and every external name. External names are resolved by the egress
  proxy, which is the only way out
  ([`gateway.md`, Egress proxy](gateway.md#egress-proxy)).
- **Names are scoped by origin,** so one silo's names are invisible to
  another. That is the local answer to the GCP backend's open
  question 6, which doesn't arise here.

### What `/info` says

`/info` and `/system/df` describe the calling silo:

- `NCPU` and `MemTotal` are its quota, not the host's;
- the counts are its own containers and images;
- `Runtimes` is its allowed and available classes;
- `OperatingSystem` is `Silo (local backend)`;
- `ServerVersion` is silod's version;
- `Swarm.LocalNodeState` is `inactive`;
- `SecurityOptions` lists `seccomp` and `no-new-privileges`.

Host facts (kernel version, hostname, host CPU and memory, the other
silos' counts) are left out or replaced. `KernelVersion` is the
class's: `gVisor` for sandbox, the guest kernel's for vm.

### Names, ids and errors

- **Names are the tenant's own.** Container, network and volume names
  are unique per silo, not across the host, because each silo has its
  own namespace in silod's database and in containerd (call L7). This
  is better fidelity than the GCP backend's `<id>-` prefix (SV-76),
  which exists only because GCE has one flat namespace per project.
- **Ids** are 64 hex characters that silod generates. The runtime's own
  ids are never shown. A prefix is resolved within the silo only.
- **Errors use Docker's shape**, `{"message": "…"}`, with Docker's
  status codes:

  | Code | When |
  | --- | --- |
  | 400 | Invalid input, or a field outside the allowlist |
  | 403 | A fence or policy refusal: privilege, host resources, an isolation class, control policy |
  | 404 | Not found, including another silo's objects, by every name form |
  | 409 | Conflict: the name is in use, the port is taken, the class is unavailable on this host |
  | 501 | Outside the subset |
  | 503 | Swarm, or silod's store unavailable |

  A control-policy refusal says `silo: this silo's control policy is
  read; …`, matching the facade 403 the GCP backend gives
  ([`gateway.md`, Layer 1](gateway.md#layer-1-the-gateway-and-silod-refuse)).

### Quota charges

| Call | Charges |
| --- | --- |
| `containers/create` | `containers` 1 and `disk_gb` (the writable layer's cap) while it exists |
| `start` | `vcpu_m` and `memory_mb`, until it stops or exits. A start over quota is 409 `silo: quota exceeded: memory_mb …`. |
| `images/create` | `disk_gb`, the unpacked size |
| `volumes/create` | `volumes` 1 and `disk_gb`, its `size` |
| `networks/create` | `networks` 1, plus the address space the subnet takes from the silo's free blocks |

Charging works as the ledger does today: charge, commit, call the
runtime, and refund on failure ([`allocators.md`, Charging and
refunds](allocators.md#charging-and-refunds)).

## 5. Compose and testcontainers

### Docker compose v2

With the subset above, compose's `up`, `down`, `ps`, `logs`, `exec`,
`run`, `stop`, `start`, `restart`, `pull`, `rm` and `top` work. They
use networks with aliases, named volumes, labels, `depends_on`, and
healthchecks. This is what must work in each class (call L11):

| Flow | `sandbox` | `vm` |
| --- | --- | --- |
| `compose up -d` of a web app, a database and a cache on the project network, services finding each other by name | Must work | Must work |
| `compose logs -f`, and `compose up` in the foreground (attach) | Must work | Must work once the logs gap is closed. Until then it shows console lines too. |
| `depends_on: condition: service_healthy` | Must work: silod runs `HEALTHCHECK` with the driver's exec, and reports `State.Health` | Must work once exec enters the wrapped container |
| `compose exec` | Must work | Once exec enters the wrapped container |
| `compose down -v` | Must work | Must work |
| `ports:` reachable from inside the silo at `docker.silo.internal` | Must work | Must work |
| `build:` | Refused (501). Use `image:` with a pre-built image. | Refused |
| `volumes:` with a host path (`./data:/data`) | Refused (403). Use a named volume. | Refused |
| `network_mode: host`, `privileged`, `cap_add`, `devices` | Refused (403) | Refused |

Running a healthcheck is the daemon's job in Docker, so it is silod's
job here, using the same exec path. A container with a `HEALTHCHECK`
and no working exec (a `vm` container before plan item 9) is reported
`unhealthy` with a message saying why. It is never reported `healthy`.

### testcontainers

testcontainers (Go, Java, Python, Node) uses `/info`, `/version`,
image pull, create with labels, start, inspect for the mapped port,
logs, exec, `docker cp`, stop and remove. It also starts **Ryuk**, a
reaper container that **bind-mounts the Docker socket**. That bind is
refused, so in a silo testcontainers runs with
`TESTCONTAINERS_RYUK_DISABLED=true`. The silo is the reaper: a CI job
in a child silo is cleaned up when that silo is deleted. Serving
"the Docker socket" inside a container as that silo's own facade is
[open question 5](#open-questions).

What must work, with the test process **inside the silo** and
`DOCKER_HOST=tcp://docker.silo.internal:2375`:

- **Generic container with a mapped port.** testcontainers connects to
  `docker.silo.internal:<mapped port>`, which is a published port of
  the caller's silo. Works in both classes.
- **Log wait strategy.** Both classes, once the `vm` logs gap is
  closed.
- **Port wait strategy.** testcontainers runs a shell check inside the
  container with exec, then connects from outside. `sandbox`: works.
  `vm`: works once exec enters the wrapped container. Until then, use a
  log or HTTP wait strategy.
- **`copyFileToContainer` before start.** Both classes, through
  [Archive](#archive-docker-cp).
- **Modules (Postgres, Redis, Kafka and the like).** `sandbox` must
  pass the Go and Java modules for Postgres, MySQL, Redis and Kafka:
  gVisor's Compatibility guide lists the language runtimes tested in
  its releases, and nothing those images need is in its known gaps.
  `vm` must pass the same set once plan item 9 lands.

With the test process **on the host** and the per-silo Unix socket,
mapped ports are reachable only if the silo's `ingress` is `host`
([Published ports](#published-ports)). Its testcontainers then
connects to `localhost:<port>`.

### What gVisor won't run

These are gVisor's documented gaps (Compatibility guide and its syscall
table). They are ABI gaps, not fence gaps. A workload that needs one
gets the `vm` class:

- kernel modules (`init_module` and `finit_module` are unimplemented);
- `perf_event_open` (unimplemented);
- eBPF beyond `CGROUP_DEVICE` programs, which load and have no effect;
- `userfaultfd` (unimplemented);
- `io_uring` (off by default, basic operations only);
- partial `iptables` and `nftables`;
- block-device filesystems (ext4, FAT) mounted inside the sandbox;
- custom device files;
- KVM inside the sandbox;
- resource limits *within* a sandbox. They are enforced by the host
  cgroup around it, which is how silod enforces them.

**Docker inside a container** ("Docker in gVisor" is a gVisor
tutorial) needs capabilities the facade refuses, so in both classes it
is refused in v1. A tenant that needs Docker uses its own silo's
facade instead, which nests by design: a child silo is the
"container in a container".

## 6. Runtime drivers

### The substrate: a private containerd (call L12)

Both drivers run on **one containerd that silod owns**:

- It runs as `silo-containerd.service`, with root
  `/var/lib/silo/containerd`, state `/run/silo/containerd`, and a
  socket readable only by root.
- It is separate from any Docker or containerd the host already runs.
  It shares no images, namespaces, iptables rules or sockets with
  them.
- containerd is the right layer because:
  - **One image store** serves both classes. That is the sharing [§11](#storage-layout-call-l21) wants.
  - **Namespaces** give each silo its own image and container
    records.
  - The **shims outlive containerd and silod**, which is what makes
    [adoption](#restart-and-adoption) possible.
  - **The OCI spec carries the network namespace path.** silod creates
    and wires each container's netns before the container exists, as
    CNI does, so a container never runs without its anti-spoofing in
    place.
  - gVisor documents `containerd-shim-runsc-v1` as its containerd
    integration (containerd quick start). granule images are ordinary
    OCI images that "run natively under standard runc, containerd"
    (granule README, "Key Architectural Properties").

A private `dockerd` was the other choice, with silod translating
requests into it. It would give logs, attach and events for free. But
dockerd creates a container's network namespace only at start, and
manages its own iptables. silod would then wire anti-spoofing after the
workload was already running, and share the host's netfilter with
Docker's rules. The facade would also drift towards forwarding to
dockerd, which is the pass-through that principle 4 forbids.

### The driver interface

Inside `pkg/backend/local`, a **driver** is one isolation class. Both
drivers implement the same interface, `runtime.Driver`:

| Method | What it does |
| --- | --- |
| `Class()` | `vm` or `sandbox` |
| `Probe(ctx)` | Whether the host can run this class now, and why not ([Detection](#detection)) |
| `Prepare(ctx, image)` | Make the image runnable in this class: nothing for `sandbox`, the wrap for `vm`. Cached and idempotent. |
| `Create(ctx, spec)` | Make a container from a `Spec`: image digest, process (argv, env, user, workdir, TTY, stdin), resources, mounts (volumes, tmpfs), netns path, labels, stop signal. It does not start it. |
| `Start`, `Stop(timeout)`, `Kill(signal)`, `Pause`, `Resume`, `Delete` | Lifecycle |
| `Wait(ctx)` | The exit status, as the workload's process reported it |
| `Exec(ctx, process)` | A new process in the container, with streams, TTY resize and exit code |
| `Streams(ctx)` | The workload's stdout and stderr (and stdin, where `OpenStdin`), apart from any console |
| `Stats(ctx)` | CPU, memory, I/O and process count, from the container's cgroup |
| `CopyIn`, `CopyOut` | Archive, in the container's own root |
| `List(ctx)` | Every container this driver has in `silo-containerd`, with silod's labels, for adoption |

A `Spec` is backend-neutral. It never contains a host path the tenant
chose, a capability, a device or a namespace other than the one silod
made: the facade can't express them, and the driver refuses them
again (belt and braces). The **fake driver** in
`pkg/backend/local/runtime/fake` implements the same interface in
memory, for fast tests ([§13](#13-testing-and-the-conformance-split)).

### gVisor driver

- **Runtime.** containerd's `io.containerd.runsc.v1` with the pinned
  `runsc`. Flags: `--platform=systrap`, `--network=sandbox`,
  `--overlay2=root:self` with a size cap (gVisor's `size=` option on
  the overlay medium) at the container's `disk_gb` charge,
  directfs on. `--platform=kvm` is not used, even on KVM hosts: gVisor
  recommends systrap inside VMs and KVM only on bare metal ("Changing
  Platforms"). A KVM host offers the `vm` class anyway.
- **Network mode: netstack** (call L13). gVisor's own network stack
  runs inside the Sentry and writes link-layer packets to the device in
  the container's netns ("Networking"). The alternative,
  `--network=host` passthrough, "decreases the isolation to the host"
  in gVisor's own words: the application would talk to the host
  kernel's network stack, which is exactly the surface gVisor exists to
  hide. The external network proxy mode is experimental. With netstack,
  the sandbox's packets leave through the veth that silod made and
  pinned, so anti-spoofing doesn't depend on the Sentry behaving.
- **Exec, logs, signals and pause** go through containerd's task API,
  which `runsc` implements ([table above](#exec-logs-and-attach)).
- **Volumes** are bind mounts in the OCI spec, served by the gofer.
- **Resource limits** are the host cgroup around the sandbox. gVisor
  says in-sandbox cgroups account but don't enforce, and that the host
  cgroup does.
- **Checkpoint/restore** is available only as raw `runsc checkpoint`
  and `restore` (Checkpoint/Restore guide), so the driver doesn't offer
  it in v1.

### granule driver

- **Runtime.** containerd's runc shim, running the wrapped image with
  the device and capability set granule asks of Docker
  ([Containers in each class](#containers-in-each-class)), and these
  settings:
  - `CHV_NET_MODE=inline`, so that netshim takes over the container's
    veth address, MAC and MTU ("Network Transparency" in granule's
    README);
  - `-dns` set to the gateway;
  - `-cpus` and `-memory-mb` from the container's resources;
  - `-disk-mode overlay` and `-disk-size-mb` from its disk charge;
  - no `-publish-declared`: publishing is silod's.

  The granule supervisor runs unprivileged inside its own netns, with
  `NET_ADMIN` only there.
- **Not through the `granule` CLI.** `granule vm create` execs `docker`
  (`GRANULE_DOCKER`) and only knows the Docker CLI's dialect. silod
  builds the same container spec for containerd itself, from granule's
  packages and documented environment (`CHV_*`, `NETSHIM_*`). Each
  granule version silod pins comes with a test that compares silod's
  spec with what `granule vm create` passes to Docker, through
  granule's fake docker (`pkg/guestboot/testdata/fakedocker`).
- **Wrapping needs a builder.** `pkg/guestwrap` generates a Dockerfile
  whose one `RUN` runs the guest image's shell, then runs
  `<docker> build`. silod runs a private `buildkitd` with the
  containerd worker for this, and nothing else. *Unverified:* whether
  `guestwrap` runs against `buildctl` without the docker CLI. If it
  doesn't, plan item 9 asks granule for a daemonless wrap. Nothing in
  a wrap executes the tenant's image, so a wrap is not tenant code
  running on the host.
- **Snapshot.** granule has the primitives: `Pause`, `Resume` and
  `Snapshot` on cloud-hypervisor's API (`pkg/hypervisor/client.go`),
  restore at VMM launch from `CHV_SNAPSHOT_PATH`, and the overlay
  saved with the snapshot (`cmd/granule-shim/main.go`, `RestoreDiskOverlay`).
  It has no cross-host or golden-image restore
  (`docs/agent-substrate-uvm-runtime.md`, mismatches 3 and 4). v1
  doesn't expose snapshots through Docker (`docker checkpoint` is
  experimental in Docker too). Fast agent tool VMs from a warm snapshot
  are [later](#later).
- **The gaps listed above** (exec into the wrapped root, the workload's
  streams apart from the console, the workload's exit code, stdin,
  virtio-fs inside the chroot, per-container spec overrides, a
  daemonless wrap) are granule changes. They are plan item 9, filed
  against gke-labs/granule. Until they land, the `vm` class runs and
  is fenced, but `exec`, health checks and exit codes are degraded as
  described.

### Snapshot and checkpoint

Docker's `checkpoint` endpoints stay refused in both classes in v1.
Neither runtime's checkpoint is wired through containerd in a form
silod could charge, audit and restore across silod restarts.

## 7. The backend interface under silod

### What silod's backend-neutral parts call today

They reach GCE directly through `gce.Client`
(`pkg/backend/gcp/gce/client.go`):

| Caller | What it uses GCE for |
| --- | --- |
| `pkg/reconcile` | Lists every object in the allowed regions, sorts it by silo label or `<id>-` prefix, deletes orphans (`DeleteInstance` and friends), checks which objects exist for dropping charges, and runs the fence |
| `pkg/fence` | `Compile` builds the global network firewall policy from `fence.Input`. `Applier` applies it rule by rule (`networkFirewallPolicies`). `Plan` orders the steps. `CheckBudget` is GCE's rule budget. |
| `pkg/ledger/rootcap` | Reads Cloud Quotas, regions, zones, accelerator types and the control plane's own instances to set the root's capacity |
| `pkg/ledger` (`classes.go`, `prices.go`) | GCE's quota classes and its price table, for preview |
| `pkg/fenceapi` | Already through interfaces: `FenceApplier`, `FenceBudget`, `SiloDeleter` (a "later task's"), `InstanceResolver` (instance targets of exports), `ToolVMs` and `ToolVMIdentifier` (agents) and `Pricer`. silod's `main.go` wires each to GCE. |
| `pkg/install` | Entirely GCP |

`fence.Input` (silos with depth, blocks and `Sealed`, grants as
directional rules with L4 ports, plus the gateway and control ranges)
is already backend-neutral. Only `Compile` onward is GCE's.

### The interface (call L14)

A new package, `pkg/backend`, holds what silod needs from **any**
backend. It is a small set of interfaces, each with one implementation
per backend:

| Interface | Methods | GCP | Local |
| --- | --- | --- | --- |
| `Inventory` | `List(ctx) ([]Object, error)`, where an `Object` has a kind, scope, name, owning silo id (from label, prefix or namespace) and whether a delete is pending; `Delete(ctx, Object)` | Today's `reconcile.list` and `reconcile.delete` | containerd namespaces, silod's netns and bridges, volume directories, nftables sets |
| `Fence` | `Apply(ctx, *fence.Input) error`, `Budget(ctx, *fence.Input) error`, `Drift(ctx) ([]string, error)` | `fence.Compile`, `Plan`, `Applier`, moved under `pkg/backend/gcp/gce/gcefence` | The nftables compiler ([§9](#9-the-local-network-fence)) |
| `Capacity` | `Root(ctx) (Capacity, error)`: classes and limits, regions, the control plane's own use | `pkg/ledger/rootcap`, moved | Host CPU, memory and disk, less the reserve, times the overcommit ([§11](#11-resources-and-quota)) |
| `Classes` | The quota classes this backend charges, and their prices for preview | GCE's (`ledger/classes.go`, `prices.go`) | `vcpu_m`, `memory_mb`, `disk_gb`, `containers`, `networks`, `volumes` |
| `Contents` | `DeleteContents(ctx, silo)` | Deletes the silo's labelled GCE objects | Stops and removes containers, networks, volumes and image records in `silo-<id>` |
| `Targets` | `Resolve(ctx, silo, name) (netip.Addr, error)` for an export's named target | Instance by zone and name | Container by name |
| `ToolVMs` | Today's `fenceapi.ToolVMs` and `ToolVMIdentifier` | The GCE facade's | A tool container, in the agent policy's class, through the Docker facade's internal path |

The rest of silod sees only these: the ledger, the tree, grants,
exports, links, the gateway's origin map (which comes from the address
ledger, not the backend), agents, the model proxy and audit. `Object`
uses the backend's own collection names (`instances`, `containers`)
only as strings for audit.

As built in step 1 (task 3404), the interfaces follow what silod
calls today rather than the sketch above where the two differ.
`Inventory.List` takes the root's allowed regions and returns the
ledger prefixes it listed completely, each `Object` carries the ledger
object it is charged as, and `Inventory.Pending` stands in for "whether
a delete is pending" (the facade's operations not yet done). `Fence` is
the applier's own surface: `WaitApplied`, `CheckBudget`, `Run`, and
`Repair` for drift. `Capacity` is `Refresh`, which reads and sets the
root's capacity as `rootcap` does today. `Targets.Resolve` takes a
scope (a zone on GCP). GCP's `Contents` stays nil until refactor item 5.

As built in step 2 (task 3406), the GCE half of the fence is
`pkg/backend/gcp/gce/gcefence` (named like `gceclient` and `gcetest`,
so that callers can import it beside the neutral `pkg/fence`). It
holds `Compile`, `Plan`, `Applier`, the rule budget (`Quota`,
`Budget`, `Measure`) and `Config`, and every test of the fence,
`links_test.go` included, since each one runs through the applier and
GCE's fake. `pkg/fence` keeps `Input`, `Silo`, `Grant` and `L4`, the
range arithmetic (now exported: `Normalize`, `Union`, `Intersect`,
`LastAddr`), the grants, `Load` and the applied sequence number
(`Applied`, `SetApplied`, `LastSeq`). `rootcap` moved whole to
`pkg/backend/gcp/gce/rootcap`; the ledger's class table coming from
`Classes` is left for a later step.

### The refactor

The refactor changes no behaviour, and lands before any local code
(plan items 1 and 2):

1. **Introduce `pkg/backend`.** Make `reconcile.Options` take
   `backend.Inventory` and `backend.Fence` instead of `gce.Client` and
   `*fence.Applier`. The GCE implementations wrap today's code.
   `reconcile.Object.Collection` becomes `Kind`.
2. **Split `pkg/fence`.** `Input`, `links.go`, `ranges.go` and the
   database reads stay as the neutral part. `compile.go`, `plan.go`
   and `apply.go` move to `pkg/backend/gcp/gce/gcefence`, and so do their
   tests, including the property test against `fenceeval`.
3. **Move `pkg/ledger/rootcap`** under `pkg/backend/gcp/gce` as its
   `Capacity`. Make the ledger's class table come from `Classes`.
4. **Give `cmd/silod` a `--backend` flag.** It builds one backend and
   hands its interfaces to `fenceapi.Options`, the reconcile, the
   leader and the gateway. The wiring that `main.go` and `leader.go`
   now do with `gce.Client` moves behind `backend/gcp`.
5. **Implement `SiloDeleter`** for GCP with `Contents`, which today is
   nil ("a later task's").

`pkg/backend/local` then gets:

- `local.Backend`, which implements the interfaces above;
- `local/runtime`, the driver interface, with `gvisor`, `granule` and
  `fake` drivers;
- `local/netfence`, the nftables compiler and applier, and its
  evaluator for tests;
- `local/host`, the probes and the capacity reader.

This follows [`facades.md`](facades.md#where-new-ones-go): the
backend's real implementation sits in sub-packages named for what they
wrap, with a fake and behaviour tests beside it.

## 8. Extracting the facade core

[`facades.md`](facades.md#the-pipeline-every-facade-shares) names the
seven steps every facade takes, and holds the extraction back until a
second facade exists. This page is that second facade, so the
extraction is designed here, from both users (call L15).

### What moves to `pkg/facade`

| Step | Shared code | What each service supplies |
| --- | --- | --- |
| 1. Resolve the target silo | `facade.Resolve(ctx, tx, caller, targetRef) (Target, error)`: the target's own silo, descendants (`via=ancestor:<id>`), `manage` grants for the resource types their scope names (`via=manage:<holder>`), and the same refusal for "no such silo" and "not yours" | How the request names its target: GCE's project in the path (`silo-<id>`, or the host project as an alias); Docker's socket or `Host` header (`<id>.docker.silo.internal`) |
| 2. The control policy | `facade.CheckControl(target, class)`, using `pkg/control`'s classification | Each endpoint's class (`read` or `mutate`) in the method table |
| 3. The method table | `facade.Table`, `facade.Method`: lookup, refusal of unknown methods, and resource types that are unknown versus not found | The table itself (`tables.go` per service) |
| 4. Location | Nothing shared | GCE checks the region or zone against allowed regions. Docker has no location: the local backend has one region, `local`, which every silo has. |
| 5. Body allowlists | `facade.CheckFields(body, allowlist)` over JSON: a field outside the list that isn't at its default is refused; labels in Silo's reserved namespaces are refused | The allowlists, and where labels live in the body (GCE `labels` and tags; Docker `Labels` on containers, networks and volumes) |
| 6. Call the backend, build the response | `facade.Respond(fields)`: copy out only allowlisted response fields | The handler, and its naming (GCE maps links to the tenant's project; Docker strips silod's labels and maps ids) |
| 7. Audit | `facade.Audit(tx, target, via, method, object, charges)`: one `facade.call` record per write, accepted or refused once the silo is known, with an extra `facade` field naming the service (`gce`, `docker`) | Nothing |

### What stays per service

[`facades.md`, What stays per service](facades.md#what-stays-per-service)
holds for Docker too:

- **handlers;**
- **allowlist tables;**
- **long-running work:** GCE's operations, and for Docker,
  synchronous calls plus `/events` and `/wait`;
- **lists and filters:** GCE's `filter/`, and for Docker the `filters`
  JSON parameter with `label`, `name`, `status`, `ancestor`,
  `network` and `volume`, applied by the facade after it lists only
  the silo's own objects, as call 26 does for GCE;
- **error shapes:** GCE's `googleapi` errors, and Docker's
  `{"message"}`;
- **path parsing and naming.**

### The order of the work

Extract first, behind the GCE facade, with no behaviour change. GCE's
facade tests are the proof (plan item 2). Then write
`pkg/facade/local/docker` against the extracted core (plan item 6).
Whatever the Docker facade needs and the core lacks goes into the core
in that task, not into a Docker-only copy.

## 9. The local network fence

### Topology (call L8)

- **`silo-net`** is one network namespace that holds every tenant
  network. It has:
  - **`gw0`**, a dummy interface with the gateway's addresses:
    `silo.internal` (10.255.255.1 by default) and `docker.silo.internal`
    (10.255.255.2), in the gateway range install reserves, as in
    [`state.md`](state.md#the-control-subnet);
  - **one Linux bridge per Docker network**, `sb-<network id>`, holding
    the network's first address;
  - **one veth pair per container per network**: the host end on the
    bridge in `silo-net`, the other end in the container's own netns,
    which silod creates before the container exists.
- **`silo-net` has no interface to the host's root namespace** (call
  L16). Nothing routes between them. The gateway, the egress proxy, DNS
  and published-port listeners are silod's sockets, **opened inside
  `silo-net`** (`setns` on a locked OS thread before `socket()`), and
  the egress proxy dials out from the root namespace. The proxies are
  the only things that carry traffic between the two. *Rootless:* the
  locked-thread `setns` works only for a root silod. In part 2, the
  namespace holder opens these sockets in `silo-net` and passes them to
  silod, and dials into `silo-net` come from a pool of sockets made
  there ([call L28](local-rootless-storage.md#sockets-across-the-two-network-namespaces-call-l28)).
- **Forwarding is on in `silo-net`, and only there.** It is what lets
  traffic go between bridges, through the fence: between networks, to
  the gateway and over links. It never reaches the host or the
  internet, because `silo-net` has no route to either.

So a rule bug inside `silo-net` can at worst expose silos to one
another. The host's own services and the host's networks (its LAN, a
cloud metadata server at `169.254.169.254`, `localhost` services) are
not reachable at all, because there is no route.

### Addresses

- **Addresses come from the same ledger as in the GCP backend.** The
  root's address space is install's `--address-space` (default
  `10.200.0.0/16`), minus the gateway range. A child's blocks are
  carved from its parent's blocks (call 30). A Docker network's subnet
  is carved from its silo's free blocks
  ([Networks](#networks)).
- **The address space doesn't have to avoid the host's networks**
  (it lives in its own netns). Install still warns when it overlaps the
  host's routes, because people reading addresses get confused.
- **A container's address** is the next free address in its network's
  subnet, or the one `IPAMConfig` named. silod records it in the
  ledger, so origin identity (source IP → silo) works as it does
  today.

### Anti-spoofing

- **Every container's veth is pinned to one MAC and one IPv4
  address**, which silod chose. The rules live in a `bridge silo`
  nftables table: one set of (port, MAC, IP) entries, and a rule per
  bridge that drops a frame from a port whose:
  - Ethernet source isn't the pinned MAC;
  - IPv4 source isn't the pinned address (except `0.0.0.0` for DHCP,
    which nothing uses and is dropped anyway);
  - ARP sender MAC or IP isn't the pinned pair;
  - EtherType isn't IPv4 or ARP. IPv6, LLDP and VLAN tags are all
    dropped.

  A port with no entry in the set drops everything. silod adds the
  entry before the container starts. Container start waits for the
  ruleset to be applied ([Applying](#applying)), so a workload never
  has an unpinned port.
- **For `sandbox`**, the pinned veth is the one whose far end gVisor's
  netstack writes to. A compromised Sentry still sends through it.
- **For `vm`**, granule's netshim takes over the veth's address and MAC
  inside the container's netns and splices `tap0` to it (granule
  README, "Network Transparency" and "Under the Hood"). So the TAP is
  **inside** the container's netns, and the host-side boundary is still
  the veth. A guest, or a compromised supervisor with `NET_ADMIN` in
  its own netns, can change what it sends, but not what the bridge
  port accepts. granule's control link (`-control-cidr`, link-local)
  stays inside the container's netns.
- **Unknown MACs aren't learned into reachability.** The bridge
  forwards by learned MAC as usual. Since every frame's source MAC is
  pinned, learning can't be poisoned.

### Rules (the `inet silo` table)

These are base chains in `silo-net`, with policy `drop`. The order is
fixed and written by silod alone:

1. **`prerouting`**: drop a packet whose input interface's network
   doesn't own its source address. This is the routed half of
   anti-spoofing, and it also catches a pinned address arriving on the
   wrong bridge.
2. **`input`** (to `gw0`'s addresses, which is the gateway). It
   allows, by source silo (an nftables set of each silo's blocks):
   - **For everyone:** DNS 53/udp+tcp; HTTP exports 80; egress
     3128; TCP export ports (20000–29999 by default); and
     published ports on `docker.silo.internal`, all ports except 2375
     and 53.
   - **For silos whose effective control policy isn't `none`:** the API
     ports, 8080 on `silo.internal` and 2375 on `docker.silo.internal`.
     There is one set, `api_ok`, and sealing a silo removes its blocks
     from it ([Sealed silos](#sealed-silos-and-never-looser)).
   - **Nothing else.** `Internal: true` networks are left out of the
     allow rules entirely.
3. **`forward`**, in order, as in the GCP fence (calls 9 and 10):
   1. **Grants first:** for each L3 link, accept the named protocol and
      ports between the two silos' blocks, in each agreed direction.
   2. **Intra-silo, same network:** never forwarded, because it is
      switched on the bridge and never routed.
   3. **Intra-silo, different networks:** drop, which is Docker's
      semantics. A container on both talks on each directly.
   4. **Everything else:** drop. There is no NAT, no masquerade and no
      default route out of `silo-net`.
4. **`output`** (silod's own sockets in `silo-net`): accept to the
   silos' blocks only from the gateway's addresses, and drop the rest.

Per-silo state is all in **sets** (blocks, sealed-ness, links). A
change to the tree or a grant is a set update, applied as one atomic
`nft` transaction.

### Applying

- **The compiler** turns `fence.Input` into one nftables ruleset: the
  chains above, plus every set's full contents.
- **The applier** loads it with `nft -f` as a single transaction, so
  the kernel switches from the old ruleset to the new one atomically.
  It records the applied sequence number as `gcefence.Applier` does, so
  `WaitApplied` works unchanged.
- **Drift** is a ruleset in the kernel that differs from the one last
  applied. The reconcile finds it every 10 minutes, and on start, by
  listing the table. It repairs it and audits `fence_drift`, as today.
- There is **no rule budget** of GCE's kind. `Budget` refuses only at a
  configured cap on set sizes, which defaults to the size of the
  address space.

### The gateway, egress and the model proxy

- **Workloads reach the gateway at `silo.internal` and
  `docker.silo.internal`.** These are routed through their bridge's
  first address, which is each container's default route.
- **Origin identity is by source address**, as in
  [`gateway.md`](gateway.md#origin-identity). It is trustworthy for
  three reasons: the anti-spoofing above, `prerouting`'s interface
  check, and blocks that are disjoint across the tree.
- **Egress is the explicit `HTTPS_PROXY`**, the gateway on 3128, with
  per-hostname policy set by ancestors
  ([`gateway.md`](gateway.md#egress-proxy)). silod puts `HTTP_PROXY`,
  `HTTPS_PROXY` and `NO_PROXY` in every container's environment unless
  the create request sets them, so the image's tools pick them up.
- **The proxy refuses** link-local, loopback and the host's own
  addresses as destinations, as today ("addresses that aren't
  internal"). On a host that is itself a cloud VM, that includes the
  metadata server, which the proxy would otherwise be able to reach
  from the root namespace.
- **The model proxy** is served only on harness session sockets, as in
  [`agents.md`](agents.md#5-the-session-socket-how-a-harness-reaches-silod).
  No workload reaches it, and the gateway doesn't serve it.
- **Tool channels** (an agent's tool server dialling out to the
  gateway) work unchanged, inside `silo-net`.

### Grants: links and exports

- **L3 links** are the `forward` grant rules above: protocol and ports,
  between two silos' blocks, in each agreed direction, appearing and
  disappearing with the link (as [`fence-api.md`, Links](fence-api.md#links-l3)).
- **Proxied exports** are the gateway's as today. The exporter's
  target is a container's address and port (`Targets.Resolve` by
  container name), and the importer reaches it at
  `silo.internal`'s export port or URL path.

### Sealed silos and never-looser

- **A sealed silo** (`control: none`) is refused twice, as in the GCP
  backend ([README, Control policy](README.md#control-policy-and-sealed-silos)):
  1. The facade and fence API refuse every call (Layer 1).
  2. The silo's blocks are absent from `api_ok`, so `input` drops its
     packets to 8080 and 2375 (Layer 2). As in the GCP backend
     ([§8 Limitations](README.md#8-limitations)), a sealed workload's
     API calls time out rather than fail.
- **`read`** has only Layer 1, for the same reason as in the GCP
  backend: the fence can't tell a read from a write.
- **On the host,** a front-door member mapped to a sealed silo is
  stopped only by silod. The Unix sockets aren't behind the fence, so
  there is no Layer 2 there, as for the GCP front door.
- **Never-looser** is checked by the fence API, unchanged, for
  egress, links, exports, control, agents, and now `isolation` and
  `ingress`. The fence only compiles what the database holds, so a
  child the API refused can't appear in a rule.

### What a rule bug would expose

| Bug | Exposed |
| --- | --- |
| Anti-spoofing entry missing or wrong | Before the fix: none. A port with no entry drops everything. A wrong entry lets one container use another address **in its own network**. That address's silo could be another silo only if two silos shared a network, which the ledger never allows. The `prerouting` check catches the routed half. |
| `forward` grant too wide | Traffic between the two silos named in the grant, on more ports than agreed |
| `forward` drop missing | Any silo to any other silo, at L3. This is the subnet backing's "a bug in the rules is a breach", contained to `silo-net` |
| `input` too wide | The gateway's other ports to a silo that shouldn't have them: a sealed silo reaching the API port (Layer 1 still refuses), or an `Internal` network reaching egress (the proxy's policy still applies) |
| A route or interface into the root namespace (not a rule bug: a topology bug) | The host. This is why `silo-net` has no such interface and nothing in the plan ever adds one, and why the conformance suite checks for it ([§13](#13-testing-and-the-conformance-split)) |

The structural defence is that **the tenant fence and the host fence
are different mechanisms**. Silos are kept apart by rules. The host is
kept apart by having no route, which no rule can open.

### As built in step 5

Step 5 (task 3412) is `pkg/backend/local/netfence`. It differs from the
text above in these places:

- **The model.** The compiler emits a small typed `Ruleset`: sets with
  their elements, and base chains whose rules are lists of matches.
  `Render` writes it as one `nft -f` transaction, in which each table
  is created, deleted and defined again. `Eval` interprets the same
  model. The property test (`TestEvaluatorMatchesIntent`) checks the
  evaluator's verdicts against what this section says, read straight
  from `fence.Input` and the topology. Samples of its random rulesets
  are also loaded with the real `nft` in a scratch namespace.
- **Applying.** The applier runs `nft -f -` in `silo-net` with
  `nsenter -U -n` into the holder. silod's user owns the holder's user
  namespace, so no privilege is needed. It does not use the holder's
  passed `netfilter` socket, which would need an nf_tables encoder in
  silod. The holder's pid is read from `nsd.state` on every call.
  `silod serve --backend=local` wires it up. It needs `--data-dir`, so
  the store is that directory's SQLite database (call L22).
- **Drift.** Drift is a listing of the two tables that differs from
  the listing this silod took after its last load. A silod that has
  loaded nothing finds none. Its first apply, which `Run` makes at
  start, replaces whatever is there.
- **Rules beyond the list above:**
  - `forward` accepts `ct state established,related` first. GCE's
    firewall is stateful, and a link that allows one direction needs
    its replies.
  - An export's grant, from the gateway range, has no `forward` rule.
    The gateway's own sockets reach the target through `output`.
  - Every `input` rule also requires the input bridge to be in
    `external`, the set of bridges of networks that aren't `Internal`.
- **Grant sets.** The kernel refuses an element of a concatenated
  interval set whose ends both lie in another element. Two links over
  the same silos (one for `all`, one for `tcp`) would hit this. So
  grants are cut into disjoint boxes before they are rendered.
- **Topology.** Networks and pins come from a `TopologySource`. Until
  the Docker facade (step 6) records them, silod's is gw0's addresses
  alone (`--local-gateway`), so no bridge is let through.
- **`gw0`** is still the holder's bridge from step 15, not a dummy.
- **What `local.New` builds.** It builds the `Fence`, and `Inventory`,
  `Contents` and `Targets` over the runtime drivers (fake only, so
  far). It builds no `Capacity`, `Classes` or `ToolVMs`.

## 10. Agents on the local backend

bsalmon, on the task: **"this version should also assume that the agent
cli's will run on the local machine with ambient permissions"**, and
**"that the agent cli's are already installed on the host machine."**

What follows from that (call L17):

- **Harnesses run on the host,** not in a container per session
  ([`agents.md` §11](agents.md#isolation-on-the-control-vm)'s
  container doesn't exist here):
  - They are the `claude` and `codex` on the agent user's `PATH`.
  - silod doesn't install, pin or update them. It records each
    session's CLI version in the agent's status and audit.
  - An adapter refuses to start a CLI older than the oldest version
    whose flags it knows (`pkg/harness`'s adapters already pass
    `--tools ""` and an MCP config).
- **They run as the agent user, with that user's ambient
  permissions:** its uid, `HOME`, files, environment and logins (for
  example `~/.claude`, `~/.codex`, and API keys in its environment).
  - The agent user is configured at install (`--agent-user`, by default
    the user who ran `sudo silo install`).
  - It is never root. silod starts each session in a transient systemd
    scope, `silo-agent-<agent id>.scope`, under that uid.
- **silod holds no model credentials on this backend.** An agent's
  credential is `ambient`, a new credential kind: whatever login or key
  the CLI finds. That means:
  - **No credential store,** and no key in the database (call 45 has
    nothing to protect here).
  - **Metering is by reported usage:**
    [`agents.md` §7](agents.md#metering)'s subscription path, which reads
    the usage the CLI reports in its event stream after each turn and
    prices it at API list price (call 47). An ambient session can
    overshoot its budget by one turn, as a subscription session can.
  - **The model proxy is still there** for an API-key credential the
    operator adds explicitly, exactly as in `agents.md`. Ambient is an
    addition, not a replacement.
- **The harness's own tools stay off,** as on the GCP backend
  (`--tools ""` and the equivalent for Codex). The tools are the silo's
  MCP tools, and every command still runs in the agent's **tool
  container** inside its silo, in the class the agent policy requires.
  Code the agent runs is fenced. The harness process itself is not.
- **The session socket is the session's identity,** as in
  [`agents.md` §5](agents.md#5-the-session-socket-how-a-harness-reaches-silod).
  But every ambient session runs as the same uid, so a file mode can't
  keep one session off another's socket. silod therefore also checks
  the peer: `SO_PEERCRED` gives the connecting pid, and silod accepts
  the connection only if that pid's cgroup is the session's own scope
  (call L18). This is checked with a pidfd, so a reused pid can't pass.
- **What "ambient" costs, plainly:** a harness is a process running as
  the agent user. If a harness is exploited (crafted tool output that
  breaks the CLI), the attacker has that user's account. That covers
  its files, its logins, its SSH keys, and its membership of the root
  silo through `/run/silo/silo.sock`, if the agent user is also the
  operator. On the GCP backend, the same exploit gets one container.
  The silo fence still holds for everything the agent runs *through its
  tools*. It does not hold for the harness. Running harnesses as a
  dedicated user that is no silo's member, with copies of the logins,
  would narrow this. That is [open question 1](#open-questions), not
  the default, because the operator asked for ambient.
- **Placement and capacity:** one host, so
  [`agents.md` §10](agents.md#10-capacity-of-the-control-vm)'s
  placement is trivial. `agent_sessions` at the root is install's
  `--max-agent-sessions`, by default `max(1, host vCPUs / 2)`.
- **Restarts** follow [`agents.md`, silod restarts](agents.md#silod-restarts):
  - Session state (the CLI's session file) is saved after every turn.
  - On start, silod kills any leftover `silo-agent-*` scope, then
    resumes `RUNNING`, `IDLE` and `WAITING` agents.
  - Tool containers are adopted like any container
    ([§12](#restart-and-adoption)).
- **The tool container** replaces the tool VM. It is made through the
  Docker facade's internal path, in the agent policy's class, with
  `silo-tools` in it:
  - **`sandbox`:** `silo-tools` is a static binary that silod bind-mounts
    read-only. It is silod's own file, not a tenant path.
  - **`vm`:** a pre-wrapped tool image that silod builds at install
    (the README's open question 27, answered locally).

  The tool server dials the gateway, as today.

## 11. Resources and quota

### Classes (call L19)

| Class | Unit | Charged |
| --- | --- | --- |
| `vcpu_m` | thousandths of a vCPU | While a container runs: its `NanoCpus` (`--cpus`), or the class default (1 vCPU) when unset |
| `memory_mb` | MiB | While a container runs: its `Memory` limit, or the class default (512 MiB) when unset. For `vm`, the guest's memory plus granule's per-VM overhead (measured per granule version and recorded in silod's class table) |
| `disk_gb` | GiB | While they exist: containers' writable layers (`--storage-opt size=`, default 10 GiB), volumes, images (unpacked size), wrapped images |
| `containers` | count | While a container exists |
| `networks` | count | While a network exists |
| `volumes` | count | While a volume exists |
| `agent_sessions` | count | As in [`agents.md`](agents.md#10-capacity-of-the-control-vm) |

These are the local backend's `Classes`. Regions don't apply. The
root's allowed regions are `["local"]`, every silo has `local`, and
the fence API's region fields hold that one value.

Unlike GCE (call 6), a stopped container charges **no** CPU or memory:

- Docker users leave stopped containers around.
- The host's CPU and memory are really free while a container is
  stopped.
- A start re-checks quota and can fail with 409.

The hard partition (call 3) is unchanged: a silo's grant is reserved
out of its parent's, so a start within the silo's own grant always
passes the ledger.

### Root capacity and overcommit (call L20)

```
root capacity = (host total − reserve) × overcommit
```

- **Host totals:** online CPUs, `MemTotal`, and the data filesystem's
  size (`statfs` of `/var/lib/silo`).
- **Reserve** is set at install: default 1 vCPU, 2 GiB or 10% of
  memory (whichever is larger), and 10% of disk. It is for silod, the
  harnesses and the host's own use.
- **Overcommit** is set at install, per class:

  | Class | Default | Maximum |
  | --- | --- | --- |
  | `vcpu_m` | 2.0 | 8.0 |
  | `memory_mb` | 1.0 | 1.0 (memory isn't overcommitted) |
  | `disk_gb` | 1.0 | 1.0 |

  Memory overcommit ends in the host OOM killer choosing among
  tenants, and a VM's memory is touched by its guest. CPU overcommit
  only slows things down.
- **`describe` on the root shows all three numbers,** so the operator
  can see what was promised.

### Enforcement beside the ledger

The ledger decides what may start, and the kernel enforces what is
running, as a second layer:

- **The cgroup tree mirrors the silo tree.** `silo.slice` →
  `silo-<root>.slice` → `silo-<child>.slice` → one cgroup per
  container. Each silo's slice gets `cpu.max` from its `vcpu_m` grant,
  `memory.max` from `memory_mb`, and `pids.max`.
- **A silo's workloads can't exceed its grant,** even if the ledger
  had a bug.
- **For `vm`,** the cgroup holds cloud-hypervisor, so the guest's
  vCPUs and memory are inside it.
- **For `sandbox`,** gVisor documents that the host cgroup is what
  limits a sandbox.
- **Disk:** writable layers are capped by gVisor's overlay size, or by
  granule's overlay size (`-disk-size-mb`). Volumes are capped by
  project quotas. Images are charged and garbage-collected when no
  silo references them. *Rootless:* project quotas need root, so with
  the default `dir` storage driver, volumes are charged but not
  enforced. Delegated ZFS enforces them
  ([call L38](local-rootless-storage.md#storage-when-rootless-and-what-each-option-guarantees-call-l38)).

### Storage layout (call L21)

*Superseded in its paths* by part 2's
[one data directory](local-rootless-storage.md#one-data-directory-call-l35)
(call L35). Everything below moves under `<data-dir>/cache/`, except
volumes, logs and agents, which move to `<data-dir>/silos/<silo id>/`,
and the store, which moves to `<data-dir>/state/`. The sharing and the
snapshot rules below are unchanged.

```
/var/lib/silo/
  silo.db                    SQLite store (WAL)
  containerd/                silo-containerd root: content store (shared blobs),
                             snapshots (overlayfs), metadata with namespace silo-<id> per silo
  buildkit/                  the private buildkitd for granule wraps
  volumes/<silo id>/<vol id> volume directories, with project quotas
  logs/<silo id>/<ctr id>    container logs, json-file framing, rotated, charged to disk_gb
  agents/<agent id>/         saved harness sessions (copies; the database is the truth)
  runtimes/runsc-<version>   pinned runsc and containerd-shim-runsc-v1
  runtimes/granule-<version> pinned granule image references and the probe image
```

- **One content store** serves both classes. An image pulled for a
  sandbox and later run as a VM is fetched once.
- **The wrapped VM image is an extra image**, made from the same
  layers.
- **Snapshots are overlayfs** for `sandbox`. For `vm`, the disk image
  inside the wrapped image is the base, and granule's qcow2 overlay (or
  a reflink clone, on XFS or btrfs) is per container (granule README,
  "How Granule Disks Work").
- **An XFS data filesystem is recommended,** for reflinks and project
  quotas. Install checks for both and warns, naming what is lost
  without them.

### Metering

- **Model use:** [§10](#10-agents-on-the-local-backend), by reported
  usage for ambient credentials and by the model proxy for explicit API
  keys. It is charged to the agent's budget, as in `agents.md` §7.
- **Resource use:** silod samples each silo's slice every minute:
  - `cpu.stat` `usage_usec`;
  - `memory.current` and `memory.peak`;
  - `io.stat`.

  It records the samples per silo, and per agent for the agent's tool
  container and its descendants. `silo describe` shows them as
  `usage`.
- **This is metering, not a budget.** v1 charges no money for host
  resources; quota is the limit. An agent's resource use is reported
  next to its model spend, so that a run's cost is visible in one
  place.

## 12. State, install and operations

### State (call L22)

- **SQLite, in WAL mode, at `/var/lib/silo/silo.db`.** It is the store
  [`state.md`](state.md#two-backends-one-interface) already has for dev
  and tests. On this backend it is the production store: one host
  means one silod, and Cloud SQL would be a remote dependency for a
  machine that otherwise has none.
- **Leadership is trivial.** The leader lease still exists, held by the
  one silod, so the code paths are the same.
- **Backups:** no backup system in v1 (bsalmon, after this page was
  written). The store sits at `<data-dir>/state/silo.db` and can be
  copied consistently while silod runs, with `VACUUM INTO` or SQLite's
  online backup API. `silo backup` is now [later](local-rootless-storage.md#later).
  silod detects a restore by itself, and then its reconcile is
  report-only until an operator clears it, as call 36 requires
  ([call L40](local-rootless-storage.md#restoring-call-l40)).

### Privileges and kernel features

*Superseded:* silod runs **rootless**, and the rootful mode described
in the first bullet below is retired
([call L26](local-rootless-storage.md#what-rootless-means-here)). Part 2
lists the [one-time root steps](local-rootless-storage.md#the-one-time-root-steps)
that replace these privileges. It also lists the extra prerequisite
checks: unprivileged user namespaces, `newuidmap` and a subuid range,
`kvm` group membership, lingering, and controller delegation. The
kernel checks in the table below still apply, and are run inside the
holder's namespaces.

- **silod runs as root.** It needs:
  - `CAP_NET_ADMIN` for netns, bridges, veths and nftables;
  - `CAP_SYS_ADMIN` for mounts, netns and project quotas;
  - containerd's privileges;
  - the right to start scopes under the agent user.

  It drops nothing it needs. Each piece that doesn't need root runs
  under its own user: the gateway's proxies on their own threads,
  `virtiofsd` sandboxed, and `buildkitd` rootful but reachable only by
  silod.
- **Install's prerequisite check** refuses or warns, naming the fix,
  in the style of [`install.md`, Prerequisite checks](install.md#prerequisite-checks):

  | Check | When it fails |
  | --- | --- |
  | Linux ≥ 5.15, cgroup v2 unified, systemd | Refuse |
  | `nf_tables`, with the `bridge` and `inet` families (probed by loading a scratch table in a scratch netns), `veth`, `bridge`, `br_netfilter` *not* required | Refuse |
  | A network namespace can be created | Refuse |
  | `/dev/kvm` and `/dev/net/tun` (`granule doctor -only kvm,tun`) | Without them, the `vm` class is off |
  | A `runsc` sandbox boots on systrap | Without it, the `sandbox` class is off |
  | At least one class available | Refuse |
  | The data filesystem supports project quotas | Warn; volume sizes are then charged but not enforced |
  | The data filesystem supports reflinks | Warn |

- **Every check is re-run at each silod start,** and the result is in
  `describe` on the root.

### Install (call L23)

`silo install --backend local` is a local, idempotent command. *Part
2 changes steps 3 and 5:* the units are **user units** with lingering,
and `silo-nsd.service` takes `silo-net.service`'s place. Paths are
under `--data-dir`. The system install is the rootless design under a
`silo` system user
([call L34](local-rootless-storage.md#the-alternative-a-system-install-under-a-silo-user-call-l34)).

1. **Check** the prerequisites above.
2. **Fetch and verify the runtimes:**
   - `runsc` and `containerd-shim-runsc-v1` at the pinned version, from
     gVisor's release bucket, with their published SHA-512 sums;
   - the pinned granule image, granule guest image and probe image,
     by digest;
   - containerd and buildkitd from the distribution's packages, or
     pinned static releases if those are too old.
3. **Write and start the units:** `silo-containerd.service`,
   `silo-buildkitd.service`, `silod.service` and `silo-net.service`.
   The last creates the namespace, `gw0` and the base nftables tables.
4. **Create the root silo:**
   - address space `--address-space`;
   - classes as probed;
   - capacity as in [§11](#root-capacity-and-overcommit-call-l20);
   - `ingress: host`;
   - the installing user mapped as the root's admin by uid;
   - `--agent-user`, defaulting to that same user.
5. **Write a docker context** for the root silo into the installing
   user's docker config, and print `silo docker-env`.

`--dry-run` prints the plan and changes nothing, as for GCP.
`silo uninstall --backend local` reverses it:

- stops and removes every container;
- removes `silo-net`, the units and the pinned runtimes;
- removes `/var/lib/silo` only with `--delete-data`.

### Restart and adoption

Part 2 adds the namespace holder's own restart and adoption, how a
reboot rebuilds `silo-net` without root, and how the disk maps back to
the store ([call L33](local-rootless-storage.md#restarts-and-reboots-call-l33)).
The steps below are unchanged, except that `silo-nsd` takes the place
of `silo-net.service`.

- **silod restarts** (upgrade or crash) leave workloads running.
  containerd's shims are separate processes. On start, silod:
  1. loads the store;
  2. lists `silo-containerd`'s tasks in every `silo-*` namespace and
     matches each to its database row by silod's labels. A task with
     no row is an orphan, handled as reconcile handles orphans (call
     27: reported, then deleted after the grace period);
  3. reapplies the full nftables ruleset and checks that every running
     container's netns, veth and pin set entry exist, recreating any
     that are missing **before** the gateway accepts connections;
  4. re-attaches to log files, which the shim writes independently of
     silod, and to health checks;
  5. resumes agents ([§10](#10-agents-on-the-local-backend)).
- **silo-containerd restarts** also leave tasks running, which is
  containerd's own behaviour with v2 shims.
- **A host reboot** stops everything. `silo-net.service` recreates the
  namespace and tables before silod starts. silod then restarts
  containers whose `RestartPolicy` is `always`, `unless-stopped` (not
  stopped by the user) or `on-failure`, which silod serves itself, as
  dockerd does. It starts them in tree order, re-probing classes first
  ([When the host can't provide a class](#when-the-host-cant-provide-a-class)).

### Teardown

- **Deleting a silo** runs `Contents.DeleteContents` for it and its
  subtree, deepest first:
  1. stop and remove its containers (and their netns and veths);
  2. remove its networks (bridges) and volumes (directories);
  3. remove its containerd namespace, letting content GC drop blobs
     no one references;
  4. remove its cgroup slice;
  5. remove its fence sets.

  Its blocks are freed under the usual one-hour rule (call 24). It is
  the same `DELETING`/`DONE` operation as today.
- **Deleting a container** removes its veth's pin entry **after** the
  task is gone, never before.

## 13. Testing and the conformance split

### Two kinds of cases (call L24)

[`testing.md`, Cases](testing.md#cases) mixes properties of
the fence with properties of GCE's facade. The suite splits them:

- **Backend-neutral fence properties**, in
  `pkg/conformance/fence`, written against the fence API and a probe
  abstraction:
  - **isolation:** `/a` → `/b`, `/a` → `/a/c` and `/a/c` → `/a`,
    unreachable on TCP, UDP and ICMP;
  - **origin spoofing:** sending from another silo's address never
    arrives;
  - **never-looser:** every policy, `isolation` and `ingress` included;
  - **grants:** an export offered but not accepted; a link proposed by
    one side only, then both, then deleted; revocation closing an open
    connection;
  - **egress:** allowed and denied names, internal and link-local
    destinations refused;
  - **sealed and `read` silos:** both layers;
  - **agents:** the cases in `agent_cases_test.go` that don't name GCE.
- **Facade-specific cases**, per facade:
  - `pkg/conformance/gce`: today's naming, refused fields, list leaks
    by every GCE name form, operations;
  - `pkg/conformance/docker`, new:
    - every refusal in [Refused](#refused);
    - names, ids and 404s across silos, by every id prefix;
    - `/info` showing no host facts;
    - the `Host`-header and socket target rules;
    - published ports answering only their own silo;
    - DNS answering only its own silo;
    - image records per silo;
    - the compose and testcontainers flows in [§5](#5-compose-and-testcontainers).
- **Local-only structural cases**, in `pkg/conformance/local`:
  - `silo-net` has no interface or route into the root namespace;
  - a probe can't reach `169.254.169.254`, the host's addresses or a
    service listening on the host's `0.0.0.0`;
  - a port with no pin entry passes nothing;
  - adoption after `systemctl restart silod` keeps containers running
    and the fence intact.

The probe abstraction is a `conformance.Target`: make a probe in a
silo, run a list of `Check`s (today's `ProbeScript`), and read the
report. The GCP target makes `e2-micro` VMs. The local target makes
probe containers through the Docker facade, **once per isolation
class**. The neutral cases run against `local/sandbox` and `local/vm`
as two targets, and against `gcp`.

### How CI runs them

- **The fake driver and the nftables evaluator, on every push, in the
  `go` job.** `pkg/backend/local/runtime/fake` runs containers as
  records with a simulated netns. `local/netfence/eval` evaluates a
  compiled ruleset for a packet, as `fake/fenceeval` does for GCE's
  policy. The neutral cases then run against "local/fake" in seconds,
  and a property test checks that the compiled ruleset never allows
  more than `fence.Input` says, as `pkg/backend/gcp/gce/gcefence/property_test.go` does
  today. **A fake runtime is needed**: without it, every fence case
  needs root, nftables and a real runtime (call L25).
- **The real local conformance, in a new job, `local-conformance`,
  on every push** that touches Go code.
  - It runs on a grain CI sandbox:
    - **`sudo` works there**, as this task's sandbox shows (Debian,
      kernel 6.1, 4 vCPUs, 8 GiB on 2026-10-12);
    - **gVisor needs no KVM;**
    - the job fetches the pinned `runsc` at its start.
  - It installs with `--backend local`, runs the neutral, Docker and
    local cases against `local/sandbox`, then uninstalls. That takes
    minutes, not the GCP suite's tens of minutes, and needs no cloud
    credentials, so it can gate merges.
  - **The `vm` target runs in the same job when `/dev/kvm` works.**
    This task's sandbox had `/dev/kvm` on 2026-10-12. *Unverified:*
    whether every CI runner does, and whether nested KVM there boots
    granule. When `granule doctor` fails, the job **reports** "vm
    class not tested on this runner" in its summary as a failed check
    of its own. It does not skip silently. Whether that check gates
    merges is [open question 9](#open-questions).
- **The GCP conformance** stays nightly, unchanged in what it covers,
  running the neutral cases with the GCP target.

## 14. Limitations

True of the local backend by design:

- **One machine is one failure domain.** silod, the state and every
  workload go down together. There are no replicas.
- **Isolation between silos is rules,** as in the subnet backing. A
  wrong rule exposes silos to one another, but not the host
  ([§9](#what-a-rule-bug-would-expose)).
- **`sandbox` is weaker than `vm`.** A gVisor escape is a kernel-level
  compromise of the host. Parents that care require `vm`.
- **The harness isn't fenced.** Ambient harnesses run as the agent
  user. A harness exploit is that user's account
  ([§10](#10-agents-on-the-local-backend)).
- **Image pulls share a cache across silos,** which leaks, by timing,
  whether another silo pulled an image
  ([Images](#images)). Part 4's pull-through cache widens this, and its
  `images.sharing: isolated` policy closes it for a subtree, at the
  cost of upstream fetches
  ([`local-registry.md`, call L73](local-registry.md#the-timing-channel-and-imagessharing-call-l73)).
- **The `vm` class is degraded until granule closes its gaps:** exec
  in the image's root, the workload's own logs, exit codes, stdin and
  volumes in the chroot ([§6](#granule-driver)).
- **No builds in the facade, and no Docker-in-Docker** in v1. Images
  are built in a container and pushed to the local registry, and
  `load` is served ([part 4](local-registry.md#9-building-images)).
- **Code isn't portable between backends' facades**, as
  [README §8](README.md#8-limitations) already says. A compose file
  runs on a local silo; a terraform module for the GCP facade doesn't.
- **IPv4 only.** IPv6 frames are dropped at the bridge.
- **silod's account is the fence.** silod runs rootless, so escaping
  into its account doesn't give the host. It does give every silo
  ([part 2, Security statement](local-rootless-storage.md#security-statement)).
- **Rootless volumes are charged, not enforced,** with the default
  `dir` storage driver. One silo can fill the data filesystem
  ([call L38](local-rootless-storage.md#storage-when-rootless-and-what-each-option-guarantees-call-l38)).
- **A container's writable layer is not data.** After a restore, or if
  the cache is lost, a container comes back from its image. Data that
  matters goes in a volume
  ([part 2](local-rootless-storage.md#data-and-what-is-not-data)).

## 15. Staged implementation plan

Each item is a task that can be filed on its own, in this order. Each
depends on the items it names. Items 14–18 were added with
[part 2](local-rootless-storage.md)'s rootless and storage decisions.
They are numbered after the others so that existing references stay
valid. Earlier items that now depend on them say so, and none of those
earlier items is built rootful first. Items 19–24 were added with
[part 3](local-git.md)'s hosted git repos and their outbound mirror.
Nothing earlier depends on them. Items 25–36 were added with
[part 4](local-registry.md)'s registry and pull-through cache. Nothing
earlier depends on them either, except that item 26 is best landed
before item 7.

1. **Backend interfaces and the reconcile on them.**
   - Add `pkg/backend` (`Inventory`, `Fence`, `Capacity`, `Classes`,
     `Contents`, `Targets`, `ToolVMs`).
   - Implement them for GCP by wrapping today's code.
   - Move `reconcile` and `cmd/silod` onto them.
   - No behaviour change; existing tests prove it.
2. **Split `pkg/fence`, and move `rootcap` under `backend/gcp`.**
   Neutral `Input`, links and ranges stay; `compile`/`plan`/`apply` and
   their tests move. Depends on 1.
3. **Extract the facade core into `pkg/facade`** (resolve, control,
   tables, field allowlists, audit), with the GCE facade as its only
   user and no behaviour change. Depends on 1.
4. **Fence API: `isolation` and `ingress` policies, backing `local`
   and region `local`.** Includes the never-looser checks and their
   tests, using the fake backend. Depends on 1.
5. **`backend/local` skeleton:**
   - SQLite as the production store;
   - the `silo-net` namespace and `gw0`;
   - the nftables compiler, applier and evaluator (`local/netfence`);
   - anti-spoofing sets;
   - the property test;
   - the fake driver.

   Depends on 2, 4 and 15 (`silo-net` is made by the holder).
6. **The Docker facade, `pkg/facade/local/docker`**, against the fake
   driver:
   - the endpoint and field tables;
   - target resolution by socket and `Host`;
   - errors;
   - charges;
   - networks, volumes and published ports in the gateway;
   - DNS.

   Depends on 3, 5 and 14 (volumes through the storage driver).
7. **The gVisor driver.**
   - `silo-containerd`, `runsc` with systrap, netstack;
   - netns wiring before create;
   - logs, exec, attach, archive, stats and cgroups;
   - adoption on restart.

   Depends on 5 and 16. It runs anywhere, CI included.
8. **`silo install --backend local` and `uninstall`.** Covers the
   prerequisite checks, the pinned runtime fetch, the units, the root
   silo and the docker context. Depends on 7 and 17.
9. **granule changes, filed against gke-labs/granule:**
   - exec inside the wrapped container's root, with its env and user;
   - the workload's stdout and stderr apart from the console;
   - the workload's exit code out of the VM;
   - stdin for attach;
   - mounting virtio-fs volumes inside the chroot;
   - per-container entrypoint, cmd, env, user and workdir without
     rewrapping;
   - a wrap without the docker CLI;
   - running unprivileged: container root mapped to a subuid,
     `/dev/kvm` bind-mounted and reached through the `kvm` group, under
     crun ([call L31](local-rootless-storage.md#kvm-for-the-vm-class-call-l31)).

   Independent of 1–8; it can start at once.
10. **The granule driver.**
    - Its containerd spec, checked against `granule vm create` through
      granule's fake docker;
    - wrapping through buildkitd;
    - virtiofsd per volume;
    - the KVM probe.

    Depends on 7, 9 and 16.
11. **Agents on the local backend.**
    - Ambient harnesses in scopes under the agent user;
    - the `ambient` credential kind and metering by reported usage;
    - peer-cgroup checks on session sockets;
    - tool containers in both classes;
    - resource metering.

    Depends on 7.
12. **The conformance split and the local CI job.**
    - Move the neutral cases behind `conformance.Target`;
    - add the GCP target, unchanged in behaviour;
    - add the local targets for `sandbox` and `vm`;
    - write the Docker and local-structural cases;
    - add `.grain/workflows` job `local-conformance`.

    Depends on 6, 7 and 10 (its `vm` half on 10).
13. **Compose and testcontainers acceptance:** the flows in [§5](#5-compose-and-testcontainers)
    as scripted cases in `local-conformance`, in both classes.
    Depends on 12.
14. **The data directory and the storage driver**
    ([part 2, §2](local-rootless-storage.md#2-storage-laid-out-for-later-backups)):
    - `--data-dir`, `layout.json` and the `state/`, `keys/`, `silos/`
      and `cache/` layout, with the store and data-directory ids
      cross-checked;
    - `pkg/backend/local/storage`: the `Driver` interface, `Ref`,
      `Guarantees`, and `Snapshot` returning `ErrSnapshotUnsupported`;
    - the `dir` driver, with the usage scan, `volume_over_quota` and
      the 409 on mounting an over-quota volume;
    - a test that nothing persistent is written outside the data
      directory (run silod against a temporary one and diff the
      filesystem).

    Depends on 1. It doesn't need the holder: the `dir` driver runs in
    silod's process in tests, and through the RPC once 15 lands.
15. **The namespace holder, `silo-nsd`**
    ([calls L27–L29](local-rootless-storage.md#namespaces-the-holder-and-silod-call-l27)):
    - user namespace through `newuidmap`, private mount namespace,
      `silo-net`, and `gw0`, with the namespace-joining code running
      before the Go runtime starts;
    - the typed RPC on `nsd.sock` and its uid check;
    - listening sockets passed with `SCM_RIGHTS`, and the dial pool;
    - nested per-container user namespaces with the netns inside, and
      the recorded uid mapping, checked at start;
    - the storage driver behind the RPC;
    - `silo unshare`;
    - prerequisite probes: unprivileged user namespaces, AppArmor's
      restriction, `newuidmap`, the subuid range.

    The first step is a spike, run in CI as an unprivileged user, that
    proves netlink and nftables from silod through passed sockets, and
    runc and runsc joining a pre-made user namespace and netns. Depends
    on 14.
16. **Rootless runtimes:** containerd and buildkitd in the holder's
    namespaces; gVisor in a caller-configured user namespace with
    netstack ([call L30](local-rootless-storage.md#gvisor-rootless-call-l30));
    crun with `keep_original_groups` for the `vm` class
    ([call L31](local-rootless-storage.md#kvm-for-the-vm-class-call-l31));
    cgroups through the user manager, with `describe` reporting missing
    delegation. Depends on 15.
17. **Units, restarts and the two install modes**
    ([calls L33 and L34](local-rootless-storage.md#restarts-and-reboots-call-l33)):
    - systemd user units and lingering;
    - holder adoption after its own restart;
    - rebuilding from the store after a reboot;
    - the one-time root steps table as install's checks;
    - the system install under a `silo` user.

    Depends on 15 and 16.
18. **Safe copies and restore detection**
    ([calls L39 and L40](local-rootless-storage.md#copying-the-state-safely-call-l39)):
    - the object sequence and its labels (`dev.silo.seq`, `.silo-seq`);
    - report-only mode on a detected rollback, `restore_detected`, and
      `silod --after-restore`;
    - re-creating rows without records, and `volume_missing`;
    - a test that copies the store with `VACUUM INTO` under write load,
      restores it over a running data directory, and checks that later
      objects are reported as orphans, fenced off and not deleted.

    Depends on 7 and 14. No backup command: that is
    [later](local-rootless-storage.md#later).
19. **Repos in the fence API, the store and `silo repo`**
    ([part 3, §2](local-git.md#2-repos-as-silo-objects)):
    - the repo object, its operations and `silo repo create|list|get|delete|url`;
    - the `repos` quota class and the never-looser `repos.mirror`
      policy;
    - the `repos` type in manage-grant scopes;
    - `silos/<id>/repos/` through the storage driver;
    - seeding.

    Depends on 4 and 14.
20. **The git service** ([part 3, §3–§4](local-git.md#3-the-git-service)):
    - smart HTTP at `silo.internal/git/` on port 80;
    - the access table, with 404 for the invisible;
    - stateless `upload-pack`/`receive-pack` with a scrubbed
      environment;
    - the `pre-receive` hook (ref namespaces, blob size, disk, local
      archive refs);
    - push audit;
    - `git-remote-silo` on the front door.

    Depends on 19, and on 15 for the listener in `silo-net`.
21. **The outbound mirror** ([part 3, §8](local-git.md#8-the-outbound-mirror)):
    - `silo mirror` and the credential in `keys/mirror/`;
    - per-repo atomic passes with leases, retries and stall audits;
    - archive refs;
    - deleting through archives;
    - renames;
    - chunked pushes.

    Tested against a local `git http-backend` behind a fault-injecting
    proxy. Depends on 20.
22. **The submodule index, and a check against real GitHub**
    ([part 3, §7](local-git.md#7-the-submodule-index)): the index
    branch with gitlinks and `url = ./`, its lease, and one supervised
    run that settles part 3's *verify* items. Depends on 21.
23. **Mapping back:** `silo repo external`, `silo repo which` and
    `silo repo external-clone`
    ([part 3, §9](local-git.md#9-mapping-back-the-ref-layout-and-silo-repo-external)).
    Depends on 21 and 22.
24. **Repo and mirror conformance cases** in `local-conformance`
    ([part 3, §11](local-git.md#11-staged-implementation-plan)).
    Depends on 12, 21 and 22.
25. **Image repos in the fence API and the store**
    ([part 4, §3](local-registry.md#3-repos-as-silo-objects)):
    - the `imageRepos` object and `silo registry`;
    - the `image_repos` quota class and the `registry` manage-grant type;
    - registry exports and imports;
    - the never-looser `images.sharing` policy.

    Depends on 4.
26. **containerd's root out of `cache/`**
    ([part 4, call L60](local-registry.md#pushed-images-are-data-call-l60)):
    layout 2, `containerd/` and `cache/snapshots/`, and the migration.
    Depends on 14; best landed before 7.
27. **The registry service for local repos**
    ([part 4, §6](local-registry.md#6-the-registry-service)):
    - `registry.silo.internal` on port 80 by `Host`;
    - names, the access table and the 404 rule;
    - uploads, manifests, mounts, tags, referrers and catalog;
    - proof of possession;
    - charging at push, first-push creation, records and audit.

    Depends on 25, 26 and 15.
28. **Registry grants:** exports, manage grants, and closing connections
    on revocation. Depends on 27.
29. **The host registry port** with the `inet_diag` uid lookup. Depends
    on 27.
30. **The Docker facade's image changes**
    ([part 4, §7](local-registry.md#7-the-docker-facade)): push, local
    pulls without egress, `save`, `load`, `sandbox` commit and `/auth`.
    Depends on 6, 7 and 27.
31. **The pull-through cache for public content**
    ([part 4, §8](local-registry.md#8-the-pull-through-cache)):
    upstreams, addressing, the per-request egress check, misses as the
    caller, tag revalidation, and facade pulls on the fetcher. Depends
    on 27 and 30.
32. **Private upstream content:** classification, revalidation with the
    caller's credentials, in-memory authorisations, and the
    canary-credential test. Depends on 31.
33. **Sharing domains** for `images.sharing: isolated`. Depends on 25
    and 31.
34. **Cache budget, eviction, GC and deletion**
    ([part 4, §10](local-registry.md#10-garbage-collection-and-deletion)).
    Depends on 27 and 31.
35. **Builders, run for real:** kaniko and rootless BuildKit in both
    classes, with the results written back into part 4. Depends on 27,
    30 and 31, and on 10 for `vm`.
36. **Registry and cache conformance cases** in `local-conformance`
    ([part 4, §12](local-registry.md#12-conformance-cases)). Depends on
    12 and 28–35.

Item 9's granule list also gains the diff of a wrapped container's
root as a tar, for `docker commit` in the `vm` class (part 4).

## Calls made on this page

L1. **Isolation class is a silo policy, `isolation: {allowed,
    default}`, ordered `vm` > `sandbox`**, handed down never-looser. A
    parent requires `vm` by allowing only `vm`.

L2. **`HostConfig.Runtime` picks the class.** It takes `silo-vm`,
    `silo-sandbox`, their aliases, and `runc` or empty for the silo's
    default. Nothing ever runs under plain runc, and there is no
    fallback between classes.

L3. **TCG is refused by default.** If it is ever added, it is a third
    class, `emulated`, below `sandbox`: off unless install says so, and
    labelled unfenced everywhere.

L4. **A Docker request's target comes from its endpoint, and its
    caller from its origin.** The endpoint is the per-silo Unix socket
    on the host, or `docker.silo.internal` with the target silo in the
    `Host` header inside. There are no tokens.

L5. **Engine API 1.41–1.47,** advertising 1.47, to be checked against
    current clients.

L6. **Image pulls are held to the silo's egress policy** and go
    through the gateway's egress proxy as the silo.

L7. **One content store, per-silo records:** containerd namespace
    `silo-<id>`. Names are the tenant's own, with no prefix.

L8. **One bridge per Docker network, all in one namespace,
    `silo-net`;** traffic between networks of a silo is dropped, as
    Docker drops it.

L9. **Volumes are quota-capped directories,** bind-mounted through the
    gofer for `sandbox` and shared over virtio-fs for `vm`.

L10. **Published ports never open a raw host port.** They answer on
     `docker.silo.internal` to the publishing silo only, and on the
     host's loopback only with `ingress: host`, as a proxied export to
     the front door.

L11. **The compose and testcontainers flows in §5 must work in
     `sandbox` at once, and in `vm` once granule's gaps are closed.**
     Ryuk is disabled; the silo is the reaper.

L12. **Both classes run on a private containerd that silod owns,** not
     on a private dockerd and not on the host's.

L13. **gVisor uses netstack (`--network=sandbox`) on systrap,** never
     host passthrough, and never the KVM platform.

L14. **silod's backend-neutral parts call `pkg/backend` interfaces,**
     and `gce.Client` stays inside `backend/gcp`.

L15. **The facade core is extracted before the Docker facade is
     written,** with GCE as its only user at first.

L16. **`silo-net` has no interface into the root namespace.** silod's
     sockets are the only bridge between them, so a rule bug can't
     reach the host.

L17. **Harnesses are the host's installed CLIs, run as the agent user
     with ambient permissions,** credential kind `ambient`, metered by
     reported usage. Their built-in tools stay off, and tools run in
     the agent's tool container.

L18. **Session sockets also check the peer's cgroup,** because ambient
     sessions share one uid.

L19. **The local quota classes are `vcpu_m`, `memory_mb`, `disk_gb`,
     `containers`, `networks`, `volumes` and `agent_sessions`.** CPU
     and memory are charged only while a container runs.

L20. **Root capacity is `(host − reserve) × overcommit`,** with CPU
     overcommit 2.0 by default, and memory and disk never
     overcommitted. The cgroup tree mirrors the silo tree.

L21. **One storage layout under `/var/lib/silo`,** with blobs shared
     between classes and wrapped images derived from them. *Its paths
     are superseded by L35*: everything is now under one `--data-dir`.

L22. **SQLite is the production store on this backend.**

L23. **`silo install --backend local` pins and verifies `runsc` and
     granule's images,** and runs its own containerd, buildkitd and
     silod units.

L24. **The conformance suite splits** into backend-neutral fence
     cases, facade cases and local structural cases. The neutral cases
     run against every target, and against both local classes.

L25. **A fake runtime and an nftables evaluator** give fast fence
     tests on every push. The real `sandbox` conformance also runs on
     every push.

L26–L40 are in part 2, under
[Calls made on that page](local-rootless-storage.md#calls-made-on-this-page).
They cover rootless operation (L26–L34) and the data directory,
storage driver, safe copies and restore (L35–L40).

L41–L55 are in part 3, under
[Calls made on that page](local-git.md#calls-made-on-this-page). They
cover hosted git repos, the fence on them, submodule URLs, the outbound
mirror to the uber repo, and its submodule index.

L56–L76 are in part 4, under
[Calls made on that page](local-registry.md#calls-made-on-this-page).
They cover the local registry, its names and fence, charging and data,
the facade's image changes, builders, the pull-through cache with
per-silo egress and private-image checks, sharing domains, and garbage
collection.

## Open questions

1. **Ambient versus a dedicated agent user.** Ambient (the operator's
   instruction) means a harness exploit is the operator's account.
   Should install offer `--agent-user=silo-agent`, a user that is no
   silo's member and holds copies of the logins, as a hardening
   option? Or should the root's agent policy be able to forbid ambient
   credentials for a subtree?
2. **The harness's own tools.** v1 keeps them off. Should an ancestor
   be able to allow "ambient tools" for a subtree, where the CLI's
   shell and file tools run on the host? That would make such agents
   explicitly unfenced, and they would be shown so in `describe`.
3. **An `emulated` (TCG) class** for hosts with neither KVM nor a
   working gVisor: worth having at all, given L3's labelling?
4. **gVisor on the KVM platform** on bare-metal KVM hosts is faster,
   but those hosts offer `vm` anyway. Is there a reason to support it?
5. **The Docker socket inside a container.** Should a bind of
   `/var/run/docker.sock` be accepted as "this silo's own Docker
   facade", served into a sandbox through the gofer and into a VM over
   vsock? That would let Ryuk and Docker-outside-of-Docker tools work
   unchanged.
6. **`docker load`** of a tarball: useful for CI and air-gapped
   images, but silod would parse an untrusted archive on the host. Run
   the import in a sandbox of its own? *Answered in part 4* (call
   L68): `load` is served, and never extracts the archive.
7. **Image sharing across silos.** Should an ancestor be able to turn
   off content sharing for a subtree, to close the timing channel, at
   the cost of disk and pulls? *Answered in part 4* (call L73): yes,
   with `images.sharing: isolated`, at the cost of upstream fetches
   rather than disk.
8. **IPv6.** The bridge drops it in v1. The README's IPv6 plan (ULA
   prefixes carved like blocks) would apply directly; granule already
   takes over a pod's IPv6 identity.
9. **The `vm` half of local conformance on CI runners without KVM.**
   Should it gate merges, or be a reported check that a human reads?
10. **Mixed nesting** (a GCE silo hosting a local child silo in a
    nested-virt VM) stays later. Should the fence API's `backing`
    already accept a value for it, so that adding it later changes no
    client?
11. **Snapshots for fast agent tool VMs**, on granule's
    `Snapshot`/restore, once granule can restore one snapshot into
    many VMs.
12. **Remote access to a local silo.** The front door is a Unix socket
    on the host. Is an SSH-forwarded socket enough for a second person,
    or does the local backend need a TCP front door with its own
    authentication?

Open questions 13–20 are in
[part 2](local-rootless-storage.md#open-questions). They cover
`CAP_NET_ADMIN` in the system install, the rootless front door for
other users, hard volume quotas without root, the subuid range, ZFS
mounting, the default account, crun for the `vm` class, and whether
logs are data.

Open questions 21–30 are in
[part 3](local-git.md#open-questions). They cover ancestors pushing
into descendants' repos, repo grants between silos, read-only code for
a subtree, the external ref layout, archive refs, a diverged index,
attribution, sandboxing git's server side, mirror lag, and hosts other
than GitHub.

Open questions 31–40 are in
[part 4](local-registry.md#open-questions). They cover multi-component
repo names, TLS, privileges for builder containers, pointing tools at
the cache, private revalidation, padding hits, the host registry port,
retention, write grants between silos, and upstream rate limits.

## Later

- Mixed nesting ([README §9](README.md#9-later)).
- `docker build`, run as a workload in the caller's own silo, in a
  builder container of the silo's class, so a build is fenced like
  anything else.
- Snapshots and warm pools for agents' tool VMs.
- A Kubernetes facade over the same local backend.
- Backups: a per-silo backup policy, WAL streaming of the store,
  off-host snapshot shipping, and `silo backup`/`silo restore`
  ([part 2, Later](local-rootless-storage.md#later)).
- For hosted git: two-way sync with the uber repo, a rewritten-tree
  folder view, the GCP backend, per-silo external repos and LFS
  ([part 3, Later](local-git.md#10-later)).
- For the registry: an outbound copy of pushed images to an external
  registry, beside the uber repo; a facade build that pushes; the GCP
  backend; signature policy; cache warming
  ([part 4, Later](local-registry.md#14-later)).
