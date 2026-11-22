# Recorded private TCP validation

Validated on 2026-11-22 in this task's disposable Debian worker, kernel
6.1.0-56-amd64. This qualifies only the exercised configuration. It does not
qualify this deployment or arbitrary workers.

The design commit 5ae66d9db83199e98561dc37c82d1b4bd19d9078 is an ancestor.
Before implementation, git ancestry and source inspection confirmed that
Task #105's PR #241 actually landed on main as merge e9a34d4, including
e621525a0e8501e9eea83a31ba4b9c187955e2d2. Its image verification, cgroups,
Bubblewrap isolation, exec trampoline and authenticated grant/control boundary
were reused.

## Reproduction and checks

~~~sh
cargo fmt --all -- --check
cargo clippy --locked --all-targets -- -D warnings
cargo clippy --locked --all-targets --target x86_64-unknown-linux-musl -- -D warnings
cargo test --locked --all-targets
cargo build --locked --release
cargo build --locked --release --target x86_64-unknown-linux-musl
python3 tests/real_worker.py --suite all \
  --binary target/x86_64-unknown-linux-musl/release/silo \
  --report /tmp/silo106-all.json
~~~

All checks above passed. The 36 Rust regression tests include real kernel
socket/SCM_RIGHTS tests, malformed/truncated/zero-length ancillary messages,
source/destination protocol misuse, the independent 128-stream aggregate
ceiling, timeouts, reservations and half-close. Native tests alone do not
qualify a worker.

The full real-worker suite passed 69 assertion groups plus its 17 explicit
rejected-launch cases. A subsequent network-only run passed again, adding
supervisor CPU and concurrent FD/thread/RSS observations. Both example
validation smoke checks passed; ELF inspection found no interpreter or
NEEDED dependency. The tested static binary SHA-256 is:

    c682bd76b351af62a6ac77fddce5df976a68b72dec33805028428b27766a61d6

Machine-readable records retain exact binary/tool/image hashes, ceilings,
assertions, actual resource counters and observations:

- [Full runner and network report](evidence/tcp-worker.json).
- [Network repeat with concurrency measurements](evidence/tcp-network.json).
- [Live listener preconditions for denial tests](evidence/tcp-listener-validation.json).
- [Initial and tuned bounded-buffer comparison](evidence/tcp-buffer-comparison.json).

Report paths, registry ports, socket inodes and task-owned resources have been
destroyed. Reports contain no bearer tokens, private keys or application secrets.

## Integrated evidence

The network fixture is a real, digest-verified OCI image served by the existing
task-local TLS registry, extracted by pinned umoci, and executed by real
Bubblewrap. It contains unmodified Debian busybox-static
1:1.35.0-4+deb12u1+b1 (stock wget/httpd/timeout/sh) and an adversarial static
C peer. It is a purpose-built root-owned image, not a claim that the official
multi-owner BusyBox image is supported. The full suite also reran the previous
stock hello-world image and its image/isolation/resource tests.

Actual isolated workloads verified:

- Stock clients reach approved names and numeric addresses; two distinct
  destinations share port 8080 with different read-only hosts aliases.
- An unrelated grant's workload cannot reach either the name or numeric endpoint.
  Missing link grants reject launch. Raw authenticated protocol and CLI calls
  reject endpoint, link, command and environment overrides. Services remain
  invisible to unauthorized instance operations.
- Undeclared services, another port on a running approved service, caller
  localhost, direct outbound IP and new reverse connections are denied.
  A further network repetition reads actual destination namespace TCP tables
  before testing: db ports 8080/8081/9000 and both other service listeners are
  present, while the setup-failure target 9001 is absent.
  Replies still work on existing directional links.
- Declared workload/wildcard relay conflicts fail before exec. Actual workload
  wildcard bind attempts receive EADDRINUSE.
- Thirty-two repeated streams, four parallel duplex streams, a 4 MiB echo,
  a reply after request EOF, TCP reset recovery, missing listeners, connection
  exhaustion and a reader that never consumes payload all behave correctly.
- Before revocation/expiry/lease tests, the suite reads the caller namespace's
  real TCP table and matches an ESTABLISHED socket inode to the supervisor's
  descriptor table. Revocation closes that active relay and removes grant-owned
  processes; services survive and accept a fresh approved grant.
- Supervisor FD/thread counts return to baseline after successful, failed,
  parallel, reset, quota and revocation paths. Only service cgroups remain until
  operator stop; all workload/helper cgroups are removed at suite completion.
- Denying the real sendmsg syscall produces unsupported_worker naming SCM_RIGHTS
  before image execution. Existing real namespace, protected-storage, controller,
  cgroup-exhaustion and crash-refusal tests continue to pass.

## Observed bounds and comparison

The repeated network run used three services and at most two concurrent callers,
with caller connection limit 4, service limit 32, a six-instance worker ceiling,
512 processes, 1 GiB aggregate job memory and 200% aggregate CPU. The reports
record all per-job and aggregate limits. Four concurrent streams had a peak
of four relay reservations, 24 supervisor FDs, six threads and 4,228 KiB RSS.
The supervisor baseline was 14 FDs, five threads and 4,044 KiB RSS; after caller
cleanup it returned to 14 FDs/five threads, with 4,196 KiB RSS. A live single
stream used 18 FDs and six threads. There is no relay thread per stream.

| Measurement | Direct private loopback | Relay |
|---|---:|---:|
| 32 sequential connect/request/reply/close operations | 0.020 s | 0.302 s |
| 4 MiB full-duplex echo | 0.013 s | 0.598 s |
| Supervisor CPU across that launch, short requests, bulk, status and final logs | 0.02 s | 0.09 s |

CPU ticks use 100 Hz and include setup/control work; they are not isolated copy
CPU. Four concurrent 1 MiB streams used about 0.05 s supervisor CPU in that
scenario. These are local samples, without statistical confidence intervals,
not throughput or availability guarantees. Direct networking remains available
only within a workload's own private namespace.

An initial 16 KiB kernel buffer request and 10 ms helper polling took about
27.2 s for the relayed 4 MiB echo. The recorded comparison motivated bounded
64 KiB kernel buffer requests, TCP_NODELAY and 2 ms helper polling. Copy buffers
remain 16 KiB per direction, capped at 4 MiB over 128 supervisor streams. There
is no custom application-payload transport or new networking subsystem.

The stalled-reader test absorbed only 335,872 bytes before backpressure and
closed after about 15.3 seconds at the idle deadline. Excess connections were
rejected with peak reservations fixed at four. Native socket tests independently
filled the global 128-stream ceiling and verified all descriptors close after
destination removal.

## Limits and environment

The [network guide](network.md) records all fixed limits and failure semantics.
Static service topology requires a verified supervisor restart for replacement.
Servers need their own application authentication; Silo forwards a loopback TCP
peer, not a client identity protocol. Fixed idle/lifetime deadlines can close
long-lived application connections. Port overlap rejection is conservative.
Serial control requests can wait behind bounded image setup; the relay and
helper leases continue independently.

The root-owned numeric 0:0 image subset, dedicated OS identity, protected bounded
tmpfs, real delegated cgroup controls and crash reattachment refusal remain.
Named volumes, persistent Postgres, packaging/releases, external workload egress,
DNS/UDP servers, dynamic topology, public APIs, MCP, attach/exec and multi-host
support were not added. A worker failing required capability checks is refused;
no isolation was relaxed to obtain these results.

Useful validation prerequisites installed in this disposable sandbox were
Rust 1.85.1 (rustfmt, Clippy, musl target), musl-tools, umoci, skopeo and
busybox-static; Bubblewrap 0.8 was already present. Only task-owned tmpfs,
cgroup and registry resources were provisioned and removed. No deployment,
live infrastructure, configuration, live grant or release was changed.
