# Private TCP services

This extends Task #105's delivered runner (PR #241, commit
e621525a0e8501e9eea83a31ba4b9c187955e2d2) with the static directional
relay from the design pinned at 5ae66d9db83199e98561dc37c82d1b4bd19d9078.
The prerequisite commit and its implementation were verified on main before
this extension. All [runner prerequisites and image restrictions](runner.md)
still apply. Named volumes remain unsupported.

## Operator policy and launches

A service uses the existing manifest schema: services.<name>.template is a
fixed image/command/environment/limits template; listen lists its TCP ports.
Bind workload servers to 127.0.0.1 (wildcard listeners also work when their
ports do not conflict). Each network.allow edge fixes its source, destination,
TCP protocol and port; destinations must be declared service listener ports.
External networking remains denied.

For example, an operator can authorize
{from: app, to: db, protocol: tcp, port: 8080}. An agent grant must include
both the approved app template and scope.links containing "db:8080", as well
as run permission. A grant missing any fixed template link cannot launch that
template. A link does not authorize service metadata, logs or lifecycle control.

Operator up preflights the service topology, starts declared services, and
returns after isolation/exec setup. It does not promise application readiness.
The --no-start flag activates policy without starting services. Failure names
the service and tears down services started by that activation. An identical
repeat while every service is running is idempotent. Services have operator-owned
svc_... instance IDs; existing instance commands provide operator logs and stop.
Authorized service metadata includes the instance ID and whether it is running.

There is no live replacement or automatic restart. If a service exits, its
connections close and new setup fails; links never retarget another instance.
Stop remaining workloads, verify cleanup, and restart the supervisor before
recreating the topology. Live policy replacement remains refused. Service and
job durations are bounded by template/worker ceilings. Credential expiry and
revocation stop grant-owned jobs; operator services retain their own lifetimes.

## Namespace and descriptor boundary

For each caller, every approved destination service has its own address in
127.77.0.0/16. Multiple approved ports of that service share its address.
Different destinations can use the same TCP port. The supervisor generates
read-only /etc/hosts aliases only for approved name.silo destinations, plus
localhost. Addresses follow the immutable manifest's sorted service names;
they are implementation details, not credentials. Guessing an address grants
no access in another caller namespace. There is no DNS service or external route.

The trusted helper binds only approved caller address/port pairs before
workload exec, without socket reuse. It remains outside workload PID and mount
namespaces. A declared service listener overlapping any outgoing relay port is
conservatively rejected with port_conflict, including the possible wildcard
case. Arbitrary later workload binds cannot steal those ports: conflicting
wildcard binds return EADDRINUSE. An undeclared late bind failure is an
application exit, not an application-readiness guarantee from run.

Each helper has a private nonblocking Unix SOCK_SEQPACKET pair with the
supervisor. Before workload exec, a descriptor/acknowledgment probe must succeed;
missing SCM_RIGHTS support fails closed with the specific worker capability.
Workloads receive no channel, setup descriptor, worker path, credential or
helper privilege. The trusted trampoline retains the existing exec confirmation
and descriptor sanitization.

An accepted TCP descriptor arrives with a local link index. Its private channel
authenticates the source; the supervisor looks up an immutable route and fixed
destination instance/listener index. Requests cannot supply a host, port or
destination. The destination helper connects only to its approved workload
listener at 127.0.0.1, then returns the connected TCP descriptor. Received
descriptors are CLOEXEC. Malformed/truncated messages close every delivered
descriptor; invalid helper messages tear down that helper's instance. The Unix
channel carries setup and descriptors only. One supervisor thread copies
application bytes directly between kernel TCP descriptors.

Both directions make independent progress with bounded buffering. EOF propagates
as a write half-close after queued bytes drain, so replies can arrive after
the caller finishes sending. Resets, setup failures and deadlines close both
sockets. Replies use the existing connection; no reverse grant is created.
Services need their own client authentication: they see a loopback peer,
not an authenticated application identity.

## Fixed bounds

| Resource | Bound |
|---|---|
| Active or pending supervisor relay streams | 128 total |
| Per-instance incoming/outgoing connections, including pending setup | Minimum of template limits.connections and 128 |
| Per-link streams | Also bounded by caller's connection limit |
| Live helper endpoints / retained runtime history | 256 each |
| Outgoing bindings / declared listeners per namespace | 32 each |
| Copy buffers | 16 KiB per direction; at most 4 MiB for all 128 streams |
| Kernel TCP socket buffers | Explicit 64 KiB send and receive requests; Linux adds accounting overhead |
| Private setup channel | Fixed 16-byte messages, one descriptor at most; 4 KiB socket buffer requests |
| Helper accept backlog | 16 per binding; accepts/setup processed in bounded batches |
| Destination connect attempt / pending setup | 100 ms / 3 seconds |
| No payload progress / absolute stream lifetime | 15 seconds / 300 seconds, also bounded by instance lease |

Pending setup consumes the same stream reservations as forwarding. Full setup
queues and exhausted quotas close new sockets; callers must handle failure.
Kernel accept queues and descriptors queued on setup channels are separately
bounded and can temporarily exist beyond the 128 supervisor reservations.
Helpers retain the runner's 256 descriptor rlimit and real cgroup process,
memory and CPU ceilings. Kernel socket accounting remains under those cgroups;
the userspace buffer bound excludes kernel overhead.

The relay thread checks leases independently of serial control requests.
Stop/security revocation synchronously removes associated relay reservations
and shuts down both descriptors before job teardown. Normal exit, setup failure,
helper loss and supervisor shutdown also close affected streams. Whole-cgroup
cleanup must be verified before terminal status. Final scoped job logs include
relay counters: accepted, rejected, peak and active. Accepted counts reserved
setup attempts, including destination setup that later fails. The relay never
logs application traffic.

## Real validation

Install the disposable test prerequisites from the runner guide plus Debian's
busybox-static. Run as a non-root user with task-owned tmpfs/cgroup provisioning
permissions as described there:

~~~sh
cargo build --locked --release --target x86_64-unknown-linux-musl
python3 tests/real_worker.py --suite all \
  --binary target/x86_64-unknown-linux-musl/release/silo \
  --report /tmp/silo-real-worker.json
~~~

Select --suite runner for previous runner tests or --suite network for integrated
networking. The network fixture is a real digest-verified OCI image containing
unmodified Debian static BusyBox client/server binaries and an adversarial
static peer. It exercises stock wget/httpd through actual Bubblewrap isolation,
plus duplex, reset, half-close, backpressure, quotas, raw-control misuse,
live revocation, leases and cleanup. It also denies the real sendmsg syscall
and verifies fail-closed setup. Namespace, storage and resource-control failures
remain covered by the runner suite. Mocked results never qualify a worker.

See [network evidence](network-evidence.md) for exact hashes, measured limits
and comparison measurements. These are disposable-worker results, not
qualification of this deployment or a production performance promise.
