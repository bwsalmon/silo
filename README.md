# Silo

Silo runs disposable rootless OCI jobs on a restricted Linux worker. Operator policy fixes the image and launch template; authenticated, scoped credentials control start, status, stop and separately authorized output. Silo shares the worker kernel; it is not a VM-grade isolation boundary.

The Rust implementation includes verified platform-specific OCI images, upstream umoci extraction, a separate trusted namespace helper, a fail-closed Bubblewrap runner, private directional TCP services, and protected managed volumes. Jobs require the static musl executable, pinned tools, protected bounded storage, delegated cgroup v2 and working kernel isolation. Native agents also require separate OS identities and protected process views; a separate UID alone does not hide command lines. An ordinary checkout or successful policy activation does not qualify a worker.

- [Reproducible bundle, installation and release checks](docs/distribution.md)
- [Runner setup, supported subset and limits](docs/runner.md)
- [Private service policy, relay bounds and tests](docs/network.md)
- [Managed volumes, Postgres workflow and recovery](docs/volumes.md)
- [Full real-worker qualification after PR #244](docs/qualification-evidence.md)
- [Recorded storage and restricted-identity CLI evidence](docs/storage-evidence.md)
- [Recorded runner evidence](docs/runner-evidence.md) and [network evidence](docs/network-evidence.md)
- [Policy, credentials and control transport](docs/foundation.md)
- [Full runtime design](docs/design.md) and [CLI design](docs/cli.md)

Named volumes survive instance replacement and verified supervisor recovery on protected persistent worker storage. There is no Docker API, external workload egress, dynamic topology, UDP/DNS server, public API, attach/exec or multi-host support.

```sh
cargo build --locked
cargo test --locked --all-targets
cargo run --locked -- validate examples/job.yaml --json
rustup target add x86_64-unknown-linux-musl
cargo build --locked --release --target x86_64-unknown-linux-musl
```

Rust 1.85.1 is pinned. GitHub Actions checks formatting, lint, regressions and two byte-compared Linux x86-64 candidate builds. The archive contains static `silo`, pinned upstream non-setuid `libexec/bwrap`, integrity-pinned source inputs, licenses/notices and a dependency inventory. Bubblewrap separately requires the x86-64 glibc loader, glibc >= 2.34 and libcap.so.2. See [rebuild, protected installation, diagnostics and release checklist](docs/distribution.md).

The delivered runtime has [real full-worker qualification](docs/qualification-evidence.md); archive qualification is measured separately against the exact extracted bundle. Generic CI smoke tests do not qualify a worker. [Candidate measurements and limitations](https://github.com/bwsalmon/silo/blob/main/docs/distribution-evidence.md) identify the tested archive/configuration. No public release is published. ARM64, automatic upgrades/storage migration, power-loss/reboot durability and production-worker approval remain unimplemented or untested; the runtime guides describe the supported subset.


The same binary now provides the requested [built-in local stdio MCP frontend](docs/mcp.md):
\`silo mcp serve\` translates fixed-template launch, scoped instance lifecycle
operations and separately granted bounded logs into authenticated supervisor calls.
A trusted launcher supplies the restricted credential through an inherited FD;
operator credentials and all launch overrides are rejected. The original MCP
deferral is superseded only for this local frontend. Native agents still require
the protected worker and separate OS isolation described in the security guides.
