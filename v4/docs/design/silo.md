# Silo v4 Architecture and Design

Silo v4 provides single-tenant virtual machine isolation for autonomous coding agents running as root. Each Silo process runs as root on a Linux host with KVM, directly managing a single Cloud Hypervisor virtual machine containing one coding agent CLI (Claude Code, OpenAI Codex, or Gemini CLI). Silo serves as the virtual machine's sole gateway to external resources, transparently proxying network traffic, mediating control plane interactions, and enforcing strict human-in-the-loop approval for environmental reconfigurations. Silo v4 is a fresh architecture implemented as Go module `github.com/bwsalmon/silo/v4`, retaining no dependencies on legacy Silo or Grain Hub codebases while strategically adapting hardened hypervisor, network, and storage components from Granule and Grain.

## Process and Execution Model

Silo adheres to a strict one-process-per-VM topology. The host-side `silo` daemon runs with host root privileges, initializes host networking interfaces, and supervises a child `cloud-hypervisor` process via its UNIX management socket. Inside the guest, a dedicated supervisor process (`silo-supervisor`) runs as root and manages exactly one instance of the configured agent CLI. If the agent process terminates with any exit code (including after an in-place CLI upgrade or version bump), `silo-supervisor` automatically restarts it with clean stdio streams, maintaining an always-on agent lifecycle.

To support future platform expansion, hypervisor operations are encapsulated behind a Go `Hypervisor` interface. The primary implementation targets Cloud Hypervisor on Linux with KVM (`/dev/kvm`). The abstraction decouples VM lifecycle commands, CPU/memory resource sizing, and device attachments from underlying hypervisor specifics, providing a clean seam for an eventual macOS implementation backed by Apple's `Virtualization.framework`.

## Storage and Rootfs Lifecycle

The virtual machine operates with a persistent root disk backed by either a raw sparse file or a dedicated block partition. In contrast to ephemeral execution sandboxes, Silo preserves guest filesystem modifications across restarts, enabling coding agents to install operating system packages, compile toolchains, configure language runtimes, and maintain long-lived project workspaces.

Silo standardizes on XFS as its base root filesystem to optimize nested virtualization. Autonomous coding agents frequently run nested container engines (Docker, Podman, containerd) or nested virtual machines to build and test software. XFS natively supports reflink and copy-on-write (CoW) extents (`reflink=1`), enabling instantaneous, zero-copy cloning of container layers and virtual disk images, while avoiding overlayfs performance bottlenecks. To ensure clean bootloader and kernel compatibility alongside modern XFS features (such as 64-bit inodes and bigtime counters), Silo structures the disk into two partitions: Partition 1 (`/boot`, 1 GiB, ext4) to host the kernel, initramfs, and boot configuration; and Partition 2 (`/`, remainder of the disk, XFS with `reflink=1,crc=1`) as the primary system root and workspace.

Crucially, the agent has full permission as root to modify the filesystem layout or reformat filesystems. To protect against catastrophic data loss, filesystem formatting is strictly an explicit operation rather than an ad-hoc check for a single filesystem signature. Silo probes backing storage on boot using filesystem-agnostic signature detection (`blkid`), recognizing both partition tables and individual filesystem superblocks (XFS, ext4, btrfs). If any valid filesystem or partition table is detected, Silo boots without modification, guaranteeing that an existing installation using XFS or another non-ext4 filesystem is never inadvertently overwritten. Initial disk provisioning occurs only on completely uninitialized storage or through an explicit operator provisioning action, and subsequent reformatting requests from the in-guest agent are exposed through the management API (`POST /v1/storage/format`) as explicit, one-time-password-approved operations.

The base image build pipeline adapts Granule's Debian overlay (`deploy/guest-image/`), packaging minimal utilities (`systemd-sysv`, `iproute2`, `kmod`, `acpid`, `e2fsprogs`, `xfsprogs`), the Silo guest supervisor, vsock communication stubs, and the required runtime dependencies for the selected agent CLI. Direct kernel boot (`vmlinux` and initial parameters) bypasses firmware overhead, enabling sub-second boot times.

## Network Topology and Transparent Proxying

Silo enforces complete network containment across a dual-stack point-to-point TAP link (`192.168.127.0/30` and `fd00:silo::/126`). Rather than disabling IPv6 (which causes Happy Eyeballs connection timeouts and breaks IPv6-only registries or enterprise endpoints), Silo fully mediates both IPv4 and IPv6 through its transparent proxy architecture. Host-level `nftables` rules in the `inet` family capture all guest traffic: TCP connections across IPv4 and IPv6 are redirected to Silo's transparent forward proxy (:8443), and DNS queries (UDP and TCP port 53) are redirected to Silo's internal DNS proxy (:5353). 

Host IP forwarding is disabled for the TAP link, and all forward-chain packets originating from the TAP are unconditionally dropped in the kernel. Raw sockets, unauthorized protocols (GRE, SCTP), rogue router advertisements (SLAAC), and unauthorized link-local multicast are discarded. Connections to host loopback addresses, private RFC 1918 subnets, and cloud metadata services (`169.254.169.254` and `[fd00:ec2::254]`) are strictly rejected. The guest requires no explicit proxy environment variables (`HTTP_PROXY`, `ALL_PROXY`), guaranteeing that sub-processes, package managers, and development tools cannot circumvent policy.

By default, Silo enforces a strict zero-trust egress posture: the VM can communicate exclusively with the canonical API endpoints required by the configured agent CLI (e.g., `api.anthropic.com:443`, `api.openai.com:443`, or `generativelanguage.googleapis.com:443`). All other outbound connection attempts are rejected and logged.

## Network Policy Interface Evaluation and Recommendation

To support agent-driven network expansion without proprietary policy dialects, three existing policy interfaces were evaluated:

1. *Raw nftables/iptables in a dedicated network namespace*: Guest-generated nftables rulesets applied to a dedicated netns, paired with DNS-snooping IP sets. While offering native kernel execution, this model leaks low-level firewall syntax into agent toolchains, lacks portability across operating systems, and creates difficult verification semantics for human diff inspection.
2. *Envoy/Squid ACL configuration*: L7 proxy access control lists matching hostnames and ports. While expressive for HTTP/HTTPS inspection, ACL configurations do not naturally accommodate L4 TCP port forwarding or non-HTTP protocols, and their configuration dialects are notoriously brittle.
3. *Kubernetes NetworkPolicy / Cilium-style declarative egress YAML*: A declarative schema specifying egress permissions by destination CIDR, fully-qualified domain name (FQDN), and port ranges, alongside explicit host-to-guest port-forwarding specifications.

Silo v4 recommends and adopts the *Cilium-style declarative egress policy schema*. This format provides clear, structured semantics for both IP and FQDN filtering, maps directly to strongly-typed Go structs, generates intuitive human-readable unified diffs for email approval, and accommodates both egress whitelisting and host port-forward bindings within a unified document. Active policies persist in `policy.yaml` on the host, surviving Silo restarts and VM reboots.

## Agent Control Plane and ACP Bridging

User interaction with the in-guest coding agent occurs via the Agent Client Protocol (ACP). Silo exposes an ACP TCP endpoint on the host bound to a configured port, authenticated by a high-entropy bearer token. To preserve network isolation and eliminate dependency on guest IP routing, Silo bridges ACP traffic into the VM over virtio-vsock (using port forward streaming patterns adapted from Granule's `pkg/forward` and `pkg/guestforward`).

The design incorporates an explicit `ControlMode` seam. While v1 bridges ACP directly to the guest agent's stdio or vsock listener, the abstraction accommodates future Claude and Codex remote control modes where external orchestration tunnels or browser-based session control can replace or run concurrently with the standard ACP listener.

Upon agent startup, the supervisor injects a standardized system prompt. The prompt explicitly informs the model of its virtualized environment, confirms that Silo mediates all network egress, details the guest management REST API, explains the email OTP approval requirement for environmental mutations, and instructs the agent to await user instructions over ACP.

## LLM Credential Handling

In v1, LLM provider API keys are injected directly into the virtual machine environment via the guest supervisor at initialization time. This enables immediate interoperability with existing CLI binaries without modifying authentication logic or re-signing TLS requests.

The architecture establishes a direct evolutionary path toward host-managed credentials. Under v2, API keys reside exclusively in host memory within Silo. Silo's transparent proxy performs TLS termination against the upstream provider, injects authentication headers (`x-api-key` or `Authorization: Bearer`), and re-encrypts upstream traffic. Transitioning to proxy-injected credentials requires installing a host-generated CA certificate into the guest system trust store and incurs minor TLS inspection overhead, but guarantees that root processes inside the guest cannot extract or leak upstream credentials.

## Guest Management API and One-Time Approval Flow

Coding agents frequently require dynamic environment changes, such as installing dependencies from new package repositories, communicating with external database staging instances, exposing local web servers for testing, upgrading the agent CLI version itself, reformatting or configuring filesystems, or upgrading the host Silo daemon. Silo exposes an HTTP REST management API accessible exclusively to the guest over vsock or the point-to-point link, authenticated via a provisioned bearer token.

To prevent autonomous agent escape or unauthorized resource access, all mutating management operations require explicit human approval via an out-of-band One-Time Password (OTP) workflow:

1. The agent submits a mutation request (e.g., adding an egress rule, exposing a port, upgrading the agent CLI version, or reformatting a filesystem) to the management API.
2. Silo creates a pending change record, calculates a canonical SHA-256 fingerprint of the request payload, generates a cryptographically secure 6-to-8 digit numeric OTP, and starts a 15-minute expiration timer.
3. Silo formats a human-readable unified diff of the requested configuration change and transmits the diff alongside the OTP to the operator's configured Gmail address.
4. The operator inspects the diff in their email client and provides the numeric OTP to the agent via their active ACP chat session.
5. The agent submits the OTP to `POST /v1/mgmt/approve`. Silo verifies the code, ensures the pending change hash has not been altered, commits the change to `policy.yaml`, updates runtime nftables and proxy filters, and returns a success response.

To protect against credential brute-forcing, Silo limits verification attempts to 3 failures per request before permanently revoking the OTP, and enforces a global rate limit of 3 OTP generation requests per 15-minute window.

For email delivery, Silo supports both standard SMTP over TLS (using a dedicated Gmail App Password) and the Google Workspace Gmail REST API (using OAuth 2.0). Silo recommends SMTP with an App Password as the default deployment mechanism, eliminating the need to provision Google Cloud projects, manage service accounts, or handle OAuth refresh token rotations on headless developer hosts.

## Daemon Lifecycle and Self-Upgrade

Silo supports automated self-upgrades initiated by the agent or operator. Upon receiving an approved upgrade request specifying a GitHub release tag (`bwsalmon/silo`), Silo downloads the target binary from the release assets, validates its cryptographic checksum against the published manifest, writes the binary to `silo.new`, and swaps the executable via an atomic file rename.

During self-upgrade, Silo re-executes itself (`syscall.Exec`) while preserving the running virtual machine. Cloud Hypervisor operates as an independent child process; Silo transfers or re-attaches to the existing Cloud Hypervisor UNIX API and vsock sockets across the re-exec boundary without terminating guest execution. If post-upgrade health checks fail, Silo automatically rolls back to the backup binary (`silo.old`) and restores previous runtime state.

## Code Reuse and Component Provenance

Silo v4 prioritizes proven, hardened implementations from related ecosystem projects under the Apache 2.0 license:

- *From `gke-labs/granule`*: Silo adapts Cloud Hypervisor process management and UNIX API socket interaction (`pkg/hypervisor`), point-to-point network namespace and TAP interface wiring (`pkg/netshim`), vsock stream multiplexing and trusted/untrusted boundary validation (`pkg/forward`, `pkg/guestforward`), and reproducible Debian minimal rootfs overlay definitions (`deploy/guest-image/`).
- *From `ai-on-gke/grain` (`pkg/hostd`)*: Silo adapts host daemon process supervision, strict socket ownership and file permission validation (`owner_unix.go`), and atomic binary replacement and self-upgrade lifecycle management.

Reused components are imported or vendor-adapted directly into the `github.com/bwsalmon/silo/v4` module hierarchy, maintaining clean boundaries and zero legacy code sharing with previous Silo or Grain Hub versions.
