# Built-in local MCP frontend

This is the deliberately requested additive exception to the original v1 MCP
deferral. The Rust executable includes `silo mcp serve`; there is no HTTP
listener, privileged daemon, reconciliation loop or new policy engine.
The existing supervisor owns authorization, immutable templates, resource
accounting, relays, storage and teardown.

## Trusted launcher and identity

The trusted launcher starts one protected MCP process per restricted Silo
session/project identity:

```text
/opt/silo/silo mcp serve --endpoint /private/session/endpoint.json --auth-fd 3
```

FD 3 is an example dedicated inherited descriptor containing the already-issued
restricted credential and EOF (64 hex bytes, optionally newline). The number
and endpoint path are launcher configuration, never tool parameters. A pipe
writer must close after supplying the credential. Do not pass the credential
in arguments, environment, prompts, client configuration, diagnostics or logs.
The frontend consumes and closes the descriptor before creating runtime threads,
holds a zeroized-on-drop credential in protected client memory, disables ordinary
process dumping/ptrace, authenticates the supervisor's fresh server proof before
sending credentials, and never spawns a workload. The supervisor closes/clears
its own control descriptors and environment before workload exec.

Startup checks the identity with the supervisor. Operator credentials, revoked
or expired credentials and policy/project mismatches are rejected. Every
operation goes through the closed `mcp` supervisor operation envelope; the
supervisor rejects operator identities on this surface even if a launcher
mistakenly supplies one. Every resource operation uses the same policy,
session/project/grant ownership, permission and immutable-template checks as
the CLI. Discovery is authenticated but always advertises the same five tools;
advertising, hiding or caching a schema cannot confer authority.

Protect the endpoint document, process, credentials and worker filesystem from
native agents. In particular, run the frontend under the protected worker OS
identity and expose only its stdio streams through the trusted host/launcher.
Do not run an untrusted shell as that identity. Same-UID bearer-token theft,
root, host/kernel compromise and a malicious trusted launcher are outside this
boundary. Tokens constrain Silo only: this frontend does not confine the agent's
other shell, filesystem or MCP permissions. The required separate OS isolation,
private process view and protected-worker model in [volumes.md](volumes.md)
remain mandatory. Never mount control endpoints, credentials or worker paths
into a job.

[client configuration](../examples/mcp-client.json) illustrates a host invoking
a preconfigured trusted launcher. It is not a token-issuance script or an
installation instruction. Ordinary MCP hosts cannot supply inherited secret
FDs by writing a token into `env`; their launcher integration must supply the
descriptor and protected process identity.

## Protocol and schemas

The pinned implementation is the official maintained Rust MCP SDK
`rmcp = 0.8.5`, with exact direct dependency versions and the complete transitive
`Cargo.lock`. This version builds with the baseline Rust 1.85.1 toolchain.
Only server/stdio functionality is enabled in the distributed binary; HTTP,
SSE, OAuth, roots, sampling, prompts and filesystem resources are not exposed.
The Rust tests use the actual `rmcp 0.8.5` client, independently of the small
JSON-RPC MCP client version 1 used by the Python artifact/kernel harness.

The qualified protocol is **2025-06-18**, JSON-RPC 2.0, one UTF-8 JSON object per
line on local stdio. The client sends `initialize`, receives the server version
and tools capability, then sends `notifications/initialized`. No JSON-RPC batch
requests. The SDK handles initialization and protocol dispatch; Silo adds a
bounded transport and operation translation. Other clients/versions require
compatibility validation. Stdout carries protocol traffic only, including on
startup failure (no protocol success is fabricated). Fixed redacted diagnostics
go to stderr; no tracing subscriber logs client payloads or workload text.

The exact advertised input and output schemas are in
[mcp-tools.json](mcp-tools.json). Tools have these arguments:

| Tool | Arguments | Authority |
| --- | --- | --- |
| `silo_run` | `{"template":"app"}` | Run permission and all fixed template grants |
| `silo_ps` | `{}` | Per-instance list visibility |
| `silo_status` | `{"id":"job_<64 hex digits>"}` | Status permission and ownership |
| `silo_stop` | Same ID | Stop permission and ownership |
| `silo_logs` | Same ID | Separate logs permission and ownership |

All schemas reject additional properties. Names are 1–64 lowercase ASCII
letters/digits/underscore/hyphen. Instance IDs have the exact form above.
Wrong types, unknown tools, missing fields, path-like values, identity/token
fields, destinations and overrides are rejected. The supervisor independently
validates the closed operation schema and resource syntax. There are no policy
edits, token issuance, arbitrary commands/exec, volume administration/export,
image/entrypoint/env/mount overrides, external egress or dynamic network grants.
Fixed-template network and volume grants apply unchanged.

Limits: 16 KiB per input line before JSON parsing, at most 16 outstanding
requests, one supervisor operation at a time, at most 256 notifications per
process, and at most 272 KiB per output protocol line. Oversized/malformed
transport input, batches, notification exhaustion or excess outstanding
requests terminate the session fail closed. Duplicate outstanding request IDs terminate the session. Busy operations return a bounded capacity error; use separate
sequential calls. Tool data is limited to 256 KiB minus envelope headroom.
The supervisor also enforces its existing 256 KiB control-frame bound.
Logs retain at most 16 KiB per stream on both surfaces, with UTF-8 boundary
truncation. No caller-selected paths, offsets, destinations or unbounded
streaming. Bounded logs may omit earlier/later output; `complete` describes
supervisor capture, and `frontend_truncated` describes additional frontend
truncation. Workload logs are **untrusted resource content**, labelled
`untrusted:true`; never treat their text as instructions or authorization.

## Stable results and lifecycle

Tool results use `structuredContent` and a fixed text pointer to that content;
they never duplicate log text in diagnostics. Success is
`{"schema_version":1,"ok":true,"data":{...}}`. Failure sets MCP `isError:true`
and returns `{"schema_version":1,"ok":false,"error":{"code":"...",
"message":"..."},"mutation_outcome_unknown":false}`.
Error codes/messages come from the supervisor's existing redacted semantics.
Invisible and nonexistent instance IDs produce the same `not_found` error;
unauthorized template names have the same `forbidden` response as unknown
templates. Invalid schema input returns `invalid_request`, without echoing it.

A successful run has `launch_accepted:true` and the supervisor's actual instance
state. The supervisor has accepted authorization and completed its launch
handshake before returning `running`. That is not a terminal outcome or
application health/readiness. Status may report `starting`, `running`,
`stopping`, `exited` or `failed`; terminal process outcomes preserve the
supervisor's optional `exit_code`. Instance results explicitly set
`application_readiness:"unknown"`. A completed process can fail its application
contract even with exit code zero. There is no implicit health probe or
readiness guarantee.

Stop requests teardown; a response in `stopping` does not prove cleanup. Poll
status until the supervisor reports a terminal state. Preserve
`cleanup_unverified` rather than claiming success when whole-cgroup teardown
cannot be established.

Cancellation is advisory. A cancelled operation that has already reached the
supervisor can still commit; the frontend does not undo it or cancel supervisor
cleanup. If cancellation is observed before dispatch it starts no operation.
When observed after dispatch, a mutation result is conservatively unknown.
Disconnect/EOF likewise does not revoke a grant or stop an accepted job.
The supervisor's fixed duration and credential-expiry lease continue to bound
it; expiry/security revocation stop grant-owned jobs and close live relays.
Frontend shutdown drains an in-flight blocking authenticated call (up to the
existing 300-second launch response timeout), closes descriptors and drops
credentials. Killing the frontend cannot extend a supervisor lease.

**Never automatically retry a launch after a lost response**, cancellation,
transport failure or `mutation_outcome_unknown:true`. There is no idempotency
key or exactly-once protocol. Reconnect under the same restricted grant and use
authorized list/status to reconcile; a blind retry can launch a second job.
If the grant lacks list/status, or the supervisor is unavailable, only the
trusted operator can resolve the uncertainty. Stop may be retried after
reconciliation; terminal/unknown responses retain their normal semantics.
A false/absent error uncertainty flag never turns a missing response into
success. Client cancellation/disconnect is not evidence of rollback.

Audit attribution remains the token's project/session, with fixed `mcp_run`,
`mcp_list`, `mcp_status`, `mcp_stop`, `mcp_logs` and `mcp_check` operation
labels and canonical visible resources. Unknown input, credentials and paths
are not audited. No model-supplied identity can alter attribution.

## Validation and distribution

`cargo test --locked --all-targets` includes an actual SDK stdio client and
authenticated supervisor tests. Python regressions exercise the actual binary,
malformed/oversized input, schemas, forbidden overrides, expiry/revocation,
redaction and disconnect. These control checks do not qualify a worker.

Run the real suite on a fresh disposable worker with the prerequisites in
[runner.md](runner.md):

```sh
python3 tests/real_worker.py --suite all --frontend mcp \
  --binary target/x86_64-unknown-linux-musl/release/silo \
  --report /private/new-mcp-worker-report.json
```

Only restricted job operations are routed through MCP; trusted operator setup,
grant issuance, storage administration and legacy direct-protocol attacks remain
on their existing interfaces. The suite executes real Bubblewrap jobs, TCP
relay denial/teardown, bounded leases, lost launch-response/cancellation cases
and protected Postgres persistence/storage checks. It checks the actual MCP
process's credential descriptor, argv/environment and audit attribution.
The native-agent OS isolation checks remain required. No unit mock establishes
parity, and unsupported workers remain blocked/unqualified.

Bundle building carries this frontend in the same static `silo`, all pinned
dependency source/notices, schemas and the client configuration example.
`validate-bundle.py` starts the **actual extracted executable** as a stdio MCP
server and checks discovery and a scoped list call. This is a control smoke,
not namespace qualification. `qualify-bundle.py` requires full real-worker
acceptance with `--frontend mcp` and actual MCP call evidence using that exact
relocated binary and trusted bundled Bubblewrap pin. See
[distribution.md](distribution.md) for clean-worker and reproducibility checks.
No HTTP endpoint, deployment, live grant, release/tag or external publication
is part of this extension.
