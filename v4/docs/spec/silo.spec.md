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
    base_image: "/var/lib/silo/images/debian-12-base.ext4"
    kernel: "/var/lib/silo/kernel/vmlinux"
    cmdline: "console=ttyS0 root=/dev/vda rw quiet panic=-1"

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
5. `vm.agent.cli`: MUST be one of `"claude"`, `"codex"`, `"gemini"`.
6. `approval.otp_length`: MUST be either `6` or `8`.
7. `approval.otp_ttl_minutes`: MUST be between `1` and `60` (default `15`).

---

## 5. Storage and Rootfs Lifecycle

### 5.1 Disk Persistence Model

The virtual machine operates on a persistent root disk. All writes inside the guest OS—including `/etc`, `/usr`, `/var`, `/home`, and `/root`—are committed directly to the backing storage. File changes, package installations, and repository checkouts persist across VM reboots, crashes, and host daemon restarts.

### 5.2 Backing Storage Types

1. **Raw File (`type: raw`)**:
   - Backed by a sparse Linux file created with `truncate(2)` or `fallocate(2)`.
   - Formatted directly as an `ext4` filesystem with `dir_index`, `has_journal`, and `ext_attr` enabled.
   - Attached to Cloud Hypervisor via `--disk path=/var/lib/silo/disks/worker-01.img,direct=on`.
2. **Block Partition (`type: block`)**:
   - Backed by an existing host raw disk partition (e.g. `/dev/nvme0n1p4` or `/dev/sdb1`).
   - Attached with direct I/O enabled (`direct=on`) to bypass host page cache overhead and eliminate double-caching.

### 5.3 First-Start Initialization Sequence

When `silo` launches, it executes the following initialization state machine:

```
+-------------------------------------------------------------+
| Check backing storage at vm.storage.path                    |
+------------------------------+------------------------------+
                               |
            +------------------v------------------+
            | Does valid ext4 filesystem exist?   |
            +-------+---------------------+-------+
                    | Yes                 | No
                    |                     |
+-------------------v---------------+   +-v-----------------------------------------+
| Mount/Verify partition integrity  |   | 1. Create sparse file (if raw) or wipe blk|
| Skip base image copy              |   | 2. Format with mkfs.ext4 -F -L "silo-root"|
+-------------------+---------------+   | 3. Stream base image payload into rootfs  |
                    |                   | 4. Inject /etc/silo/mgmt.token            |
                    |                   | 5. Inject agent CLI default configuration |
                    |                   | 6. Flush buffers (fsync)                  |
                    |                   +---------------------+---------------------+
                    |                                         |
                    +-------------------+---------------------+
                                        |
                         +--------------v--------------+
                         | Launch Cloud Hypervisor VMM |
                         +-----------------------------+
```

### 5.4 Base Image Construction

The default base image is a reproducible Debian 12 (Bookworm) minimal rootfs derived from Granule's `deploy/guest-image/` build pipeline:
1. `debootstrap --variant=minbase --components=main bookworm /target http://deb.debian.org/debian`
2. Install minimal packages: `systemd-sysv`, `iproute2`, `kmod`, `acpid`, `udev`, `e2fsprogs`, `ca-certificates`, `curl`, `git`, `build-essential`.
3. Overlay Silo components:
   - `/usr/local/bin/silo-supervisor`: Supervisor daemon and vsock bridge.
   - `/etc/systemd/system/silo-supervisor.service`: Autostart unit.
   - Node.js / Python / Go runtimes required by Claude Code, Codex, and Gemini CLI.
4. The kernel is a custom or distro-packaged direct-boot kernel (`vmlinux`) compiled with `CONFIG_VIRTIO_PCI=y`, `CONFIG_VIRTIO_BLK=y`, `CONFIG_VIRTIO_NET=y`, and `CONFIG_VHOST_VSOCK=y`.

---

## 6. Network Topology and Enforcement

### 6.1 Point-to-Point Addressing

- **Host Interface (`silo-tap0`)**: `192.168.127.1/30`
- **Guest Interface (`eth0`)**: `192.168.127.2/30`
- **Broadcast**: `192.168.127.3`
- **Subnet**: `192.168.127.0/30`

### 6.2 Host nftables Ruleset

Silo dynamically provisions an isolated `nftables` table named `silo_vm` upon startup. The ruleset guarantees that guest traffic can NEVER bypass Silo:

```nftables
table inet silo_vm {
    chain prerouting {
        type nat hook prerouting priority dstnat; policy accept;

        # Redirect all guest DNS queries to Silo internal DNS proxy
        iifname "silo-tap0" udp dport 53 redirect to :5353
        iifname "silo-tap0" tcp dport 53 redirect to :5353

        # Redirect all guest TCP traffic to Silo transparent forward proxy
        iifname "silo-tap0" tcp dport != { 53, 5001 } redirect to :8443
    }

    chain forward {
        type filter hook forward priority filter; policy drop;

        # Strictly drop all forward frames originating from the tap interface.
        # No guest traffic is permitted to route directly onto host interfaces!
        iifname "silo-tap0" drop
    }

    chain input {
        type filter hook input priority filter; policy drop;

        # Allow established connections from proxy
        ct state established,related accept

        # Allow guest to communicate with Silo transparent proxy and DNS
        iifname "silo-tap0" ip saddr 192.168.127.2 ip daddr 192.168.127.1 tcp dport { 8443, 9090 } accept
        iifname "silo-tap0" ip saddr 192.168.127.2 ip daddr 192.168.127.1 udp dport 5353 accept
        iifname "silo-tap0" ip saddr 192.168.127.2 icmp type echo-request accept

        # Drop everything else from the TAP
        iifname "silo-tap0" drop
    }

    chain output {
        type filter hook output priority filter; policy accept;
    }
}
```

### 6.3 Leak Prevention and Hardening

1. **IPv6 Disabling**: IPv6 is completely disabled on both the TAP interface and the guest kernel via `sysctl net.ipv6.conf.silo-tap0.disable_ipv6=1` and kernel parameter `ipv6.disable=1`.
2. **Metadata Protection**: Host-level drops reject any traffic targeting `169.254.169.254` (cloud metadata) or link-local ranges `169.254.0.0/16`.
3. **Host LAN Isolation**: Traffic to RFC 1918 subnets (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` excluding the point-to-point link) is rejected by default.
4. **Raw Socket / Protocol Dropping**: Non-TCP/UDP protocols (e.g. GRE, SCTP, raw IP tunneling) are dropped at the `input` and `prerouting` chains.
5. **No Routing**: `net.ipv4.ip_forward` is NOT enabled on the host TAP interface. Silo acts as an application-layer proxy, not an IP router.

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

#### 8. `POST /v1/silo/upgrade`
Requests self-upgrade of the host Silo daemon.

**Request**:
```json
{
  "release_tag": "v4.1.0"
}
```

#### 9. `POST /v1/mgmt/approve`
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

#### 10. `POST /v1/mgmt/cancel`
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
- `POLICY_CHANGE_REQUESTED`, `POLICY_CHANGE_CANCELLED`
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
