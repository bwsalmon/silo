# Silo v1 CLI design

**Status:** Full v1 interface design. The Rust [policy/control foundation](foundation.md) and [single-image runner](runner.md) implement authenticated activation and scoped job start/status/stop/final logs on a qualified worker. [Static private services and links](network.md) are implemented. Named volumes and the storage examples below remain future acceptance behavior; the runnable current example is [job.yaml](../examples/job.yaml).

The CLI is a client of one local supervisor. Every request is authorized server-side. An operator owns policy, services, and volumes; a restricted agent can ask for launches and limited lifecycle operations. The CLI is not a Docker replacement or an arbitrary command runner.

## Operator workflow

The operator starts the supervisor through trusted deployment configuration and uses a separate operator control identity. For v1, activate one operator-owned manifest rather than accepting policy paths from restricted callers.

```sh
silo validate ./silo.yaml
silo up ./silo.yaml --no-start
silo volume create fixtures
silo volume create results
silo volume create pgdata
silo up ./silo.yaml
silo ps
```

- `validate <manifest>` checks schema, references, and policy consistency without creating objects or starting workloads. Worker capability checks happen at activation and launch.
- `volume create <name>` creates the named directory using its operator-defined ownership and limits. The supervisor uses managed IDs, not names as paths. A declaration is not creation. The declaration in the activated manifest supplies ownership and limits.
- `up <manifest>` validates and activates the operator's manifest and starts its declared operator-owned services. It does not start ordinary jobs. Manifest and image references are trusted operator inputs; a remote client never sends an arbitrary worker path for the daemon to open. An identical repeat is idempotent; reject live policy replacement in v1. Stop all workloads and verify cleanup before applying a policy change. A failed service setup reports the service and failure; CLI success does not imply app-level readiness.
- `volume list` and `volume inspect <name>` reveal metadata, not contents or host paths. `volume rm <name>` is explicitly destructive, operator-only, and refuses mounted or uncertain volumes. No force.
- `logs <id>` is a bounded snapshot and a distinct permission; no default agent log access. `stop <id>` is scoped to a stable instance ID, not a mutable service name.

Activate policy separately from service startup when preparing a fresh worker: use `silo up ./silo.yaml --no-start` first, then create the volumes, then repeat `silo up ./silo.yaml`. No implicit mkdir or volume deletion on activation.

## Restricted agent workflow

```sh
silo run app --json
silo ps --json
silo status job_01 --json
silo stop job_01 --json
```

`run <template>` creates a disposable instance for an approved fixed template. It returns only after isolation and relay setup succeed and the process starts; it does not wait for app readiness or imply successful completion. The supervisor returns an instance ID. Inspect with `status`; separately authorized logs are opt-in. There is no agent override for entrypoint, arguments, environment, paths, image references, mounts, endpoints, or executable input. A new instance uses a new ID even when launched from the same template.

Example grant for a variant of the runtime design template that adds a private exclusive-writer results volume: start at most one live `app` jobs; see status and stop only instances created by this grant; use `fixtures` read-only and `results` read/write through that template; reach `db.silo:5432` only through its declared directional TCP link. The operator-owned database mounts `pgdata` read/write. The agent's DB role, not Silo's mount mode, limits SQL access. Neither the link nor the launch scope exposes `pgdata`, database logs, or process controls. If the agent can read one mount and write another, it can copy between them.

The template pins image digest, command, environment and trusted executable inputs. Never give an agent-writable script, config that can load code, plugin, or initialization directory to a protected workload. Logs, writable results and outbound channels are separate capabilities, not harmless by default.

## Local control and credentials

- Operator identities and agent grants are separate. The trusted launcher binds a short-lived opaque token to one agent session, project, immutable policy version, and scope. Grant issuance and revocation are trusted launcher operations, not agent CLI commands.
- Prefer a client or broker that holds the token on the agent's behalf. A directly launched CLI can read it from a dedicated inherited descriptor, selected by a nonsecret option such as `--auth-fd 3`. Do not put credentials in arguments, environment variables, shell history, manifests, prompts, workload images or logs. Close the descriptor before spawning children. Do not mount the control endpoint or expose control descriptors inside workloads. The deployment must protect the transport and launcher from other OS principals; loopback alone is not authentication.
- Use a private, authenticated supervisor endpoint; select an available transport when qualifying the worker. A filesystem Unix socket is not required if the worker blocks it. No public or remote API in v1. API methods have the same authorization and information boundaries as the CLI.
- Expiry stops new requests. Jobs have bounded leases and must stop no later than their lease deadline; explicit security revocation closes active links and initiates teardown of grant-owned jobs immediately. Operator services and named volumes survive. API credentials do not confine native agents with direct worker filesystem or namespace access.

## Output and lifecycle

Human output is concise. With `--json`, stdout contains one JSON value on success or failure; diagnostics go to stderr without credentials. Error codes are stable and callers should not parse message strings. Status is one of `starting`, `running`, `stopping`, `exited`, or `failed`. An exited process has an exit code when available; a successful status query exits zero even if the work itself exited nonzero. Lists omit inaccessible resources; inaccessible and unknown instance IDs have the same public response. Stop is idempotent for an authorized terminal instance.

| CLI exit | Example error codes |
|---|---|
| 0 | Success |
| 2 | `invalid_request`, `invalid_manifest` |
| 3 | `unauthenticated`, `forbidden`, `not_found` |
| 4 | `volume_busy`, `quota_exceeded`, `policy_in_use`, `port_conflict` |
| 5 | `unsupported_worker`, `launch_failed`, `supervisor_unavailable`, `cleanup_unverified` |

Do not report a stopped workload or release its write lock until every workload process is gone. If recovery cannot verify teardown after supervisor loss, refuse reattachment. If a launch response is lost, inspect instances rather than blindly retrying a mutating call; automatic retries are out of scope.

## Expected acceptance transcript

Run with a qualified worker, two isolated agent identities and fixed operator and client templates. The environment supplies credentials through the trusted launcher; none are shown.

```text
agent A$ silo run app --json
{"id":"job_01","template":"app","state":"running"}
agent A$ silo status job_01 --json
{"id":"job_01","template":"app","state":"exited","exit_code":0}
agent B$ silo status job_01 --json
{"error":{"code":"not_found","message":"No visible instance with that ID"}}
agent A$ silo run db --json
{"error":{"code":"forbidden","message":"Launch is not allowed"}}
agent A$ silo volume inspect pgdata --json
{"error":{"code":"not_found","message":"No visible volume with that name"}}
agent A$ silo run app --mount pgdata:/data:rw --json
{"error":{"code":"invalid_request","message":"Unknown option: --mount"}}
agent A$ silo stop job_01 --json
{"id":"job_01","state":"exited","exit_code":0}
```

Configure the approved test template to use stock psql, exercise the allowed SQL role, verify fixture reads and denied fixture writes, and write results. Have the unrelated client verify it cannot connect even if given the numeric loopback address. Separately exercise the raw private protocol to verify that rejected CLI arguments cannot bypass supervisor policy. Revoke a running grant with an open TCP connection and verify teardown; attempt a second writer and a restart after supervisor loss, then verify persisted rows after safe recovery.

Defer attach, exec, arbitrary volume content commands, live policy editing, external egress, DNS, remote/public APIs and MCP integration.
