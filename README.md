# Silo

Silo is building disposable rootless OCI jobs and private TCP services for a restricted Linux worker. Operator policy controls fixed job templates, named volumes and service access. Silo shares the worker kernel; it is not a VM-grade isolation boundary.

This repository currently implements the Rust **policy and authenticated control foundation**. It validates manifests, activates immutable policy without starting workloads, issues scoped credentials through trusted operator control, and enforces permissions server-side. Workload launches, storage operations and the namespace helper fail closed as unavailable. No worker is qualified for untrusted jobs.

- [Implemented commands, build, schema and limitations](docs/foundation.md)
- [Runtime design and release gates](docs/design.md)
- [Full CLI design](docs/cli.md)

```sh
cargo build --locked
cargo test --locked --all-targets
cargo run --locked -- validate examples/foundation.yaml --json
```

Rust 1.85.1 is pinned in `rust-toolchain.toml`. GitHub Actions runs Rust formatting, lint, tests, native and static musl builds. See the foundation guide for authenticated operator setup. The illustrative image digests in the example are not fetched.
