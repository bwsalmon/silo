# Recorded disposable-worker validation

Validated on 2026-11-22 in this task's disposable Debian environment, kernel
`6.1.0-56-amd64`. This qualifies the exercised configuration only, not the
deployment that dispatched the task or arbitrary workers.

Prerequisite delivery was checked on main: PR #239's policy/control implementation
and its server-authentication fixes are present, including `9fc4b32`,
`acbf311` and `222b35c`. Design commit
`5ae66d9db83199e98561dc37c82d1b4bd19d9078` is an ancestor. The later main CI
bootstrap fix (PR #240, main `847246f`) was incorporated unchanged.

## Reproduction and results

```sh
cargo fmt --all -- --check
cargo clippy --locked --all-targets -- -D warnings
cargo test --locked --all-targets
cargo build --locked --release
cargo clippy --locked --all-targets --target x86_64-unknown-linux-musl -- -D warnings
cargo build --locked --release --target x86_64-unknown-linux-musl
python3 tests/real_worker.py \
  --binary target/x86_64-unknown-linux-musl/release/silo \
  --report /tmp/silo105-real-worker.json
```

All checks above passed. The 28 default Rust tests cover verification,
malicious archives, authenticated CLI/protocol behavior and synthetic lifecycle
invariants. They are distinct from the real-kernel suite: **34 assertion groups
and 17 explicit rejected-launch cases passed**. Both committed example manifests
also passed validation using the static binary. ELF inspection found no Silo
interpreter or NEEDED dependency.

The [machine-readable report](evidence/runner-worker.json) records the exact
binary/tool/image hashes, worker ceilings, measured counters and assertions.
Its temporary paths/registry port have already been destroyed. It contains no
credential values or private keys.

The stock image was
`docker.io/library/hello-world@sha256:5e22040d441e5fb3aed38368acbe8486b575d7018df38dbdfbc7311fbb2ef3a9`;
verified Linux amd64 manifest:
`sha256:d1a8d0a4eeb63aff09f5f34d4d80505e0ba81905f36158cc3970d8e07179e59e`.
An authenticated scoped agent launched it, observed `exited` with exit code
zero and separately authorized output. Its stock greeting mentions Docker;
Silo used no Docker daemon or compatibility layer.

A static C adversarial workload, assembled into digest-addressed OCI layers
and served over a task-local HTTPS registry, exercised the actual runtime.
The suite verified:

- Image entrypoint/args/env/workdir defaults and fixed overrides; rejected
  UID/GID, named user, volume, healthcheck and malformed-executable requirements.
- Actual whiteout/opaque-directory behavior through umoci, safe absolute
  symlink extraction, and rejection of malicious paths, links, ownership,
  set-ID/device entries, corrupted registry blobs and altered manifests.
- Separate helper/workload PID and mount namespaces sharing only their private
  network namespace. Only loopback was present.
- A deliberately inherited descriptor 427, credential descriptor and sentinel
  worker environment did not reach image code. Effective/permitted/bounding
  capabilities were zero, no_new_privs and seccomp were active.
- Host files, host loopback, external IP, AF_VSOCK/non-IP sockets, nested
  namespaces and mounts were denied. A separate native OS principal could
  neither browse image storage nor join helper namespaces.
- Scoped/invisible status/stop, independent log permission, grant and aggregate
  instance ceilings, nonzero exits, bounded output, duration/credential leases,
  revocation, normal-exit orphan removal and setsid-grandchild teardown.
- Missing configuration, wrong tool pin, shared/unbounded storage, fake cgroup
  files, an actually unwritable CPU control and a kernel-denied user namespace
  all failed closed. Supervisor death killed descendants; stale endpoint
  recovery was refused.

Measured exhaustion evidence from the final run:

| Control | Actual observation |
|---|---|
| Scratch | `/tmp` reached ENOSPC at 4,194,304 bytes, half the 8 MiB total scratch ceiling |
| Processes | `pids.events max=1`; deliberately orphaned children were removed |
| Memory | `oom=1`, `oom_kill=5`, `oom_group_kill=1`; helper loss became failed with no fabricated exit code |
| CPU | 397 ms workload CPU over 2 seconds at 20% CPU; 21 throttled periods |

Test tools were Debian packages Bubblewrap `0.8.0-2+deb12u1`, umoci
`0.4.7+ds-3+b7`, and skopeo `1.9.3+ds1-1+b10`. Bubblewrap was non-setuid,
hash pinned and invoked at `/usr/bin/bwrap`; its inspected direct NEEDED
libraries were `libselinux.so.1`, `libcap.so.2` and `libc.so.6`, supplied
by this disposable worker. Packaging/library/source-notice validation for a
distributed release remains pending.

## Practical limits

This is the small, root-owned, numeric `0:0` image subset described in the
[runner guide](runner.md). A stock BusyBox image with nonzero archive owners
was correctly rejected. The supervisor needs a dedicated OS identity without
extra supplementary-group authority, protected bounded tmpfs and a delegated
cgroup subtree. Tests provisioned only task-owned resources and launched their
own non-root supervisor with cleared supplementary groups; no deployment
settings, global isolation switches, infrastructure or grants changed.

Services, relays and volumes remain unsupported. Output is a bounded final
snapshot and can be lost if its helper is killed. Control requests remain
serial and can wait behind bounded image setup. Tokens/runtime records/audit
remain in memory, and crash recovery refuses reattachment. These results do
not establish ARM support, packaged clean-worker installation, persistent data,
TCP relay behavior, production performance or VM-grade isolation. No release
was published and no live workload was deployed.
