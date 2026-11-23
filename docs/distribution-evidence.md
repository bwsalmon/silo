# Measured Linux x86-64 candidate qualification

On **2026-11-23**, source **865720414cd8e98b74233734856c54bd1219aee0**
passed distribution acceptance on this task's disposable Debian 12 worker and
a separate clean Debian runtime filesystem in a private mount namespace.
Prerequisite PRs #244 and #245 were verified on main before implementation.
This qualifies only the recorded artifact/configuration, not production or
another worker.

| Item | Measured result |
| --- | --- |
| Archive | `silo-0.1.0-linux-x86_64.tar.xz`, 9,707,484 bytes |
| Archive SHA-256 | `30d67a3eadb9488c90a435ae432b709611c951dc2725fe6cc6f1898c2f165bea` |
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
Rust/LLVM notices including the unwinder’s separate legacy notice block,
musl copyright/source with Rust's exact security-patch recipe,
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
**1 safe launch / 15 pre-exec rejections**; the builder worker also had
**1 / 15**. No alternate data was exposed.

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
    --sha256 30d67a3eadb9488c90a435ae432b709611c951dc2725fe6cc6f1898c2f165bea \
    --reports /private/evidence/new-run
```

The final clean acceptance used a fresh `debootstrap --variant=minbase` Debian 12
root with ca-certificates, sudo, Python, libcap2, binutils, gcc, libc6-dev,
musl-tools, skopeo, umoci, openssl, busybox-static, e2fsprogs, util-linux and
procps, containernetworking-plugins and iptables. Its CNI/iptables packages were an apt dependency of skopeo's Debian
common package; Silo did not invoke them or change firewall rules. An earlier
provisioning attempt left that virtual dependency unconfigured; `apt-get
--fix-broken install --no-install-recommends` completed it. The final fresh
root included the provider and libc headers explicitly and bootstrapped
successfully. Only this new root's trust store
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

Three earlier runs of archive
`1fa91e7ff0673d65ef87f330ba6d63ea09e2687ad48989d748c61a8450cbf5a7`
remain explicitly **blocked**:

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

[CI run 45142599948](https://github.com/bwsalmon/silo/actions/runs/45142599948)
and run 45142608597 completed successfully for the source above, including both
independent builds, 13 extracted-archive tests and CI artifact generation.
The [candidate artifact](https://github.com/bwsalmon/silo/actions/runs/45142599948/artifacts/14803565213)
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

## Requested built-in MCP extension

On **2026-11-23**, implementation commit
**`cf58181869a33168c7fb020f87f0883cc73c5f5b`** passed the additive local stdio
MCP qualification. Git ancestry verified the landed task #104–#109 PRs
#239 and #241–#244/#246, plus the intervening #245 real-worker qualification,
before extending the baseline. The original MCP deferral is deliberately
superseded only for this frontend; the other security boundaries remain.
See [MCP protocol/operator instructions](mcp.md) and
[exact schemas](mcp-tools.json).

| Item | Measured MCP-extension result |
| --- | --- |
| Archive | `silo-0.1.0-linux-x86_64.tar.xz`, **19,536,612 bytes** |
| Archive SHA-256 | `c5ae6f81f303ae674ca038d71267c23df8a7109e900b9bb4eb3d63e82071035a` |
| Silo SHA-256 | `6423f06d1400043b5bd38d8c21c8713dbaf84a8cb61e891c57ed0d1a64d555eb` |
| Bundled Bubblewrap SHA-256 | `8d261496c2452c932f1e249d08a1beb30a807504a274b45deb1df802b72d09e0` (unchanged upstream 0.13.1) |
| Source snapshot SHA-256 | `62a073a805fb475492d47916b6310edb057ee14fbb61da69fc09e2066b25b14f` |
| Reproducibility | Two independent local builds and the independent CI build produced the same complete archive hash |
| Protocol/client | Local stdio MCP **2025-06-18**; pinned official **rmcp 0.8.5** server and SDK test client; independent `silo-acceptance` JSON-RPC client version 1 |
| Regression checks | Formatting, native/musl Clippy with warnings denied, **49 Rust tests**, **17 Python tests against the extracted archive**, actual initialization/discovery/scoped-call artifact smoke |
| Builder-worker archive acceptance | **113 passing assertion groups**, **525 actual MCP calls**, all runner/network/volume suites, qualified worker/archive verdicts, verified cleanup |
| Clean-runtime archive acceptance | **113 passing assertion groups**, **515 actual MCP calls**, all suites, qualified worker/archive verdicts, verified cleanup |

The unaltered [clean worker report](evidence/distribution-mcp-clean-worker.json),
[clean archive verdict](evidence/distribution-mcp-clean-archive.json),
[builder worker report](evidence/distribution-mcp-builder-worker.json) and
[builder archive verdict](evidence/distribution-mcp-builder-archive.json) bind
these observations to the exact extracted executable and bundled helper.
[Independent verification](evidence/distribution-mcp-verification.json) records
source/harness/report hashes, prerequisite merge commits, staged clean-root
harness equality, actual ELF results and post-run disposal. The
[clean package inventory](evidence/distribution-mcp-clean-packages.tsv) records
185 runtime/prerequisite packages; the [kernel process profile](evidence/distribution-mcp-clean-profile.txt)
records worker UID/GID 1000:1000 and zero effective/permitted/inheritable/ambient
capabilities. The two isolated native-agent principals remain 65534 and 65533.

The separate freshly bootstrapped Debian 12 filesystem contained **no system
Bubblewrap, Rust compiler, Cargo/cache or Rust checkout**. Only the candidate
and matching Python/C acceptance harness were staged. The retained
[provisioning script](evidence/distribution-mcp-prepare-root.sh) and
[private-root runner](evidence/distribution-mcp-run-root.sh) show the
`pivot_root`, old-root detachment and rootless execution; this shares the
worker's **6.1.0-56-amd64** kernel and is not a new VM/kernel qualification.
Provisioning initially stopped at a missing local CA parent directory; creating
that directory and resuming installation completed the fresh root. The
already trusted task CA was installed only in that disposable root, with its
hash recorded. Sudoers and package changes were confined to it. Sudo emitted a
fixed hostname-resolution warning because the disposable root lacked the host
name entry; acceptance completed successfully and MCP protocol diagnostics
remained redacted. The runtime root and temporary extracted installation were
disposed after exporting evidence; independent checks found no recorded
cgroups, mounts, loop backing files or agent bridges left behind.

The exact distributed binary served the five MCP tools. Restricted launches,
list/status/stop and separately granted logs traversed the authenticated
supervisor. Actual tests covered cross-principal denials, malformed/oversized
messages, strict schemas and forbidden overrides, expired/revoked identities,
credential FD closure and argv/environment non-disclosure, audit attribution,
cancellation/disconnect with a deliberately lost launch response, and bounded
lease teardown. The existing live TCP revocation, namespace/control-descriptor
separation, egress denial and protected-volume/Postgres persistence/recovery
assertions remained enabled. Operator setup and legacy raw-protocol/actual OS
principal attacks stayed on their original interfaces; MCP gained no operator
capabilities. Unit mocks did not qualify these runs. Missing helper/worker
capabilities still failed closed, and rejected profiles stayed unqualified.

Two preliminary direct-worker runs are retained as **blocked**, not used for
qualification: [temporary log-bound mismatch](evidence/distribution-mcp-failed-log-bound.json)
and [test adapter dropping extra CLI override arguments](evidence/distribution-mcp-failed-adapter.json).
The frontend retained the supervisor's 16 KiB-per-stream snapshot contract,
and the adapter now maps extra override inputs into rejected MCP arguments.
No security assertion was weakened or skipped. Both final archive runs above
passed with those corrections and the stricter transport validation.

Reproduce the exact-artifact checks with the commands in
[distribution.md](distribution.md); `qualify-bundle.py` now requires actual
MCP call evidence and invokes `--frontend mcp`. The
[implementation CI run](https://github.com/bwsalmon/silo/actions/runs/45149404721)
completed both jobs successfully and its [candidate artifact](https://github.com/bwsalmon/silo/actions/runs/45149404721/artifacts/14806157060)
has the same archive hash (14-day CI retention). `grain ci wait` was denied for
lack of `grain#ci`; completed verdicts and logs were checked with authorized
`grain ci list/log` and PR commands. Evidence files and this attestation remain
excluded from archive inputs, so the evidence-only commit preserves those
archive bytes. Final-head CI must finish before the PR is complete.

The scope is only this recorded disposable Debian 12 x86-64 configuration and
exact artifact. ARM64, XFS, reboot/power-loss, production approval, remote/HTTP
MCP and confinement of the agent's other tools are not claimed. The protected
worker/OS model, session/project scope, immutable templates, fixed network and
volume grants, redacted errors, expiry/revocation and supervisor-owned teardown
remain required. No deployment, production grant/configuration change,
release tag, external package publication or automatic follow-up task occurred.

A [supplemental two-project executable test](evidence/distribution-mcp-crossproject-test.py)
then launched one real job in each of two protected project supervisors using
the same extracted archive. Its [raw report](evidence/distribution-mcp-crossproject-worker.json)
records successful direct cross-project list/status/stop/log denials,
invisible-versus-unknown equivalence, rejected identity/project/token/endpoint/
destination overrides, foreign-credential rejection before initialization,
and verified teardown of both jobs. This partial supplement deliberately
retains the full gate's **blocked** qualification (it does not run the network
and volume suites); the two complete qualified reports above establish runtime
parity. Initial attempts using a [shared cgroup domain](evidence/distribution-mcp-crossproject-shared-cgroup-rejected.json)
and [shared storage root](evidence/distribution-mcp-crossproject-shared-storage-rejected.json)
failed closed and remain unqualified. The corrected test supplies each
supervisor its own protected storage and cgroup roots, as required by the
existing worker contract. All supplemental domains, mounts and the extracted
installation were removed. Recreating the build source snapshot after adding
all evidence produced the same source archive hash recorded above.
