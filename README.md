# Silo

Silo runs disposable rootless OCI jobs on a restricted Linux worker. Operator policy fixes the image and launch template; authenticated, scoped credentials control start, status, stop and separately authorized output. Silo shares the worker kernel; it is not a VM-grade isolation boundary.

The Rust implementation includes verified platform-specific OCI images, upstream umoci extraction, a separate trusted namespace helper, and a fail-closed Bubblewrap runner. Jobs require the static musl executable, pinned tools, protected bounded storage, delegated cgroup v2 and working kernel isolation. An ordinary checkout or successful policy activation does not qualify a worker.

- [Runner setup, supported subset and limits](docs/runner.md)
- [Recorded disposable-worker evidence](docs/runner-evidence.md)
- [Policy, credentials and control transport](docs/foundation.md)
- [Full runtime design](docs/design.md) and [CLI design](docs/cli.md)

Services, TCP links and named volumes remain unsupported in this slice. There is no Docker API, external workload egress, dynamic topology, UDP/DNS server, public API, MCP frontend, attach/exec or multi-host support.

```sh
cargo build --locked
cargo test --locked --all-targets
cargo run --locked -- validate examples/job.yaml --json
rustup target add x86_64-unknown-linux-musl
cargo build --locked --release --target x86_64-unknown-linux-musl
```

Rust 1.85.1 is pinned. GitHub Actions checks formatting, lint, regression tests, native and static builds. Real-worker acceptance is a separate opt-in test requiring disposable kernel resources; see the runner guide. This repository does not publish a release archive or bundle third-party executables yet.
