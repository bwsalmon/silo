# Measured Linux x86-64 candidate qualification

On **2026-11-23**, source **95e3080bde67696d34714c534e2bcc3900c18a3b**
passed distribution acceptance on this task's disposable Debian 12 worker and
a separate clean Debian runtime filesystem in a private mount namespace.
Prerequisite PRs #244 and #245 were verified on main before implementation.
This qualifies only the recorded artifact/configuration, not production or
another worker.

| Item | Measured result |
| --- | --- |
| Archive | `silo-0.1.0-linux-x86_64.tar.xz`, 9,706,144 bytes |
| Archive SHA-256 | `767cd95115f17a0f3d9c9bb64a61971fa8e73d6b13f89d51a93b45d63bb14c0e` |
| Silo SHA-256 | `f803a3a5d7f80fb57decc0fde0e5ca0f645be3de3de633773197ecb5b6840024` |
| Bubblewrap 0.13.1 SHA-256 | `8d261496c2452c932f1e249d08a1beb30a807504a274b45deb1df802b72d09e0` |
| Reproducibility | Two fresh local source/target trees produced identical complete archives; independent CI builds produced the same archive SHA-256 |
| Actual ELF | Silo: no interpreter / DT_NEEDED. Bubblewrap: ELF64 x86-64, glibc loader, libc.so.6 and libcap.so.2; required glibc symbols through 2.34 |
| Regression checks | Formatting, native and musl Clippy with warnings denied, **47 Rust tests**, **13 Python reporting/package tests** against the extracted archive passed |
| Actual worker checks | **110 assertion groups per environment**, all runner/network/volume suites completed, both worker and archive verdicts qualified, cleanup verified |

The [clean worker report](evidence/distribution-clean-worker.json) and
[archive verdict](evidence/distribution-clean-archive.json) are unaltered.
The [builder-worker report](evidence/distribution-builder-worker.json) and
[archive verdict](evidence/distribution-builder-archive.json) record the second
real run. [Independent verification](evidence/distribution-verification.json)
binds their hashes to the source commit/tree, exact source snapshot, archive,
binaries, harness sources, package inventory, runtime libraries and cleanup.

## Artifact and worker observations

The archive contains unmodified checksum-pinned upstream Bubblewrap source and
its LGPL notice, complete Silo/vendored Rust dependency source and inventory,
Rust/LLVM notices, musl copyright/source with Rust's exact security-patch recipe,
and GCC runtime notices/exception. Compiler/header/tool payloads were checked
against **90 SHA-pinned Debian packages**; installed Rust compiler/Cargo and
host/musl standard libraries were checked against components from the pinned
Rust 1.85.1 manifest.

Both complete archives were built using `scripts/build-bundle.py` in separate
temporary trees. `scripts/qualify-bundle.py` authenticated the archive checksum,
extracted it, validated payload modes/hashes and the actual ELF/library
requirements, relocated the entire tree, and invoked the existing real harness
with **both the extracted silo and extracted libexec/bwrap**.

The clean runtime root contained **no system bwrap, Rust compiler, Cargo cache
or Rust checkout**. Only the archive and the matching acceptance scripts/C
fixtures were staged there; hashes of those scripts are recorded independently.
The [complete package inventory](evidence/distribution-clean-packages.tsv)
records its 186 installed packages. It used Debian glibc 2.36 and libcap 2.66;
glibc 2.34 is an ELF symbol requirement, not a separately tested worker version.

The environment shared the disposable worker's **6.1.0-56-amd64** kernel.
Provisioning used a private mount namespace and a new filesystem root through
`pivot_root`, with the old root detached. The supervisor then ran as
**1000:1000 with zero effective/permitted/inheritable/ambient capabilities**.
A real unprivileged user-namespace probe passed in that context. The two native
principals **65534:65534 and 65533:65533** had empty supplementary groups,
no_new_privs, zero host capabilities and private PID/mount/proc views, confirmed
by kernel observations. Both failed actual write/create attempts against the
installed binaries, manifest and bundle root.

All original runtime assertions remained enabled. Real runner/relay checks
included named/numeric allow and deny, duplex/backpressure, lease/revocation,
namespace/descriptor separation and missing-capability rejection. Helper tests
used missing, substituted, wrong-architecture, non-executable, setuid/setgid,
group-writable, symlinked and actually missing-library executables. A malicious
executable named bwrap on caller PATH was never executed. Separate package
tests checked relocation including spaces, unsafe archive entries, changed
pins and an actual dynamic-loader failure for an absent library.

Protected storage used the existing 1 GiB / 65,536-inode image tmpfs and four
ext4 loop slots of 64/64/256/64 MiB, each with 4,096 inodes and
nosuid,nodev,noexec. The clean report records actual scratch exhaustion at
4 MiB, a pids `max` event, five OOM kills / one group OOM kill, and
21 throttled CPU periods. These are kernel observations, not mock counters.

Stock digest-pinned Postgres/psql used actual UID/GID 70:70 and its normal data
directory. Authorized SQL committed/read the row; secret-table/server-file
access, COPY PROGRAM and role escalation were denied. The row survived server
replacement and supervisor SIGKILL with a live writer, refused fresh-state
reattachment and explicit recovery after verified whole-cgroup removal.
The source-swap race made 16 attempts in each run: the clean worker had
**2 safe launches / 14 pre-exec rejections**; the builder worker had
**5 / 11**. No alternate data was exposed.

## Reproduction and rejected profiles

Follow [distribution instructions](distribution.md) and the existing
[runner](runner.md) / [storage](volumes.md) prerequisites. The measured build and
acceptance commands were:

```sh
python3 scripts/build-bundle.py --out dist/release-first
python3 scripts/build-bundle.py --out dist/release-second
cmp dist/release-first/silo-0.1.0-linux-x86_64.tar.xz \
    dist/release-second/silo-0.1.0-linux-x86_64.tar.xz
python3 scripts/qualify-bundle.py \
    --archive dist/release-first/silo-0.1.0-linux-x86_64.tar.xz \
    --sha256 767cd95115f17a0f3d9c9bb64a61971fa8e73d6b13f89d51a93b45d63bb14c0e \
    --reports /private/evidence/new-run
```

The second acceptance used a fresh `debootstrap --variant=minbase` Debian 12
root with ca-certificates, sudo, Python, libcap2, binutils, gcc, libc6-dev,
musl-tools, skopeo, umoci, openssl, busybox-static, e2fsprogs, util-linux and
procps. Its CNI/iptables packages were an apt dependency of skopeo's Debian
common package; Silo did not invoke them or change firewall rules. The initial
debootstrap left that virtual dependency unconfigured; `apt-get --fix-broken
install --no-install-recommends` completed it. Only this new root's trust store
received the already trusted package-cache CA; TLS verification stayed enabled
and the CA fingerprint is in the verification record.

Provision the runtime root only in a task-owned private mount namespace:
bind it as a mount point, provide task-context /dev, /sys and /proc,
`pivot_root` into it, detach the old root, then drop to the dedicated worker
UID/GID with cleared groups/capabilities before running the qualifier. Sudo was
configured solely inside that disposable root to provision its own fixtures.
No host/deployment sudoers, grants, sysctls, parent controllers or live
configuration were changed. A fresh VM satisfying the same recorded
requirements can run the qualifier directly; a chroot is not an equivalent
worker configuration.

Three failed runs remain explicitly **blocked**:

- [Missing fixture headers](evidence/distribution-failed-headers.json):
  minimal-root gcc could not find errno.h while compiling the seccomp fixture.
  libc6-dev was added to that disposable root; no runtime library was bypassed.
- [Chroot profile](evidence/distribution-failed-chroot.json):
  Silo reported **Worker denies CLONE_NEWUSER**. Linux rejects unprivileged
  user namespaces when the process root differs from the mount-namespace root.
  This chroot remains unqualified. The later private namespace with its own
  root retained the same rootless supervisor and every Silo isolation check.
- [Source-race diagnostic](evidence/distribution-failed-source-race.json):
  the previous archive safely rejected a renamed mount source with
  unsupported_worker, while the strict test required launch_failed.
  Source 95e3080 recognizes upstream's `Can't open source` diagnostic.
  The test was kept strict, a Rust regression preserves unsupported_worker
  for actual namespace denial, and both full real runs passed afterward.

## Cleanup, CI and limits

Independent post-run checks found no Silo fixture cgroups, mounts, loop backing
files, persistent data roots, agent bridges or temporary installations. Each
worker report and archive wrapper separately verified cleanup. The separate
runtime root was disposed after reports/package/library observations were
exported; it holds no live named data.

[CI run 45140564761](https://github.com/bwsalmon/silo/actions/runs/45140564761)
and run 45140572819 completed successfully for the source above, including both
independent builds, 13 extracted-archive tests and CI artifact generation.
The [candidate artifact](https://github.com/bwsalmon/silo/actions/runs/45140564761/artifacts/14802350454)
contains the same archive hash (14-day CI retention). `grain ci wait` was denied
because this task lacks grain#ci; completed forge verdicts and logs were read
through the authorized `grain ci list/log` and PR commands instead. Generic CI
does not claim worker qualification.

Evidence files are excluded from archive inputs as documented, so adding this
attestation does not change the accepted archive. These measurements do not
qualify another host, kernel, library/tool version or native-agent launcher.
There was no ARM64, XFS, reboot/power-loss, VM tenant-boundary or production
qualification. No release, tag, registry publication, deployment, live grant or
configuration change occurred. All v1 exclusions and runtime limits remain in
force; operator release/installation/workload approval are separate decisions.
