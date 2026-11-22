# Rust policy and control foundation

This describes the policy/control foundation of the [runtime design](design.md)
and [CLI contract](cli.md), based on design commit
`5ae66d9db83199e98561dc37c82d1b4bd19d9078`. The subsequent
[single-image runner](runner.md) adds verified OCI jobs and a separate trusted
namespace helper to the same Cargo package and executable.

## Build and checks

Linux x86-64, Rust 1.85.1, a C linker and binutils are sufficient. Rustup reads
`rust-toolchain.toml`; dependencies are fixed by `Cargo.lock`.

```sh
cargo build --locked --release
cargo fmt --all -- --check
cargo clippy --locked --all-targets -- -D warnings
cargo test --locked --all-targets
rustup target add x86_64-unknown-linux-musl
cargo build --locked --release --target x86_64-unknown-linux-musl
```

GitHub Actions runs formatting, lint, tests, native release build, and a musl
build with ELF interpreter/dependency inspection and a validation smoke test.
It does not build or publish a release archive. No Bubblewrap binary is bundled
yet; tool pins and real-worker validation are described in the runner guide.
Licensing/source distribution and a packaged clean-worker smoke test remain
release gates. No Go pipeline is restored.

Tests use disposable directories and private ephemeral loopback endpoints.
Binary integration tests activate policy, issue credentials through dedicated
descriptors, exercise allowed metadata queries, and deny expired/revoked tokens,
policy edits and protocol overrides. Tests with synthetic instance records use
the production authenticated protocol handler to check cross-agent/project
visibility, distinct list/status/log/stop permissions, terminal stop idempotence,
quotas, and lease/revocation teardown scheduling. Synthetic records are not
evidence of a launched process, enforced storage, or completed teardown.

## Commands delivered

| Command | Current behavior |
|---|---|
| `validate FILE` | Validate locally, return content-derived policy ID; no objects created |
| `supervisor --state-dir DIR --auth-fd FD` | Run foreground private authenticated control server |
| `up FILE --no-start` | Operator-only activation, persisted policy, no workloads or volumes |
| `ps` | Only instances visible under the separate list permission |
| `run TEMPLATE`, `status ID`, `stop ID`, `logs ID` | Scoped single-image jobs on qualified workers; logs need their own permission |
| `volume list/inspect NAME` | Authorized **declaration metadata**, explicitly `created:false`; no host paths or contents |
| `service list/inspect NAME` | Authorized declaration metadata, explicitly `started:false` |
| `operator issue GRANT.json --token-fd FD` | Trusted operator only; credential goes exclusively to output FD |
| `operator revoke GRANT_ID` | Trusted operator only; reject new requests and queue owned-instance teardown |
| `operator audit` | Operator-only bounded audit snapshot |
| `operator shutdown` | Graceful exit only when no unverified nonterminal instances remain |

Control commands require `--endpoint /absolute/private/endpoint.json` and
`--auth-fd FD`. No credential option accepts a token value. Descriptors must be
distinct and at least 3. The launcher transfers ownership of each descriptor;
credential input is 64 hex characters with an optional newline, followed by EOF.
Silo consumes and closes it before sending a request or entering the supervisor.
There is no credential environment variable or automatic retry.

`volume create/rm` return `unsupported_worker` (exit 5) after authorization.
`run` requires the [qualified runner](runner.md). Operator `up` now starts
[static private services](network.md); the foundation example still includes
unsupported mounts and cannot launch. Activation may persist policy before
service startup failure is reported. Attach/exec, overrides,
external egress, UDP/DNS, dynamic topology, public/remote APIs, MCP and multi-host
operation remain absent.

`--json` writes exactly one JSON value to stdout for success or error. Invalid
CLI arguments, manifests and raw protocol fields have fixed, redacted messages.
Exit codes follow [the CLI table](cli.md#output-and-lifecycle): 2 for invalid
input, 3 for authentication/authorization/visibility, 4 for policy/quota conflict,
5 for unavailable infrastructure or implementation. A status query succeeds
regardless of a terminal workload's exit code.

## Trusted operator example

Use only a disposable test directory for this example. The operator and
restricted client must have separate OS protection in a real deployment.
A broker holding credentials on the client's behalf is preferred.

```sh
umask 077
state=$(mktemp -d)
chmod 700 "$state"
python3 - "$state/operator.token" <<'PY'
import os, secrets, sys
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "w") as f:
    f.write(secrets.token_hex(32) + "\n")
PY
silo supervisor --state-dir "$state" --auth-fd 3 3<"$state/operator.token" &
# Wait for "$state/endpoint.json" to appear, or inspect supervisor failure.
silo validate examples/foundation.yaml --json
silo up examples/foundation.yaml --no-start --json \
  --endpoint "$state/endpoint.json" --auth-fd 3 \
  3<"$state/operator.token" >"$state/activation.json"
python3 - "$state" <<'PY'
import json, pathlib, sys, time
root = pathlib.Path(sys.argv[1])
grant = {
    "session": "agent-a", "project": "example",
    "policy": json.loads((root / "activation.json").read_text())["policy"],
    "expires_at": int(time.time()) + 300,
    "scope": {
        "templates": ["app"], "permissions": ["run", "list", "status", "stop", "volume_inspect"],
        "volumes": ["fixtures"], "mounts": {"fixtures": "ro", "results": "rw"},
        "links": ["db:5432"], "max_instances": 1
    }
}
(root / "grant.json").write_text(json.dumps(grant))
PY
silo operator issue "$state/grant.json" --token-fd 4 --json \
  --endpoint "$state/endpoint.json" --auth-fd 3 \
  3<"$state/operator.token" 4>"$state/agent.token"
silo volume inspect fixtures --json --endpoint "$state/endpoint.json" \
  --auth-fd 3 3<"$state/agent.token"
# Expected: created:false. No volume was created.
silo run app --json --endpoint "$state/endpoint.json" \
  --auth-fd 3 3<"$state/agent.token"
# Expected: unsupported_worker, exit 5. No instance was launched.
silo operator shutdown --json --endpoint "$state/endpoint.json" \
  --auth-fd 3 3<"$state/operator.token"
```

The token files above belong to the trusted test launcher; do not give restricted
agents filesystem access to that directory, put tokens in prompts/argv/env/logs,
or mount any control/credential path into workloads. Tokens constrain API calls,
not a native agent's shell.

## Manifest and immutable policy

[examples/foundation.yaml](../examples/foundation.yaml) is the complete schema
example, with illustrative digests that are not fetched. Version 1 requires
`project` and `network.external: deny`. Named maps have unique, bounded names;
unknown fields, duplicate declarations, unsupported enum values and unpinned
images are rejected. Services wrap a fixed `template` and declared TCP ports.
Every template fixes its UID/GID and limits. Nonempty command/workdir fields
supply absolute overrides; omitted/empty fields use verified image defaults. Each
template requires nonzero process, memory, CPU, scratch, connection, duration and instance ceilings.
Duration is at most one day; instance ceilings are at most 256.

Volumes declare ownership, byte ceiling and `exclusive-writer` sharing. Mounts
refer to declared volumes with explicit `ro/rw` modes and nonoverlapping absolute
targets. Network edges refer to known caller templates and declared service TCP
ports. Restricted operations accept no destination address or host filesystem path.

Executable inputs must come from the immutable image. Command paths, declared
code/config/plugin inputs and obvious command arguments cannot refer to mutable
mounts. Mounts cannot shadow system/code/control paths. Operators must enumerate
indirect code-loading inputs: static validation cannot infer arbitrary program
behavior, environment-driven loaders or application configuration. Runtime must
preserve image immutability and enforce these mounts before execution.

All permission sets default to empty. A job's `agent` set independently permits
`run/list/status/logs/stop`; volume and service `agent` sets permit only their
respective list/inspect operations. A token cannot expand these sets.
A token's `scope.templates`, `permissions`, metadata `volumes/services`,
template `mounts`, fixed `links` (`service:port`) and `max_instances` are distinct.
Mount scope does not expose volume metadata; a service metadata grant does not
create a connection. Launch requires every fixed mount/link to fit the token,
in addition to the template's operator permissions. There is no direct
agent-controlled mount or service connection operation.

Canonical sorted typed JSON defines the SHA-256 policy ID. Resource IDs include
the immutable policy, resource kind and name, never host paths. Policy equality
is semantic after typed defaults, not YAML formatting. Documents/frames are
bounded to 256 KiB and normalized policies to 128 KiB; there are at most 256 named
resources/edges. Activation writes create-only `policy.json` under an exclusive
state lock and syncs it. Identical repeats succeed; any replacement fails
`policy_in_use`, including after a clean restart. To apply another policy use a
fresh protected directory only after verified cleanup; there is no live edit.

## Private transport, persistence and limits

The supervisor binds only `127.0.0.1` on an ephemeral TCP port and creates an
owner-only endpoint file containing the address and an independent random
256-bit server key, renewed on every start. Before sending any bearer token,
clients verify an HMAC-SHA256 server proof over a fresh random challenge using
that protected key. A stale port taken by another local user cannot collect
credentials, and recorded proofs cannot be replayed. The key is never sent over
the socket and grants no operator or resource permissions. Protect endpoint
file contents like credentials; do not log or mount them into workloads. Clients
without access must use the trusted broker rather than copying the server key
into an agent environment. Old address-only endpoint files fail closed.
Every request still requires an independently authenticated opaque bearer token.
The directory must be
absolute, symlink-free, owned by the supervisor UID, mode 0700 or stricter, with
trusted ancestors preventing untrusted rename. Endpoint/policy/lock files use
owner-only modes and no-follow/close-on-exec descriptors. No remote binding or
environment-controlled helper path exists. Loopback availability and filesystem
protection are checked. Namespace, protected storage and kernel resource
qualification happen at job launch and cannot be inferred from activation success.

The private version-1 protocol uses four-byte big-endian lengths and strictly
typed JSON. A client challenge and server proof precede the authenticated request.
One request per connection, two-second I/O timeouts and a 256 KiB frame bound
constrain exchanges. Authenticated launch clients allow 300 seconds for bounded
image setup after the server handshake. The server is serial and intended
for local control, not high request throughput. An abusive local caller can
delay other calls; no availability guarantee against the worker's OS peers is
made. Policy activation sends typed content, never a path for the server to open.

Bootstrap operator credentials come from the trusted launcher. The supervisor
keeps only their SHA-256 digest. Operator issuance uses 256 bits of OS entropy;
stored agent records contain only token digests, session/project, immutable
policy version, expiry (at most one day), revocation and scopes. Issuance is
capped at 4096 records per supervisor lifetime, including revoked records.
Restricted principals cannot issue/revoke credentials, activate policy or access
audit records. A different token for the same session does not inherit another
grant's instances.

Policy persists; issued credentials, runtime records and audit records do not.
A clean restart invalidates all issued agent credentials. An exclusive lock
prevents concurrent supervisors. A stale endpoint after abnormal loss causes
startup refusal; there is no recovery/reattachment path or cleanup assertion.
Do not remove recovery evidence to resume an uncertain writer.

Audit retains the newest 512 records with sequence, time, principal,
operation, managed resource ID and allow/deny plus error code. It excludes
tokens, arbitrary input, manifests, paths, logs and service traffic; it is an
operator-only in-memory ring, not a durable compliance log. An authorized
runtime request that cannot execute records `allowed:true` with
`unsupported_worker`.

Lifecycle states are `starting/running/stopping/exited/failed`. Lease expiry,
security revocation and explicit stop move active instances to `stopping`;
reservations remain until the runner verifies whole-cgroup cleanup. The helper
independently bounds leases by credential expiry and template duration. Synthetic
foundation fixtures still test scheduling without claiming real teardown; the
separate [real-worker suite](runner-evidence.md) verifies actual process cleanup,
resource exhaustion and crash refusal.

[Static services and TCP relay](network.md) now extend this foundation.
Named volumes, PostgreSQL persistence and recovery reattachment remain unsupported. Worker qualification never follows from API
quotas, activation, synthetic fixtures or mocks alone.
