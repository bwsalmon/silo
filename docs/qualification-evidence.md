# Full real-worker qualification after PR #244

On 2026-11-22, the unchanged runner, relay, protected-volume and qualification
code at merged main **`c6485cb46c3a9da84ef0ef4962447da148711b9c`** passed a fresh
`--suite all` run on Task #115's disposable Linux x86-64 worker. That commit
merges PR #244 / Task #108. The promoted specification is
`5ae66d9db83199e98561dc37c82d1b4bd19d9078`, with the delivered CLI/storage
clarifications described in [runner.md](runner.md) and [volumes.md](volumes.md).

The [unaltered runner report](evidence/task-115-worker-attempt-1.json) records
**99 passing assertion groups**, `result: passed`, completed suites
`runner/network/volumes`, `cleanup.verified: true`, and the delivered
`qualification.verdict` result **`qualified`**, with no blocking reasons.
The [supplemental observations](evidence/task-115-verification.json) bind that
report's SHA-256 to the source commit/tree, source-file hashes, build, worker
packages and independent post-run cleanup checks. No runtime or harness defect
was demonstrated; this change supplies the missing measured evidence.
Historical reports, including `storage-parent-guard.json`, remain unchanged
and retain only their original scope. The latter still returns `blocked` under
the current gate; it was not used to qualify this run.

## Build and reproduction

Use a fresh disposable worker satisfying [all prerequisites](runner.md#disposable-validation)
and [protected storage requirements](volumes.md#worker-storage), with the tool
pins in the report. This run used Rust **1.85.1**, the committed `Cargo.lock`,
and these commands from the source commit above:

```sh
rustup toolchain install 1.85.1 --profile minimal \
  --component rustfmt --component clippy --target x86_64-unknown-linux-musl
cargo fmt --all -- --check
cargo clippy --locked --all-targets -- -D warnings
cargo clippy --locked --all-targets --target x86_64-unknown-linux-musl -- -D warnings
cargo test --locked --all-targets
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -p 'test_*.py'
cargo build --locked --release --target x86_64-unknown-linux-musl
binary=target/x86_64-unknown-linux-musl/release/silo
readelf -l "$binary" > /tmp/silo-elf-headers
readelf -d "$binary" > /tmp/silo-elf-dynamic
! grep -q INTERP /tmp/silo-elf-headers
! grep -q NEEDED /tmp/silo-elf-dynamic
PYTHONDONTWRITEBYTECODE=1 python3 tests/real_worker.py --suite all \
  --binary "$binary" --report docs/evidence/task-115-worker-attempt-1.json
```

Formatting, both Clippy invocations, **46 Rust tests**, five Python reporting
regressions, and validation of all three example manifests passed. Inspection
of the actual release ELF found no interpreter or dynamic dependencies.
The tested executable SHA-256 is
`56f63de84cbc1d09220ab13f61ed13e44fe231b0b8e6ec1543eac19af0c872f9`;
`Cargo.lock` SHA-256 is
`467dcbe8ddd876255daf05a7d495dd3e7fd722175ec78fe5958f62fdad02c41b`.
The Python reporting tests are bookkeeping checks; qualification came from the
subsequent real-kernel execution and teardown.

To evaluate the saved evidence with the delivered gate:

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=tests python3 - <<'PY'
import json
from qualification import verdict
with open('docs/evidence/task-115-worker-attempt-1.json') as f:
    report = json.load(f)
assert verdict(report) == report['qualification']
assert verdict(report)['status'] == 'qualified'
PY
```

## Recorded worker and boundaries

The worker was Debian 12, kernel `6.1.0-56-amd64`, x86-64, four logical CPUs,
16,373,325,824 bytes RAM and no swap. Persistent backing was `/dev/vda`, ext4.
Unprivileged user namespaces were enabled (`1`), the namespace ceiling was
`62289`, and the AppArmor unprivileged-userns restriction sysctl was absent.
Existing parent cgroup v2 delegation already supplied CPU, memory and pids;
only a new task-owned subtree and this test process were provisioned/migrated.
No parent controllers, global sysctls, network policy or deployment settings
were changed.

The supervisor ran as UID/GID 1000:1000 with supplementary groups cleared and
zero host capabilities. The actual native-agent probes and CLI calls ran as
**65534:65534** and **65533:65533**, each with empty supplementary groups,
zero effective/permitted/inheritable/ambient capabilities, `no_new_privs=1`,
a private PID/mount context and private procfs. Kernel profiles and namespace
inodes are in `agent_kernel_profiles`; each probe saw only PID 1. Actual OS
probes denied access to catalog/data/image storage and supervisor process
state, signaling, and mount-source substitution.

Image storage was a protected **1 GiB / 65,536-inode tmpfs**. Persistent storage
used four task-owned ext4 loop filesystems of **64/64/256/64 MiB**, each with
4,096 inodes and `nosuid,nodev,noexec`, below an owner-only `/var/tmp` root.
The report preserves the exact `worker`, `network_worker`, `storage_worker`
and `postgres_example` configuration, including aggregate and template limits.
The storage profile used a 2 GiB aggregate memory ceiling, 768 MiB per job,
512 aggregate tasks, 2,000 CPU millis, and bounded scratch/disk/duration;
CPU/pids/memory/swap settings were kernel enforced.

Exact executable hashes for Bubblewrap, skopeo, umoci, static BusyBox and Silo,
and all stock/fixture image digests are in `pins`. Bubblewrap was non-setuid
Debian `0.8.0-2+deb12u1`, with the required directory-FD binds and userns/tmpfs
features. Skopeo was `1.9.3+ds1-1+b10`, umoci `0.4.7+ds-3+b7`.
This fresh image needed `skopeo`, `umoci`, `musl-tools`, `busybox-static` and
`e2fsprogs` installed, plus pinned Rust; the supplemental artifact records
versions of all relevant packages. Full qualification also requires binutils,
gcc, openssl, Python, sudo and util-linux mount/loop/setpriv support.

## Acceptance audit and measurements

| Canonical v1 scope | Actual assertions and observations |
| --- | --- |
| Static runner and isolation | Stock digest-pinned OCI execution, image defaults/overrides, whiteouts and malicious archive rejection; separate helper/workload PID and mount namespaces; no control FD/environment inheritance; denied host files, egress, namespace joins/creation; fail-closed missing capabilities and bad pins. |
| Resource limits and lifecycle | Scratch filled at 4 MiB; pids exhaustion incremented `max`; memory exhaustion recorded five OOM kills and one group OOM kill; CPU had 21 throttled periods. Quotas, expiry, revocation, setsid grandchildren, normal parent exit and supervisor loss all required verified whole-tree cleanup. |
| Real TCP relay | Stock wget named/numeric allow and unrelated-client deny; directional and reverse-connection denial; other-port/service/host/outbound denial; parallel duplex, half-close, reset, repeated streams, quotas, stalled readers, FD separation and revocation/expiry teardown. Missing SCM_RIGHTS and listener conflicts failed closed. |
| Protected storage and stock Postgres | Real RO write denial and disk ENOSPC; writer/readers exclusion; stable IDs and name-reuse denial; catalog/source/target/symlink races and mutable executable-input rejection. Stock initdb/server/psql used UID/GID 70:70 and `/var/lib/postgresql/data`, with TCP-only configuration. Restricted SQL denied secret-table/server-file reads, COPY PROGRAM and role escalation. |
| Persistence and actual native-agent authority | Committed row survived server replacement and live-writer supervisor SIGKILL (nine processes initially). Fresh-state reattachment was denied; explicit recovery succeeded only after kernel-verified cgroup removal, followed by another persisted-row query. The two-identity CLI transcript and OS probes exercised scoped visibility, status, logs, stop, launch/mount overrides and filesystem/process denials. |

Direct private-loopback measurements were 0.016495 s for 32 short exchanges
and 0.011313 s for 4 MiB; relayed measurements were 0.311235 s and 0.596868 s.
These are single-run fixture timings, not a throughput guarantee. The report
also records per-scenario supervisor CPU/RSS/FD observations. Supervisor
FD/thread counts returned from the live relay state to the baseline **14/5**.
All 16 concurrent source-substitution attempts rejected before exec; ordinary
catalogued mounts succeeded in the other storage cases.

After suite teardown, independent procfs, cgroup, `findmnt` and
`sudo losetup --json --list` observations confirmed no tested-binary processes,
Silo test cgroups, mounts referencing the recorded roots, loop devices,
persistent roots, worker roots or native-agent fixture directories remained.
The supplemental artifact records those exact removed paths and empty results.

This qualifies only this disposable worker and tested native-agent launcher
profile. It does not authorize deployment or enabling untrusted workloads.
It provides no power-loss/reboot, backup, XFS, cloud replacement, ARM64 or
VM-grade isolation certification. V1 exclusions remain unchanged. Release
packaging, bundled Bubblewrap source/notices/dependencies and qualification of
the actual archive remain Task #109's subsequent scope.
