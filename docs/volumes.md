# Protected managed volumes

This extends the delivered private TCP supervisor (PR #242). Policy declarations
are not storage creation. The operator activates with `up MANIFEST --no-start`,
explicitly creates each volume, then repeats `up MANIFEST` to start services.
Jobs start only through `run TEMPLATE`. Repeating identical activation, volume
creation, or an authorized terminal instance stop is idempotent. Live policy
replacement is refused.

## Worker storage

Keep the supervisor, its state, image storage and persistent data outside agent
workspaces under a dedicated non-root OS identity. Native agents must use
different unprivileged OS identities **and a protected process view**. A separate
UID alone does not hide another process's command line on ordinary Linux procfs.
The trusted native-agent launcher must supply private PID/mount namespaces with
a private procfs (as tested), or an equivalently verified OS boundary that hides
worker processes. Do not issue agent credentials to an unqualified native-agent
context. Silo's local worker checks cannot prove the protection of an external
launcher merely from its configured UID. The [runner prerequisites](runner.md),
static executable, pinned tools, real namespaces, protected bounded image tmpfs,
delegated cgroup v2 controls and whole-tree kill are still mandatory.

Add an optional absolute `volume_root` to private `worker.json`, for example
`/private/silo-data`. It must be an owner-only directory on ext4 or XFS, with
trusted symlink-free ancestors. It must be separate from image storage. The
supervisor holds an exclusive lifetime lock there. An absent root or unsupported
filesystem fails volume operations closed with a specific capability error.
Never put this root on tmpfs or a shared agent filesystem.

A privileged **operator**, outside the rootless supervisor, provisions a finite
pool of empty filesystems at `volume_root/slots/slot_0`, `slot_1`, etc. Each slot:

- Is a separate ext4 or XFS mount, owned by the supervisor with mode 0700.
- Has `nosuid,nodev,noexec` mount flags.
- Has total filesystem block capacity at most the volume's declared
  `max_bytes`, and at most 65,536 inodes. A directory on a larger shared
  filesystem does not qualify.
- Starts empty, including removal of an ext4 `lost+found` directory before
  use. The backing storage and mount lifecycle must themselves be durable and
  protected by the operator. The supervisor never mounts disks or invokes sudo.

An operator may use fixed-size ext4 files backed by persistent storage, attached
as loop filesystems, or dedicated bounded filesystems. Slots are a deliberately
small v1 allocation mechanism, not a quota daemon or a privileged mount broker.
Create fails if no empty slot fits; it never silently uses an unbounded directory.
The sum of allocated declared ceilings must fit the worker's `disk_bytes`
ceiling; image storage is independently bounded by that same worker ceiling.
Filesystem metadata reduces usable data capacity. Sparse logical file lengths
can exceed allocated capacity; allocated blocks and inodes remain kernel bounded.

`volume create NAME` allocates a random managed ID, records policy/name/slot,
numeric ownership, byte ceiling and directory device/inode in a protected
catalog, and creates a mode-0700 data directory. The name never becomes a host
path. Repeated creation returns the same initialized ID. No launch performs
recursive chown. Catalog entries and their directory are synced before use. Catalogs contain at most 256 managed entries; each attachment journal is capped at 64 KiB, with capacity checked before launch or journal replacement.

Only one numeric container UID/GID pair below 65535 is mapped per workload.
It maps to the dedicated supervisor host UID/GID. A volume's pair must match its
template. The verified rootless umoci image extraction flattens archive owners
to that host identity; the image is read-only inside the workload. This supports
Postgres at numeric 70:70 without granting host root, supplementary groups or
extra host mappings. It does **not** preserve multiple distinct image owners.
Images needing multi-user ownership or privilege transitions remain unsupported.

## Attachment and deletion

One writer excludes every other attachment. Readers may share only with readers.
The catalog durably journals every attachment and its exact whole-workload
cgroup before the helper can launch. Image/preparation failure cannot release a
journal until the helper is reaped and its whole cgroup is empty and removed.
Normal exits, stop, grant expiry and revocation use the existing verified cleanup
path. Instance replacement never removes data.

Sources are selected only through the catalog. Protected parents prevent agent
rename; O_NOFOLLOW directory opens reject final links, and device/inode checks
reject substitutions both in the supervisor and fresh helper. Bubblewrap uses its race-checking `--bind-fd`/`--ro-bind-fd` operations and verifies the final mounted device/inode against the opened descriptor before image execution. A pinned build that supplies these operations is required. No arbitrary worker path is a control input.
Mount targets must be disjoint, outside protected paths and have real directory
components in the protected image. The init-hook directory
`/docker-entrypoint-initdb.d` is protected too. Data symlinks resolve within the
isolated workload root. The trusted trampoline closes all setup/source
descriptors on image exec.

`volume list` and `volume inspect NAME` return metadata, never content or host
paths. Agent metadata requires both policy permission and grant scope; mount,
metadata, network, SQL role and raw log permissions are independent. The trusted issuer resolves mount/metadata names to created IDs; deleting and recreating a friendly name never transfers an old grant. Issue a new grant after explicit creation.
`volume rm NAME` is operator-only, idempotent after removal, and has no force
option. Any active or uncertain attachment refuses deletion. Interrupted catalog
commits poison the in-process catalog; partial initialization or deletion
requires operator recovery. Do not edit or discard uncertain records to resume.

## Supervisor loss and explicit recovery

Parent-death protections help terminate descendants, but supervisor absence
never proves cleanup. Attachment journals survive process death. A stale
endpoint and any old cgroup descendants block ordinary restart. A different
state directory or token cannot bypass the volume journal.

For the same immutable policy, an operator can recover in this order:

1. Verify the old supervisor is gone; keep its protected state and catalog.
2. Kill **each recorded task-owned whole cgroup** using `cgroup.kill`; verify
   `cgroup.events` reports `populated 0`, then remove that empty cgroup.
   Never remove or alter unrelated worker cgroups.
3. Restart the trusted supervisor using the same protected data root and cgroup
   parent. After verified cleanup, retire the old stale endpoint according to the
   trusted launcher procedure, or use a fresh protected control state with the
   identical manifest. This does not recreate volumes.
4. Run `silo volume recover NAME` as the operator for each uncertain attachment.
   This command refuses any recorded cgroup that still exists, including an
   empty one, and refuses a changed cgroup parent. Kernel removal of the
   journaled whole tree, together with the exclusive data-root lock and trusted
   helper parent-death protocol, supplies the cleanup proof.
5. Repeat `up MANIFEST`, then verify application state.

There is no force reattachment. Corrupt/partial catalog records, changed mount
device/inode identity, lost storage, or a missing cleanup proof require offline
operator investigation. Recovery does not repair filesystem/database corruption.
The v1 catalog is bound to an immutable policy version; policy migration,
backups and cross-worker transfer are outside this workflow.

Static services can be recreated through `up` only after **all** instances in
the current topology are terminal with verified cleanup. Running services make
identical `up` a no-op; a partially stopped topology cannot be changed live.
Replacement allocates fresh instance IDs while keeping volume IDs.

## Trusted executable inputs

The operator must audit and pin the image, command, environment and **every**
input that can affect executable behavior. Enumerate indirect scripts, plugins,
SQL initialization files and code-loading configuration in `executable_inputs`.
Declared executable inputs must resolve inside the immutable image, outside all mounts and scratch. A service may generate protected private state/config through a fixed operator-only initializer, as the Postgres fixture does; no agent can write that state.
The supervisor also rejects known path-valued arguments/environment aliases
that reach agent-writable volumes. Agents cannot change any launch field or
replace policy through the raw protocol.

These checks are not a program analyzer or a shell parser. Fixed inline command
text is trusted operator code. An operator-approved program that secretly loads
unlisted agent-writable code would delegate that program's effective access;
such a template is invalid and must not be approved. Ordinary data input/output
is allowed; never treat SQL scripts, psql backslash commands, initialization
hooks, or plugin configuration as inert data. Read-only mounts are not evidence
that their contents were operator-pinned.

## Stock Postgres acceptance

The disposable suite uses unmodified
`docker.io/library/postgres@sha256:f2f98726820d0342055351c6e9d36e705f5bfffb3dd53ff556325114b08afb6d`
(Postgres 16, Linux amd64 selected by verified index). Both server and stock
psql come from this image. Its actual PGDATA is
`/var/lib/postgresql/data`, and the operator pins UID/GID 70:70.
An operator-only fixed initializer runs stock initdb, uses a private bootstrap
TCP listener, creates a SCRAM application role and grants only the test table.
The service uses `unix_socket_directories=''` and listens on private loopback.
No pathname Unix sockets, host-network join, external egress or modified server
binary are required. Service startup confirms exec, not SQL readiness.

The app reads its fixture, fails to write that read-only mount, writes results
and inserts/selects a row as the restricted role. An unrelated client fails
by name and numeric address while the real database is listening. SQL attempts
to read the secret table/server files, COPY PROGRAM or assume the operator role
fail. A new server reads the committed row from the same managed volume.

Run the [real worker suite](runner.md#disposable-validation) with `--suite volumes`
or `--suite all`. In addition to the runner dependencies it needs ext4 tools,
mount/loop support, persistent task-owned backing space and util-linux setpriv.
It provisions/removes only its own filesystems, cgroups and OS identity bridge
directories. It never creates system users or changes deployment settings.
The two-identity JSON transcript runs actual CLI processes as UIDs 65534 and
65533, with no supplementary groups, no_new_privs, private PID/mount namespaces,
a private procfs and their own private endpoint/credential copies. Those
namespaces belong only to disposable task processes; no host/deployment procfs
mount or configuration is changed. Separate OS probes
verify that those identities cannot browse worker storage or process state,
signal the supervisor or replace mount sources.

Acceptance includes a live-writer supervisor SIGKILL, refused fresh-state
reattachment, explicit recovery after kernel-verified tree removal, and another
successful persisted-row query. This tests process/service replacement on the
recorded worker; it is not a power-loss, backup, reboot, XFS, or cloud workspace
durability certification. See the committed [storage evidence](storage-evidence.md).

## Running the supplied fixture

[examples/postgres.yaml](../examples/postgres.yaml) is a complete disposable
policy with public test passwords, not a production secret configuration.
Prepare four slots and a worker with the ceilings described above. With fresh
operator credentials supplied through the trusted launcher on every call:

1. `validate examples/postgres.yaml`, then `up examples/postgres.yaml --no-start`.
2. Explicitly `volume create fixtures`, `results`, `pgdata`, and `other`.
3. `run seed`; poll its returned ID with `status` until verified `exited` with
   exit code zero. This initializes fixture data without granting agent writes.
4. `run initialize`; wait for its own verified zero exit. This runs stock initdb
   and creates the restricted SQL role before any service attachment.
5. `up examples/postgres.yaml`. Repeating it is idempotent. Application readiness
   is checked separately by bounded stock psql queries in the approved clients.
6. Issue a grant for `app`/`verify`, with `run/list/status/stop`, mounts
   `fixtures:ro` and `results:rw`, link `db:5432`, and the desired instance ceiling.
   Add fixture metadata permissions separately if needed. Do not grant database
   mounts, service controls or raw logs by default.
7. From the isolated agent context, `run app --json`, query `status ID --json`,
   inspect the filtered `ps --json`, and `stop ID --json`. Only an explicitly
   authorized logs grant may call `logs ID` for its own instances.
8. As operator, stop all nonterminal instances and wait for verified cleanup;
   repeat `up` and run `verify` to observe the durable row. Volume metadata/IDs
   survive this replacement. Volume removal remains a separate operator action.

The acceptance harness executes these steps and the denial/recovery cases with
real credentials and OS identities; its JSON report includes the transcript.
