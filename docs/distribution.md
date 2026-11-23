# Linux x86-64 candidate distribution

The archive contains a static musl `silo` and unmodified upstream Bubblewrap
**0.13.1** at `libexec/bwrap`. It also contains manifests, examples, operator
guides, dependency/license inventory, notices, exact Bubblewrap source and
complete Silo/vendored-crate source. Separate musl copyright/source and Rust’s
exact musl security-patch recipe are included, alongside Rust/LLVM and GCC
runtime-library notices. It is a candidate artifact, not a published
release or permission to run untrusted jobs.

Silo is inspected for **no ELF interpreter and no DT_NEEDED entries**.
Bubblewrap is checked independently: ELF64 x86-64, interpreter
`/lib64/ld-linux-x86-64.so.2`, and `libcap.so.2` / `libc.so.6`.
The current build requires glibc symbols through **GLIBC_2.34**. These libraries
are supplied by the worker and are not bundled. The validator resolves the
actual libraries and executes the exact helper's version command with a clean
environment. A static Silo does not make Bubblewrap static, nor does either
binary provide kernel privileges.

## Rebuild without publishing

Use an isolated **Debian 12 amd64** builder. CI uses the immutable builder image
digest in `packaging/inputs.lock.json`; this describes a build environment,
not a Docker interface in Silo. The complete compiler/header/tool dependency
closure is version- and SHA-256-pinned in `packaging/builder.lock.json`.
The build verifies installed compiler, interpreter, library, header and
pkg-config payloads against those verified Debian archives. Documentation,
translations and local machine configuration are not compilation inputs.
Retain a private cache/mirror of the exact archives if upstream retention is
insufficient; a missing or changed archive must fail, never silently repin.

Bootstrap Python 3, CA certificates, curl, git and xz transport tools in the
disposable builder, then run from a trusted source checkout:

```sh
# Root is used only inside the disposable builder for exact build packages.
sudo python3 scripts/builder.py --install
python3 scripts/bootstrap-rust.py
python3 scripts/build-bundle.py --out dist/first
python3 scripts/build-bundle.py --out dist/second
cmp dist/first/silo-0.1.0-linux-x86_64.tar.xz \
    dist/second/silo-0.1.0-linux-x86_64.tar.xz
(cd dist/first && sha256sum -c SHA256SUMS)
```

Rust 1.85.1's manifest and rustup bootstrap are hash-pinned. Rustup verifies
component downloads against that manifest, and the build compares the installed
Rust compiler/Cargo and host/musl standard-library payloads against those exact
component archives. Cargo.lock pins crate versions and
registry checksums; Cargo vendors those inputs and compiles offline/frozen.
Bubblewrap's upstream archive hash, build options and fixed SOURCE_DATE_EPOCH
are pinned. Builds use fresh temporary source/target trees, fixed ownership,
permissions, archive ordering/times and remapped compiler paths. The builder
never executes `cargo update`, tags, publishes or uses release credentials.

`source/silo.tar.xz` is the exact build input snapshot. To rebuild from the
bundle, extract it into a fresh directory, initialize a local git repository
and add its files so the script can enumerate them (`git init && git add .`).
The separate `source/rust-dependencies.tar.xz` retains full crate source and
notices; it can seed Cargo's vendor directory/cache for disconnected builds.
Rust toolchain and Debian archives still need their pinned cache entries.
The source snapshot deliberately excludes
`docs/distribution-evidence.md` and `docs/evidence/distribution-*`: measured
attestations about an archive are stored separately to avoid making the
archive's hash depend on its own report. All executable, test, packaging,
lockfile and operator-guide changes remain build inputs.

Reproducibility means byte equality for the same source inputs, pins and
builder architecture, not arbitrary compiler/platform equivalence. CI performs
two independent builds and compares the complete archives. Its standard CI
artifact contains the archive, SHA256SUMS and inventory for 14 days.
`contents: read` is the only repository permission; there is no release/tag
trigger, registry publishing, signing secret or external publication step.
Generic CI performs package regressions and CLI/helper smoke checks; **it does
not qualify a workload worker**.

## Protected installation model

The trusted operator owns the installation and every ancestor. Use a dedicated
non-root supervisor UID and different restricted native-agent UIDs, with the
protected process views described in [runner.md](runner.md). Never extract into
an agent workspace, writable volume, shared writable directory or job image.
Jobs receive neither the installation tree nor worker configuration. Owner/root
directories must prevent untrusted rename, and files must not be group/world
writable, setuid, setgid or symlinks. Root-owned sticky temporary ancestors are
accepted only for a private task-owned qualification directory.

Obtain the expected archive SHA-256 through a trusted review/CI channel.
A checksum supplied beside an untrusted archive is not authentication.
Use the repository's trusted validation script; do not execute a script from
an unverified archive as root.

```sh
# Example paths only: provision the parent through trusted operator setup.
# The extraction destination must not already exist.
python3 scripts/validate-bundle.py \
  --archive /private/download/silo-0.1.0-linux-x86_64.tar.xz \
  --sha256 <trusted-archive-sha256> \
  --extract-to /private/installations/candidate
python3 scripts/validate-bundle.py \
  --root /private/installations/candidate/silo-0.1.0-linux-x86_64
```

The extractor verifies the external checksum before extraction and rejects
absolute/traversal paths, duplicates, links, special files and unexpected modes.
The validator checks protected paths, every payload hash/mode, no extra files,
actual ELF requirements, actual helper/library availability and all three
example manifests. It prints the **absolute `bwrap.path` and SHA-256** stanza
for the protected, owner-only `worker.json`. Merge that stanza into the existing
worker configuration; preserve skopeo/umoci pins, bounded storage and cgroup
requirements. Inspect the installed tree again after any operator ownership
change. If installing as root, grant the dedicated supervisor read/execute
access through protected directories; do not make binaries writable to agents.

Start the supervisor using its exact `.../silo` path. There is no helper discovery
through PATH, no environment override and no fallback to system Bubblewrap.
The existing runtime rechecks the protected absolute helper path, non-setuid/
non-setgid mode and configured hash, including immediately before helper exec.
Relocation requires moving the whole protected tree, rerunning validation and
updating the exact worker pin before starting a fresh supervisor. Do not use a
`current` symlink or update binaries beneath a running supervisor.

## Worker qualification and diagnostics

Use a **new disposable worker** meeting [runner requirements](runner.md) and
[protected persistent-storage requirements](volumes.md). Build tools are not
runtime dependencies of Silo, but the acceptance fixtures require musl-gcc/gcc,
binutils, Python, sudo, skopeo, umoci, openssl, busybox-static, e2fsprogs and
util-linux mount/loop/setpriv support. These test prerequisites are recorded
separately from the archive's runtime libraries.

```sh
python3 scripts/qualify-bundle.py \
  --archive dist/first/silo-0.1.0-linux-x86_64.tar.xz \
  --sha256 <trusted-archive-sha256> \
  --reports /private/evidence/new-qualification
```

This command requires a new report directory, extracts into private task-owned
storage, checks and relocates the tree, and runs the existing real
`--suite all` harness with both `--binary` and **`--bwrap`** from that
extraction. It never discovers system bwrap. Reports bind the archive, executable,
helper and worker report hashes. Actual native principals 65534 and 65533 must
fail to write either binary/manifest or create a replacement in the bundle root.
Runner/relay, Postgres persistence/recovery, resource limits and helper failure
cases use the real kernel. Cleanup failures cannot produce a qualified result.

Diagnose in this order:

- Check archive checksum, protected ownership/ancestors, exact installation path
  and file modes. Missing, substituted, linked or wrong-architecture helpers
  must fail; do not point the worker at a convenient PATH replacement.
- Inspect `readelf -l`, `readelf -d`, `readelf --version-info` and the validator's
  loader output. Install compatible trusted glibc/libcap packages when absent;
  do not use caller LD_LIBRARY_PATH/LD_PRELOAD or copy arbitrary libraries.
- Check the recorded namespace policy, actual protected tmpfs and ext4/XFS
  mounts, cgroup filesystem/controllers/migration permissions and native-agent
  process view. Packaging, `validate`, policy activation, helper `--version`
  and mock tests do not qualify those capabilities.
- Preserve `unsupported_worker`, `launch_failed` and `cleanup_unverified`
  diagnostics. A missing CLONE_NEWUSER capability, controller write permission,
  directory-FD bind support, seccomp/descriptor operation or bounded storage
  is a blocker. Do not change global sysctls, loosen isolation, replace real
  controls with mock files or silently skip acceptance.

The [measured candidate evidence](https://github.com/bwsalmon/silo/blob/main/docs/distribution-evidence.md)
records exact scope and failures. A previously qualified configuration does
not qualify another machine, kernel, tool pin or agent OS context.

## Upgrade and cleanup

Stop admission through the trusted launcher, stop all instances/services and
verify whole-cgroup cleanup before replacing a supervisor. Back up named-volume
data and the protected catalog through an operator-established process.
Automatic schema migration, power-loss/reboot durability and backup tooling are
not implemented. Never delete a volume to repair a version mismatch.

Install the new archive into a separate protected directory, validate it, update
the exact executable/helper pins, then requalify on a disposable worker with
the intended configuration. Policy replacement still requires verified shutdown.
On uncertain supervisor-loss recovery, preserve the journal and refuse
reattachment; follow explicit operator `volume recover` guidance only after
whole-tree removal has been proven. There is no in-place binary hot upgrade.

The qualification harness removes its task-owned workloads, cgroups, tmpfs,
ext4 loop mounts, persistent fixtures and agent bridge. Keep reports and the
trusted archive/checksum for audit. Inspect failed-run cleanup evidence before
removing leftover resources; do not unmount/delete another task's paths.
Once all references are gone, the operator may remove a retired installation.
Deleting a binary archive/install tree must never implicitly delete named data.

## Candidate release checklist

- [ ] Verify prerequisite merges, clean source provenance, pinned inputs and licenses.
- [ ] Run formatting, Clippy and Rust/reporting/package regressions.
- [ ] Build twice; compare archive bytes and verify SHA256SUMS.
- [ ] Inspect both actual ELF files and resolve the helper's real libraries.
- [ ] Verify relocation, malicious PATH/substitution, missing/wrong-architecture
      helpers, bad modes and actual missing-library failure.
- [ ] Qualify the exact extracted archive on a new disposable worker: full runner,
      relay, limits, protected Postgres persistence/recovery and two actual principals.
- [ ] Require qualified worker and archive reports, verified cleanup and independent
      checks for leftover cgroups, mounts, loop fixtures and agent bridges.
- [ ] Bind measured evidence to source snapshot/archive/binary/helper/report hashes,
      record compatibility limits and finish CI with a clean merge into main.
- [ ] Have the operator separately decide release signing/publication, installation,
      production approval and enabling workloads. This task performs none of them.

Linux x86-64 is the sole supported distribution target; ARM64 is untested and
not shipped. Silo shares the kernel and is not a VM boundary. Exclusions remain:
Docker compatibility, external workload egress, dynamic topology, UDP/DNS server,
public API/ingress, MCP frontend, attach/exec and multi-host support. Image
ownership mapping, native-agent process-view, storage migration and crash
recovery limits in the runtime guides remain in force.
