# Silo v4 Normative Specification

## 1. Scope and Conventions

This document specifies the architecture, interface contracts, protocol schemas, security controls, and operational lifecycle of Silo v4. Silo v4 is a single-tenant virtual machine manager designed to isolate autonomous coding agents executing with root privileges on a Linux host with KVM.

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "NOT RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in BCP 14 (RFC 2119, RFC 8174).

---

## 2. Architecture and Process Layout

Silo adheres to a single-tenant model: exactly one host `silo` daemon process manages exactly one virtual machine instance executing exactly one coding agent CLI.

```
+-----------------------------------------------------------------------------------------+
| Host OS (Linux Kernel >= 5.15 with /dev/kvm)                                            |
|                                                                                         |
|  +-----------------------------------------------------------------------------------+  |
|  | silo (Host Daemon, root)                                                          |  |
|  |   - Config & State Manager (reads silo.yaml, policy.yaml)                         |  |
|  |   - Hypervisor Controller (manages cloud-hypervisor over Unix socket)             |  |
|  |   - Network & Proxy Engine (nftables, transparent TCP/DNS proxy)                  |  |
|  |   - ACP Host Listener (TCP :port with Bearer token)                               |  |
|  |   - Management & Approval Engine (REST API, OTP state machine, Gmail transport)   |  |
|  +------------------+------------------------------+--------------------+------------+  |
|                     |                              |                    |               |
|      Unix API / Vsock Sockets                      |                    |               |
|                     |                              |                    |               |
|  +------------------v--------------------+         |                    |               |
|  | cloud-hypervisor (Child Process)      |         |                    |               |
|  |  +---------------------------------+  |         |                    |               |
|  |  | Guest VM (1 vCPU..N, Memory)    |  |         |                    |               |
|  |  |                                 |  |         |                    |               |
|  |  |  +---------------------------+  |  |         |                    |               |
|  |  |  | silo-supervisor (Guest    |  |  |         |                    |               |
|  |  |  |   init / root process)    |  |  |         |                    |               |
|  |  |  |   - vsock ACP bridge      |  |  |         |                    |               |
|  |  |  |   - Agent restarter loop  |  |  |         |                    |               |
|  |  |  +-------------+-------------+  |  |         |                    |               |
|  |  |                | (fork/exec)    |  |         |                    |               |
|  |  |  +-------------v-------------+  |  |         |                    |               |
|  |  |  | Agent CLI (claude / codex |  |  |         |                    |               |
|  |  |  |   / gemini) [root]        |  |  |         |                    |               |
|  |  |  +---------------------------+  |  |         |                    |               |
|  |  |                 | (TCP/DNS)     |  |         |                    |               |
|  |  |  Guest Kernel   |               |  |         |                    |               |
|  |  |    eth0 (192.168.127.2/30)      |  |         |                    |               |
|  |  +-----------------+---------------+  |         |                    |               |
|  +--------------------|------------------+         |                    |               |
|                       |                            |                    |               |
|               TAP (silo-tap0)                      |                    |               |
|         Host IP: 192.168.127.1/30                  |                    |               |
|                       |                            |                    |               |
|                       +----------------------------+ (Redirect to proxy)|               |
|                                                                         |               |
|                                                 External Internet <-----+ (Filtered)    |
+-----------------------------------------------------------------------------------------+
```

### 2.1 Process Hierarchy and Roles

1. **Host Daemon (`silo`)**:
   - MUST run as root (`uid=0`, `gid=0`) on the host system to manage `/dev/kvm`, create network TAP devices, configure `nftables` rulesets, and bind privileged ports.
   - MUST spawn and supervise the `cloud-hypervisor` process as a direct child.
   - MUST hold exclusive ownership of the VM lifecycle, storage backings, network policy rules, and management approval workflows.
2. **Virtual Machine Monitor (`cloud-hypervisor`)**:
   - Executed by `silo` with restricted capabilities (`CAP_NET_ADMIN` inherited or granted for TAP interaction).
   - Manages vCPU threads, guest physical memory mapping, virtio block devices, virtio-net, and virtio-vsock.
   - Exposes a UNIX domain socket (`chv-api.sock`) exclusively to `silo`.
3. **Guest Supervisor (`silo-supervisor`)**:
   - Runs as PID 1 or early systemd unit (`silo-supervisor.service`) inside the guest.
   - Connects to host `silo` over virtio-vsock to bridge ACP control streams.
   - Supervises the selected coding agent CLI process. If the agent process terminates with any exit code, `silo-supervisor` MUST restart it within 100 milliseconds with an exponential backoff capped at 5 seconds.
4. **Agent CLI (`claude` / `codex` / `gemini`)**:
   - Runs as root inside the VM to permit arbitrary workspace tool execution, container orchestration inside VM, and OS package updates.
   - Communicates with the model control plane exclusively via ACP bridged through `silo-supervisor`.

### 2.2 IPC Channels

- **Cloud Hypervisor Management**: Host UNIX domain socket at `/run/silo/{vm-name}/chv-api.sock`.
- **Virtio-Vsock**: Host UNIX domain socket at `/run/silo/{vm-name}/chv-vsock.sock`. Host acts as vsock CID 2; Guest acts as vsock CID 3.
  - Vsock Port `5000`: ACP stream bridge.
  - Vsock Port `5001`: In-guest Management REST API transport (optional local loopback alternative to link-local gateway).
- **Network Interface**: Point-to-point TAP interface named `silo-tap-{id}` on the host, bound to `eth0` in the guest.

---

## 3. Hypervisor Abstraction Interface

To allow future support for macOS via Apple's `Virtualization.framework`, all hypervisor interactions MUST be implemented behind the Go `Hypervisor` interface.

```go
package hypervisor

import (
	"context"
	"io"
	"time"
)

// VMConfig encapsulates hypervisor-agnostic virtual machine parameters.
type VMConfig struct {
	Name         string
	CPUs         int
	MemoryMB     int64
	DiskPath     string
	KernelPath   string
	Initramfs    string
	KernelArgs   string
	TapDevice    string
	VsockSocket  string
	APISocket    string
	ConsoleLog   string
}

// VMStatus represents the observed runtime state of the VM.
type VMStatus struct {
	State     State
	PID       int
	Uptime    time.Duration
	ExitError error
}

type State string

const (
	StateCreated    State = "Created"
	StateRunning    State = "Running"
	StatePaused     State = "Paused"
	StateShuttingDown State = "ShuttingDown"
	StateStopped    State = "Stopped"
	StateFailed     State = "Failed"
)

// Hypervisor defines the contract required to drive a single VM instance.
type Hypervisor interface {
	// Create initializes the VM instance and allocates hypervisor resources.
	Create(ctx context.Context, cfg VMConfig) (VMInstance, error)
}

// VMInstance represents an active, instantiated virtual machine.
type VMInstance interface {
	// Start boots the VM kernel.
	Start(ctx context.Context, stdout, stderr io.Writer) error
	// Pause suspends vCPU execution.
	Pause(ctx context.Context) error
	// Resume unpauses vCPU execution.
	Resume(ctx context.Context) error
	// PowerButton triggers ACPI graceful shutdown.
	PowerButton(ctx context.Context) error
	// Kill forces immediate VM termination.
	Kill(ctx context.Context) error
	// Resize dynamically updates vCPU or memory allocations.
	Resize(ctx context.Context, cpus int, memoryMB int64) error
	// Status queries live hypervisor telemetry.
	Status(ctx context.Context) (VMStatus, error)
	// Wait blocks until the hypervisor process exits.
	Wait() error
}
```

The concrete Linux implementation `cloudHypervisor` wraps Cloud Hypervisor CLI execution and UNIX socket HTTP interactions, adapting `gke-labs/granule/pkg/hypervisor`.

---

## 4. Configuration Schema (`silo.yaml`)

The host daemon reads configuration from `silo.yaml`. The schema is defined below:

```yaml
version: "v4"

vm:
  name: "silo-worker-01"
  vcpus: 4
  memory_mb: 8192
  
  storage:
    type: "raw"           # Enum: "raw" (sparse file) | "block" (block device)
    path: "/var/lib/silo/disks/worker-01.img"
    size_gb: 64           # Required if type="raw"
    partition_layout: "two_partition_gpt" # 2-partition GPT layout (p1: boot, p2: root)
    root_filesystem: "xfs" # Primary root filesystem ("xfs" with reflink=1 | "ext4" | "btrfs")
    boot_filesystem: "ext4" # Dedicated boot filesystem ("ext4")
    init_mode: "format_if_empty" # Enum: "format_if_empty" (format only if zeroed/unpartitioned) | "manual" (require explicit initialization)
    base_image: "/var/lib/silo/images/debian-12-base.ext4"
    kernel: "/var/lib/silo/kernel/vmlinux"
    cmdline: "console=ttyS0 root=/dev/vda2 rw quiet panic=-1"

  agent:
    cli: "claude"         # Enum: "claude" | "codex" | "gemini"
    version: "latest"
    environment:
      ANTHROPIC_API_KEY: "secret-key-v1" # Injected in guest for v1
      WORKSPACE_DIR: "/root/workspace"

network:
  tap_name: "silo-tap0"
  host_ip: "192.168.127.1"
  guest_ip: "192.168.127.2"
  subnet_mask: "255.255.255.252"
  dns_port: 5353
  transparent_proxy_port: 8443
  
  # Default LLM provider endpoints allowed without user approval
  default_egress:
    - host: "api.anthropic.com"
      port: 443

acp:
  listen_address: "0.0.0.0"
  port: 9000
  auth_token: "acp_sec_018f3a9b9a8e7e1b"
  vsock_port: 5000

management:
  vsock_port: 5001
  token_path_in_guest: "/etc/silo/mgmt.token"
  token: "mgmt_sec_8f9a2e1d7c4b3a20"
  policy_file: "/var/lib/silo/conf/policy.yaml"

approval:
  otp_length: 6           # Integer: 6 or 8
  otp_ttl_minutes: 15     # Default: 15
  max_attempts: 3         # Lockout after 3 failures
  rate_limit_per_15m: 3   # Max 3 change requests per 15 minutes

email:
  transport: "smtp"       # Enum: "smtp" | "gmail_api"
  to: "operator@example.com"
  
  smtp:
    host: "smtp.gmail.com"
    port: 587
    username: "alerts@example.com"
    app_password: "xxxx-xxxx-xxxx-xxxx"
    from_name: "Silo VM Security"
    
  gmail_api:
    credentials_json: "/etc/silo/gcp-credentials.json"
    token_cache: "/var/lib/silo/gmail-token.json"
```

### 4.1 Schema Validation Rules

1. `vm.vcpus`: Integer, `1 <= vcpus <= host_logical_cores`.
2. `vm.memory_mb`: Integer, `1024 <= memory_mb <= host_available_memory`.
3. `vm.storage.type`: MUST be either `"raw"` or `"block"`.
4. `vm.storage.path`: MUST be an absolute path. If `type="block"`, it MUST identify an existing block device node (`S_ISBLK`).
5. `vm.storage.root_filesystem`: MUST be a supported filesystem identifier (`"xfs"`, `"ext4"`, `"btrfs"`). Defaults to `"xfs"`.
6. `vm.storage.init_mode`: MUST be either `"format_if_empty"` or `"manual"`.
7. `vm.agent.cli`: MUST be one of `"claude"`, `"codex"`, `"gemini"`.
8. `approval.otp_length`: MUST be either `6` or `8`.
9. `approval.otp_ttl_minutes`: MUST be between `1` and `60` (default `15`).

---

## 5. Storage and Rootfs Lifecycle

### 5.1 Disk Persistence and In-Guest Filesystem Flexibility

The virtual machine operates on a persistent root disk. All writes inside the guest OS—including package installations, workspace checkouts, build artifacts, container storage, and toolchains—persist across VM reboots, crashes, and host daemon restarts.

The agent executes with root privileges inside the guest. Crucially, the agent MUST have the capability to reformat partitions, adjust mount options, or switch filesystems (e.g. migrating data volumes to XFS or btrfs) to suit its development and testing needs. The guest kernel (`vmlinux`) includes built-in driver support for `ext4` (`CONFIG_EXT4_FS=y`), `xfs` (`CONFIG_XFS_FS=y`), and `btrfs` (`CONFIG_BTRFS_FS=y`), and the guest image includes userland utilities (`e2fsprogs`, `xfsprogs`, `btrfs-progs`).

### 5.2 Two-Partition GPT Disk Architecture

Silo standardizes on a **two-partition GUID Partition Table (GPT)** layout. Partition 2 is formatted with **XFS** as the default primary root filesystem.

```
+-----------------------------------------------------------------------------------------+
| GPT Disk Backing (Raw Sparse File or Block Partition: 64+ GiB)                          |
|                                                                                         |
|  +-----------------------------+  +--------------------------------------------------+  |
|  | Partition 1: /dev/vda1      |  | Partition 2: /dev/vda2                           |  |
|  | Label: silo-boot            |  | Label: silo-root                                 |  |
|  | Size: 1 GiB                 |  | Size: Remainder of storage (e.g. 63 GiB)          |  |
|  | Filesystem: ext4            |  | Filesystem: XFS (reflink=1, crc=1, ftype=1)      |  |
|  | Mount: /boot                |  | Mount: / (Root Filesystem & Workspace)           |  |
|  | - Kernel & modules          |  | - Guest OS hierarchy (/etc, /usr, /var)          |  |
|  | - Initramfs & boot configs  |  | - Agent workspace (/root/workspace)              |  |
|  | - Safe fallback rescue root |  | - Nested virt storage (/var/lib/docker, containerd|
|  +-----------------------------+  +--------------------------------------------------+  |
+-----------------------------------------------------------------------------------------+
```

#### Rationale for Two Partitions and Default XFS:
1. **Nested Virtualization and Container Cleanliness**: Autonomous coding agents frequently run container engines (Docker, Podman, containerd) and nested microVMs (Cloud Hypervisor / QEMU over `/dev/kvm`) to build containers, run test matrices, and execute untrusted code. Modern container storage drivers (`overlay2`) and VM disk managers require **reflink / Copy-on-Write (CoW)** block allocation (`mkfs.xfs -m reflink=1,crc=1`). Reflink enables instantaneous, zero-copy cloning of container layers and virtual machine images, eliminates physical disk bloat during builds, and prevents known overlayfs inode exhaustion bugs.
2. **Boot Partition Separation**: Direct kernel boot and hypervisor init artifacts reside on Partition 1 (`/boot`, 1 GiB, ext4). Separating `/boot` on ext4 guarantees universal bootloader and direct-boot kernel compatibility, preventing conflicts with advanced XFS filesystem features (such as 64-bit inodes, sparse inobt, and bigtime timestamps).

### 5.3 Filesystem-Agnostic Probing and Explicit Format Safety Protocol

#### The Silent Overwrite Hazard
If Silo performed naive, single-filesystem probing (e.g. checking only whether an ext4 superblock exists), any guest that reformatted its root partition to XFS would be misdiagnosed upon daemon restart as "unformatted storage". Silo would then execute `mkfs` and extract the base image, catastrophically destroying the user's running system.

#### Mandatory Probing Algorithm
On daemon startup, Silo MUST probe the backing storage using filesystem-agnostic signature detection (`libblkid` or scanning standard magic offsets):
1. **GPT Partition Table Probe**: Check for GPT signature (`"EFI PART"` at LBA 1).
2. **Partition 1 Superblock Probe**: Probe `/dev/vda1` (or offset `2048 * 512`) for ext4 magic (`0xEF53`).
3. **Partition 2 Superblock Probe**: Probe `/dev/vda2` for recognized superblocks:
   - XFS: Magic `"XFSB"` (`0x58465342`) at offset 0.
   - ext4: Magic `0xEF53` at offset 1024.
   - btrfs: Magic `"_B5HRfS_M"` at offset 65536.
   - swap, f2fs, and other recognized Linux filesystem signatures.

**Safety Rule**: If ANY recognized partition table or filesystem superblock is detected, Silo MUST treat the disk as initialized and boot directly without modifying, repartitioning, or formatting the storage.

#### Explicit First-Start Initialization State Machine

```
+-------------------------------------------------------------+
| Check backing storage at vm.storage.path                    |
+------------------------------+------------------------------+
                               |
            +------------------v------------------+
            | Does ANY valid filesystem or GPT    |
            | partition signature exist? (blkid)  |
            +-------+---------------------+-------+
                    | Yes                 | No (Empty / Zeroed)
                    |                     |
+-------------------v---------------+   +-v-----------------------------------------+
| Keep existing partitions intact.  |   | Check vm.storage.init_mode:               |
| NEVER format or overwrite!        |   | - If "manual": Abort with error.          |
| Proceed to boot VM kernel.        |   | - If "format_if_empty":                   |
+-------------------+---------------+   |   1. Create sparse file (if type="raw")   |
                    |                   |   2. Write GPT table (p1: 1G, p2: rest)   |
                    |                   |   3. mkfs.ext4 -L "silo-boot" /dev/vda1   |
                    |                   |   4. mkfs.xfs -m reflink=1 /dev/vda2      |
                    |                   |   5. Extract Debian base image to vda2    |
                    |                   |   6. Populate /boot on vda1               |
                    |                   |   7. Inject /etc/silo/mgmt.token          |
                    |                   |   8. Sync & flush buffers                 |
                    |                   +---------------------+---------------------+
                    |                                         |
                    +-------------------+---------------------+
                                        |
                         +--------------v--------------+
                         | Launch Cloud Hypervisor VMM |
                         +-----------------------------+
```

### 5.4 In-Guest Explicit Storage Formatting (`POST /v1/storage/format`)

When an agent needs to reformat its root or data volume (e.g. switching between XFS and btrfs, or wiping temporary build partitions), formatting is exposed as an **explicit, OTP-gated management operation**:

1. The agent calls `POST /v1/storage/format` specifying `target: "root"`, desired `filesystem: "xfs"`, and `mkfs_options: ["-m", "reflink=1"]`.
2. Silo creates a pending change record and generates an email alert with a destructive warning diff.
3. The operator reviews the diff and provides the single-use OTP code to the agent.
4. The agent submits the code to `POST /v1/mgmt/approve`.
5. Upon approval, Silo pauses the VM, unmounts the partition, runs `mkfs`, optionally re-seeds the base image if requested, and resumes/reboots the VM.

### 5.5 Base Image Construction

The default base image is a reproducible Debian 12 (Bookworm) minimal rootfs derived from Granule's `deploy/guest-image/` build pipeline:
1. `debootstrap --variant=minbase --components=main bookworm /target http://deb.debian.org/debian`
2. Install packages: `systemd-sysv`, `iproute2`, `kmod`, `acpid`, `udev`, `e2fsprogs`, `xfsprogs`, `btrfs-progs`, `ca-certificates`, `curl`, `git`, `build-essential`.
3. Overlay Silo components:
   - `/usr/local/bin/silo-supervisor`: Supervisor daemon and vsock bridge.
   - `/etc/systemd/system/silo-supervisor.service`: Autostart unit.
   - Node.js / Python / Go runtimes required by Claude Code, Codex, and Gemini CLI.
4. Direct-boot kernel (`vmlinux`) compiled with `CONFIG_VIRTIO_PCI=y`, `CONFIG_VIRTIO_BLK=y`, `CONFIG_VIRTIO_NET=y`, `CONFIG_VHOST_VSOCK=y`, `CONFIG_XFS_FS=y`, and `CONFIG_EXT4_FS=y`.

---

## 6. Network Topology and Enforcement

### 6.1 Point-to-Point Addressing (Dual-Stack)

Silo provisions a dual-stack point-to-point network between the host TAP device and the guest virtual machine:

- **IPv4 Point-to-Point Link**:
  - Host Interface (`silo-tap0`): `192.168.127.1/30`
  - Guest Interface (`eth0`): `192.168.127.2/30`
  - Broadcast: `192.168.127.3`
  - Subnet: `192.168.127.0/30`

- **IPv6 Point-to-Point Link (ULA)**:
  - Host Interface (`silo-tap0`): `fd00:silo::1/126`
  - Guest Interface (`eth0`): `fd00:silo::2/126`
  - Subnet: `fd00:silo::/126`

### 6.2 Host nftables Ruleset

Silo dynamically provisions an isolated `nftables` table named `silo_vm` in the `inet` address family upon startup. Because the `inet` family inspects both IPv4 and IPv6 packets simultaneously, the ruleset guarantees that guest traffic can NEVER bypass Silo:

```nftables
table inet silo_vm {
    chain prerouting {
        type nat hook prerouting priority dstnat; policy accept;

        # Redirect all guest DNS queries (IPv4 and IPv6) to Silo internal DNS proxy
        iifname "silo-tap0" udp dport 53 redirect to :5353
        iifname "silo-tap0" tcp dport 53 redirect to :5353

        # Redirect all guest TCP traffic (IPv4 and IPv6) to Silo transparent forward proxy
        iifname "silo-tap0" tcp dport != { 53, 5001 } redirect to :8443
    }

    chain forward {
        type filter hook forward priority filter; policy drop;

        # Strictly drop all forward frames originating from the tap interface.
        # No guest traffic (IPv4 or IPv6) is permitted to route directly onto host interfaces!
        iifname "silo-tap0" drop
    }

    chain input {
        type filter hook input priority filter; policy drop;

        # Allow established connections from proxy
        ct state established,related accept

        # Allow IPv4 guest to communicate with Silo transparent proxy and DNS
        iifname "silo-tap0" ip saddr 192.168.127.2 ip daddr 192.168.127.1 tcp dport { 8443, 9090 } accept
        iifname "silo-tap0" ip saddr 192.168.127.2 ip daddr 192.168.127.1 udp dport 5353 accept
        iifname "silo-tap0" ip saddr 192.168.127.2 icmp type echo-request accept

        # Allow IPv6 guest to communicate with Silo transparent proxy and DNS
        iifname "silo-tap0" ip6 saddr fd00:silo::2 ip6 daddr fd00:silo::1 tcp dport { 8443, 9090 } accept
        iifname "silo-tap0" ip6 saddr fd00:silo::2 ip6 daddr fd00:silo::1 udp dport 5353 accept
        iifname "silo-tap0" ip6 saddr fd00:silo::2 icmpv6 type echo-request accept

        # Allow IPv6 Neighbor Discovery for point-to-point link operation
        iifname "silo-tap0" icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert } accept

        # Drop everything else from the TAP
        iifname "silo-tap0" drop
    }

    chain output {
        type filter hook output priority filter; policy accept;
    }
}
```

### 6.3 Leak Prevention and Enforcement Proof

This section demonstrates why a process running with root privileges inside the guest virtual machine cannot circumvent Silo's enforcement under any vector:

1. **IPv6 Mediation (Why IPv6 Is Supported and Cannot Bypass Silo)**:
   - Modern development tools, package registries (PyPI, npm, GitHub, Debian mirrors), and enterprise cloud VPCs routinely operate dual-stack. Disabling IPv6 causes artificial Happy Eyeballs connection timeouts (RFC 8305 delay penalties while waiting for IPv6 connection drops) or complete network failure in IPv6-only environments.
   - Silo handles IPv6 with the exact same zero-bypass security posture as IPv4:
     - The `table inet` rules apply to IPv4 and IPv6 packets without distinction.
     - All outbound IPv6 TCP SYN packets are redirected by nftables (`redirect to :8443`) into Silo's dual-stack transparent forward proxy listener bound to `[::]:8443`.
     - Kernel packet forwarding is disabled on the host TAP device, and the `forward` chain drops all frames originating from `silo-tap0` (`iifname "silo-tap0" drop`).
     - IPv6 Router Advertisements (RA) and SLAAC are disabled on the host TAP (`net.ipv6.conf.silo-tap0.accept_ra=0`, `net.ipv6.conf.silo-tap0.forwarding=0`), preventing rogue default gateway spoofing.
2. **Metadata Protection**:
   - Host nftables rules reject all traffic targeting IPv4 cloud metadata (`169.254.169.254`), IPv4 link-local (`169.254.0.0/16`), and IPv6 cloud metadata (`fd00:ec2::254`, `[fe80::169:254:169:254]`).
3. **Host LAN and Intranet Isolation**:
   - Outbound attempts targeting RFC 1918 subnets (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), host loopback (`127.0.0.0/8`), or IPv6 ULA/site-local ranges outside the point-to-point link are blocked by default unless explicitly allowed by an approved policy rule.
4. **Raw Sockets and Non-TCP/UDP Protocols**:
   - Even if guest root creates raw sockets (`SOCK_RAW`) to craft raw IP, GRE, SCTP, or ICMP tunneling frames, the host kernel NEVER routes them: `net.ipv4.ip_forward` is not enabled for the TAP, the `forward` chain drops all frames, and the `input` chain drops any non-TCP/UDP packet not matching point-to-point ICMP echo.
5. **Vsock Isolation**:
   - Virtio-vsock is an isolated host-guest hypervisor channel (`VMADDR_CID_HOST` = 2, `VMADDR_CID_LOCAL` = 3). Vsock carries no IP routing and cannot pivot to external host network devices. Only ports explicitly opened by Silo (ACP 5000, Management API 5001) listen for connections.

### 6.4 DNS Proxy and Domain Resolution

Silo operates an internal DNS proxy on port `5353`. When the guest issues a DNS query:
1. Silo checks the requested FQDN against the active egress policy.
2. If the FQDN matches an allowed policy rule, Silo resolves the upstream DNS record, caches the mapping `(IP -> FQDN, TTL)`, installs temporary destination IP allowlist entries in the proxy filter, and returns the real IP to the guest.
3. If the FQDN is not permitted by policy, Silo returns an `NXDOMAIN` response and logs the unauthorized lookup attempt.

---

## 7. Policy Interface and Persistence

### 7.1 Schema (`policy.yaml`)

Silo adopts a declarative, Cilium-inspired egress specification. Active rules are persisted in `policy.yaml`:

```yaml
version: "v4"
rules:
  # Base default rules for agent operation
  - name: "llm-api-anthropic"
    description: "Allow Claude Code to reach Anthropic API"
    action: "Allow"
    egress:
      - toFQDN: "api.anthropic.com"
        ports:
          - port: 443
            protocol: "TCP"

  # User-approved custom egress rule
  - name: "github-access"
    description: "Allow git clone and push to GitHub"
    action: "Allow"
    egress:
      - toFQDN: "github.com"
        ports:
          - port: 443
            protocol: "TCP"
      - toFQDN: "api.github.com"
        ports:
          - port: 443
            protocol: "TCP"

  # User-approved CIDR egress rule
  - name: "internal-database"
    description: "Access staging Postgres instance"
    action: "Allow"
    egress:
      - toCIDR: "10.200.1.50/32"
        ports:
          - port: 5432
            protocol: "TCP"

port_forwards:
  - name: "web-dev-server"
    description: "Expose Vite dev server running in guest"
    host_ip: "127.0.0.1"
    host_port: 5173
    guest_port: 5173
    protocol: "TCP"
```

### 7.2 Persistence and Reload Semantics

1. Every policy modification MUST be atomically written to `policy.yaml.tmp` and renamed to `policy.yaml` (`rename(2)`).
2. Upon restart, Silo reloads `policy.yaml` and re-synchronizes its proxy filters and port forwarding listeners.
3. If `policy.yaml` is corrupted or absent, Silo falls back to the default minimal policy specified in `silo.yaml` (LLM endpoint only).

---

## 8. Agent Client Protocol (ACP) and Control Plane

### 8.1 Host Listener

Silo opens a TCP listener on the host at `acp.listen_address:acp.port` (e.g. `0.0.0.0:9000`). All incoming connections MUST present the bearer token configured in `acp.auth_token` via the initial ACP connection handshake or HTTP/WebSocket upgrade header:

```http
Authorization: Bearer <acp.auth_token>
```

Connections lacking a valid token MUST be terminated immediately.

### 8.2 Vsock Bridging Architecture

Silo bridges host ACP streams into the guest over virtio-vsock port `5000`:
1. Host caller dials `host_ip:acp_port`.
2. Silo validates the bearer token.
3. Silo dials the guest vsock endpoint (`cid=3, port=5000`).
4. In-guest `silo-supervisor` accepts the vsock connection and connects it to the agent CLI's ACP stdio pipes or local loopback listener.
5. Bidirectional byte streaming is executed using zero-copy splicing (`splice(2)` or Go `io.Copy`).

### 8.3 Remote Control Seam

To accommodate future Claude Code and OpenAI Codex "remote control" architectures (where external web sessions or cloud coordination hubs orchestrate the agent), Silo defines the `ControlMode` interface:

```go
type ControlMode interface {
	// Name returns the identifier of the control mode (e.g., "acp", "claude-remote").
	Name() string
	// Start initiates the control channel listeners.
	Start(ctx context.Context, vsockDialer VsockDialer) error
	// Stop tears down control channels.
	Stop() error
}
```

---

## 9. In-Guest Agent and Supervisor

### 9.1 Supported Agent CLIs

| Agent CLI | Binary Name | Default ACP Command / Invocation |
|-----------|-------------|----------------------------------|
| Claude Code | `claude` | `claude --acp --non-interactive` |
| OpenAI Codex | `codex` | `codex server --acp` |
| Gemini CLI | `gemini` | `gemini-cli --acp-mode` |

### 9.2 In-Guest Prompt Template

The supervisor initializes the agent session with the following normative prompt template:

```
You are executing inside an isolated, single-tenant Linux virtual machine managed by Silo v4.
Your process runs with root privileges inside this guest environment.

ENVIRONMENT CONSTRAINTS:
1. All network egress is transparently mediated by the host Silo daemon.
2. Direct network connections outside your assigned policy are blocked.
3. Currently active egress endpoints:
{{range .ActiveRules}}
   - {{.Name}}: {{.Description}} ({{.Spec}})
{{end}}

MANAGEMENT & RECONFIGURATION:
You have access to the Silo Management REST API at: http://192.168.127.1:9090
Management Token: {{.ManagementToken}}

If you need to access external resources (new package registries, GitHub repositories, databases, web APIs) or expose local ports to the user:
1. Submit an egress or port-forward request to POST /v1/policy/rules or POST /v1/ports/expose.
2. Silo will email a human-readable diff and a single-use One-Time Password (OTP) to the operator.
3. Inform the operator that you have requested this access, and ask them for the approval code.
4. When the operator provides the code, submit it to POST /v1/mgmt/approve.
5. Do not attempt to bypass network restrictions directly.

Awaiting instructions from the user over ACP.
```

### 9.3 Agent CLI Version Management and Upgrades

Silo explicitly supports upgrading the coding agent CLI inside the virtual machine through two complementary mechanisms:

1. **In-Guest Self-Upgrade**:
   Because the agent runs as root, it can execute native package update commands directly inside the guest whenever network egress to the corresponding package repository is permitted by policy:
   - Claude Code: `npm install -g @anthropic-ai/claude-code@latest`
   - Codex: `npm install -g @openai/codex@latest` or binary replacement
   - Gemini CLI: `npm install -g @google/gemini-cli@latest` or Go install
   
   When the package update finishes, the CLI process exits or issues `kill(getpid(), SIGTERM)`. `silo-supervisor` traps process termination and immediately relaunches the updated CLI executable, picking up the new version without requiring a VM reboot.

2. **Host-Managed CLI Upgrade (`POST /v1/agent/upgrade`)**:
   The agent can also request Silo to manage and pin its CLI version in `silo.yaml`:
   - The agent calls `POST /v1/agent/upgrade` with the target `version` string (e.g. `"1.3.0"` or `"latest"`).
   - Silo formats a unified diff of `silo.yaml` (`vm.agent.version`) and dispatches an OTP approval email to the operator.
   - Upon operator approval via `POST /v1/mgmt/approve`, Silo signals `silo-supervisor` over vsock to execute the package installation, verify the binary invocation via `<cli> --version`, update `silo.yaml` on the host, terminate the active CLI PID, and relaunch the agent on the new version.
   - If the new binary fails validation or crashes upon launch, `silo-supervisor` automatically rolls back to the previous package version.

---

## 10. LLM Credential Handling

### 10.1 v1 Implementation: In-Guest Key Injection

In Silo v1, provider credentials (e.g. `ANTHROPIC_API_KEY`) are injected into the agent environment inside the guest by `silo-supervisor`. This provides zero-cost compatibility with standard provider CLI binaries.

### 10.2 v2 Implementation: Proxy Injection Architecture

Under v2, Silo eliminates credentials from the guest entirely:
1. Provider API keys reside exclusively in host memory within `silo`.
2. Silo generates a local ephemeral root Certificate Authority (`Silo Local CA`) and installs the CA public cert into `/etc/ssl/certs/` inside the guest image on first boot.
3. The transparent proxy terminates TLS connections directed to `api.anthropic.com` / `api.openai.com`.
4. Silo injects the appropriate authentication header (`x-api-key: ...` or `Authorization: Bearer ...`).
5. Silo establishes a fresh TLS connection to the upstream provider and proxies requests and SSE response streams.

### 10.3 Migration Cost Analysis

| Component | v1 (In-Guest Key) | v2 (Proxy Injection) | Cost of Transition |
|-----------|-------------------|----------------------|--------------------|
| Credential Storage | Guest memory & env | Host process memory | Minimal; move keys to host `silo.yaml` |
| Guest Trust Store | Stock Debian CA certs | Custom CA cert injected | Must provision local CA cert into base image |
| Proxy Overhead | Transparent TCP SNI proxy | Full TLS MITM termination | Increased CPU usage for crypto; SSE buffering |
| Threat Posture | Compromised root steals API key | Compromised root CANNOT steal API key | Primary security benefit |

---

## 11. In-Guest Management REST API

The Management API is accessible to the guest at `http://192.168.127.1:9090`. All requests MUST include the header `Authorization: Bearer <mgmt.token>`.

### 11.1 API Endpoints Catalog

#### 1. `GET /v1/status`
Returns VM telemetry and pending approvals.

**Response (200 OK)**:
```json
{
  "vm_name": "silo-worker-01",
  "status": "Running",
  "uptime_seconds": 14205,
  "vcpus": 4,
  "memory_mb": 8192,
  "agent": {
    "cli": "claude",
    "status": "Running",
    "pid": 284
  },
  "pending_approval": {
    "change_id": "chg_9a8b7c6d",
    "type": "PolicyUpdate",
    "created_at": "2026-11-01T00:15:00Z",
    "expires_at": "2026-11-01T00:30:00Z"
  }
}
```

#### 2. `POST /v1/policy/rules`
Requests the addition or replacement of an egress policy rule.

**Request**:
```json
{
  "name": "npm-registry",
  "description": "Allow downloading node dependencies",
  "egress": [
    {
      "toFQDN": "registry.npmjs.org",
      "ports": [{ "port": 443, "protocol": "TCP" }]
    }
  ]
}
```

**Response (202 Accepted)**:
```json
{
  "change_id": "chg_9a8b7c6d",
  "status": "PENDING_APPROVAL",
  "diff": "--- policy.yaml\n+++ policy.yaml\n+  - name: npm-registry\n+    toFQDN: registry.npmjs.org:443\n",
  "expires_in_seconds": 900,
  "message": "Approval code sent to operator email. Await operator code and call /v1/mgmt/approve."
}
```

#### 3. `DELETE /v1/policy/rules/{name}`
Requests deletion of an existing policy rule. Triggers email OTP approval.

#### 4. `POST /v1/ports/expose`
Requests exposing a guest service on the host.

**Request**:
```json
{
  "name": "local-api",
  "description": "Expose guest test backend on host",
  "host_port": 8080,
  "guest_port": 8080,
  "protocol": "TCP"
}
```

**Response (202 Accepted)**: Triggers email OTP flow with port-forward diff.

#### 5. `DELETE /v1/ports/{name}`
Requests unexposing a host port. Triggers email OTP approval.

#### 6. `POST /v1/vm/resize`
Requests dynamically resizing CPU or memory allocations.

**Request**:
```json
{
  "vcpus": 8,
  "memory_mb": 16384
}
```

#### 7. `POST /v1/vm/restart`
Requests a clean ACPI restart of the virtual machine. Triggers email OTP approval.

#### 8. `POST /v1/storage/format`
Requests explicitly reformatting a guest partition (e.g. converting or re-initializing the root partition as XFS, ext4, or btrfs).

**Request**:
```json
{
  "partition": "root",
  "filesystem": "xfs",
  "mkfs_options": ["-m", "reflink=1", "-b", "size=4096"],
  "reapply_base_image": false
}
```

**Response (202 Accepted)**:
```json
{
  "change_id": "chg_4e8a1c90",
  "status": "PENDING_APPROVAL",
  "diff": "--- storage.status\n+++ storage.status\n- /dev/vda2: ext4 (63GB)\n+ /dev/vda2: xfs [reflink=1] (63GB) [DESTRUCTIVE REFORMAT]\n",
  "expires_in_seconds": 900,
  "message": "Destructive format approval code sent to operator email. Await operator code and call /v1/mgmt/approve."
}
```

#### 9. `POST /v1/agent/upgrade`
Requests upgrading the coding agent CLI version in the guest and pinning it in `silo.yaml`.

**Request**:
```json
{
  "version": "1.3.0",
  "revert_on_failure": true
}
```

**Response (202 Accepted)**:
```json
{
  "change_id": "chg_7c1d3e8f",
  "status": "PENDING_APPROVAL",
  "diff": "--- silo.yaml (vm.agent)\n+++ silo.yaml (vm.agent)\n- version: 1.2.0\n+ version: 1.3.0\n",
  "expires_in_seconds": 900,
  "message": "Agent CLI upgrade approval code sent to operator email. Await operator code and call /v1/mgmt/approve."
}
```

#### 10. `POST /v1/silo/upgrade`
Requests self-upgrade of the host Silo daemon.

**Request**:
```json
{
  "release_tag": "v4.1.0"
}
```

#### 11. `POST /v1/mgmt/approve`
Submits the human operator's OTP code to execute the pending mutation.

**Request**:
```json
{
  "change_id": "chg_9a8b7c6d",
  "otp": "839201"
}
```

**Response (200 OK)**:
```json
{
  "change_id": "chg_9a8b7c6d",
  "status": "APPLIED",
  "applied_at": "2026-11-01T00:16:30Z"
}
```

#### 12. `POST /v1/mgmt/cancel`
Cancels a pending change request.

### 11.2 Standard Error Response Schema

```json
{
  "error": {
    "code": "INVALID_OTP",
    "message": "The provided approval code is incorrect. 2 attempts remaining.",
    "details": {
      "attempts_remaining": 2
    }
  }
}
```

| HTTP Status | Error Code | Description |
|-------------|------------|-------------|
| 400 Bad Request | `INVALID_PAYLOAD` | Malformed JSON or invalid schema parameters |
| 401 Unauthorized | `UNAUTHORIZED` | Missing or invalid management token |
| 403 Forbidden | `INVALID_OTP` | Incorrect OTP entered |
| 403 Forbidden | `OTP_EXPIRED` | OTP expiration deadline (15 min) has passed |
| 403 Forbidden | `CHANGE_LOCKED` | Maximum failed attempts reached; change revoked |
| 409 Conflict | `CHANGE_IN_PROGRESS` | Another change is already awaiting approval |
| 429 Too Many Requests | `RATE_LIMITED` | Exceeded 3 requests per 15-minute window |
| 500 Internal Error | `SYSTEM_ERROR` | Hypervisor or host OS error |

---

## 12. One-Time Password (OTP) and Approval State Machine

```
                      +-------------------+
                      |       IDLE        |
                      +---------+---------+
                                |
             POST /v1/policy or | 1. Calculate SHA256(canonical_json)
             POST /v1/ports etc.| 2. Generate 6/8-digit OTP
                                | 3. Send email to operator
                                v
                      +-------------------+
        +------------>| PENDING_APPROVAL  |---------------+
        |             +---+-----------+---+               |
        |                 |           |                   |
        | Invalid OTP     |           | Valid OTP         | TTL Exceeded
        | (attempts < 3)  |           |                   | (15 minutes)
        |                 |           v                   |
        |            +----+----+  +---+---------+         v
        +------------+ RETRY   |  |  APPROVED   |    +----+----+
                     +---------+  +---+---------+    | EXPIRED |
                                      |              +---------+
         Invalid OTP                  | Apply Change
         (attempts >= 3)              v
              +------------+     +----+---------+
              |   LOCKED   |     |   APPLIED    |
              +------------+     +--------------+
```

### 12.1 Cryptographic Change Binding

The OTP is cryptographically bound to the change payload. Silo computes:
$$\text{ChangeHash} = \text{HMAC-SHA256}(K_{\text{silo}}, \text{CanonicalJSON}(\text{RequestPayload}))$$
If the agent issues another request or mutates the pending payload, the change ID and hash change, instantly invalidating the issued OTP.

### 12.2 Brute-Force and Rate-Limiting Controls

1. **Attempt Throttling**: A maximum of `3` invalid OTP submissions are permitted per change ID. On the 3rd failure, the state machine transitions to `LOCKED` and the change is permanently deleted.
2. **Global Velocity Limit**: No more than `3` mutating change requests may be initiated within any 15-minute moving window.
3. **Time-To-Live (TTL)**: OTP codes expire exactly `15 minutes` after creation.

---

## 13. Email Transport

### 13.1 Comparison: SMTP vs Gmail API

| Dimension | SMTP with App Password | Gmail API with OAuth 2.0 |
|-----------|------------------------|--------------------------|
| Configuration Complexity | Low: Server, Port, User, App Password | High: GCP Project, OAuth Client ID, Consent Screen, Scopes |
| Dependencies | Go standard library (`net/smtp`) | GCP Client Libraries (`google.golang.org/api/gmail/v1`) |
| Headless Host Ergonomics | Seamless; static credentials | Requires initial interactive OAuth consent flow |
| Token Lifecycle | Static unless revoked | Requires refresh token management and token rotation |
| Operational Reliability | High; widely supported | Subject to API quotas and GCP OAuth permission changes |

**Recommendation**: Silo adopts **SMTP with an App Password** as the standard, recommended transport. Gmail API OAuth 2.0 is supported as an optional enterprise transport for organizations prohibiting App Passwords.

### 13.2 Email Notification Template

```email
Subject: [Silo Security Alert] Approval Required: Policy Change on silo-worker-01
To: operator@example.com
From: "Silo VM Security" <alerts@example.com>
Content-Type: multipart/alternative; boundary="boundary-silo-01"

--boundary-silo-01
Content-Type: text/plain; charset="utf-8"

Silo VM 'silo-worker-01' has requested a system modification.

CHANGE DETAILS:
Type: Network Policy Update
Change ID: chg_9a8b7c6d
Digest: a3f1e9c2b4...8d9e

PROPOSED CONFIGURATION DIFF:
--- policy.yaml
+++ policy.yaml
@@ -14,3 +14,7 @@
+  - name: npm-registry
+    description: Allow downloading node dependencies
+    toFQDN: registry.npmjs.org
+    ports: [443/TCP]

APPROVAL CODE (Valid for 15 minutes):
========================================
             839201
========================================

To approve this change, copy the 6-digit code above and provide it to your agent in your active ACP chat session.
If you did not authorize this change, ignore this email or cancel the change in the CLI.

--boundary-silo-01
Content-Type: text/html; charset="utf-8"

<html>
<body style="font-family: sans-serif; line-height: 1.5; color: #1f2937;">
  <h2 style="color: #111827;">Silo Security Approval Request</h2>
  <p>Virtual Machine <strong>silo-worker-01</strong> has requested a configuration change.</p>
  
  <div style="background-color: #f3f4f6; padding: 12px; border-radius: 6px; font-family: monospace;">
    <strong>Change ID:</strong> chg_9a8b7c6d<br>
    <strong>Action:</strong> Network Egress Policy Update
  </div>

  <h3>Proposed Diff</h3>
  <pre style="background: #1e1e1e; color: #d4d4d4; padding: 12px; border-radius: 6px; overflow-x: auto;">
--- policy.yaml
+++ policy.yaml
<span style="color: #4ec9b0;">+  - name: npm-registry</span>
<span style="color: #4ec9b0;">+    description: Allow downloading node dependencies</span>
<span style="color: #4ec9b0;">+    toFQDN: registry.npmjs.org</span>
<span style="color: #4ec9b0;">+    ports: [443/TCP]</span>
  </pre>

  <div style="margin: 24px 0; padding: 16px; background-color: #ecfdf5; border: 1px solid #10b981; border-radius: 8px; text-align: center;">
    <div style="font-size: 14px; color: #065f46; font-weight: bold; text-transform: uppercase;">One-Time Approval Code</div>
    <div style="font-size: 36px; font-weight: 800; letter-spacing: 6px; color: #047857; margin: 8px 0;">839201</div>
    <div style="font-size: 12px; color: #047857;">Expires in 15 minutes &bull; Single-use only</div>
  </div>

  <p style="font-size: 13px; color: #6b7280;">Provide this code to the agent in your active ACP chat session to apply the change.</p>
</body>
</html>
--boundary-silo-01--
```

---

## 14. Daemon Lifecycle and Self-Upgrade

### 14.1 Upgrade Execution Sequence

1. **Request & Verification**:
   - The agent or operator requests an upgrade via `POST /v1/silo/upgrade` with `release_tag`.
   - Requires email OTP approval.
2. **Download & Checksum Validation**:
   - Silo fetches the binary from GitHub Releases (`https://github.com/bwsalmon/silo/releases/download/{tag}/silo-linux-amd64`).
   - Silo downloads `checksums.txt` and verifies the SHA-256 digest.
3. **Atomic Replacement**:
   - Silo writes the executable to `/usr/local/bin/silo.new` with permissions `0755`.
   - Silo renames `/usr/local/bin/silo` to `/usr/local/bin/silo.old`.
   - Silo renames `/usr/local/bin/silo.new` to `/usr/local/bin/silo`.
4. **State Preservation and Exec**:
   - Cloud Hypervisor operates as an independent process. Silo preserves the running VM across upgrades by passing the open Cloud Hypervisor API socket and vsock file descriptors across the `execve` boundary or re-attaching to the known UNIX socket paths.
   - Silo executes `syscall.Exec("/usr/local/bin/silo", os.Args, os.Environ())`.
5. **Health Check & Auto-Rollback**:
   - Upon restart, the new Silo process checks communication with Cloud Hypervisor and verifies the management API.
   - If health checks do not pass within 30 seconds, a watchdog script restores `/usr/local/bin/silo.old` and re-executes the previous binary.

---

## 15. Logging and Audit

All operational and security events are written to a structured JSON audit log located on the host at `/var/log/silo/audit.log`.

```json
{
  "timestamp": "2026-11-01T00:16:30.124Z",
  "vm_name": "silo-worker-01",
  "event": "APPROVAL_SUCCESS",
  "change_id": "chg_9a8b7c6d",
  "change_type": "POLICY_UPDATE",
  "details": {
    "rule_name": "npm-registry",
    "to_fqdn": "registry.npmjs.org",
    "ports": [443]
  },
  "sha256": "a3f1e9c2b4...8d9e",
  "operator_email": "operator@example.com",
  "attempts": 1
}
```

### Audited Events Catalog

- `VM_START`, `VM_STOP`, `VM_CRASH`, `VM_RESIZE`
- `AGENT_START`, `AGENT_EXIT`, `AGENT_RESTART`
- `AGENT_UPGRADE_REQUESTED`, `AGENT_UPGRADE_SUCCESS`, `AGENT_UPGRADE_FAILED`
- `POLICY_CHANGE_REQUESTED`, `POLICY_CHANGE_CANCELLED`
- `STORAGE_FORMAT_REQUESTED`, `STORAGE_FORMAT_SUCCESS`, `STORAGE_FORMAT_LOCKED`
- `OTP_ISSUED`, `OTP_ATTEMPT_FAILED`, `OTP_LOCKOUT`, `APPROVAL_SUCCESS`
- `PROXY_EGRESS_BLOCKED`, `DNS_LOOKUP_BLOCKED`
- `PORT_EXPOSED`, `PORT_UNEXPOSED`
- `UPGRADE_ATTEMPT`, `UPGRADE_SUCCESS`, `UPGRADE_ROLLBACK`

---

## 16. Threat Model and Security Analysis

### 16.1 Threat 1: Compromised Guest Root
- **Vector**: The agent executes untrusted code that exploits root inside the guest OS, attempting to escape into the host network or pivot to private infrastructure.
- **Mitigations**:
  - The guest kernel and userspace run inside a KVM hardware-virtualized container managed by Cloud Hypervisor.
  - Network TAP interface has no IP forwarding (`ip_forward=0`).
  - Host `nftables` drops all forward traffic from `silo-tap0`.
  - All outbound TCP/DNS traffic is redirected to Silo proxy listeners; raw sockets and direct packet routing are dropped.
  - Cloud metadata (`169.254.169.254`) and host RFC 1918 networks are strictly filtered.

### 16.2 Threat 2: Leaked In-Guest Management Token
- **Vector**: A malicious script inside the guest reads `/etc/silo/mgmt.token` and calls the management API to grant itself full outbound access.
- **Mitigations**:
  - The management token ONLY grants permission to *request* a change.
  - No mutating change can be applied without the 6-digit OTP sent out-of-band to the operator's email.
  - The agent has no access to the operator's email.

### 16.3 Threat 3: Intercepted or Brute-Forced OTP
- **Vector**: An attacker attempts to brute-force the 6-digit OTP code against `POST /v1/mgmt/approve`.
- **Mitigations**:
  - Rate limiting allows at most 3 failed attempts before permanently invalidating the code.
  - An attacker has a $\frac{3}{1,000,000} = 0.0003\%$ probability of guessing a 6-digit code.
  - Codes expire after 15 minutes.
  - Velocity limits prevent creating more than 3 requests per 15 minutes.

### 16.4 Threat 4: DNS Tunneling and Domain Fronting
- **Vector**: A compromised guest attempts to exfiltrate data via arbitrary DNS queries to external DNS servers.
- **Mitigations**:
  - All port 53 UDP/TCP queries are forcibly redirected to Silo's internal DNS proxy.
  - External DNS servers cannot be contacted directly.
  - Non-whitelisted domains receive `NXDOMAIN` without upstream query forwarding.

### 16.5 Threat 5: Accidental or Unauthorized Storage Overwrite
- **Vector**: A host daemon restart misidentifies a guest customized with XFS as unformatted and wipes the root partition, or a rogue script inside the guest commands Silo to reformat the storage.
- **Mitigations**:
  - Silo performs filesystem-agnostic signature probing (`blkid`) detecting GPT, XFS (`XFSB`), ext4, and btrfs superblocks before considering any format action. If any valid filesystem or partition is detected, formatting is strictly prevented.
  - In-guest reformatting requests via `POST /v1/storage/format` require out-of-band operator email OTP verification with explicit destructive warning diffs.

---

## 17. Verification and Test Plan

### 17.1 Unit Testing Matrix
- `TestConfigValidation`: Verify valid and invalid YAML combinations.
- `TestPolicyDiffGeneration`: Ensure unified diffs format cleanly for plain text and HTML.
- `TestOTPStateMachine`: Verify timeouts, single-use invalidation, attempt lockout, and HMAC matching.
- `TestManagementAPI`: Verify HTTP status codes and error responses using a simulated in-memory store.

### 17.2 Integration Testing
- `TestFakeHypervisor`: Emulate Cloud Hypervisor using `fakechv` (adapted from Granule) to test VM lifecycle, resize, and socket reconnects.
- `TestMockSMTPServer`: Run an in-process mock SMTP server to verify email formatting, MIME boundaries, and header integrity.
- `TestVsockBridge`: Test multiplexed ACP and management streams across simulated vsock pairs.

### 17.3 Real-VM End-to-End (e2e) CI Pipeline
Executed on Linux CI runners with `/dev/kvm` hardware virtualization enabled:
1. Build minimal Debian rootfs and package `silo-supervisor`.
2. Launch real `silo` daemon with `cloud-hypervisor`.
3. Boot virtual machine and verify sub-second ACPI start.
4. Verify guest cannot ping host LAN or reach `169.254.169.254`.
5. Execute curl command inside VM to `https://api.anthropic.com` and confirm success.
6. Execute curl command to `https://github.com` and confirm connection drop (`PROXY_EGRESS_BLOCKED`).
7. Call `POST /v1/policy/rules` from inside VM requesting `github.com`.
8. Intercept mock email, extract OTP, call `POST /v1/mgmt/approve`.
9. Retry curl command to `https://github.com` and verify connection succeeds.

---

## 18. Implementation Tasks and Phased Roadmap

1. **Phase 1: Module Scaffolding & Hypervisor Abstraction**
   - Initialize Go module `github.com/bwsalmon/silo/v4` under `v4/`.
   - Adapt `granule/pkg/hypervisor` into `v4/pkg/hypervisor`, providing `Hypervisor` interface and `cloud-hypervisor` runner.
   - Implement `v4/pkg/config` with YAML schema parser and validators.
2. **Phase 2: Network Infrastructure & Transparent Proxy**
   - Implement `v4/pkg/netshim` for TAP interface setup and IP configuration.
   - Implement `v4/pkg/firewall` to program host `nftables` redirect and filter chains.
   - Implement `v4/pkg/proxy` transparent TCP proxy with SNI inspection and DNS proxy.
3. **Phase 3: Base Image Pipeline & Guest Supervisor**
   - Adapt `granule/deploy/guest-image/` to build reproducible minimal Debian Bookworm rootfs with direct kernel boot.
   - Implement `v4/cmd/silo-supervisor` and vsock communication listeners.
   - Implement rootfs partition formatting and first-start image unpacking.
4. **Phase 4: ACP Control Plane & Agent Supervision**
   - Implement host ACP TCP listener with bearer token authentication.
   - Implement vsock bridging between host ACP listener and guest `silo-supervisor`.
   - Implement agent process supervision loop with automatic crash restart.
5. **Phase 5: Management REST API & OTP Approval Workflow**
   - Implement in-guest management REST API handlers on host proxy listener.
   - Implement cryptographic OTP generator and state machine with attempt tracking.
   - Implement email notification transport (SMTP over TLS and Gmail API OAuth).
6. **Phase 6: Daemon Self-Upgrade & End-to-End Validation**
   - Implement binary download, checksum verification, atomic swap, and re-exec.
   - Implement Cloud Hypervisor socket preservation across daemon restarts.
   - Author e2e test suite running real Cloud Hypervisor VMs on `/dev/kvm`.

---

## 19. Component Reuse and Provenance

| Silo v4 Component | Source Project | Adapted Source Path | License | Description / Modifications |
|-------------------|----------------|---------------------|---------|-----------------------------|
| Hypervisor Runner & API Client | `gke-labs/granule` | `pkg/hypervisor` | Apache 2.0 | Adapted for standalone host execution and socket re-attaching |
| TAP Setup & Netlink | `gke-labs/granule` | `pkg/netshim` | Apache 2.0 | Simplified to point-to-point host-to-guest TAP configuration |
| Stream Forwarding | `gke-labs/granule` | `pkg/forward`, `pkg/guestforward` | Apache 2.0 | Used for ACP and management stream multiplexing over vsock |
| Guest Base Image | `gke-labs/granule` | `deploy/guest-image/` | Apache 2.0 | Debian Bookworm minimal overlay with agent dependencies |
| Extents Storage | `gke-labs/granule` | `pkg/extents`, `pkg/qcow2` | Apache 2.0 | Reference for raw and block direct-IO storage backing |
| Host Daemon Supervision | `ai-on-gke/grain` | `pkg/hostd` | Apache 2.0 | Socket ownership validation (`owner_unix.go`) and process supervision |
| Self-Upgrade Flow | `ai-on-gke/grain` | `pkg/upgrade` | Apache 2.0 | Binary verification, atomic rename, and `syscall.Exec` pattern |
