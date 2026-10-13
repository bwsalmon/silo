# Silo local backend, part 2: rootless operation, and a data directory backups can be added to

## Status

Design draft, docs only, written in task 3356 (filed from task 3024).
No implementation code. Like the rest of Silo, implementation waits
for bsalmon's approval.

This page is a companion to [`local-backend.md`](local-backend.md)
(task 3350). It records two decisions bsalmon made after that page was
written:

1. **silod runs fully rootless.** Once the packages are installed,
   nothing runs as root ([§1](#1-rootless-operation)).
2. **No backup system yet.** Storage is laid out so that one can be
   added later without migrating data or changing an API
   ([§2](#2-storage-laid-out-for-later-backups)).

Where this page and `local-backend.md` disagree, **this page wins**.
The paragraphs it replaces in `local-backend.md` say so and link here:
§2's outline, the topology note in §9, §11's storage layout (call L21),
and §12's state, privileges, install and restart sections.

Calls continue the numbering of `local-backend.md`'s calls, from
**L26**. They are listed in [Calls made on this page](#calls-made-on-this-page).
Open questions continue from **13** ([Open questions](#open-questions)).
The new steps of the staged plan are items 14 to 18 of
[`local-backend.md` §15](local-backend.md#15-staged-implementation-plan).

### What was checked, and against what

- **gVisor**: the Rootless guide (`/docs/user_guide/rootless/`), the
  Systemd cgroup driver guide and the Networking guide at gvisor.dev,
  fetched on 2026-10-12. Quotes below are from those pages.
- **Linux kernel behaviour** (namespace ownership, `setns`, netlink
  capability checks, `SO_PEERCRED` translation, project quota IDs):
  stated from the man pages (`user_namespaces(7)`, `setns(2)`,
  `unix(7)`) and the kernel as the author knows them. Nothing was run.
  Claims that the implementation must check against a real kernel are
  marked *verify*.
- **passt/pasta**: passt.top didn't return a page from the sandbox. Its
  description below is taken from the task and from passt's
  documentation as the author knows it (*verify*).
- **OpenZFS and btrfs delegation**: stated from their man pages as the
  author knows them (*verify*).

## Contents

1. [Rootless operation](#1-rootless-operation)
   - [What "rootless" means here](#what-rootless-means-here)
   - [Who silod runs as](#who-silod-runs-as)
   - [Namespaces: the holder and silod](#namespaces-the-holder-and-silod-call-l27)
   - [Sockets across the two network namespaces](#sockets-across-the-two-network-namespaces-call-l28)
   - [Containers in nested user namespaces](#containers-in-nested-user-namespaces-call-l29)
   - [gVisor rootless](#gvisor-rootless-call-l30)
   - [KVM for the vm class](#kvm-for-the-vm-class-call-l31)
   - [cgroups](#cgroups)
   - [pasta: an optional fallback, not the hot path](#pasta-an-optional-fallback-not-the-hot-path-call-l32)
   - [Storage when rootless, and what each option guarantees](#storage-when-rootless-and-what-each-option-guarantees-call-l38)
   - [Restarts and reboots](#restarts-and-reboots-call-l33)
   - [The one-time root steps](#the-one-time-root-steps)
   - [Security statement](#security-statement)
   - [The alternative: a system install under a `silo` user](#the-alternative-a-system-install-under-a-silo-user-call-l34)
2. [Storage laid out for later backups](#2-storage-laid-out-for-later-backups)
   - [One data directory](#one-data-directory-call-l35)
   - [Per-silo subtrees](#per-silo-subtrees-call-l36)
   - [The storage driver](#the-storage-driver-call-l37)
   - [Data, and what is not data](#data-and-what-is-not-data)
   - [Copying the state safely](#copying-the-state-safely-call-l39)
   - [Restoring](#restoring-call-l40)
3. [Calls made on this page](#calls-made-on-this-page)
4. [Open questions](#open-questions)
5. [Later](#later)

## 1. Rootless operation

### What "rootless" means here

**silod and everything it starts run without root** (call L26). That
covers containerd, the shims, `runsc`, the granule supervisor,
cloud-hypervisor, buildkitd and the agents' harnesses.

- **silod creates a user namespace and owns a network namespace inside
  it.** That network namespace is `silo-net` from
  [`local-backend.md` §9](local-backend.md#9-the-local-network-fence),
  unchanged. Every bridge, veth, TAP and nftables table of the fence
  lives in it. The host kernel still enforces all of it. Being
  `CAP_NET_ADMIN` in the user namespace that owns `silo-net` is enough
  to create and change them.
- **`/dev/kvm` comes from the `kvm` group**, for granule's `vm` class
  ([KVM for the vm class](#kvm-for-the-vm-class-call-l31)).
- **gVisor runs in a user namespace that silod configured** (the
  "caller-configured" mode in gVisor's guide), for the `sandbox` class
  ([gVisor rootless](#gvisor-rootless-call-l30)).
- **No user-level NAT on the hot path.** Tenant traffic never passes
  through a userspace network stack on its way out. Egress is the
  explicit proxy, as it already is. The proxy's listening socket is in
  `silo-net`, and it dials out from the host network namespace as an
  ordinary user socket
  ([Sockets across the two network namespaces](#sockets-across-the-two-network-namespaces-call-l28)).

The rootful mode in `local-backend.md`
[§12](local-backend.md#privileges-and-kernel-features) (silod as root)
is retired. The supported modes are rootless (the default) and the
[system install](#the-alternative-a-system-install-under-a-silo-user-call-l34),
which runs the same design under a dedicated account.

"Rootless" still uses two setuid helpers that the distribution's
`uidmap` package installs, `newuidmap` and `newgidmap`. They map
`/etc/subuid` ranges into a user namespace and run once each time the
namespace is created. This is the same dependency rootless Docker,
Podman and containerd have. Nothing else on the host runs as root for
silo.

### Who silod runs as

- **By default, silod runs as the user who runs `silo install`.** That
  is the operator's own account. It is the simplest setup, and it is
  the only one in which "agent CLIs run with ambient permissions"
  ([`local-backend.md` §10](local-backend.md#10-agents-on-the-local-backend))
  means the operator's own logins.
- **`--user <name>` runs silod as a dedicated account** that root
  created beforehand. That is the hardening in
  [`local-backend.md` open question 1](local-backend.md#open-questions).
- **The agent user is always silod's user** in both modes. An
  unprivileged silod can't start a process as another uid, so
  `--agent-user` is accepted only if it names silod's own user.
  Harness scopes are transient units in that user's systemd manager
  (`silo-agent-<id>.scope`), and the peer-cgroup check (call L18) is
  unchanged.

### Namespaces: the holder and silod (call L27)

The design needs one property that a single process can't have. silod
must read the **host's** uids from `SO_PEERCRED` on the front door,
and it must also administer `silo-net`. Those two needs pull in
opposite directions:

- **A process inside silod's user namespace sees other users wrongly.**
  `SO_PEERCRED` translates the peer's uid into the receiver's user
  namespace. A host uid that the namespace doesn't map (every user but
  silod's own) arrives as the overflow uid, 65534. The front door's
  uid-to-silo mapping would collapse.
- **A process outside it can't `setns` into `silo-net`.** `setns(2)`
  into a network namespace needs `CAP_SYS_ADMIN` in the caller's own
  user namespace as well as in the target's. An unprivileged process in
  the host's user namespace doesn't have it. A thread of a process
  *inside* the user namespace can enter `silo-net`, but can't come back
  to the host's network namespace. That namespace is owned by the
  initial user namespace, where the process has no capabilities.

So the local backend runs as **two processes** from one binary:

| Process | User namespace | Network namespace | Does |
| --- | --- | --- | --- |
| **`silod`** | The host's | The host's | Everything in `local-backend.md`: the API, the store, the ledger, the fence compiler, the Docker facade, the gateway's proxies and the front door. It runs as silod's user with **no capabilities**. |
| **`silo-nsd`**, the namespace holder | Its own: root inside, mapped to silod's user, plus the `/etc/subuid` range | Holds `silo-net`, and a private mount namespace | Creates and holds the namespaces, and serves silod a narrow privileged RPC (below). containerd, the shims and buildkitd run inside its namespaces. |

- **silod is the owner of the user namespace.** A process whose euid
  owns a user namespace has every capability in it, from the parent
  namespace, without entering it. That is why silod can use netlink and
  nftables sockets that were opened in `silo-net` and handed to it. The
  kernel checks the capability against `silo-net`'s owning user
  namespace, for both the socket's opener and its sender
  (`netlink_ns_capable`) (*verify* on the target kernels).
- **`silo-nsd` is small and single-threaded where it has to be.**
  `setns` into a user namespace is refused to a multi-threaded process,
  which every Go program is. The namespace-joining code therefore runs
  before the Go runtime starts, as runc's `nsexec` does, or in a small
  C helper.
- **The RPC is a socketpair** that silod gets when it connects to
  `$XDG_RUNTIME_DIR/silo/nsd.sock`. The holder accepts only silod's
  uid, by `SO_PEERCRED`. It serves only the operations the fence and
  the drivers need:
  - create, pin and delete a container's network namespace (and its
    user namespace: [Containers in nested user namespaces](#containers-in-nested-user-namespaces-call-l29));
  - create and delete bridges and veths, and set addresses and MACs;
  - open a netlink or nftables socket in `silo-net` and pass it back;
  - open a listening socket in `silo-net` on a given address and port
    and pass it back;
  - fill the dial pool ([next section](#sockets-across-the-two-network-namespaces-call-l28));
  - create, delete and measure a volume's directory, through the
    [storage driver](#the-storage-driver-call-l37), because the files
    in it belong to subuids that silod's own account can't touch.

  Each operation is typed. There is no "run this command", so a
  compromise of silod's RPC client gains nothing that silod's own
  account doesn't already have.
- **Pinning.** Container network namespaces are bind-mounted at
  `/run/silo/netns/<container id>` **inside the holder's private mount
  namespace**. The bind mount keeps a namespace alive without any
  process in it, and the namespace's lifetime is then the holder's
  mount namespace, not silod's.
- **`rootlesskit` with `--net=host`** is how rootless containerd and
  buildkitd usually get this shape. It may serve as `silo-nsd`'s
  namespace-creating half (*verify*). If it does, silod adds the RPC as
  a child process inside it. silod doesn't take rootlesskit's network
  drivers (`slirp4netns`, `pasta`, `vpnkit`): `silo-net` is silod's
  own and has no route out.

### Sockets across the two network namespaces (call L28)

A Linux socket stays in the network namespace it was created in,
wherever its descriptor goes afterwards. That is the whole mechanism:

- **Listening in `silo-net`.** The gateway (`silo.internal`), DNS, the
  Docker endpoint (`docker.silo.internal:2375`), the egress proxy
  (3128), export ports and published ports are listening sockets that
  **`silo-nsd` opens in `silo-net` and passes to silod** with
  `SCM_RIGHTS`. silod accepts on them as on any other socket. Ports
  under 1024 (53, 80) are allowed, because the holder has
  `CAP_NET_BIND_SERVICE` in `silo-net`'s owning user namespace. This
  replaces "setns on a locked OS thread" in
  [`local-backend.md` §9](local-backend.md#topology-call-l8), which
  works only for a root silod.
- **Dialling out.** The egress proxy, image pulls (call L6) and the
  model proxy dial from **silod's own network namespace, the host's**,
  as ordinary user sockets. To the rest of the network they are
  connections from silod's user on this host. Nothing is translated:
  there is no NAT, no TUN device and no userspace TCP stack in between.
  The proxy terminates and re-originates every connection, as it
  already does.
- **Ingress, the other way round.** Exports to the front door,
  `ingress: host` published ports (call L10) and `silo exec`-style
  tunnels listen in the host's namespace and connect **into**
  `silo-net`. Each connection into `silo-net` needs a socket that was
  created there. So silod keeps a **dial pool**: unconnected TCP
  sockets that `silo-nsd` created in `silo-net`, refilled in the
  background. silod takes one per connection, binds it to the
  gateway's address and connects it to the target container. One RPC
  per refill, not per connection.
- **Host ports.** A listener in the host's namespace is an unprivileged
  socket. It can't bind a port under
  `net.ipv4.ip_unprivileged_port_start` (1024 by default), so
  `ingress: host` ports are allocated from 1024 upward.
- **What stays the same.** `silo-net` still has no interface or route
  into the host's namespace (call L16). The only paths between the two
  are silod's sockets, and the conformance suite still checks for
  routes.

### Containers in nested user namespaces (call L29)

In rootless containerd, a container's "root" is by default the root
of the holder's user namespace, which is **silod's own host uid**. An
escape from cloud-hypervisor or from gVisor's gofer would then land in
silod's account, which controls the fence. So:

- **Every container runs in its own user namespace, nested in the
  holder's.** Container uid 0 to 65535 maps to a range of subuids, and
  container root is never silod's uid.
- **Ranges are per silo when `/etc/subuid` has room.** That takes 65536
  ids per silo, so a 64 Ki-silo host needs 2^32 subuids. Install asks
  for a range of that size and reports what it got. When there is room
  for only one range (the common default, 65536), every container shares
  it, and `describe` on the root says `uid_isolation: shared`.
- **The container's network namespace is created inside its user
  namespace.** The granule supervisor's `NET_ADMIN` must cover the
  netns it runs in, and `runsc` must be able to scrape it. `silo-nsd`
  therefore creates the user namespace first and the netns as its
  child, then passes both paths in the OCI spec. As root of the parent
  user namespace, it can still wire the veth between `silo-net` and the
  container's netns (*verify* with runc and runsc joining an existing
  user namespace by path).
- **What the step-15 spike found** (`pkg/backend/local/nsd/testdata/spike.sh`,
  run by CI's `nsd` job as an unprivileged user):
  - netlink and nftables messages from silod, capless in the host's
    namespaces, are accepted in `silo-net` through sockets the holder
    opened and passed. That settles the `netlink_ns_capable` *verify*
    above on the CI kernel (Debian 12, 6.1).
  - **runc** joins the pinned user namespace and netns by path.
    runc before 1.2 also wants the joined namespace's mapping repeated
    in `uidMappings`/`gidMappings`; 1.2 and later warn if it is.
  - **runsc can't join a user namespace by path.** Its gofer calls
    `setns` from a multi-threaded process and gets `EINVAL`. It works
    when runsc is **started inside** the pinned user namespace, with
    only the netns in the spec, which is gVisor's caller-configured
    mode (call L30). netstack then scrapes the veth's address. So the
    gVisor driver (item 7/16) has to start the runsc shim inside each
    container's user namespace, for example through `silo-nsd`'s
    `join` stage, not just name the namespace in the spec.
  - A container's root filesystem has to be on a mount made inside the
    holder's mount namespace, as containerd's snapshot mounts are. On a
    directory inherited from the host's mount namespace, runc's
    `remount-propagation` of the rootfs gets `EPERM` in this setup.
  - Pins live in `$XDG_RUNTIME_DIR/silo/netns/` on a tmpfs the holder
    mounts in its own mount namespace, rather than `/run/silo/netns`,
    which an unprivileged user can't create.
- **The mapping is part of the state.** Files in volumes are owned by
  subuids on disk. If `/etc/subuid` changed between runs, every
  volume's owners would shift. silod records its mapping in the store
  and **refuses to start** if the mapping it gets differs, naming the
  change.

### gVisor rootless (call L30)

gVisor's Rootless guide describes three ways to run rootless. Only one
fits:

| gVisor's method | What it is | For silod |
| --- | --- | --- |
| 1. `runsc --rootless` | runsc re-executes itself in a new user namespace with only the caller mapped | **No.** In gVisor's words: "The create command is not supported … mainly only suitable for `runsc do`", "Save/restore functionality is not supported", and "gVisor's Netstack is not supported, meaning you have to use the host network". The last one alone rules it out (call L13). |
| 2. Caller-configured user namespace (Docker, Podman) | Something like rootlesskit creates the user namespace, and runsc is invoked with uid 0 already established in it, with subuids from `/etc/subuid` through `newuidmap` | **Yes.** This is the holder's shape. gVisor's guide says netstack works in this mode when the caller provides the network devices. Its own example is TAP plus slirp; silod's is a veth in a netns it made. |
| 3. "True runsc rootless mode" | runsc runs as non-root with mappings in `config.json` | **No.** "This method currently lacks support for network namespacing." |

So the `sandbox` class runs `containerd-shim-runsc-v1` inside
`silo-nsd`'s namespaces, exactly as in `local-backend.md`
[§6](local-backend.md#gvisor-driver): `--platform=systrap`,
`--network=sandbox` and `--overlay2=root:self` with a size cap. The
OCI spec names the user namespace and netns that silod prepared, and
runsc scrapes the veth's address, MAC, MTU and routes from it ("runsc
still scrapes the container's network namespace and configures netstack
with the same interface names, MTUs, … addresses, and routes",
Networking guide).

Limits that follow:

- **Checkpoint and restore** are not offered, as already decided in
  `local-backend.md`. Rootless doesn't change that.
- **cgroups** go through the systemd driver in the user's manager
  ([cgroups](#cgroups)). gVisor's systemd driver needs systemd ≥ 244
  and cgroup v2. With a rootless container on cgroup v2, it defaults
  its slice to `user.slice`. silod always sets `CgroupsPath` to its own
  slice.
- **The single-uid fallback isn't used.** gVisor documents an
  unprivileged mapping of size 1 for environments without `newuidmap`.
  Images whose files belong to several uids then can't be unpacked
  without ignoring `chown` errors, which would change images' meaning.
  Install refuses a host without `newuidmap` and a subuid range, and
  names the fix.
- *Verify:* that `containerd-shim-runsc-v1` passes a pre-made user
  namespace path through to runsc in method 2 with `--network=sandbox`.
  The gVisor driver task (item 7) proves it in CI before anything else.

### KVM for the vm class (call L31)

- **silod's user is in the `kvm` group.** `/dev/kvm` is usually
  `root:kvm 0660`. Group membership is a one-time root step
  ([below](#the-one-time-root-steps)).
- **The group has to survive into the container.** Supplementary
  groups are kernel credentials, and a process in a user namespace
  keeps them even if the namespace doesn't map them. But an OCI runtime
  calls `setgroups(2)` with the image's groups when it starts the
  workload, and that drops `kvm`. Two ways to keep it:
  1. **crun's `run.oci.keep_original_groups=1` annotation** (what
     Podman's `--group-add keep-groups` uses) keeps the caller's
     supplementary groups. It needs no root. **This is the default**:
     the `vm` class runs its granule container under crun instead of
     runc. Nothing else about the container changes.
  2. **Map the `kvm` gid** into the holder's namespace with an
     `/etc/subgid` line (`<user>:<kvm gid>:1`, a root step), then add
     it to the container's `additionalGids`. This is the fallback if
     crun turns out not to suit granule's container.
- **Devices are bind mounts.** A rootless runtime can't `mknod` or use
  a device cgroup. `/dev/kvm` and `/dev/net/tun` (mode 0666) are
  bind-mounted into the granule container, which is what rootless
  runtimes do with `--device`.
- **The TAP lives in the container's netns**, created by the granule
  supervisor with `NET_ADMIN` in that netns's user namespace, as in
  `local-backend.md`. Rootless changes nothing there.
- *Verify:* granule's supervisor, netshim and cloud-hypervisor run
  unchanged when container root isn't host root, and with `/dev/kvm`
  bind-mounted rather than created. This is part of plan item 9's
  granule work.

### cgroups

- **silod's slice tree lives in its user's systemd manager**, under
  `user@<uid>.service/silo.slice`, and keeps the shape of
  `local-backend.md` [§11](local-backend.md#enforcement-beside-the-ledger):
  one slice per silo, mirroring the tree, with `cpu.max`, `memory.max`
  and `pids.max`.
- **systemd delegates only some controllers to user managers by
  default:** `cpu`, `memory` and `pids` on current systemd. `io` (for
  `io.stat` metering and I/O limits) and `cpuset` need a drop-in for
  `user@.service` (`Delegate=cpu cpuset io memory pids`), which is a
  one-time root step. Without it, CPU and memory limits hold and I/O is
  neither limited nor metered. `describe` says which.
- **Root capacity** (call L20) is unchanged. The host's totals are what
  they are, and the user slice can't exceed them.

### pasta: an optional fallback, not the hot path (call L32)

`pasta` (from the passt project) attaches a network namespace to the
host without root. It is a fallback, and **silo doesn't use it in
v1**:

- **What it is for.** Direct egress, where a workload opens its own
  connection to the internet rather than going through the egress proxy.
  Silo doesn't allow that today: egress is the explicit proxy, and
  transparent egress is [later](README.md#9-later).
- **What it does.** It terminates the namespace's TCP and UDP in
  userspace and re-originates them as ordinary sockets on the host.
  TCP runs at near line rate. It carries **only TCP, UDP and ICMP
  echo**: no other IP protocols, no raw frames. Outbound traffic
  **appears to come from the host's IP**, so the destination can't tell
  silos apart, and per-silo source addresses don't survive it.
- **Where it would attach.** Never to `silo-net`, which would give
  every silo a route out and undo call L16. If direct egress is ever
  allowed, it would be a per-silo opt-in, granted never-looser by
  ancestors, through a separate egress namespace with its own fence
  rules, reported as `egress: direct` in `describe`.

### Storage when rootless, and what each option guarantees (call L38)

`local-backend.md` assumed XFS project quotas for volumes. An
unprivileged process can't use them: setting a file's project ID is
allowed only from the initial user namespace, and setting quota limits
needs `CAP_SYS_ADMIN` there. Rootless storage is therefore one of these:

| Option | Needs | Volumes | Writable layers | What is guaranteed |
| --- | --- | --- | --- | --- |
| **Plain directories** (the default) | Nothing | A directory per volume under the silo's subtree | gVisor's `--overlay2` size cap, or granule's disk size | **Volumes: charged, not enforced.** The ledger charges each volume's declared size (default 10 GiB) against `disk_gb`. silod measures real use every 5 minutes and at container stop. A volume over its charge is reported in `describe` and audited (`volume_over_quota`), and new containers that mount it are refused with 409, but **writes aren't stopped**. The data filesystem is the only hard limit, so one silo can fill the disk for every silo. **Writable layers: enforced** by the runtime, in both classes. |
| **Overlay** (overlayfs in the holder's user namespace on Linux ≥ 5.11, `fuse-overlayfs` before that) | Nothing | As plain directories | containerd's rootless snapshotter | **The same as plain directories.** overlayfs has no quota of its own. It is how image layers and writable layers are stacked, not a volume quota. |
| **ZFS, delegated** | A one-time root `zfs allow <user> create,destroy,mount,quota,refquota,snapshot,hold,send,userprop <pool>/silo` | A dataset per silo, and a dataset per volume under it | As above | **Volumes: enforced.** `refquota` on each volume's dataset and `quota` on each silo's dataset are hard limits in ZFS, and a silo's total can't exceed its grant even when its volumes are overcommitted. *Verify:* OpenZFS on Linux doesn't let a delegated non-root user **mount** a dataset. Mounting needs either OpenZFS ≥ 2.2's user-namespace delegation (`zfs zone <userns> <dataset>`, run by root each time the holder's user namespace is created), or root-mounted parents with children inheriting mountpoints. Until that is settled, ZFS is optional and not tested in CI. |
| **btrfs** (later) | Mounted with `user_subvol_rm_allowed` (root, once) for deletion | A subvolume per volume | As above | **Charged, not enforced**, as plain directories: qgroup limits need `CAP_SYS_ADMIN`. Its value is cheap unprivileged snapshots, for the [snapshot hook](#the-storage-driver-call-l37). |

Whichever option is chosen:

- **The image cache** is charged to every silo that references an
  image (call L21 unchanged), and garbage-collected when no silo does.
  It is never limited by a kernel quota.
- **Logs** are capped by silod's own writer: size-rotated, and charged
  to `disk_gb`.
- **The [reserve](local-backend.md#root-capacity-and-overcommit-call-l20)**
  (10% of disk by default) is what keeps the filesystem from filling
  up when volumes overrun their charge. Install warns when the data
  directory shares a filesystem with `/home` or `/`.

### Restarts and reboots (call L33)

**Units.** Install writes **systemd user units** in silod's user's
manager, and turns on **lingering** (`loginctl enable-linger <user>`),
so that the manager, and silod with it, starts at boot without a login
and stays after logout:

| Unit | Runs |
| --- | --- |
| `silo-nsd.service` | The holder: user namespace, mount namespace, `silo-net`, `gw0`, the base nftables tables |
| `silo-containerd.service` | containerd, joined to the holder's namespaces (`BindsTo=silo-nsd.service`) |
| `silo-buildkitd.service` | buildkitd for granule wraps, likewise |
| `silod.service` | silod, in the host's namespaces (`After=` the others) |

**silod restarts** (an upgrade or a crash) change nothing in the
namespaces. silod reconnects to `nsd.sock` and asks for its listening
sockets again: they are new sockets, because silod's old ones closed
with it. It then adopts containers exactly as in
[`local-backend.md` §12](local-backend.md#restart-and-adoption): list
the tasks, match them to rows by label, reapply the ruleset and the
pin sets, and only then serve.

**silo-nsd restarts** don't destroy the namespaces. A namespace lives
as long as anything references it: containerd and the shims are in the
holder's user and mount namespaces, and the pinned netns bind mounts
live in that mount namespace. A new holder **adopts** the existing
namespaces. It reads the previous holder's pid from
`$XDG_RUNTIME_DIR/silo/nsd.state`, checks it with a pidfd, or finds a
surviving member (containerd's pid), and joins it through
`/proc/<pid>/ns/{user,mnt,net}`. silod then checks the uid mapping
(call L29). Only if nothing is left does it create fresh namespaces,
which is the reboot case.

**A host reboot** removes every namespace, interface and ruleset.
Nothing about them is on disk. At boot, lingering starts the user
manager and the units:

1. `silo-nsd` creates a fresh user namespace with the same mapping,
   which silod checks against the store, and a fresh `silo-net`.
2. silod loads the store and rebuilds everything that isn't on disk
   from it: bridges, veths, `gw0`'s addresses, the nftables ruleset and
   pin sets, the cgroup slices, and the listening sockets.
3. It restarts containers by their `RestartPolicy`, in tree order, as
   `local-backend.md` §12 says.

**How the disk maps back to the state.** Each running object has one
stable key in the store, and the disk is addressed by those keys
alone:

| On disk | Keyed by | Found again by |
| --- | --- | --- |
| `silos/<silo id>/volumes/<volume id>/` | Silo id, volume id | The volume row |
| `silos/<silo id>/logs/<container id>/` | Container id | The container row |
| `silos/<silo id>/agents/<agent id>/` | Agent id | The agent row |
| `containerd/` namespaces `silo-<silo id>`, containers labelled `dev.silo.container=<id>` | Silo id, container id | containerd's own records, matched to rows by label |
| Network namespaces, bridges, veths, rules, slices | Not on disk | Rebuilt from rows at start |

A directory or containerd record with no row is an **orphan**. A row
with no record is **recreated** ([Restoring](#restoring-call-l40)).

### The one-time root steps

"No root after the packages are installed" means these happen with
the packages, or once by hand. Install checks each and names the
command for any that is missing:

| Step | Why | Without it |
| --- | --- | --- |
| Packages: `uidmap` (`newuidmap`/`newgidmap`), containerd, crun, runsc and its shim, buildkit, optionally `passt` | Everything | Refuse |
| A subuid/subgid range for silod's user (Debian's `useradd` adds 65536 by default) | Nested user namespaces | Refuse |
| Unprivileged user namespaces allowed: `kernel.unprivileged_userns_clone=1` where it exists, and on Ubuntu ≥ 23.10 an AppArmor profile that allows silod's binaries to create them (`kernel.apparmor_restrict_unprivileged_userns`) | The holder | Refuse |
| silod's user in the `kvm` group | The `vm` class | `vm` is off |
| Lingering for silod's user (allowed to the user itself by some polkit policies) | Starting at boot, staying after logout | silod runs only while its user is logged in. Install warns. |
| A `user@.service` drop-in delegating `io` and `cpuset` | I/O limits and metering | I/O is unlimited and unmetered |
| `/run/silo` (a `tmpfiles.d` entry, owned by silod's user, mode 0755) | Front-door sockets other local users can reach | The front door is in `$XDG_RUNTIME_DIR/silo/`, which is mode 0700, so **only silod's own user can reach it** ([open question 14](#open-questions)) |
| `zfs allow` on a dataset | The ZFS storage driver | Plain directories |

None of these grants silod a capability in the host's namespaces.

### Security statement

- **A tenant who escapes into silod's account controls the fence.**
  That account owns `silo-net`, and so the nftables rules, the bridges
  and the pin sets. It also holds the store, the keys, every silo's
  volumes (through the holder) and the ambient agent logins. It can
  open sockets on the host network as that user, and so can reach
  what the egress proxy refuses: localhost services, the LAN, and a
  cloud metadata server. Inside Silo, that is total compromise.
- **Rootless limits the damage to that account and its namespaces.**
  Such an attacker isn't root on the host. It can't load kernel
  modules, change the host's own firewall or routes, read other users'
  files, or touch other users' processes. It also can't keep a foothold
  anywhere silod's account can't write. With silod as root (the first
  draft's mode), the same escape was the whole host.
- **The account is not the first thing a tenant reaches.** Container
  root is a subuid (call L29), so escaping the workload lands in an
  unprivileged uid with no rights over silod's files, the netns or the
  holder's RPC. Reaching silod's account needs a second escape. This
  holds only while the subuid range is separate from silod's uid,
  which install checks.
- **When silod runs as the operator's own account (the default)**, that
  account is the operator's: their files, SSH keys and logins. The
  dedicated `--user` account, or the
  [system install](#the-alternative-a-system-install-under-a-silo-user-call-l34),
  narrows this to an account that holds only silo.
- **What rootless doesn't change.** The host kernel is shared. A
  kernel exploit from a workload (through gVisor's Sentry, or through
  KVM) is still a host compromise, whatever uid it starts from.

### The alternative: a system install under a `silo` user (call L34)

The operator's sketch of the alternative: `silo install` runs once as
root, creates a `silo` user in the `kvm` group, and runs silod under
systemd with `User=silo` and `AmbientCapabilities=CAP_NET_ADMIN`.

One correction: **`CAP_NET_ADMIN` alone can't build `silo-net`.**
Creating a network namespace (`unshare(CLONE_NEWNET)`) and entering one
(`setns`) need `CAP_SYS_ADMIN`, and the gateway's sockets have to be
opened inside it. Adding `CAP_SYS_ADMIN` would make the account nearly
root. So the system install **runs the same design as rootless mode**:
the holder makes a user namespace and owns `silo-net` in it. What
changes is only who sets it up, and how:

| | Rootless (default) | System install |
| --- | --- | --- |
| Account | The operator's own, or `--user` | `silo`, a system user created by `silo install` as root, in `kvm`, with subuid and subgid ranges sized for per-silo ranges (call L29) |
| Units | User units, with lingering | System units: `silo-nsd.service`, `silo-containerd.service`, `silo-buildkitd.service` and `silod.service`, all with `User=silo`, plus `Delegate=yes` on silod's slice |
| Data directory | `$XDG_DATA_HOME/silo` (`~/.local/share/silo`) | `/var/lib/silo` (`StateDirectory=silo`, mode 0700) |
| Front door | `$XDG_RUNTIME_DIR/silo/`, silod's user only, unless `/run/silo` was created | `/run/silo/` (`RuntimeDirectory=silo`, mode 0755), reachable by every local user, as `local-backend.md` describes |
| Agents | Ambient as the operator | Ambient as `silo`: the CLIs' logins must be made as `silo` (`sudo -u silo claude login`) |
| Capabilities | None | **None needed.** `AmbientCapabilities=CAP_NET_BIND_SERVICE` only if `ingress: host` should bind ports under 1024 |
| Root after install | Never | Never. `silo install` and `silo uninstall` run as root, silod never does. |

Whether to drop `CAP_NET_ADMIN` from the operator's sketch is
[open question 13](#open-questions). This page assumes it is dropped,
because nothing in the design uses it in the host's namespaces.

## 2. Storage laid out for later backups

The operator doesn't want a backup system yet. These rules are there
so that adding one later is a new feature, not a migration.

### One data directory (call L35)

**All of silod's persistent state lives under one directory**,
`--data-dir`. Its default is `~/.local/share/silo` in rootless mode and
`/var/lib/silo` for the system install. Nothing persistent is written
anywhere else. Runtime state (sockets, pids, pinned namespaces) is under
`$XDG_RUNTIME_DIR/silo` or `/run/silo`, and is never data.

```
<data-dir>/
  layout.json                     layout version and data-dir id; written once, read at every start
  state/
    silo.db                       the SQLite store (WAL mode)
    silo.db-wal, silo.db-shm      SQLite's own files: never copied on their own
  keys/                           mode 0700: silod's own secrets (below)
  silos/<silo id>/                one subtree per silo: that silo's tenant data
    volumes/<volume id>/          a volume's contents (files owned by the silo's subuids)
    agents/<agent id>/            saved harness sessions (copies; the store is the truth)
    logs/<container id>/          container logs, json-file framing, rotated
    repos/<repo id>.git/          hosted git repos, bare (part 3, local-git.md)
  mirror/                         the uber-repo mirror's own repos (part 3); kept, not precious
  containerd/                     silo-containerd's root: the content store and its metadata (bolt); part 4, call L60
  cache/                          re-creatable; never data
    snapshots/                    the overlayfs snapshotter's root_path: containers' writable layers
    buildkit/                     buildkitd's state for granule wraps
    images-wrapped/               wrap outputs, if outside containerd
    runtimes/                     pinned runsc, shim, granule and probe images
```

- **`layout.json`** holds `{"layout": 2, "data_dir_id": "<random>"}`.
  Layout 1 kept containerd's whole root, snapshots included, at
  `cache/containerd/`; layout 2 is part 4's
  ([call L60](local-registry.md#pushed-images-are-data-call-l60)), and
  its migration moves `cache/containerd/` to `containerd/` and the
  overlayfs snapshotter's state to `cache/snapshots/`, each with one
  rename on one filesystem.
  silod refuses a data directory with a newer layout, and migrates an
  older one forward in place, under the store's own migration rules
  ([`state.md`](state.md)). The id also appears in the store, so that a
  store and a data directory that don't belong together are noticed.
- **`keys/`** holds secrets silod generates or is given: the key that
  encrypts explicit API-key credentials in the store (agents.md
  call 45, when an operator adds one), and anything later pages add.
  Keys are files, never rows. Copying the store doesn't copy a key,
  and a later backup can treat keys under their own policy.
- **This replaces `local-backend.md`'s layout (call L21).** Its content
  (one content store shared by both classes, wrapped images derived
  from the same layers, overlay snapshots for `sandbox`, granule's
  overlay per `vm` container) is unchanged. It has moved under
  `cache/`.

### Per-silo subtrees (call L36)

- **Each silo's tenant data is exactly `silos/<silo id>/`** (or its
  dataset). Nothing outside that subtree is a silo's data, and nothing
  in it belongs to another silo.
- **The subtree is flat, not nested by the tree.** `/a/b`'s data is at
  `silos/<b's id>/`, not under `/a`'s. Moving a silo
  ([`fence-api.md`](fence-api.md)) then moves no files, and a later
  per-silo backup selects silos by the tree in the store, not by
  directory structure. Ids never change, so a path never does.
- **Creating a silo creates its subtree** (through the storage driver's
  `Create`), and deleting it removes the subtree, last in
  [Teardown](local-backend.md#teardown)'s order.
- **Subuid ownership follows the subtree** when ranges are per silo
  (call L29). A later backup run per silo reads one silo's files with
  that silo's ids, inside the holder (`silo unshare`, below).

### The storage driver (call L37)

Volumes, and the per-silo subtrees that hold them, go through one small
interface in `pkg/backend/local/storage`:

```go
// Driver stores silos' tenant data: a subtree per silo, a volume per
// Docker volume. The first implementation is plain directories.
type Driver interface {
	// Name is "dir", "zfs" or "btrfs". It is recorded in the store
	// at install and never changes for a data directory.
	Name() string

	// Guarantees says what this driver enforces: whether volume
	// sizes are hard limits, whether Snapshot works. describe shows it.
	Guarantees() Guarantees

	// Create makes a silo's subtree (ref.Volume empty) or a volume in it,
	// capped at size where the driver enforces sizes. It is
	// idempotent and returns the path the runtime mounts.
	Create(ctx context.Context, ref Ref, size int64) (path string, err error)

	// Delete removes a volume, or a silo's whole subtree. It is idempotent.
	Delete(ctx context.Context, ref Ref) error

	// Usage reports bytes and inodes in use, for metering and the
	// over-quota check.
	Usage(ctx context.Context, ref Ref) (Usage, error)

	// Snapshot is reserved for backups. It makes a read-only,
	// point-in-time copy of a volume or a subtree under name. Every v1
	// driver returns ErrSnapshotUnsupported. No v1 code path calls it.
	Snapshot(ctx context.Context, ref Ref, name string) (Snapshot, error)
}

type Ref struct {
	Silo   ids.SiloID
	Volume string // empty means the silo's subtree
}
```

- **`dir`** is the first and only v1 driver. `Create` is `mkdir` plus
  ownership, `Delete` is a recursive remove, and `Usage` is a walk. Its
  `Guarantees` say "sizes charged, not enforced; no snapshots".
- **It runs in the holder.** File operations on subuid-owned trees
  happen inside `silo-nsd`'s user namespace, where those uids are
  mapped. silod calls the driver through the holder's RPC.
- **`zfs` and `btrfs` come later, behind the same interface.** A
  dataset or subvolume per silo and per volume. Their `Snapshot` works.
  Choosing a driver is an install-time setting, and a data directory
  keeps its driver, so a later driver is for new installs. A move
  between drivers is a copy, outside this design.
- **The interface has no "backup" in it.** A later backup system calls
  `Snapshot`, then reads the snapshot. The facade, the fence API and
  the store don't change.

### Data, and what is not data

| What | Data? | Why |
| --- | --- | --- |
| `state/silo.db` | **Yes** | The tree, the ledger, grants, every row |
| `keys/` | **Yes**, under its own policy | Losing it makes stored API-key credentials unreadable. Copying it copies secrets. |
| `silos/<id>/volumes/` | **Yes** | Tenants' data |
| `silos/<id>/agents/` | Yes, but recoverable | Copies of sessions the store also records. A lost copy ends a session's resume, not the agent. |
| `silos/<id>/logs/` | Kept, not precious | Lost logs are lost history. A backup may skip them. |
| `silos/<id>/repos/` | **Yes** | Hosted git repos ([part 3](local-git.md#5-storage-and-quota)). Copied with `git bundle`, not file by file |
| `mirror/` | Kept, not precious | The submodule index's own history, and the mirror's quarantine. Rebuilt from the store if lost |
| `containerd/` content referenced by a registry repo, and the store's repo and tag rows | **Yes** | The only copy of a pushed image. A later backup exports it per repo as an OCI image layout through the registry, never by copying the directory ([`local-registry.md`, call L60](local-registry.md#pushed-images-are-data-call-l60)) |
| `containerd/` content referenced only by image records or the cache | No | Re-pulled on demand, through the silo's egress policy (call L6) |
| `cache/snapshots/`: **containers' writable layers** | **No, explicitly** | A container is re-created from its row with a fresh writable layer from its image. Docker's own guidance applies: data that matters goes in a volume. `local-backend.md` §14 gets this as a limitation. |
| `cache/buildkit/`, wrapped images, `cache/runtimes/` | **No** | Re-wrapped and re-fetched by digest |

One consequence to state plainly: **an image that has gone from its
registry can't be re-pulled.** A restored silo whose containers name
such an image gets each container back as a row with
`State.Error: image <ref> could not be pulled`. Pinning images for a
silo is in [Later](#later).

### Copying the state safely (call L39)

No backup command ships in v1 (the `silo backup` line in
`local-backend.md` §12 moves to [Later](#later)). What v1 does
guarantee is that the state **can be copied consistently while silod
runs**:

- **The store:** SQLite in WAL mode can be copied online, by either:
  - `sqlite3 state/silo.db "VACUUM INTO '/path/copy.db'"`, which writes
    a compacted, transactionally consistent copy. It is a single
    read transaction, so silod's writers keep going.
  - SQLite's online backup API (`sqlite3_backup_*`, `.backup` in the
    shell), which copies page by page and restarts if a write
    interrupts it.

  Copying `silo.db` with `cp` while silod runs is **not** safe, because
  the WAL holds committed pages that aren't in it yet.
  `silo.db-wal` and `silo.db-shm` are never copied on their own.
- **silod never holds the store in an exclusive lock** outside
  migrations, so either method works from another process as silod's
  user. silod checkpoints the WAL regularly (SQLite's default
  auto-checkpoint), so a copy is never far behind the log.
- **Volumes** have no consistent online copy with the `dir` driver.
  A copy of a running silo's volumes is crash-consistent at best. A
  consistent copy needs the silo's containers stopped, or a driver with
  `Snapshot`. This is why the hook is reserved.
- **Reading volumes** needs the silo's subuids. `silo unshare -- <cmd>`
  runs a command inside the holder's user namespace, as `podman
  unshare` does, so that `tar` or `rsync` see the files' real owners
  and can restore them.

### Restoring (call L40)

[`state.md` SV-54](state.md#backups-and-recovery)'s rule, call 36, applies unchanged:
**a restored silod starts its reconcile in report-only mode.** On this
backend, restoring is putting an older `silo.db` (and, optionally,
older volumes) back into the data directory and starting silod.

- **silod notices a restore by itself.** The store keeps a
  monotonically increasing object sequence. Every container, network
  and volume that silod creates is labelled with the sequence number it
  was created at (`dev.silo.seq` in containerd, and a `.silo-seq` file
  in each volume and subtree). At start, if any object on disk carries a
  sequence number higher than the store's high-water mark, the store is
  older than the disk. silod then:
  - enters report-only mode;
  - audits `restore_detected`, with both numbers;
  - shows it in `describe` on the root until an operator runs
    `silo reconcile --apply`.

  `silod --after-restore` forces the same mode for a restore that also
  replaced the cache.
- **Later workloads show up as orphans, not deletions.** Containers,
  volume directories and subtrees created after the restore point have
  no rows. In report-only mode they are listed, never deleted. The
  orphan list is the record of what happened after the restore point,
  as on GCP.
- **Orphans come back fenced off.** An orphan container's veth has no
  entry in the pin set, and "a port with no entry drops everything"
  ([anti-spoofing](local-backend.md#anti-spoofing)). Its network
  namespace is also never wired to a bridge that the restored store
  doesn't know. So until an operator decides, an orphan can't reach
  any silo or the gateway, and no silo can reach it. An orphan's
  addresses may also be free again in the restored ledger, and the
  missing pin entry is what stops a reuse from colliding.
- **Rows without records are recreated.** A container row whose
  containerd record is gone (the cache wasn't restored, or was wiped)
  is re-created from the row: stopped, with a fresh writable layer,
  and an event saying so. A volume row whose directory is missing is
  reported (`volume_missing`) and not re-created empty, because an
  empty volume that looks like the old one is worse than an error.
- **This makes restoring safe:** nothing silod does after a restore
  destroys anything the restore didn't, and an operator who restored the
  wrong copy can restore again.

## Calls made on this page

L26. **silod runs fully rootless by default:** no root after the
     packages are installed, except the `newuidmap`/`newgidmap` helpers
     every rootless runtime uses. The first draft's root mode is
     retired.

L27. **Two processes, one binary.** silod lives in the host's user and
     network namespaces with no capabilities, so `SO_PEERCRED` sees real
     uids. `silo-nsd` holds the user namespace, a private mount
     namespace and `silo-net`, and serves silod a typed privileged RPC.
     containerd, the shims and buildkitd run in its namespaces.

L28. **Sockets cross namespaces as descriptors.** Listening sockets in
     `silo-net` are made by the holder and passed with `SCM_RIGHTS`.
     Dials out are silod's own sockets in the host namespace. Dials
     into `silo-net` use a pool of sockets the holder made there. There
     is no NAT and no userspace network stack on any tenant path.

L29. **Every container runs in its own user namespace.** Container root
     is a subuid, never silod's uid, with ranges per silo when
     `/etc/subuid` allows. The mapping is recorded, and silod refuses
     to start if it changes.

L30. **gVisor runs in a caller-configured user namespace (gVisor's
     method 2),** with netstack on the netns silod made. `runsc
     --rootless` and gVisor's "true rootless" mode are never used.

L31. **The `vm` class gets `/dev/kvm` through the `kvm` group,** kept
     into the container by crun's `keep_original_groups`, with a
     mapped `kvm` gid as the fallback. Devices are bind mounts.

L32. **pasta is an optional fallback for direct egress only.** It is
     never attached to `silo-net` and is unused in v1, because silo
     doesn't allow direct egress.

L33. **Restarts:** systemd user units with lingering. A silod restart
     re-requests its sockets and adopts. A holder restart adopts the
     surviving namespaces. A reboot rebuilds everything that isn't on
     disk from the store.

L34. **The non-rootless alternative is the same design under a
     dedicated `silo` system user,** set up once by root as system
     units. `CAP_NET_ADMIN` is not granted because nothing uses it
     (open question 13).

L35. **One data directory, `--data-dir`,** with a versioned layout:
     `state/`, `keys/`, `silos/` and `cache/`. Nothing persistent lives
     outside it. This replaces call L21's paths, and keeps its content.

L36. **Each silo's tenant data is exactly `silos/<silo id>/`,** flat
     rather than nested by the tree, so a later backup can be per silo
     and per policy.

L37. **Volumes go through `storage.Driver`** (`Create`, `Delete`,
     `Usage`, and a reserved `Snapshot`). `dir` is the only v1
     driver; `zfs` and `btrfs` come later behind the same interface.

L38. **Disk guarantees are stated per driver.** With `dir` or overlay,
     volumes are charged and not enforced, and writable layers are
     enforced by the runtime. With delegated ZFS, volumes are enforced.
     `describe` says which applies.

L39. **The store is copied online with `VACUUM INTO` or SQLite's backup
     API, never with `cp`.** Container writable layers, images, wraps
     and runtimes are explicitly not data.

L40. **silod detects a restore by object sequence numbers** and starts
     the reconcile in report-only mode (call 36). Later workloads are
     reported as orphans and stay fenced off. Rows without records are
     recreated, except volumes, which are reported missing.

## Open questions

13. **`CAP_NET_ADMIN` in the system install.** The operator's sketch
    grants it ambiently. This page finds no use for it, because
    `silo-net` needs a user namespace anyway (L34). Drop it?
14. **The front door for other local users, rootless.** Without root,
    the sockets are in `$XDG_RUNTIME_DIR` (mode 0700), so only silod's
    user can reach them. Is a single-user rootless host the expected
    case? Or should the packages always create `/run/silo` with
    `tmpfiles.d`?
15. **Hard volume quotas without root.** Accept "charged, not
    enforced" for `dir`? Or add a tiny root-owned helper that only
    assigns XFS project IDs and limits? Or make delegated ZFS the
    recommended setup?
16. **The subuid range.** Ask for 2^32 subuids, for per-silo ranges, or
    accept a shared range (`uid_isolation: shared`) on default hosts?
17. **ZFS mounting under delegation on Linux** (`zfs zone` per user
    namespace, run by root at every holder start). Is ZFS worth
    supporting in rootless mode at all, or only in the system install?
18. **Default account.** Should rootless silod run as the operator by
    default, as this page assumes because the agent CLIs' logins are
    the operator's? Or should it run as a dedicated user, which
    narrows a silod compromise but means logging the CLIs in again?
19. **crun for the `vm` class.** Is changing the granule container's
    runtime from runc to crun acceptable to granule, or should the
    `kvm` gid be mapped instead (a root step)?
20. **Logs as data.** Should container logs be part of a later per-silo
    backup by default, or excluded like the cache?

## Later

- **Backup policy in Silo's own API:** a `backup` policy per silo,
  inherited and never-looser (a child can't back up less often or keep
  copies for less time than its parent requires).
- **Continuous WAL streaming** of the store (for example Litestream) to
  a second location, for point-in-time recovery like Cloud SQL's.
- **Off-host snapshot shipping:** `storage.Driver.Snapshot` plus `zfs
  send` (or btrfs `send`), per silo, to another host or a bucket.
- **`silo backup` and `silo restore`** as commands over the primitives
  above: the store copy, `silo unshare` and the restore rule.
- **Pinning a silo's images** in the cache, so a restore doesn't
  depend on a registry still having them.
- **The `zfs` and `btrfs` storage drivers.**
- **Direct egress through pasta,** if transparent egress is ever
  allowed (call L32).
