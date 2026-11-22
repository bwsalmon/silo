# Silo v1: rootless OCI jobs, private services, and named volumes

**Status:** Design proposal; not implemented. Local proofs cover an allowed/denied named TCP service and passing connected TCP sockets across namespaces.

## Goal

Run ordinary Linux OCI images on a single restricted Linux worker, without Docker, host root, or host firewall changes. An agent can start a job or service; operator-owned policy determines what it can reach and which data it can mount. An unmodified app should be able to dial a configured name and port, for example `db.silo:5432`. Containers are disposable; named volumes are not.

Not in v1: Docker API compatibility, full OCI runtime compliance, arbitrary internet access, UDP, public ingress, multi-host networking or replicated storage. Bubblewrap shares the worker kernel; this is not a VM-grade tenant boundary.

## Architecture

- **Silo supervisor:** Owns launch authorization, image cache, instance state, volume catalog, network grants, logs, and cleanup. Begin with a CLI over a private control endpoint and one operator-owned manifest. No MCP server, dynamic grants, public API, or background reconciliation in the first slice. A loopback control endpoint, if used, must authenticate requests; loopback alone is not authentication. Do not mount the control endpoint, credentials, worker socket, or machine filesystem into jobs. An MCP frontend can translate agent requests later; it does not make policy.
- **OCI runner:** Resolve an authorized image reference to a manifest digest, select the worker platform, verify fetched blobs, and use a mature unpacker that handles layers and whiteouts. Keep the cache read-only for jobs. Honor entrypoint, default args, environment, working directory and user for the supported subset. Reject unsupported UID/GID mappings or mount requirements rather than silently changing identity. Mount a read-only rootfs, scratch paths as tmpfs, and explicitly granted volumes.
- **Namespace launcher:** Create a private network namespace before Bubblewrap and bring up only its loopback via the Linux interface ioctl. Use separate mount and PID namespaces and a minimal device view. Drop capabilities before running the image and forbid nested namespace creation in the hardened profile. Do not install a default route or join the worker’s network namespace. The trusted helper sets up loopback and binds approved listeners before launching the image. It shares only the private network namespace with the image, not its PID or mount namespace; never pass its control descriptors or privileges to image code. Fail launch if setup fails or a relay port conflicts with a workload listener, including wildcard binds.

## Control-plane permissions and subagent tokens

Treat each subagent session as a separate Silo principal. The trusted launcher binds a short-lived opaque bearer token to that session and its project. The worker stores only a token digest plus the grant, expiry, and revocation state. Authenticate each request and check the requested operation and resource server-side. Listing, inspection, and logs are authorized separately; hiding a tool in the agent UI is not enforcement.

An illustrative grant for an integration-test agent:

- Launch the approved `app` job template (pinned image, fixed executable and mount profile), up to two instances.
- Read lifecycle status and stop only instances created under its own grant. Raw logs, stdout attachment, and outputs require separate, explicit grants.
- Start a fixed workload with the predeclared `app → db:5432` link if the operator approved it for that workload. This example token does not let the agent open the link itself or mount a volume. No direct external egress.
- No policy edits, arbitrary launch overrides, volume deletion, token issuance, or access to other agents’ jobs.

The effective permission is the intersection of operator policy, the token’s scope, and the immutable job template. Operator policy sets both reachability and data access; a token cannot expand either. Mounting a volume and accessing a database over TCP are distinct grants. A volume’s read-only mount does not make the database read-only; database roles still govern SQL.

Only the supervisor or trusted launcher issues tokens in v1. Prefer a trusted client or MCP bridge that holds the token and sends authenticated requests, rather than passing it in a prompt, command line, image environment, or logs. A bearer token is usable by whoever holds it; session attribution depends on protecting it. Never mount the control endpoint into workloads. Record the principal, operation, resource, and allow/deny result, never the token.

Revocation or expiry stops new API requests immediately. Launches carry a bounded lease; an explicit security revocation should also close the affected links and stop the grant-owned jobs, because rejecting new API calls does not remove a live mount or socket. Operator-owned services and named volumes survive. Test cross-agent denial, expired and revoked credentials, mount or egress escalation, and teardown of an already-open connection.

These tokens constrain Silo API operations only. They do not change a native subagent’s shell, MCP tools, or other permissions outside Silo. If the agent process itself must be confined, launch it behind a separate OS sandbox and give it only the intended Silo interface.

## Named services and grants

A service declares its name and TCP listener ports. Silo gives each permitted destination a distinct loopback address inside the caller namespace, with a generated, read-only `/etc/hosts` alias such as `db.silo`. Start with static mappings for each launch. There is no DNS server in v1; apps using normal system hostname resolution work, while apps requiring DNS queries need separate support. Hostname visibility is not enforcement.

For `app → db:5432`, the trusted helper binds the approved loopback address and port inside app's namespace. After accept, it passes the connected TCP socket to the supervisor over its private Unix socket pair using `SCM_RIGHTS` (file descriptor passing). The helper in db's namespace opens a second TCP connection only to the approved listener and returns its descriptor. The supervisor then copies bytes directly between those two TCP sockets. The Unix control channel carries setup messages and descriptors, not application payload. No custom data multiplexing protocol and no per-packet userspace networking.

The supervisor identifies the source by the private helper channel and selects the destination from an immutable, directional grant. The app never chooses the destination or receives helper descriptors. Compose ordinary kernel sockets with bounded, full-duplex streaming, half-close, deadlines, and per-link connection limits; close every socket on error, stop, or revocation. A link allows replies, not new reverse connections. Processes in the same namespace share reachability. The destination app remains responsible for authenticating the client.

Static mappings are enough for the first slice. Recreate affected jobs when a grant changes; on explicit revocation, close active sockets and stop the affected job. Do not publish external egress in the first slice.

External access is distinct and off by default. If implemented, relay only to an operator-approved fixed endpoint, or offer a narrow application-level operation. No arbitrary destination parameters, unrestricted proxy, or bypass of the worker’s own egress restrictions. OCI image pulls by the supervisor are separate from job egress.

## Named volumes

A volume is an independent directory on the worker, keyed by a project and a name, such as `demo/pgdata`. It lives under Silo’s private data root, separate from the image cache and instance scratch. Store a generated ID and metadata in the catalog; never turn an untrusted name directly into a filesystem path.

- Create explicitly, then grant an instance a mount such as `pgdata → /var/lib/postgresql/data (rw)`. The target is an example; use the path required by the pinned image. Support `ro` and `rw`; sharing is explicit. Default to one writer, with no concurrent attachments while it holds the volume. Allow multiple readers when no writer is active. Acquire locks before launch and release only after verifying the old writer and its children exited. After a supervisor crash, hold the writer lock or refuse to reattach until that can be established. Don’t assume a missing supervisor means the writer is gone.
- Configure ownership for the supported image UID/GID at creation or initialization. Validate mount sources and targets without following symlinks outside managed directories; reject overlapping or protected mount targets. Do not accept arbitrary host bind paths from the agent or recursively change ownership on every launch.
- Stop, remove, and recreate an instance without touching its volume. Deletion is a separate operator action and is refused while mounted. Volumes are durable within the worker’s data root, not necessarily across cloud workspace replacement. Backups and migration are separate concerns.

### Per-resource access: deny unless granted

Subagents may access the volumes and running instances explicitly named in their grants. They may not read, write, inspect, or control any other resource, even within the same project. Scope every permission to a stable resource ID (or, for processes, an operator-approved selector such as instances created under this grant); a friendly name is not authorization.

| Resource | Separate grants | Example |
|---|---|---|
| Volume | Read contents or mount read-only; write or mount read-write; export and delete are separate operator actions | Agent A may read `fixtures` and read/write `results`, but cannot list or access `pgdata`. |
| Running instance | See status; read logs; attach or exec; stop | Agent A may inspect and stop its own test job, but cannot inspect or stop the database. Attach and exec are deferred. |
| Service endpoint | Connect to a named service and port | Agent A's test job may reach a specifically approved listener. No shared namespace or access to other listeners. |

Keep the control identity separate from the workload identity. A job launch is not a storage grant: a fixed, approved workload may use a protected volume without giving its launcher access. Pin all inputs that affect executable behavior, not just the image: entrypoint, args, env, config, scripts, plugins, and init hooks. Do not feed agent-writable executable inputs to a protected workload. Prefer an operator-owned database with a least-privilege application role over putting the agent in the database service. Conversely, allowing arbitrary exec, command overrides, or agent-writable code in a workload gives the agent effective access to everything that workload can reach. Treat that as delegation of the workload's full effective access and reject it when it exceeds the agent's grants. Apply the same reasoning to logs, artifacts, network services, and any other output that could disclose protected data. If an agent can read and write in the same workload, assume it can copy data between those grants.

Enforce this outside the API too. Keep the worker data root outside shared workspaces; use a protected worker or an enforced OS boundary so agents cannot browse its filesystem, inspect live process state, or join its namespaces directly. An API token cannot protect files against a subagent with unrestricted shell or file access as the worker's OS user. Do not claim per-resource isolation in that deployment.

Acceptance test: Agent A can read its read-only fixture volume and write its results, but cannot write fixtures, read or write Agent B's volume, or mount it under a different name. It can manage its own instance but cannot inspect or control Agent B's or exec into a protected database. Verify the denies both through the API and from the agent's actual OS context. Also test attempts to gain access by choosing a privileged launch template, overriding a command, or requesting an artifact or service link.

## Example policy (illustrative)

```yaml
services:
  db:
    image: postgres@sha256:<pinned-digest>
    listen: [5432]
    mounts:
      - {volume: pgdata, target: /var/lib/postgresql/data, mode: rw}
jobs:
  app:
    image: registry.example/app@sha256:<pinned-digest>
  unrelated:
    image: registry.example/test@sha256:<pinned-digest>
network:
  allow:
    - {from: app, to: db, protocol: tcp, port: 5432}
  external: deny
volumes:
  pgdata: {sharing: exclusive-writer}
```

See the [CLI design](cli.md) for the operator and agent interfaces. The operator owns policy; an agent may launch only within an approved scope. Record image digests, applied grants, mount modes, and lifecycle events without logging credentials or service traffic.

## Limits and release gates

Enforce worker-level ceilings for process count, memory and CPU, scratch and persistent disk usage, connection count, and run duration before accepting untrusted jobs. The supervisor bounds relay sockets and buffers independently. If the deployment cannot enforce a required ceiling, refuse untrusted jobs; an instance-count quota is not a resource limit. Qualify every supported worker configuration explicitly.

The file-descriptor path is the default because it removes a transport protocol, not because it has a measured speedup. Before optimizing, compare direct private-loopback TCP to the relayed path for connection setup, short requests, bulk throughput, supervisor CPU, and memory/FD use at steady concurrency. Keep one supervisor and use ordinary buffered copies unless those measurements justify more complexity.

## Evidence and first milestones

**Verified in a restricted-worker prototype:** Bubblewrap ran an unpacked Alpine image. Separately, stock curl reached a named test service through a one-stream relay between Bubblewrap namespaces; a second job resolved the same name but couldn't connect. All three jobs lacked a direct outbound IP route. A follow-up passed connected TCP descriptors from two private namespaces to the supervisor and forwarded a ping/pong between them. That follow-up was a transport probe, not yet an integrated Bubblewrap launcher or throughput benchmark. The netlink path to create veth and named Unix socket creation were blocked. TCP-only Postgres and production resource-limit support are untested.

1. **Runner and relay.** Use one pinned image on a single worker. Prove trusted-helper separation from the image's PID and mount namespaces, no control FD inheritance, and allow/deny by hostname and numeric address. Test parallel duplex streams, half-close, resets, stalled readers, quotas, and FD cleanup. Measure before optimizing.
2. **Real service and storage.** Run stock Postgres and stock psql with an operator-pinned TCP-only configuration (`unix_socket_directories=''`) if the worker blocks pathname Unix sockets. Use the pinned image's actual data path and pre-owned volume. Allowed client connects; unrelated client fails; a restricted DB role can't read disallowed data. Write a row, replace the server, verify persistence, and simulate supervisor loss with a live writer. Test rejected second-writer, read-only writes, and cross-agent reads, exec, and control. Separately verify an agent-writable fixture cannot become executable input to a protected template.
3. **Release.** Validate resource ceilings and storage protection from the actual agent environment. Only then accept untrusted workloads. Defer external egress, dynamic topology, multi-host networking, and MCP integration.

References: [OCI image configuration](https://github.com/opencontainers/image-spec/blob/main/config.md), [OCI layers](https://github.com/opencontainers/image-spec/blob/main/layer.md), [Bubblewrap](https://github.com/containers/bubblewrap), [Unix socket descriptor passing](https://man7.org/linux/man-pages/man7/unix.7.html).
