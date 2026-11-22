# Single-image job runner

This implements the OCI runner from the design pinned at
`5ae66d9db83199e98561dc37c82d1b4bd19d9078`, building on PR #239's delivered
policy/control foundation and PR #241's verified-image runner. The subsequent
[private TCP service extension](network.md) reuses this isolation and lifecycle
boundary. [Protected named volumes](volumes.md) extend the same cleanup proof. `up --no-start` activates
policy; operator `up` now starts declared private services.

## Trusted worker setup

Run the supervisor as a dedicated non-root OS principal, with no effective,
permitted, inheritable or ambient host capabilities, and no supplementary groups
beyond its primary GID. A rootless helper cannot remove extra groups after
setgroups is disabled for a single-GID map, so inherited extra authority is
rejected before launch. Restricted native agents
must use a different non-root OS principal, a protected process view and a trusted control broker. The tested native-agent launcher supplies private PID/mount namespaces and a private procfs; a different UID alone still exposes command lines on ordinary procfs. Validate the actual agent OS context before issuing credentials; a configured UID is not proof of that external boundary. A
token cannot isolate another shell running as the supervisor's user. The
`agent_uid` setting identifies that operator-established boundary; it does not
create users or move agents. Actual OS separation must be tested.

Build the static Linux x86-64 musl executable. The runner checks the executable's
ELF program headers for an interpreter and shared dependencies; static PIE
relocations are allowed. A native dynamically linked CLI can use the control
transport, but cannot act as the workload supervisor: its trusted trampoline
must run in images without host libraries.

Provide an owner-only `worker.json` in the supervisor's protected state directory.
It is local trusted configuration, never an agent request or environment override.
All paths must be absolute and symlink-free, with trusted ownership and protection
against untrusted writes/renames. Executable pins are exact SHA-256 hashes of
non-setuid/non-setgid files. The supervisor never searches an agent-controlled
`PATH` for tools.

Example shape (replace paths, UID and hashes with verified operator inputs):

```json
{
  "bwrap": {"path": "/opt/silo/libexec/bwrap", "sha256": "<64 lowercase hex>"},
  "skopeo": {"path": "/usr/bin/skopeo", "sha256": "<64 lowercase hex>"},
  "umoci": {"path": "/usr/bin/umoci", "sha256": "<64 lowercase hex>"},
  "storage": "/private/silo-images",
  "cgroup": "/sys/fs/cgroup/private-silo/jobs",
  "agent_uid": 65534,
  "ceilings": {
    "processes": 256,
    "memory_bytes": 536870912,
    "cpu_millis": 2000,
    "scratch_bytes": 16777216,
    "disk_bytes": 268435456,
    "duration_secs": 300,
    "max_instances": 2
  }
}
```

An optional `registry_ca_dir` selects a protected directory of registry CA files
for skopeo's certificate-directory options. TLS verification remains enabled.
Anonymous HTTPS registry pulls are supported; registry credential forwarding is
not implemented. The supervisor's registry fetches do not grant workload egress.

The operator must supply:

- An exclusive owner-only tmpfs mount for `storage`, with size at most
  `disk_bytes` and at most 65,536 inodes. A shared tmpfs directory or unbounded
  disk directory is insufficient. The image cache is protected by a lifetime
  lock and is never mounted into jobs; only the rootfs is mounted read-only.
- An exclusive, empty cgroup v2 domain with delegated `cpu`, `memory` and
  `pids` controllers, writable resource files and `cgroup.kill`. The supervisor
  lives outside this jobs subtree. Migration requires write permission on the
  destination and common-ancestor `cgroup.procs`; a writable child alone is
  insufficient. Silo sets and reads back aggregate and per-instance
  `pids.max`, `memory.max`, `memory.swap.max=0`, `memory.oom.group=1` and
  `cpu.max`, and verifies the actual cgroup filesystem.
- User, network, PID, mount, IPC, UTS and cgroup namespaces, single-ID mappings,
  the private-loopback interface ioctls, unnamed Unix socketpairs/descriptor
  passing, `close_range`, memfd, seccomp and Bubblewrap's
  `--disable-userns --assert-userns-disabled` and bounded tmpfs options.

Aggregate CPU is a rate: `cpu_millis=1000` means one CPU, using a 100 ms
period. Template ceilings cannot exceed the worker's ceilings. Process limits
include helper threads, Bubblewrap and the workload; the supported minimum is
eight tasks, with at least 32 MiB worker memory and 8 KiB scratch. Scratch is
split between `/tmp` and `/run`, each rounded down to a 4 KiB page, so rounding
cannot exceed the approved total. CPU and memory are kernel limits, independent
of grant/template/worker instance quotas. Protected image storage has its own
mount size/inode limit. Persistent volumes additionally require a protected ext4/XFS catalog and bounded filesystem slots; see the volume guide.

No command changes global sysctls, routes, firewall rules or worker grants.
Missing capabilities cause `unsupported_worker`, with the failed capability
in the message. Failed exec/image setup causes `launch_failed`. Qualification
is completed by actual setup before a launch returns success, not by a mock
or a configuration declaration.

## Image verification and supported configuration

Templates still require a digest-pinned registry image, as established in the
policy foundation. The supervisor hashes the authorized raw manifest/index,
selects exactly one Linux amd64 platform, and verifies that immutable platform
manifest. Skopeo fetches the selected image. Every config and compressed layer
must match the approved descriptor's digest and size; all uncompressed layer
diff-IDs are verified too. Docker registry manifest media types may be normalized
to OCI for umoci, but config/layer content must remain identical.

Upstream umoci performs rootless extraction and layer/whiteout application.
Silo does not implement an extractor. Its independent tar preflight rejects
absolute/traversing entry paths, escaping links, special/sparse files, privileged set-ID
file bits, out-of-range ownership and unsupported PAX/xattr metadata, trailing nonzero archive data,
and size/inode/diff-ID violations. Gzip members are decoded through a bounded
reader. Absolute image symlinks resolve within the image root, including during
safe extraction. Extraction results are inspected without following symlinks.

Limits are 1 MiB per metadata object, 256 MiB per compressed blob, 64 layers,
512 MiB total expanded layer bytes (also bounded by storage capacity), and
32,768 archive entries/unpacked inodes. These deliberate bounds exclude larger
images. Failed staging is removed only after setup-process cleanup is verified.
Prepared images are reused within a supervisor lifetime; stale staging is
rebuilt after a clean restart. Existing cgroup descendants prevent reuse after
uncertain cleanup.

Supported image execution fields:

| Field | Behavior |
|---|---|
| `Entrypoint` + `Cmd` | Combined defaults when template `command` is absent/empty; a fixed nonempty template command replaces both |
| `Env` | Image environment plus fixed template overrides; no inherited worker environment |
| `WorkingDir` | Image default, or `/`; a nonempty template `workdir` overrides it |
| `User` | Absent/empty allows a template-pinned numeric UID/GID; a numeric image user must match the template; each must be below 65535 |
| `Labels`, `Volumes`, `ExposedPorts`, `StopSignal` | Inert image metadata; no implicit mounts/listeners or application shutdown signal |

A relative executable name is searched only in absolute directories from the
image's PATH (or the fixed standard default). Relative executable paths with
slashes are unsupported. Required executable inputs and workdir resolve within
the immutable image, outside scratch/pseudo-filesystems. Execution configuration
is bounded to 256 arguments/environment keys and 64 KiB of strings.

Only one UID/GID pair is mapped to the dedicated worker OS identity. Verified
rootless umoci extraction flattens supported numeric archive owners onto that
identity; the immutable image therefore does not preserve multiple owners.
Setgid directories are allowed for stock Postgres; setuid entries and setgid
files remain rejected. Named users, supplementary mappings, health checks,
OnBuild, Shell, escaped arguments and unknown runtime requirements are rejected.
Volume ownership must match the template; no recurring recursive chown occurs.
The [volume guide](volumes.md) describes the tested numeric 70:70 Postgres setup.

## Namespace and exec boundary

The supervisor spawns a fresh trusted helper with a cleared environment. The
helper closes every inherited descriptor above stderr, enters the already
limited cgroup, prevents privilege gain, creates its own user/network namespaces
and brings up only `lo` using `SIOCGIFFLAGS/SIOCSIFFLAGS`. It installs no
default route and never joins host networking. Namespace mutation happens
before the helper starts any threads, never in the supervisor.

Bubblewrap runs at the pinned exact path. It creates separate user/PID/mount,
IPC, UTS and cgroup namespaces, keeps the helper's private network namespace,
mounts the read-only image, read-only proc/minimal devices and bounded scratch,
drops all capabilities and forbids further user namespaces. An x86-64 seccomp
filter rejects namespace joins/creation, mounts and kernel-control operations.
Only AF_UNIX/AF_INET/AF_INET6 sockets are admitted; AF_VSOCK and other families
that could bypass IP network isolation are denied;
`clone3` returns ENOSYS to allow libc's ordinary clone fallback. Cross-architecture
and x32 syscall entry is killed.

Bubblewrap's child-PID notification alone is insufficient to confirm exec.
A read-only bind of the same trusted static Silo executable at
`/run/.silo-exec` runs a small trampoline inside the hardened namespaces. Its
one-use stdin socket receives execution configuration and returns a new
CLOEXEC error-pipe descriptor to the helper. It replaces stdin with `/dev/null`
and execs the image. Only the trampoline owns the write end: successful exec
closes it; failed chdir/exec reports an error. The image inherits only null input
and dedicated output/error pipes. It inherits no helper protocol, readiness,
credential, exec-error, worker-socket or arbitrary parent descriptor.

## Lifecycle and output

Authorization reuses the policy/template/grant intersection from the foundation.
A successful `run` returns a stable ID and platform manifest digest after setup
and exec. It does not promise application readiness or successful completion.
Status and stop remain owner/scoped operations; unknown/invisible IDs match.

Each job's lease is bounded by both template duration and credential expiry.
The helper enforces an absolute deadline independently of supervisor requests;
stop/revocation also kills the cgroup. Helper parent-death protection and
Bubblewrap parent-death/PID-namespace teardown cover supervisor loss. The
supervisor marks a terminal state only after killing/verifying an empty whole
cgroup, reaping its helper and removing the cgroup. Stopping or uncertain
instances retain reservations and prevent new launches until resolved.
Unexpected helper loss becomes `failed` without a fabricated exit code.

Separately authorized `logs` returns bounded final stdout/stderr snapshots,
up to 16 KiB of original bytes per stream, plus cgroup counters. Before final
collection it returns empty snapshots with `complete:false`. Bytes are decoded
lossily as UTF-8. A killed helper can lose output; `capture` explicitly reports
`unavailable_after_helper_loss`. This is not streaming or attach. Runtime history
is limited to 256 instances per supervisor lifetime.

Policy persists; tokens, runtime records and audit remain in memory. A stale
endpoint or existing cgroup descendants refuses ordinary restart/reattachment.
The explicit operator volume recovery procedure requires verified whole-tree
removal; no command infers completion from supervisor loss.

The control server remains serial. Image setup has bounded subprocess deadlines
(60 seconds per tool invocation; at most four invocations), and a launch client
allows 300 seconds for its response. Concurrent status/stop/revocation requests
can wait behind setup; existing jobs' independent helper leases still apply.
This slice provides no high-throughput/latency availability guarantee.

## Disposable validation

Default Rust tests cover parsers, verification, authentication and synthetic
lifecycle invariants; they do not qualify a worker. The opt-in real suite uses
an actual stock pinned image and a static adversarial OCI fixture served by a
task-local TLS registry. It requires passwordless sudo only to provision/remove
its own tmpfs/cgroup, migrate its own test process and start a non-root test
supervisor with its own supplementary groups cleared. It never enables missing
parent controllers, changes sysctls or relaxes isolation to pass.

```sh
# Disposable environment prerequisites: Rust 1.85.1, musl target, binutils,
# bubblewrap with --bind-fd/--ro-bind-fd, skopeo, umoci, musl-tools, gcc, openssl, python3, sudo, busybox-static, e2fsprogs, mount/loop support, util-linux setpriv.
cargo build --locked --release --target x86_64-unknown-linux-musl
python3 tests/real_worker.py --suite all \
  --binary target/x86_64-unknown-linux-musl/release/silo \
  --report /tmp/silo-real-worker.json
```

The full suite also runs the stock Postgres and actual restricted-identity CLI acceptance; `--suite volumes` selects that portion. Use disposable persistent backing space with at least 448 MiB for volume filesystems and sufficient tmpfs/memory for the 288 MiB expanded stock image. The Postgres suite uses a 2 GiB worker memory ceiling and a 768 MiB per-job ceiling, including setup tools.

The report records exact tool/image/binary hashes, kernel, assertions and actual
resource counters. A missing worker feature makes this suite fail with its
precise reason; it is never silently skipped or treated as qualified.
Third-party source/notices, packaged Bubblewrap library validation and release
archive distribution remain separate release gates. No release is published.
