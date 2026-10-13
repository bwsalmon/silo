# Silo local backend, part 3: git repositories for agents, mirrored into one external repo

## Status

Design draft, docs only, written in task 3360 (filed from task 3024).
No implementation code. Like the rest of Silo, implementation waits
for bsalmon's approval. The tasks in the
[staged plan](#11-staged-implementation-plan) are filed one by one
after that.

This page is the third part of the local backend, after
[`local-backend.md`](local-backend.md) (task 3350) and
[`local-rootless-storage.md`](local-rootless-storage.md) (task 3356).
It adds something Silo doesn't have today: **git hosting**. bsalmon set
the direction when the task was filed:

- **One external repo.** Silo is pointed at a single external git repo,
  the **uber repo**. Agents in silos create what look to them like their
  own repos, and clone, branch and push to them normally. silod packs
  all of them into the uber repo.
- **Local backend first.** [§10](#10-later) says how this carries over
  to the GCP backend, without designing that in detail.
- **Outbound only.** silod mirrors into the uber repo and never reads
  changes back from it. Two-way sync is [later](#10-later).
- **Packed as branches, not directories.** Each repo keeps its own
  exact history under a prefix in the uber repo, and SHAs are the same
  in both views.
- **A folder view** of the uber repo's `main`, with a folder per repo,
  was a note for later in the task. bsalmon then commented: "since
  submodule is a layer on top of branches, let's implement that now."
  So the **submodule index** (the preferred option) is designed here
  in full and is part of the staged plan
  ([§7](#7-the-submodule-index)). The rewritten-tree option stays
  [later](#10-later). This page reads "implement now" as "in this
  design's scope and plan". There is no git hosting to implement it on
  yet, and this task is docs only.

Where this page and the first two parts disagree, **this page wins**.
It changes nothing in them except to add things: a data-directory entry,
a policy, a quota class, a manage-grant resource type and a gateway
path. Each one says so where it is introduced.

Calls continue the numbering of the earlier parts, from **L41**
([Calls made on this page](#calls-made-on-this-page)). Open questions
continue from **21** ([Open questions](#open-questions)). The new
staged-plan steps are items 19 to 24 of
[`local-backend.md` §15](local-backend.md#15-staged-implementation-plan).

### What was checked, and against what

- **git 2.39.5**, run in this task's sandbox against a bare "uber" repo
  served by `git http-backend` over smart HTTP, with both protocol v0
  and v2. The following worked as described on this page:
  - a submodule whose `.gitmodules` URL is `./` (and also
    `../<name>.git`) resolved to the superproject's own remote, and
    `git submodule update --init` checked out the gitlinked commit;
  - with `--single-branch`, git fetched the gitlinked SHA by id when it
    was reachable **only from a non-default branch**, and also when it
    was reachable **only from a ref outside `refs/heads`**
    (`refs/silo-archive/...`). Over smart HTTP, `upload-pack` runs
    stateless and accepts any `want` reachable from a ref, in v0 as in
    v2;
  - `git submodule update --remote` followed a `branch =` set to the
    namespaced branch, once the clone had that branch;
  - the external-clone recipe in [§6](#external-view-not-rewritten)
    resolved a submodule whose URL was an in-silo
    `http://silo.internal/git/...` URL.
- **GitHub** (its limits, how it shows submodules, which refs it
  accepts) couldn't be checked from the sandbox: docs.github.com
  didn't answer. Claims about GitHub are stated as the author knows
  them and marked *verify*. Plan item 22 checks them against a real
  repo before anything depends on them.
- **Silo**: this repository at `7462acc`, in particular the first two
  parts of the local backend, [`gateway.md`](gateway.md),
  [`fence-api.md`](fence-api.md) and [`agents.md`](agents.md).

## Contents

1. [The shape](#1-the-shape)
2. [Repos as silo objects](#2-repos-as-silo-objects)
3. [The git service](#3-the-git-service)
4. [The fence on repos](#4-the-fence-on-repos)
5. [Storage and quota](#5-storage-and-quota)
6. [Submodules between silo repos](#6-submodules-between-silo-repos)
7. [The submodule index](#7-the-submodule-index)
8. [The outbound mirror](#8-the-outbound-mirror)
9. [Mapping back: the ref layout and `silo repo external`](#9-mapping-back-the-ref-layout-and-silo-repo-external)
10. [Later](#10-later)
11. [Staged implementation plan](#11-staged-implementation-plan)
12. [Calls made on this page](#calls-made-on-this-page)
13. [Open questions](#open-questions)

## 1. The shape

```
 inside silo-net                     host netns (silod, no capabilities)            outside
┌──────────────────────┐            ┌────────────────────────────────────┐        ┌──────────────────┐
│ agent tool container │  HTTP :80  │ gateway listener (passed by        │        │ uber repo        │
│ in /team-a/x         │──────────▶│ silo-nsd, call L28)                │        │ (e.g. GitHub)    │
│ git clone/push       │ /git/...   │   │                                │        │                  │
│ http://silo.internal │            │   ▼                                │  push  │ refs/heads/silo/ │
│   /git/team-a/x/     │            │ git service ── git upload-pack /   │──────▶│  team-a/x/foo.git│
│   foo.git            │            │                receive-pack        │ (only  │  /main ...       │
└──────────────────────┘            │   │  writes                         │ silod) │ refs/silo-archive│
                                    │   ▼                                │        │ main (index)     │
 host                               │ <data-dir>/silos/<id>/repos/*.git  │        └──────────────────┘
┌──────────────────────┐ front-door │   │ dirty                          │
│ operator: git clone  │──socket───▶│   ▼                                │
│ silo::/team-a/x/foo  │            │ mirror ── per-repo passes, leases, │
└──────────────────────┘            │           archives, then the index │
                                    │ keys/mirror/  (the one credential) │
                                    └────────────────────────────────────┘
```

- A repo is a **silo object**, made and deleted through the fence API
  and `silo repo` ([§2](#2-repos-as-silo-objects)).
- Workloads and agents reach it at **`http://silo.internal/git/...`**,
  on the gateway's existing port 80. Who they are comes from origin
  identity, as everything else on the gateway does
  ([§3](#3-the-git-service)).
- The bare repo lives in **its silo's subtree** of the data directory
  ([§5](#5-storage-and-quota)).
- After every push, the **mirror** pushes the repo's refs into the uber
  repo under a namespace, without ever destroying anything that was
  there ([§8](#8-the-outbound-mirror)). Then it updates the
  **submodule index** on the uber repo's `main`
  ([§7](#7-the-submodule-index)).
- **Only silod holds the uber repo's credential.** Nothing in a silo
  holds any credential for git, internal or external.

## 2. Repos as silo objects

### The object (call L41)

A repo belongs to exactly one silo, as a container or a volume does.

```json
{
  "name": "foo",
  "id": "r-8k2m4q",
  "silo": "/team-a/x",
  "default_branch": "main",
  "clone_url": "http://silo.internal/git/team-a/x/foo.git",
  "clone_url_by_id": "http://silo.internal/git/-k7f3qa/foo.git",
  "size_bytes": 1843200,
  "create_time": "2026-10-12T18:54:00Z",
  "mirror": {
    "state": "IN_SYNC",
    "external_prefix": "refs/heads/silo/team-a/x/foo.git/",
    "last_push_time": "2026-10-12T18:54:03Z",
    "lag_seconds": 0,
    "last_error": null
  }
}
```

- **Names are per silo.** `/team-a/x` and `/team-b/y` can each have a
  `foo`, and so can `/team-a/x` and its own child `/team-a/x/foo`. A
  name is 1 to 63 characters of `[a-z0-9._-]`. It starts with a letter
  or digit, contains no `..`, and doesn't end in `.git` or `.lock`.
  silod adds `.git` itself in URLs and refs (call L46). Names are
  lowercase so that clones on case-insensitive filesystems (macOS) can't
  collide.
- **The id** (`r-` and six base-32 characters) is what the store and
  the data directory use. A repo can't be renamed in v1. A silo
  rename or move changes the repo's URL and external refs, not its id
  ([Renames and moves](#renames-and-moves-call-l53)).
- **`default_branch`** is the bare repo's `HEAD`. It is set at create
  (`main` by default) and changed only by `PATCH`, never by a push. The
  index follows it ([§7](#7-the-submodule-index)).
- **Repos are created explicitly.** There is no push-to-create: a push
  to a repo that doesn't exist gets 404, like a clone. An agent that
  wants a repo calls `silo repo create`, which its silo's authority
  allows.

### Operations

| Call | Does |
| --- | --- |
| `POST /silo/v1/silos/{silo}/repos` | Create. Body: `name`, optional `default_branch`, optional `seed` ([below](#seeding)) |
| `GET /silo/v1/silos/{silo}/repos` | List. `?recursive=true` also lists descendants' repos, each with its silo path |
| `GET .../repos/{repo}` | Get one, with its mirror state |
| `PATCH .../repos/{repo}` | Change `default_branch` |
| `DELETE .../repos/{repo}` | Delete ([§8, Deleting](#deleting-a-repo-or-a-silo-call-l49)). A long-running operation that finishes once the repo's refs are archived in the uber repo, or at once when mirroring is off |

These follow [`fence-api.md`](fence-api.md)'s conventions: errors,
`request_id`, `validate_only`, pagination, and `NOT_FOUND` for anything
the caller may not see. Who may call which is in
[§4](#4-the-fence-on-repos).

`silo` gets a `repo` group. Paths are relative to the caller's own silo
unless they start with `/`:

```
silo repo create foo [--default-branch main] [--seed /team-a/bar]
silo repo list [--recursive] [/team-a]
silo repo get foo
silo repo delete foo
silo repo url foo                 # prints the clone URL for this caller
silo repo clone /team-a/x/foo     # on the host: git clone silo::/team-a/x/foo
silo repo external foo            # where it is in the uber repo (§9)
silo repo archives foo            # its archive refs, local and external
silo mirror status | set | retry | adopt
```

### Seeding

A child silo can't read its parent's repos ([§4](#4-the-fence-on-repos)).
That is deliberate, but agents often want to start a helper with a copy
of the parent's code. So `create` takes a **seed**: an existing repo the
**caller** may read, which silod copies server-side into the new repo
(`git clone --bare --no-local` of its heads and tags):

- `POST /silo/v1/silos/team-a/x/helper/repos` from `/team-a/x`, with
  `{"name": "work", "seed": {"repo": "/team-a/x/foo"}}`, gives
  the child its own `work`, holding `foo`'s history. The child can push
  to it, and the parent can read it back.
- The seed is checked against the **caller's** read access, not the
  target silo's. Seeding moves code down the tree, where an ancestor
  could have pushed it anyway, and never up or sideways.
- The copy is a snapshot. Nothing links the two repos afterwards.

## 3. The git service

### Where it listens (call L42)

- **Inside silos: smart HTTP at `http://silo.internal/git/`**, on the
  gateway's port 80. That is the port that carries exports and tool
  channels, and every silo already reaches it
  ([`local-backend.md` §9](local-backend.md#rules-the-inet-silo-table)).
  The gateway reserves the path `/git/` beside `/x/` and `/r/`.
- **Rootless, nothing new is opened.** Port 80 on `gw0` is a listening
  socket that the namespace holder opened in `silo-net` and passed to
  silod (call L28). The git service is a handler behind it, in silod's
  own process. Its connections never leave `silo-net` on the tenant
  side, and git traffic never goes near the egress proxy.
- **Not on the API port.** Fetching and pushing are data, like a
  volume's writes, not control-plane calls. So they stay on port 80,
  which sealed silos keep ([§4](#sealed-and-read-silos-call-l44)). Creating and
  deleting repos is on the fence API, port 8080, like everything else
  that changes infrastructure.
- **On the host,** the front door is a Unix socket and git can't speak
  HTTP over one. `silo` ships **`git-remote-silo`**, a git remote helper.
  `git clone silo::/team-a/x/foo` (or `silo::-k7f3qa/foo`) runs it. It
  connects to the front-door socket, where `SO_PEERCRED` maps the user
  to a silo as for every front-door call. It then speaks git's smart
  protocol over that connection through the helper's `connect`
  capability. `silo repo clone` is a shortcut for it.
- **No SSH, no `git://`.** Smart HTTP is the only transport. It carries
  protocol v2 (the `Git-Protocol` header is passed through) and partial
  clone (`--filter`).

### Who is calling

- **Origin identity, no tokens.** As on every gateway path, the caller
  is the silo that owns the source address
  ([`gateway.md`, Origin identity](gateway.md#origin-identity)). A
  request carries no credential, and git's `Authorization` header is
  ignored if a client sends one. That is principle 5: nothing is handed
  into a workload for identity, so an agent holds no credential to leak.
- **Agents are their silo.** An agent's git commands run in its tool
  container, so they arrive from the tool container's address and act
  as the agent's silo, like any other tool call. On this backend each
  tool container has its own address, so the audit also names the agent
  (`agent=<id>`). That is attribution, not authority: inside a silo
  everyone is the silo ([README §3](README.md#identity)).
- **Harnesses don't use git.** Their built-in tools are off
  ([`local-backend.md` §10](local-backend.md#10-agents-on-the-local-backend)).
  The session socket doesn't serve `/git/`.
- **Front-door members** act as the silo they're mapped to, through
  `git-remote-silo`.

### How a request is served

1. The gateway resolves the caller's silo, then the path:
   `/git/<silo path>/<repo>.git/<git endpoint>` or
   `/git/-<silo id>/<repo>.git/<git endpoint>`. The repo is the
   path segment that ends in `.git`. Silo names are DNS labels and
   can't contain a dot, so the split is unambiguous.
2. silod checks access ([§4](#4-the-fence-on-repos)). `info/refs` and
   `git-upload-pack` need **read**. `git-receive-pack` needs **write**.
   A repo the caller may not read is 404 for every endpoint, whether it
   exists or not.
3. silod runs **the git binary itself**: `git upload-pack
   --stateless-rpc` or `git receive-pack --stateless-rpc` (with
   `--advertise-refs` for `info/refs`), in the bare repo. silod does what
   `git http-backend` does, but in its own code, so that it controls
   headers, limits and access. Pushes to one repo are serialized by a
   per-repo lock, and fetches never take it.
4. The environment is scrubbed. `GIT_CONFIG_NOSYSTEM=1` and
   `GIT_CONFIG_GLOBAL=/dev/null` keep the operator's own `~/.gitconfig`
   and credential helpers out, because rootless silod usually runs as
   the operator. `HOME` is an empty directory, and
   `GIT_TERMINAL_PROMPT=0` is set.
5. Each repo's config is silod's, written at create and checked at
   start:
   - `core.hooksPath` points at silod's own read-only hooks directory,
     never at the repo's;
   - `receive.fsckObjects=true`;
   - `uploadpack.allowFilter=true`;
   - `uploadpack.hideRefs` and `receive.hideRefs` hide
     `refs/silo-archive/` ([§8](#local-archive-refs-call-l48));
   - `receive.maxInputSize` is set per push from the silo's free disk
     ([§5](#5-storage-and-quota)).
6. silod's **`pre-receive` hook** (a small helper in silod's binary)
   refuses a push that:
   - updates a ref outside `refs/heads/` and `refs/tags/`. Only those
     two namespaces map to the uber repo ([§9](#9-mapping-back-the-ref-layout-and-silo-repo-external));
   - adds a blob over the mirror's file-size limit (100 MB for GitHub,
     [§8](#size-and-limits-call-l50)), so that a push that the uber repo would
     refuse fails now, with a message, rather than stalling the mirror;
   - would take the silo over its `disk_gb` grant ([§5](#5-storage-and-quota)).

   It also writes the **local archive refs** for tips the push would
   orphan ([§8](#local-archive-refs-call-l48)).
7. After a successful push, silod records the repo as dirty, with the
   store's next sequence number, and audits `repo.push` with each ref's
   old and new SHA. **The client's push succeeds at this point**, and
   the mirror runs later ([§8](#8-the-outbound-mirror)).

`git` ≥ 2.39 on the host is a prerequisite that install checks. That is
the version this page was checked against. It is not pinned, because
the protocol is stable and the server side is what every distribution
ships.

## 4. The fence on repos

### Who may do what (call L43)

| Caller, relative to the repo's silo | Read (clone, fetch) | Write (push) | Create, delete, `PATCH` |
| --- | --- | --- | --- |
| The silo itself | Yes | Yes | Yes |
| A proper ancestor | **Yes** | No ([open question 21](#open-questions)) | Yes, as for any descendant object |
| The holder of an active `manage` grant on it or an ancestor, with `repos` in the scope | With `read` or `write` access | With `write` access | With `write` access |
| Anyone else, siblings included | No: 404 | No: 404 | No: `NOT_FOUND` |

- **A silo sees and pushes to its own repos.** A parent reads its
  descendants' repos, the way it reads their audit. It can't push into
  them in v1. It can give a child code by seeding a repo it creates
  for the child ([Seeding](#seeding)).
- **No sideways access in v1.** Siblings and cousins can't read each
  other's repos. A common ancestor can read both, and can seed either
  from the other. A repo-read grant between silos, consented on both
  sides like an export, is [open question 22](#open-questions).
- **Every row is cut down by the caller's own effective control
  policy**, as in [`fence-api.md`](fence-api.md#who-may-do-what), for
  create, delete and `PATCH`. For git traffic, see
  [below](#sealed-and-read-silos-call-l44).

### Manage grants: the `repos` resource type

- A manage grant's `facade.resources` gains the type **`repos`**
  ([`fence-api.md`, Scope](fence-api.md#scope)). It sits beside the
  Docker facade's types: `["containers", "volumes", "repos"]`. `["*"]`
  includes it.
- `access: read` lets the holder clone and fetch the target's repos and
  its descendants' repos. `access: write` also lets it push, create and
  delete them.
- **Never looser than the grant:**
  - The holder gets no more than the scope. A deploy pipeline with
    `repos` at `read` can fetch the cell's code and can't push to it.
  - The holder gets no more than its own control policy allows on the
    fence API.
  - When the grant ends, silod tells the gateway, which **closes the
    holder's open git connections** within one second, as it does for
    a revoked export. A push that has already reached `receive-pack`
    either completes or is rolled back by git's own ref transaction.
    Nothing is half-applied.
  - Calls are audited under the target, `via=manage:<holder>`.
- **Seeding through a grant.** A holder with `write` may create a repo
  in the target seeded from any repo the holder itself can read, its
  own included. Code moves into the target that way. Nothing moves from
  the target to the holder except through `read`.

### Sealed and `read` silos (call L44)

- **Git traffic is a data path.** It is on port 80, which the fence
  keeps open for sealed silos, as it does for exports and tool channels
  ([`gateway.md`, What a sealed silo keeps](gateway.md#what-a-sealed-silo-keeps)).
  So a sealed or `read` silo **keeps fetching from and pushing to its
  existing repos**. A sealed cell that runs a build bot keeps working.
- **Creating, deleting and `PATCH` are control-plane calls.** A sealed
  silo is refused twice, as for every fence API call: by silod
  (`CONTROL_DENIED`), and by the fence dropping port 8080. A `read`
  silo may list and get, and nothing else.
- **A sealed cell is changed from outside.** An ancestor creates and
  deletes its repos. A manage-grant holder with `repos` at `write` can
  also push.
- An ancestor that wants a cell's code to be read-only can't say so
  yet. [Open question 23](#open-questions) proposes a `repos.write`
  policy.

### What egress allowlists mean here

- **In-silo git is not egress.** `silo.internal` is in every
  container's `NO_PROXY` ([`local-backend.md` §9](local-backend.md#the-gateway-egress-and-the-model-proxy)),
  so git goes straight to the gateway and never through port 3128. A
  silo with an empty egress policy can clone and push its repos.
- **Agents need no external egress for their code to be mirrored.** An
  ancestor can deny `github.com` and the agent still never touches it.
  Agents hold no external credential either way.
- **The mirror is silod's own connection** from the host's network
  namespace (call L28), like an image pull's dial. It isn't checked
  against any silo's egress policy, because it isn't a silo's traffic.
- **So code leaves the host regardless of a silo's egress policy.**
  That is the point of the uber repo, but it means egress policy no
  longer describes what data can leave a silo. An ancestor that wants a
  subtree's code to stay on the host sets **`repos.mirror: false`**
  (call L54). It is a new policy, handed down like egress: if any
  ancestor has it off, it is off, and a child can't turn it back on
  (`LOOSER_THAN_PARENT`). `describe` shows the effective value and the
  silo it comes from. Mirroring is on at the root by default once a
  mirror is configured.

### Repo names are scoped per silo

Two silos can each have `foo`. Everything that names a repo carries its
silo:

- the URL: `/git/team-a/x/foo.git`;
- the store's key: `(silo id, name)`;
- the disk path: by id, `silos/<silo id>/repos/<repo id>.git`;
- the external refs: `silo/team-a/x/foo.git/...`.

## 5. Storage and quota

### On disk (extends call L35)

```
<data-dir>/
  silos/<silo id>/
    repos/<repo id>.git/          a bare repo; files owned by silod's own uid
      .silo-seq                   creation sequence, for restore detection (call L40)
  keys/
    mirror/                       the uber repo's credential (§8); mode 0700
  mirror/
    index.git/                    the submodule index's own commits (§7); kept, not precious
```

- **Repos are in their silo's subtree** (call L36). Moving a silo moves
  no files, deleting a silo removes its repos with its subtree, and a
  later per-silo backup gets the silo's code with its volumes.
- **Files belong to silod's uid, not to the silo's subuids.** Only
  silod's git processes touch them, and they are never mounted into a
  container. They are created through the storage driver's `Create`,
  with a new `Ref` kind for repos, so that a later `zfs` driver puts
  them in the silo's dataset and its quota.
- **Maintenance:** silod runs `git maintenance run --auto` on a repo
  after pushes, under its lock. Nothing is pruned while any ref, local
  archive refs included, reaches it.
- **In [part 2's data table](local-rootless-storage.md#data-and-what-is-not-data):**
  `silos/<id>/repos/` is **data**. `keys/mirror/` is data, under the
  keys' own policy. `mirror/index.git` is kept but rebuildable: losing it
  loses only the index's own history, not the index ([§7](#7-the-submodule-index)).
- **Online copies:** a bare repo copied file by file while it is being
  pushed to or repacked can be inconsistent. A later backup copies each
  repo with `git bundle create --all` under the repo's lock, or through
  the storage driver's `Snapshot`. This is the same rule as volumes
  (call L39), with a better answer, because git can bundle.

### Quota

- **A new count class, `repos`** (adding to call L19). It is handed
  down like `volumes`, and its default at the root is unlimited. A
  parent can give a child `repos: 0`.
- **Disk is charged to `disk_gb` by measured size**, as volumes are,
  and measured after each push and every 5 minutes.
- **Unlike volumes, repos are enforced, at push.** silod is the only
  writer, so the hook can refuse:
  - `receive.maxInputSize` is the silo's free `disk_gb` headroom when
    the push starts;
  - the `pre-receive` hook refuses a push that left the silo over its
    grant.

  One push can overshoot by at most its own pack, and the next one is
  refused with `silo: /team-a/x is over its disk_gb grant (12.1 of 12
  GiB)`.
- **Restore** works as for volumes (call L40). A repo directory whose
  `.silo-seq` is newer than the store is an orphan: reported, never
  deleted, and not served until an operator decides. A repo row whose
  directory is missing is `repo_missing`, and the repo isn't re-created
  empty. What a restore means for the mirror is in
  [§8](#after-a-restore).

## 6. Submodules between silo repos

### The URL form (call L45)

A silo-hosted repo has two URLs, and both resolve the same way from
every silo, because `silo.internal` is the same address everywhere
(principle 5):

```
http://silo.internal/git/<silo path>/<repo>.git        by path:   http://silo.internal/git/team-a/x/foo.git
http://silo.internal/git/-<silo id>/<repo>.git         by id:     http://silo.internal/git/-k7f3qa/foo.git
```

- **Use one of these, absolute, in `.gitmodules`:**

  ```
  [submodule "libs/foo"]
  	path = libs/foo
  	url = http://silo.internal/git/team-a/x/foo.git
  ```

  The path form is for people. The id form survives a rename or move of
  the silo, so it is the right one for a reference that should last.
- **No relative submodule URLs between silo repos.** `../foo.git` works
  inside a silo, because it resolves against
  `http://silo.internal/git/team-a/x/`. But in the uber repo it resolves
  against the uber repo's own URL, and points at nothing. `silo repo
  get` warns when a repo's default branch has one.
- **There is no `self` shorthand in URLs.** A URL in a commit has to
  mean the same thing in every silo that clones it.

### The fence decides, at clone time

- Cloning a submodule is an ordinary fetch, by the cloning silo, from
  the submodule's silo. It is allowed exactly when
  [§4](#4-the-fence-on-repos) lets the **reader** read that repo, and is
  a 404 otherwise. Nothing new is checked.
- So a parent's repo can include its children's repos as submodules,
  and the parent can clone it recursively. A child that clones the same
  parent repo gets the parent's repo if it was given a seed of it, and
  404 on any submodule it may not read.
- **silod doesn't check what a gitlink points at when it is pushed.** A
  gitlink is a SHA and a URL in a file. Pushing a reference to a repo
  the pusher can't read reveals nothing and grants nothing.

### External view: not rewritten

The mirror never rewrites a commit, because SHAs must be identical in
both views. So a submodule URL in a mirrored commit still says
`http://silo.internal/git/...`, which means nothing outside. That is
**documented, and fixed up at clone time**, not rewritten:

- **Every silo-hosted commit is in the uber repo**, under some ref. A
  submodule whose URL points at a silo repo can therefore always be
  fetched **from the uber repo itself**, by SHA. This was checked with a
  single-branch clone and a SHA reachable only from another branch
  ([What was checked](#what-was-checked-and-against-what)).
- **`silo repo external-clone /team-a/y/agent`** does it for a person
  who has `silo`. It clones the repo's namespaced default branch from
  the uber repo, points every `http://silo.internal/git/` submodule at
  the uber repo's URL, and recurses.
- **Without `silo`,** the index's generated `README.md` carries the
  recipe. Run it in the clone, before `git submodule update`, and again
  in each nested submodule (`git submodule foreach --recursive`):

  ```sh
  UBER=$(git remote get-url origin)
  git config -f .gitmodules --get-regexp '\.url$' | while read -r key url; do
    case $url in http://silo.internal/git/*) git config "$key" "$UBER" ;; esac
  done
  git submodule update --init --single-branch
  ```

  A `url.<base>.insteadOf` rule can't do this. It rewrites a prefix and
  keeps the rest of the URL, but every silo repo maps to the **same**
  uber URL.

## 7. The submodule index

This was a later option in the task, and is in scope after bsalmon's
comment ([Status](#status)). It is the preferred folder view: a
browsable branch in the uber repo, with one folder per repo, built on
top of the namespaced branches without changing them.

### What it is (call L51)

- **The index branch** is `main` by default, configurable
  (`--index-branch`). It is a normal branch that only silod writes.
- **Its tree** holds:
  - one **gitlink** (mode `160000`) per mirrored repo, at
    `<silo path>/<repo>.git`, pointing at the external tip of the repo's
    default branch. A repo in the root silo is at `foo.git`. A repo
    whose default branch doesn't exist yet has no entry;
  - **`.gitmodules`**, one entry per gitlink, sorted:

    ```
    [submodule "team-a/x/foo.git"]
    	path = team-a/x/foo.git
    	url = ./
    	branch = silo/team-a/x/foo.git/main
    ```

  - **`README.md`**, generated: what this repo is, a table of repos
    (path, silo id, default branch, namespaced branch, last mirrored),
    the ref layout from [§9](#9-mapping-back-the-ref-layout-and-silo-repo-external),
    and the external-clone recipe from [§6](#external-view-not-rewritten).
- **The `.git` on folder names** is not decoration. Without it, repo
  `foo` of `/team-a/x` (a gitlink at `team-a/x/foo`) and any repo of
  child silo `/team-a/x/foo` (under the directory `team-a/x/foo/`) would
  need the same path to be both a file and a directory. Silo names
  can't contain a dot, so `foo.git` can never be a silo's folder. The
  refs use the same marker for the same reason (call L46).
- **`url = ./` points the submodule back at the uber repo itself.** git
  resolves a URL starting `./` or `../` against the superproject's
  remote, so `./` becomes the URL the person cloned from: `https` or
  `ssh`, whatever its name. This was checked with git 2.39 over smart
  HTTP ([What was checked](#what-was-checked-and-against-what)).
  `../<uber name>.git` also works, but breaks when the uber repo is
  renamed or cloned from a mirror. *Verify on GitHub* (plan item 22):
  that the web UI links a `./` submodule to the commit. If it doesn't,
  the configured fallback is `../<uber name>.git`, which silod knows
  from the mirror URL and regenerates on the next index commit if the
  URL changes.
- **`branch =`** names the namespaced branch, so that
  `git submodule update --remote` follows the repo's latest default
  branch in a full clone. It was checked in a full clone. With
  `--single-branch` the clone doesn't have the branch, and only the
  pinned SHA is fetched, which is the normal case.

### What it costs and gains

| Gains | Costs |
| --- | --- |
| Exact SHAs: the gitlink is the mirrored commit, and nothing is rewritten | GitHub shows a folder as `foo.git @ abc1234`, linking to the commit, and shows **no files inline** under it (*verify* the link) |
| One small commit per mirror batch, with no copying of content | GitHub code search covers the default branch, and doesn't descend into submodules, so it finds nothing in the repos (*verify*) |
| A folder layout people can browse | A full checkout needs `--recurse-submodules`. Each submodule is a clone of the **whole** uber repo unless `--single-branch` is passed, so the README says to pass it |
| Derived entirely from the namespaced refs, so it can be rebuilt or dropped at any time | GitHub truncates very large directory listings (*verify* the threshold), which matters for a silo with thousands of repos or children |

### How it is written

- **After each mirror batch** ([§8](#the-mirror-loop-call-l47)), the index pass
  builds the tree from the store's record of **what the uber repo
  already has**, never from local refs. So the index never points at a
  commit that hasn't been mirrored yet. Passes are debounced (10 s by
  default), so a burst of pushes makes one index commit.
- **The commit** is written in `mirror/index.git` with plumbing
  (`mktree`, `commit-tree`). That repo holds only the index's own trees
  and commits, because gitlinks don't pull the commits they point at
  into a pack. Its parent is the previous index commit. Its message
  lists the repos that changed, and ends in a `Silo-Index: 1`
  trailer. The author and committer are fixed (`silo <silo@localhost>`),
  so the commit carries no host details.
- **It is pushed with a lease** on the index branch: the expected old
  value is the last index commit silod pushed. If someone else has
  pushed to the index branch, the lease fails. silod then stops
  writing the index, audits `mirror_index_diverged`, and shows it in
  `silo mirror status`. It never force-pushes over a human's commit
  ([open question 26](#open-questions) covers what the operator does
  next).
- **On a repo that already has history,** the index branch must be
  absent, or its tip must carry `Silo-Index: 1`. Otherwise `silo
  mirror set` refuses and asks for a different `--index-branch`. Silo
  never takes over a `main` that people use.
- **A lost `mirror/index.git`** is rebuilt from the store. The next
  index commit is pushed after silod fetches the external index tip as
  its parent: that one ref of silod's own commits, never tenant data.
- **Turning the index off** (`--index-branch ""`) leaves the branch as
  it is and stops updating it.

### The other option, kept for later

The **rewritten tree** copies each repo's files into its folder. Files
are inline on GitHub and searchable, but SHAs change, merges flatten,
and only one branch per repo can be shown. It is [later](#10-later).
Either view can be generated later **from the same mirrored refs**,
with no change to the git service, the namespaces or the mirror, so
the choice made here closes nothing off.

## 8. The outbound mirror

### Configuration and the credential (call L52)

```
silo mirror set --url https://github.com/acme/agents.git \
                --credential-file ~/agents-deploy-key \
                [--prefix silo] [--index-branch main]
```

- **Set by the root only:** root members, or install's `--mirror-*`
  flags. The URL, the prefix and the index branch are in the store. The
  credential is copied into `keys/mirror/` and never written to the
  store (call L35).
- **The credential** is either an **SSH deploy key with write
  access**, which is recommended because GitHub scopes it to exactly
  this one repo, or an **HTTPS token** file (a fine-grained token with
  `contents: write` on this repo). For SSH, silod pins the host key in
  `keys/mirror/known_hosts` when the mirror is set, and refuses an
  unknown one afterwards.
- **Only silod ever holds it** ("secrets behind proxies",
  [README §9](README.md#9-later)): it is never in a repo's config, a
  URL, a command line, a container or an agent's environment. silod
  passes it to its own `git push` through `GIT_SSH_COMMAND` with
  `-o IdentitiesOnly=yes`, or through a credential helper that reads
  the file. The operator's own git credentials are kept out, as in
  [§3](#how-a-request-is-served).
- **silod dials out from the host's network namespace** as an ordinary
  user socket (call L28). An `HTTPS_PROXY` in silod's own environment
  is honoured, for hosts behind a corporate proxy.
- **Changing the prefix or the URL** makes a new target. silod pushes
  everything to it again, under the new layout. A push sends only
  objects the remote doesn't have, so a prefix change on the same repo
  costs ref updates and no objects. **The old refs are left alone**:
  silod never deletes refs under a layout it no longer writes.

### Ref layout (call L46)

With the default prefix `silo`, for repo `foo` of silo `/team-a/x`:

| What | Uber-repo ref |
| --- | --- |
| Branch `main` | `refs/heads/silo/team-a/x/foo.git/main` |
| Branch `feature/a` | `refs/heads/silo/team-a/x/foo.git/feature/a` |
| Tag `v1.0` | `refs/tags/silo/team-a/x/foo.git/v1.0` |
| A tip overwritten or deleted at 18:54:00 UTC | `refs/silo-archive/team-a/x/foo.git/20261012T185400Z-1/heads/main` |
| An archived tag | `refs/silo-archive/team-a/x/foo.git/20261012T185400Z-1/tags/v1.0` |
| The index | `refs/heads/main` |
| A repo of the root silo | `refs/heads/silo/foo.git/main` |

- **This differs from the task's example** (`silo/team-a/x/foo/main`) by
  the `.git` after the repo name. Without it, two different repos can
  need the same ref name. Repo `main` of silo `/team-a/x/foo` and
  branch `main` of repo `foo` in `/team-a/x` are both
  `silo/team-a/x/foo/main`. And git can't hold
  `silo/team-a/x/foo/main` and `silo/team-a/x/foo/main/x` at once (a
  ref can't be both a file and a directory). Silo names can't contain a
  dot, so `<repo>.git` marks exactly where the silo path ends and the
  branch begins ([open question 24](#open-questions) asks whether
  bsalmon prefers to refuse colliding names instead).
- **The prefix** (`silo`) is configurable, and is the first component
  of every live ref. Archives are under `refs/<prefix>-archive/`.
- **Archives aren't branches.** They live outside `refs/heads` and
  `refs/tags`, so they don't fill GitHub's branch list, and a plain
  clone doesn't fetch them. They are still ordinary refs: fetchable by
  name, and they keep their commits reachable and fetchable by SHA.
  *Verify* that GitHub accepts pushes to a custom `refs/` namespace
  (plan item 22). If it doesn't, archives become branches under
  `refs/heads/<prefix>-archive/` ([open question 25](#open-questions)).
- **Timestamps** are UTC, `YYYYMMDDTHHMMSSZ` (`:` isn't allowed in a
  ref), plus a sequence number that is unique within the second.

### The mirror loop (call L47)

The mirror is **state-based**. It makes the uber repo's namespace for
each repo equal to that repo's local refs, rather than replaying events.

- **Dirty repos.** A push, a create, a delete, a `PATCH`, or a rename
  or move of an ancestor silo marks a repo dirty in the store, with a
  sequence number. At start, every repo is dirty once. That is a cheap
  comparison, and it covers a crash between a push and its store write.
- **A pass over one repo,** under the repo's lock:
  1. Read the local refs: `refs/heads/*`, `refs/tags/*` and the local
     archive refs ([below](#local-archive-refs-call-l48)).
  2. Map them to uber-repo names ([Ref layout](#ref-layout-call-l46)).
  3. Compare them with `mirror_refs`, the store's record of what the uber
     repo has (external ref name → SHA).
  4. Push the difference in **one atomic push** (`git push --atomic
     --porcelain`). Each ref is pushed with an explicit lease
     (`--force-with-lease=<ref>:<recorded SHA>`, or "must not exist" for
     a new ref). The archive refs are part of the same push as the
     overwrite they protect, so the uber repo never shows a
     force-push without its archive.
  5. On success, record the new SHAs in the same transaction that
     clears the repo's dirty sequence (if no newer push arrived in the
     meantime).
- **In order.** Passes for one repo never overlap, so the uber repo
  sees each repo's states in the order they were pushed. A burst of
  pushes during one pass is coalesced into the next pass, and nothing in
  it is lost: fast-forwards keep their history, and overwrites leave
  archive refs ([below](#local-archive-refs-call-l48)). Different repos are
  independent, with up to 4 passes at once by default.
- **The index pass** runs after repo passes ([§7](#how-it-is-written)).
- **Retries.** A failed pass (network, a 5xx, a timeout) is retried with
  exponential backoff from 1 s, capped at 5 minutes, forever. The queue
  is the dirty set in the store, so it survives restarts.
- **The local push never waits.** Pushes succeed while the uber repo is
  unreachable, the credential is wrong, or GitHub is down. The repo's
  `mirror.state` becomes `PENDING` (with `lag_seconds`), then `STALLED`
  after an hour, audited as `mirror_stalled`. `silo mirror status`
  lists stalled repos with their last error, and `silo mirror retry`
  runs a pass now. A bad credential stalls every repo at once, and the
  root's `describe` says so.
- **Never reading back.** The mirror pushes. The only thing it reads
  from the uber repo is ref advertisements, for leases and for
  [adopting](#when-the-uber-repo-disagrees). It never fetches tenant
  history into a silo's repo.

### Local archive refs (call L48)

Force-pushes and deletions never destroy history, on either side:

- **Locally, before an update lands,** silod's `pre-receive` hook looks
  at each ref the push changes. It writes
  `refs/silo-archive/<UTC ts>-<seq>/heads/<branch>` (or `/tags/<tag>`)
  pointing at the **old** tip when:
  - the update isn't a fast-forward, or deletes the ref, **and**
  - the old tip isn't reachable from any other ref of the repo.

  A branch deleted after it was merged needs no archive, because its
  commits are still on `main`. Hooks can't see whether a push will
  succeed, so an atomic push that is then rejected leaves an archive ref
  of a tip that is still live. That is harmless.
- **These refs are hidden** from fetches and refused on push
  (`uploadpack.hideRefs`, `receive.hideRefs`). A rebasing agent's
  clones don't fill up with them. `silo repo archives foo` lists them,
  and `silo repo restore-ref foo <archive> <branch>` makes a branch
  from one, for the repo's own silo and its ancestors.
- **Externally,** the mirror pushes local archive refs like any other
  ref, mapped to `refs/silo-archive/<path>/<repo>.git/...`. A
  force-push in a silo becomes, in one atomic push to the uber repo, the
  archive ref plus the forced update. Because every tip the uber repo
  ever had is first a local ref, **the uber repo loses nothing even
  when a tip was overwritten locally before the mirror ever pushed it.**
- **Archives are kept forever** in v1. Nothing prunes them, locally or
  externally. Their count is in `describe` and in [Size and
  limits](#size-and-limits-call-l50).

### When the uber repo disagrees

The lease is what keeps silod from destroying something it didn't
write: a human's push to a `silo/...` branch, or the uber repo's state
after a restore of silod.

- **A lease failure on any ref fails the atomic push.** silod then
  retries the pass without the refs whose lease failed. Those refs
  become `DIVERGED`, are audited (`mirror_diverged`, with the expected
  and actual SHAs), and appear in `silo mirror status`. The rest of the
  repo keeps mirroring.
- **silod never overwrites a diverged ref by itself.** The operator
  runs either:
  - `silo mirror adopt foo [--ref ...]`, which archives the uber
    repo's current tip under `refs/<prefix>-archive/.../<ts>-<seq>/external/<branch>`
    and then pushes the local tip with a lease against it. To create
    that archive ref, silod fetches the one external commit into a
    quarantine repo, `mirror/quarantine.git`, never into the silo's
    repo. This is the one place the mirror reads objects, and the
    objects only go back out; or
  - `silo mirror adopt foo --keep-external`, which records the external
    SHA as mirrored and leaves it. The next local push to that branch
    then needs a lease against the external tip, and fails again unless
    it is a fast-forward of it.
- **First contact.** When a mirror is first set, or a store is lost,
  `mirror_refs` is empty, so every push expects the refs to be absent.
  Refs that already exist under the prefix show up as diverged, and
  nothing is overwritten until the operator adopts them.

### After a restore

A restored silod starts in report-only mode (call L40), and **the
mirror is paused with it**. Its local refs are older than the uber
repo's. After `silo reconcile --apply`, the mirror resumes, and every
ref that moved after the restore point fails its lease and becomes
diverged. The operator adopts with archives, or keeps the external
tips. So the uber repo is never rolled back by a restore, and it is
also an off-host copy of every repo's branches and tags. It is not a
backup system: it has no repo config, no hidden refs other than
archives, and no store.

### Deleting a repo or a silo (call L49)

- **Deleting a repo:**
  1. silod marks the repo `DELETING`, which refuses new fetches and
     pushes, and removes it from listings.
  2. The mirror makes one atomic push that:
     - creates `refs/<prefix>-archive/<path>/<repo>.git/<ts>-<seq>/heads/*`
       and `.../tags/*` for **every** live branch and tag;
     - deletes the live `refs/heads/<prefix>/<path>/<repo>.git/*` and
       tags, each with a lease.

     Its existing archive refs stay where they are.
  3. The next index commit drops the gitlink. Older index commits still
     point at commits that the archive keeps reachable, so the index's
     own history still checks out.
  4. Only then does silod delete the local bare repo. With mirroring
     off or no mirror configured, step 2 is skipped. While the uber
     repo is unreachable, the operation stays running and the local
     repo stays on disk, charged to nobody and listed in the root's
     `describe` as `repos_pending_delete`.
- **A name freed this way can be reused at once.** A new `foo` starts
  with no live refs. The old one's archives are under timestamps that
  can't collide.
- **Deleting a silo** with `force` deletes its repos as above, and does
  the same for every descendant's repos first, deepest first. The
  silo's subtree is removed last, as in
  [Teardown](local-backend.md#teardown). Without `force`, a silo with
  repos is `FAILED_PRECONDITION`, as for any non-empty silo.
- **Nothing silod does ever deletes an archive ref,** or a live ref whose
  tip isn't archived or reachable from another ref in the same push.

### Renames and moves (call L53)

When a silo is renamed or moved, its path changes, and so does every
external ref name of its repos and its descendants' repos. silod
marks them dirty. Each pass pushes, atomically, **new names for the
same SHAs plus the deletion of the old names** (with leases). No
objects are sent, and nothing needs archiving, because every commit
stays reachable under the new name. The archive refs move the same way.
The index commit moves the gitlinks. In-silo URLs by path change too.
URLs by id don't, which is why [§6](#6-submodules-between-silo-repos)
recommends them for long-lived references.

### Size and limits (call L50)

GitHub's limits, as the author knows them (*verify* all of them in
plan item 22):

| Limit | GitHub | What silod does |
| --- | --- | --- |
| A file over 100 MB | Push rejected (warning from 50 MB) | Refused **at the local push** by the `pre-receive` hook, with the limit in the message. The limit is configurable, with 100 MB as the default for a `github.com` URL |
| A push over 2 GB | Rejected | A first push of a big repo is split. silod pushes ancestors of the tips in steps of at most 1 GB of new objects (estimated from `rev-list --objects --disk-usage`), each under a temporary ref, `refs/<prefix>-staging/<repo id>`, which the last step deletes |
| Repository size | Recommended under 1 GB, strongly under 5 GB; GitHub may get in touch beyond | Reported: `silo mirror status` shows the uber repo's estimated size, the sum of mirrored repos' packs. One uber repo concentrates every silo's code, so this is the limit most likely to bite. Per-silo external repos are [later](#10-later) |
| Webhook and Actions push events | Not created for a push of more than 3 tags, or of very many branches (5,000) at once | Pushes are per repo and usually carry few refs. A bulk re-push (new prefix, a large rename) is batched at 1,000 refs. The README warns that the uber repo should have **no workflows triggered on `silo/**`**, because every mirror push would run them |
| Branch rules and rulesets | Can forbid force-pushes or deletions on matching branches | `silo mirror set` checks with one test push to `refs/heads/<prefix>/.silo-probe` (create, force, delete). It refuses a target where any step fails, and names the rule |
| Number of refs | No hard limit known to the author | Every push receives the whole ref advertisement. Push still uses protocol v0, which can't filter by prefix. At about 100 bytes a ref, 10,000 refs (a thousand repos with a few branches each, plus archives) is about 1 MB per push, which is acceptable. Pushes are coalesced, and archives are out of `refs/heads`, so clones don't pay it. `silo mirror status` reports the count |

A default `git clone` of the uber repo fetches every live branch and tag
of every repo. The README says so, and shows the single-branch and
partial-clone forms (`--single-branch -b silo/...`,
`--filter=blob:none`).

## 9. Mapping back: the ref layout and `silo repo external`

A person looking at the uber repo finds one silo's repo by its path:

```
refs/heads/<prefix>/<silo path>/<repo>.git/<branch>
refs/tags/<prefix>/<silo path>/<repo>.git/<tag>
refs/<prefix>-archive/<silo path>/<repo>.git/<ts>-<seq>/heads/<branch>
refs/<prefix>-archive/<silo path>/<repo>.git/<ts>-<seq>/tags/<tag>
refs/<prefix>-archive/<silo path>/<repo>.git/<ts>-<seq>/external/<branch>     (adopt, §8)
refs/heads/<index branch>:<silo path>/<repo>.git                                 (the gitlink)
```

- `<silo path>` is the path without its leading `/` (empty for the
  root). Its components are DNS labels, so the first component that
  ends in `.git` is the repo.
- On GitHub, a branch is
  `https://github.com/acme/agents/tree/silo/team-a/x/foo.git/main`,
  and the index folder is
  `https://github.com/acme/agents/tree/main/team-a/x`.
- The index README's table maps paths to silo ids, because a path can
  be reused after a silo is deleted.

`silo repo external` prints the mapping for a repo the caller may read
(`--json` for scripts):

```
$ silo repo external /team-a/x/foo
repository  https://github.com/acme/agents.git
silo        /team-a/x (k7f3qa)
branches    refs/heads/silo/team-a/x/foo.git/*
default     silo/team-a/x/foo.git/main
tags        refs/tags/silo/team-a/x/foo.git/*
archives    refs/silo-archive/team-a/x/foo.git/*   (3)
index       main:team-a/x/foo.git
mirror      IN_SYNC at 2026-10-12T18:54:03Z
clone       git clone --single-branch -b silo/team-a/x/foo.git/main https://github.com/acme/agents.git foo
```

- `--branch feature/a` or `--tag v1.0` prints that one ref.
- `silo repo which refs/heads/silo/team-a/x/foo.git/feature/a` goes the
  other way, from an uber-repo ref to the silo, the repo and the branch.
  It reports a path that no longer exists as such, and names the silo
  id from the index if it can.
- For a repo whose mirroring is off, it says `mirror  off (by /team-a)`
  and prints no external refs.

## 10. Later

- **Two-way sync.** Reading changes back from the uber repo into silo
  repos. That needs:
  - a rule for who may push to `silo/...` branches externally, and as
    which silo those pushes count;
  - what happens when both sides move: refuse, or archive and take
    one;
  - a fence check, because an external push into a sealed cell's repo
    is a change from outside the tree.

  The leases and archives here are already the conflict detection it
  would need.
- **The rewritten-tree folder view** ([§7](#the-other-option-kept-for-later)),
  generated from the same refs, on a branch of its own, so it can sit
  beside the submodule index.
- **The GCP backend.** The design carries over with these changes:
  - the git service is a path on the gateway's port 80, behind GCP's
    fence as it is here;
  - repos live on the control VM's data disk (or a regional disk for
    failover), under the same per-silo layout;
  - the credential is in Secret Manager, read only by silod's service
    account;
  - the mirror dials out through the control VM's own egress, not a
    tenant path;
  - with several silod replicas, only the leader runs the mirror
    ([`state.md`](state.md#more-than-one-silod-and-the-leader-call-34)),
    and pushes reach the replica that holds the disk, as tool channels
    do.

  Nothing in the fence API, the URLs or the ref layout changes.
- **Per-silo external repos.** An ancestor sets a different mirror
  target for a subtree. The root holds every credential, and a subtree
  can't name a target the root hasn't registered. Within that target,
  the layout is the same, relative to the subtree's root. This also
  answers the uber repo's size limit.
- **Git LFS.** v1 doesn't serve the LFS API. A repo that uses LFS
  pushes pointer files, and its LFS uploads fail. Serving LFS means a
  second store per silo, and mirroring it to GitHub's LFS quota.
- **Push attribution in the uber repo.** Which silo and agent pushed
  each commit is in silod's audit, not in the commit, because adding it
  would change SHAs. `refs/notes/<prefix>` could carry it externally
  ([open question 27](#open-questions)).
- **Sandboxing the server-side git processes**
  ([open question 28](#open-questions)).

## 11. Staged implementation plan

These are items **19 to 24** of
[`local-backend.md` §15](local-backend.md#15-staged-implementation-plan),
numbered after part 2's so that existing references stay valid. Each
can be filed on its own, in this order.

19. **Repos in the fence API, the store and `silo repo`.**
    - The repo object, its operations and errors, and `silo repo
      create|list|get|delete|url`;
    - the `repos` quota class;
    - the `repos.mirror` policy, never-looser;
    - the `repos` type in manage-grant scopes;
    - the `silos/<id>/repos/` layout through the storage driver, with
      `.silo-seq`;
    - seeding.

    Bare repos are made with `git init --bare` and silod's config. Uses
    the fake backend. Depends on 4 and 14.
20. **The git service.**
    - `/git/` on the gateway's port 80, with path and id resolution;
    - the access table in [§4](#4-the-fence-on-repos), and 404 for the
      invisible;
    - `upload-pack` and `receive-pack` run stateless, with the scrubbed
      environment;
    - the `pre-receive` hook: ref namespaces, the blob size limit, disk
      enforcement and local archive refs, with hidden refs;
    - the per-repo lock and dirty marking;
    - `repo.push` audit with agent attribution;
    - closing connections when a manage grant ends;
    - `git-remote-silo` and `silo repo clone` on the front door;
    - `silo repo archives` and `restore-ref`.

    Tested with real git clients against silod in-process, with the
    holder's socket replaced by a plain listener. Depends on 19, and on
    15 for the listener in `silo-net`.
21. **The mirror.**
    - `silo mirror set|status|retry|adopt`;
    - the credential in `keys/mirror/`, SSH host-key pinning, and the
      branch-rule probe;
    - `mirror_refs` and the dirty set;
    - per-repo atomic passes with leases, backoff and the stall audit;
    - archive refs on overwrite, deletion and adopt, and the quarantine
      repo;
    - deletion of repos and silos through archives;
    - renames and moves;
    - chunked first pushes and ref batching;
    - pausing in report-only mode.

    Tested against a local uber repo served by `git http-backend`, with
    a fault-injecting proxy in front (refusals, timeouts, a rejected
    lease, a 2 GB-style size refusal). Depends on 20.

    **Built (task 3424),** in `pkg/mirror`, with migration 0026
    (`mirror_refs`, `mirror_repos`, `mirror_diverged`) and the
    configuration in `settings` as `mirror.*`. Where it differs from
    §8:
    - The probe branch is `refs/heads/<prefix>/_silo-probe`. `.silo-probe`
      isn't a valid ref name (a component can't start with a dot), and
      no silo or repo can be named `_silo-probe`.
    - `quarantine.git` is in `cache/mirror/`, not `mirror/`: it keeps
      nothing that can't be fetched again, and the data directory's
      layout is unchanged.
    - The dirty set is the change log: `repo` and `repo.push` changes
      make a repo dirty, `silo` and `policy` changes make every repo
      dirty, and `mirror_repos.mirrored_seq` is what a pass covered.
    - An adopted external tip is archived under
      `.../<ts>-<seq>/external/<branch>` (`external/tags/<tag>` for a
      tag). Archives the mirror makes itself (of a deleted repo's refs)
      have a sequence of `m<n>`.
    - No live ref is updated or deleted unless the push keeps its old
      tip. So after `adopt --keep-external`, a local tip that doesn't
      fast-forward the external one is held back as diverged again.
    - Deleting a silo with `force` hands each repo to the mirror but
      doesn't wait for it: the silo goes, and its repos are in
      `pending_deletes` until archived.
    - Not done yet: install's `--mirror-*` flags; the repo object's
      `mirror.state`, which still says only `OFF` or `UNCONFIGURED`;
      and `repos_pending_delete` in the root's `describe` (it is in
      `silo mirror status`).
22. **The submodule index, and checking GitHub.**
    - `mirror/index.git`, the generated tree, `.gitmodules` and
      README;
    - the lease on the index branch and the `Silo-Index` check;
    - rebuild after loss.

    Then one run against a real scratch GitHub repo, with a human
    reading the result. It checks the *verify* items on this page:
    - how `url = ./` is shown and linked;
    - fetching by SHA from `refs/silo-archive/` and from a
      non-default branch;
    - whether custom ref namespaces are accepted;
    - the size, file and event limits;
    - rulesets.

    The run's findings are written back into this page. Depends on 21.
23. **Mapping back.** `silo repo external`, `silo repo which`, and
    `silo repo external-clone` with the submodule-URL fix-up. Depends on
    21, and on 22 for the index line.
24. **Conformance.** Local-structural cases in `local-conformance`
    (item 12):
    - a sibling gets 404 on clone and push;
    - an ancestor can clone and can't push;
    - a manage grant with `repos: read` fetches and can't push, and
      loses its connection on revoke;
    - a sealed silo pushes and can't create;
    - a silo with no egress clones and pushes;
    - `repos.mirror: false` keeps a subtree out of the uber repo;
    - a force-push leaves an archive in the uber repo with the same SHA;
    - the uber repo going down doesn't fail a push, and the mirror
      catches up;
    - a restore doesn't roll the uber repo back.

    Depends on 12, 21 and 22.

## Calls made on this page

L41. **Repos are silo objects in the fence API,** named per silo,
     stored by id under the silo's subtree, and created explicitly.
     There is no push-to-create. A child gets a parent's code by
     seeding, a server-side copy that the caller must be able to read.

L42. **Git is smart HTTP on the gateway's port 80 under `/git/`,**
     served by the git binary that silod runs stateless with a scrubbed
     environment and its own hooks. On the host, it is a remote helper
     over the front-door socket. There is no SSH, and no tokens:
     identity is by origin.

L43. **Access:** a silo reads and writes its own repos. Ancestors read,
     and create and delete. Manage-grant holders get what the `repos`
     resource type and its access allow. There is nothing sideways in
     v1, and a repo the caller can't read is a 404.

L44. **Fetching and pushing are a data path.** Sealed and `read` silos
     keep them on their existing repos. Creating, deleting and changing
     repos are control-plane calls, refused to them.

L45. **Submodule URLs are absolute in-silo URLs,** by path or by silo
     id, the same from every silo. They aren't rewritten for the
     mirror. They are fixed up at external clone time to point at the
     uber repo, which holds every commit.

L46. **External refs carry the repo as `<repo>.git`:**
     `refs/heads/<prefix>/<silo path>/<repo>.git/<branch>`, tags
     likewise, and archives under `refs/<prefix>-archive/`, outside
     branches. This differs from the task's example to make names
     unambiguous and free of file/directory conflicts.

L47. **The mirror is state-based:** per-repo atomic pushes with
     leases, which converge the namespace to the local refs. It is
     ordered per repo, retried forever, and the local push never waits
     for it.

L48. **Nothing pushed is ever lost.** Overwrites and deletions that
     would orphan a tip leave a hidden local archive ref, which is
     mirrored in the same atomic push as the overwrite. A ref that
     disagrees with the lease is never overwritten without an operator,
     and is archived first.

L49. **Deleting a repo or a silo archives every live ref externally,**
     drops the index entry, and only then deletes the local repo.
     Archives are never deleted.

L50. **Uber-repo limits are met locally:** oversized blobs are refused
     at the local push, big first pushes are chunked, ref updates are
     batched, and size and ref counts are reported.

L51. **The submodule index is built now,** on `main` by default:
     gitlinks at `<silo path>/<repo>.git`, `url = ./`, `branch =` the
     namespaced branch, one commit per mirror batch, pushed with a
     lease, and never on top of a branch that silod didn't write.

L52. **One mirror target, set by the root.** Its credential is a file
     in `keys/mirror/` (preferably a write deploy key), used only by
     silod, dialled from the host namespace, and not subject to any
     silo's egress policy.

L53. **Renames and moves re-push the same SHAs under new names** and
     delete the old names in the same atomic push. Ids in URLs survive
     them, and paths don't.

L54. **`repos.mirror` is a never-looser policy.** An ancestor can keep
     a subtree's code off the uber repo, and a child can't turn
     mirroring back on.

L55. **Repo disk is enforced at push,** charged to `disk_gb`, and
     counted in a new `repos` quota class.

## Open questions

21. **Ancestors pushing into descendants' repos.** The task says a
    parent reads. [Call 55](README.md#calls-made-in-this-draft) lets
    ancestors write descendants' facades. Should git follow call 55, so
    that a parent can push fixes to a child's repo, or stay read-only,
    with seeding as the way down?
22. **Repo grants between silos.** Should a silo be able to offer a
    repo for reading (or writing) to another silo, consented on both
    sides like an export? Without that, sibling agents share code only
    through a common ancestor.
23. **Read-only code for a subtree.** Should an ancestor be able to set
    `repos.write: false`, so that a sealed production cell can't change
    its own code either, as it can't change its infrastructure?
24. **The `.git` marker.** Is `silo/team-a/x/foo.git/main` acceptable?
    The other way to make names unambiguous is to keep
    `silo/team-a/x/foo/main` and refuse a repo name that equals a child
    silo's name, and the other way round. That couples silo naming to
    repos in the fence API, and still leaves branch names that look like
    paths.
25. **Archives as branches.** Custom refs keep the branch list clean,
    but GitHub's web UI doesn't show them. Should archives be branches
    under `<prefix>-archive/`, so that someone can find them on GitHub?
    And should anything ever prune them?
26. **A diverged index branch.** When someone pushes to the index
    branch, should silod stop (as here), move its index to a new branch,
    or merge its commit on top of the human's?
27. **Attribution in the uber repo.** Should silod also mirror a
    `refs/notes/<prefix>` that records, per commit, which silo (and
    agent) pushed it, and when?
28. **Running `upload-pack` and `receive-pack` in a sandbox.** They
    parse untrusted packs as silod's user, on the host, as `git
    http-backend` does on every git server. Should they run in a
    `sandbox`-class container per push, at a cost in latency, as
    open question 6 asks for `docker load`?
29. **Mirror lag as a signal to agents.** Should an agent be able to
    wait until its push has reached the uber repo (`silo repo wait-
    mirrored`), for workflows that hand a ref to a human?
30. **Which repo hosts the uber repo.** This page assumes GitHub for
    its limits. Is any other host in scope for v1? The design is plain
    git, except for the limits table and the branch-rule probe.
