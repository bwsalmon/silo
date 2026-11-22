# Protected storage and durable Postgres evidence

Recorded on 2026-11-22 in disposable task-owned environments. This qualifies
only the recorded worker **and native-agent OS profile**, not this deployment
or another worker. API activation and synthetic tests are not qualification.

## Prerequisite and provenance

Task #106's implementation `d685785511274f8d219dc4078d31e44df157ec12`
was verified on main through PR #242, merge `99b487a`. Its runner, cgroup
cleanup, authenticated control and private TCP relay are reused. Design commit
`5ae66d9db83199e98561dc37c82d1b4bd19d9078` is an ancestor. Canonical design/CLI
changes between that baseline and main were status/link updates.

The machine-readable [full report](evidence/storage-worker.json) records exact
worker settings, tool/image/binary hashes, kernel counters, network regression
measurements, the two-identity transcript and actual agent kernel state.
The [standalone JSON transcript](evidence/storage-cli.json) contains
18 real CLI invocations/responses; no control credentials are included.
The [supplied policy](../examples/postgres.yaml) contains public disposable SQL
passwords, never production or control credentials.

## Validation

- Formatting, native and musl Clippy with warnings denied, all 44 Rust tests,
  native release build and static musl release build passed.
- Static ELF inspection found no interpreter or shared dependencies. All three
  example manifests pass the built CLI's validation smoke checks.
- The integrated real-worker suite passed **99 assertion groups**, plus
  17 explicitly rejected image/launch cases. Existing namespace, storage,
  cgroup exhaustion, child cleanup, relay allow/deny, duplex/backpressure,
  revocation, expiry and missing-capability regressions still pass.
- Final tested static executable SHA-256: `1f4382ea2a53b3454b3fdcc7652df668d0e382826dd7dc0fe9d1b0ed270b2bcf`.
- Kernel: `6.1.0-56-amd64`. Full pins and ceilings are in the report.

Reproduction on a disposable qualified machine:

```sh
cargo fmt --all -- --check
cargo clippy --locked --all-targets -- -D warnings
cargo clippy --locked --all-targets --target x86_64-unknown-linux-musl -- -D warnings
cargo test --locked --all-targets
cargo build --locked --release
cargo build --locked --release --target x86_64-unknown-linux-musl
python3 tests/real_worker.py --suite all \
  --binary target/x86_64-unknown-linux-musl/release/silo \
  --report /tmp/silo-real-worker.json
```

The suite requires the [documented prerequisites](runner.md#disposable-validation),
including Bubblewrap's race-checking directory-FD bind operations, skopeo, umoci,
Rust/musl tools, busybox-static, e2fsprogs, mount/loop support, util-linux setpriv,
and sudo solely for disposable resource/OS-context provisioning. The recorded
Bubblewrap package was Debian `0.8.0-2+deb12u1`; feature support and its exact
hash matter, not a minimum version string alone.

## Storage and database observations

The catalog and loop backing files were on protected ext4 under a task-owned
`/var/tmp` directory. Four separate ext4 filesystems had 64/64/256/64 MiB
capacities, 4096 inodes each and nosuid/nodev/noexec mounts. Image storage was
an independent protected 1 GiB/65536-inode tmpfs. The Postgres profile enforced
a 2 GiB aggregate job memory ceiling, 768 MiB per job, delegated CPU/pids limits,
and bounded scratch. A 100 MiB write to a 64 MiB results filesystem actually
returned ENOSPC. Read-only fixture writes actually returned EROFS.

Stock Postgres index:
`docker.io/library/postgres@sha256:f2f98726820d0342055351c6e9d36e705f5bfffb3dd53ff556325114b08afb6d`.
Verified Linux amd64 platform:
`sha256:9d9e487c1bd6413c52ccb3c02f46c8e37fa1e1d1a32a84519eb4e68d0ad1da30`.
Stock initdb, server and psql used the image's real
`/var/lib/postgresql/data` and numeric UID/GID 70:70. Kernel-visible identity and
data ownership were asserted. Bootstrap and service used TCP only, with pathname
Unix sockets disabled. Private namespace TCP tables confirmed port 5432 really
listened while the unrelated named/numeric client was denied.

The authorized SCRAM role inserted/read `durable-row`. Actual Postgres permission
errors denied the secret table, server file reads, COPY PROGRAM and assuming the
operator role. A fresh server instance read the committed row through stock psql.
A SIGKILL then killed the actual supervisor with a live database writer and
9 cgroup processes. A fresh control state refused
reattachment; the journal also refused deletion. Recovery succeeded only after
task-owned whole cgroups were killed, observed empty and removed, followed by
explicit operator `volume recover`. Another fresh server returned the same row.

Writer/readers exclusion, concurrent readers, wrong ownership, missing storage
capacity, aggregate disk overcommit, unmounted deletion and ID stability were
verified. The writer test observed actual live children and proved all exited
before the reservation became reusable. Recreating a friendly volume name
allocated a different ID; an old mount/metadata grant could not follow it.

The concurrent source-substitution test made 16 attempts:
2 bound the catalogued directory safely, and
14 rejected before image exec. Bubblewrap directory-FD
operations compare the final mount's device/inode against the opened descriptor;
plain pathname binds through procfs descriptors are not used. Target symlinks,
source links, relative/config/environment aliases, init-hook targets and raw
launch overrides were rejected. Agent-authorized code first wrote a real script
into results; protected templates could not execute that script.

## Native-agent boundary and limits

Both actual restricted clients used UIDs/GIDs 65534 and 65533, zero capabilities,
empty supplementary groups, no_new_privs and private PID/mount namespaces with
private procfs. The report records kernel-confirmed values. They could not browse
data/catalog/images, read worker environment/FDs/command lines, join worker
namespaces, signal the supervisor, inspect/stop/log the database, or substitute
sources. API list/status were grant-filtered. Raw logs required a separate grant
and could only disclose that grant's own instances.

A separate probe found that **UID separation alone exposes command lines on
ordinary host procfs**. Such a native-agent profile is not qualified. A trusted
agent launcher must enforce the protected process view; Silo cannot infer that
external protection merely from worker.json's agent UID. The suite created only
private task namespaces; it did not remount host procfs or change deployment
configuration.

Rootless extraction flattens image owners onto one mapped UID/GID, while keeping
the image read-only and capabilities dropped. This is not multi-user image
ownership preservation or full OCI compliance. Operators must audit/pin all
executable inputs; alias checks are not a shell parser or program analyzer.
The catalog is policy-version bound; migration/backups are not implemented.
Power-loss, reboot, XFS and cloud workspace durability were not tested. Packaging,
release archives, attach/exec and all other excluded v1 features remain deferred.

Every recorded task-owned cgroup, storage mount, loop mount, persistent data
root and agent bridge directory was removed. No deployment, live infrastructure,
configuration, live grants or releases changed; no follow-up work was filed.
