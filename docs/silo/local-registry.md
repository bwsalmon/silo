# Silo local backend, part 4: a host-local OCI registry with a pull-through cache

## Status

Design draft, written in task 3449 (filed from task 3024). Like the
rest of the local backend, the steps in the
[staged plan](#13-staged-implementation-plan) are filed one by one.
Task 3450 landed the first part of step 25: image repos in the store
and the fence API, the `image_repos` class, the `registry` scope type
and `silo registry` ([§13](#13-staged-implementation-plan) says what of
step 25 is left). Task 3480 landed step 27's registry handler over a
fake content store; containerd's half is step 27b. Task 3483 landed
step 30's facade endpoints over the same fake. Task 3484 landed step
31's pull-through cache for public content, over the same fake and a
test upstream ([§13](#13-staged-implementation-plan) says what is left).

This page is the fourth part of the local backend, after
[`local-backend.md`](local-backend.md) (task 3350),
[`local-rootless-storage.md`](local-rootless-storage.md) (task 3356) and
[`local-git.md`](local-git.md) (task 3360). Today the local design can
only **pull** images. `push`, `commit`, `load`, `/images/{name}/push`
and build all return 501
([`local-backend.md`, Refused](local-backend.md#refused)), so an agent
can't make an image that another container or another silo can run.
bsalmon set the direction when the task was filed:

- **silod hosts an OCI registry, local only.** It speaks the OCI
  Distribution API through the gateway at `registry.silo.internal`,
  inside `silo-net`, and is reachable from the host's front door. Its
  blobs are in the same `silo-containerd` content store, so each is
  stored once by digest. A push charges the pushing silo's `disk_gb`,
  and sharing stays silod's saving, not a silo's discount (Images §4).
  There is **no outbound copy** to an external registry for now.
- **The fence is the model of `local-git.md`.** Names are scoped per
  silo. A silo pushes and pulls its own repos, ancestors read their
  descendants', manage grants are never looser, sealed silos are
  handled, and anything sideways is an explicit export. Every push,
  pull and delete is audited. No registry credential is ever needed
  for a local repo.
- **The Docker facade** serves `docker tag` and `docker push` to
  `registry.silo.internal/...`. A pull from the local registry skips the
  egress check, because it never leaves the host, but still needs read
  permission. `docker build` stays refused. The supported way to build
  is rootless BuildKit or kaniko in a container, then a push.
- **A pull-through cache** for Docker Hub, gcr.io, ghcr.io and other
  configured upstreams, once for the whole host. Each silo's own egress
  policy still decides what it may get from the cache, every request is
  audited as that silo's pull, and a cached private image is never
  served to a silo that didn't present valid credentials for it.
- **Garbage collection and deletion** across the registry, image
  records and the cache.

Where this page and the earlier parts disagree, **this page wins**. It
changes these things in them, and each is marked where it is
introduced:

- the facade's [Refused](local-backend.md#refused) table: push, `save`,
  `load`, `commit` (for `sandbox`) and `/auth` move out of it
  ([§7](#7-the-docker-facade));
- [Images](local-backend.md#images) and call L6: every facade pull goes
  through the cache's fetcher, with the same egress rule
  ([§8](#8-the-pull-through-cache));
- the data directory (call L35): `containerd/` moves out of `cache/`,
  because pushed images are data ([§4](#4-storage-charging-and-what-is-data));
- open questions 6 (`docker load`) and 7 (turning sharing off for a
  subtree) of `local-backend.md` are answered here
  ([call L68](#calls-made-on-this-page), [call L73](#the-timing-channel-and-imagessharing-call-l73)).

Calls continue the numbering of the earlier parts, from **L56**
([Calls made on this page](#calls-made-on-this-page)). Open questions
continue from **31** ([Open questions](#open-questions)). The new
staged-plan steps are items **25 to 36** of
[`local-backend.md` §15](local-backend.md#15-staged-implementation-plan).

### What was checked, and against what

github.com didn't answer from the sandbox, so upstream projects'
documentation was read from their Go module archives on
`proxy.golang.org`, which carry the repositories' `docs/` and READMEs:

- **BuildKit v0.34.0** (2026-10-07): `docs/rootless.md`,
  `docs/buildkitd.toml.md` and `examples/kubernetes/README.md`.
- **kaniko**: `GoogleContainerTools/kaniko` v1.24.0 (its last release,
  May 2025) and its maintained fork `chainguard-dev/kaniko` v1.25.19
  (2026-08-26): the READMEs and `run_in_docker.sh`. The fork's README
  says the original was **archived in June 2025**, that no major
  feature work is planned, and that the old
  `gcr.io/kaniko-project/executor` images are unmaintained.
- **gVisor**, at gvisor.dev on 2026-10-13: the Compatibility guide and
  the "Docker in gVisor" tutorial. Also gVisor's source at
  `a676ebdd060a` (2026-10-12): `runsc/cmd/tar.go` (the
  `runsc tar rootfs-upper` command) and `runsc/config/flags.go` (the
  `--oci-seccomp` flag and its default).
- **containerd v2.4.1**: `docs/hosts.md` (mirrors and the `ns` query
  parameter), `docs/ops.md` (the content sharing policy),
  `docs/garbage-collection.md` (leases and `gc.*` labels), and the
  overlayfs snapshotter's `root_path` option
  (`plugins/snapshots/overlay/plugin/plugin.go`).
- **distribution v3.1.2**: `docs/content/recipes/mirror.md`, the
  pull-through cache recipe.
- **OCI distribution-spec v1.1.1**: `spec.md`, for the name grammar,
  cross-repository mounts and the referrers API.
- **Silo**: this repository at `c82a1a6`, in particular the three
  earlier parts, [`gateway.md`](gateway.md) and
  [`fence-api.md`](fence-api.md), and `pkg/backend/local/datadir`.

Nothing on this page was run. Claims about how a builder behaves under
gVisor or in a granule VM that the documentation doesn't settle are
marked *unverified*. Plan item 35 runs them.

## Contents

1. [The shape](#1-the-shape)
2. [Names and addresses](#2-names-and-addresses)
3. [Repos as silo objects](#3-repos-as-silo-objects)
4. [Storage, charging, and what is data](#4-storage-charging-and-what-is-data)
5. [The fence on the registry](#5-the-fence-on-the-registry)
6. [The registry service](#6-the-registry-service)
7. [The Docker facade](#7-the-docker-facade)
8. [The pull-through cache](#8-the-pull-through-cache)
9. [Building images](#9-building-images)
10. [Garbage collection and deletion](#10-garbage-collection-and-deletion)
11. [What a bug would expose](#11-what-a-bug-would-expose)
12. [Conformance cases](#12-conformance-cases)
13. [Staged implementation plan](#13-staged-implementation-plan)
14. [Later](#14-later)
15. [Calls made on this page](#calls-made-on-this-page)
16. [Open questions](#open-questions)

## 1. The shape

```
 inside silo-net                       host netns (silod, no capabilities)                      outside
┌───────────────────────────┐         ┌──────────────────────────────────────────────┐        ┌───────────────┐
│ /team-a/x: kaniko,        │ HTTP:80 │ gateway listener (passed by silo-nsd, L28)   │        │ docker.io     │
│ crane, buildctl ...       │────────▶│   Host: registry.silo.internal ─▶ registry   │        │ gcr.io        │
│ registry.silo.internal/   │  /v2/   │   │                                          │        │ ghcr.io ...   │
│   team-a/x/app:1          │         │   ├─ local repos ─ access table (§5)         │        └───────▲───────┘
│   docker.io/library/...   │         │   │                                          │                │
└───────────────────────────┘         │   └─ upstream paths ─ cache (§8)             │  miss: through │
┌───────────────────────────┐ 2375    │        │ egress check as the caller          │  the egress    │
│ docker CLI in a silo      │────────▶│        │ private: revalidate with the        │  proxy, as the │
│ docker push / pull        │ facade  │        │ caller's credentials                │  caller ───────┘
└───────────────────────────┘         │        ▼                                     │
 host                                 │ silo-containerd content store (one per host) │
┌───────────────────────────┐ facade  │   namespaces: silo-<id>   (image records)    │
│ operator: docker push     │ socket  │               silo-registry (repo manifests) │
│ crane / skopeo            │────────▶│               silo-cache  (cache entries)    │
└───────────────────────────┘ or      │   <data-dir>/containerd/  (data, §4)         │
                    127.0.0.1:5115    └──────────────────────────────────────────────┘
                    (uid from inet_diag, §5)
```

- **One registry per host**, served by silod in its own process, behind
  the gateway's port 80, the port that already carries exports, tool
  channels and git ([§6](#6-the-registry-service)).
- **Local repos** belong to silos, like git repos. Their names carry
  the silo's path ([§2](#2-names-and-addresses)), and the access table
  is git's ([§5](#5-the-fence-on-the-registry)).
- **Upstream paths** (`registry.silo.internal/docker.io/...`) are the
  pull-through cache. Each request is checked against the caller's own
  egress policy, as if it had pulled from the upstream itself
  ([§8](#8-the-pull-through-cache)).
- **One content store.** Pushed blobs, cached blobs and the blobs behind
  every silo's image records are the same files in
  `silo-containerd`'s content store, stored once by digest. Who may read
  them, and who pays for them, is decided per repo and per silo by
  silod, never by whether a blob happens to be on disk
  ([call L62](#the-registry-never-answers-from-global-content-call-l62)).
- **Nothing leaves the host** except cache misses, which are each the
  calling silo's own egress.

## 2. Names and addresses

### The address (call L56)

- **`registry.silo.internal`** is a new name in the gateway's DNS
  ([`local-backend.md`, DNS](local-backend.md#dns)). It answers with
  `silo.internal`'s address (10.255.255.1 by default) for every silo.
- **Port 80, plain HTTP, selected by `Host`.** The port 80 listener
  sends a request whose `Host` is `registry.silo.internal` to the
  registry handler, and nothing else. It sends every other `Host` to the
  existing handlers (`/x/`, `/git/`, `/r/`, the tool endpoints), where
  `/v2/` is a 404. So no routing mistake on one side reaches the other.
- **No TLS.** Traffic in `silo-net` never leaves the host, which is why
  the facade's port 2376 isn't served either
  ([`local-backend.md`, Address](local-backend.md#address-and-how-a-request-names-its-silo)).
  The cost is that clients have to be told the registry is plain HTTP.
  The Docker facade needs nothing, because silod is the client there
  ([§7](#7-the-docker-facade)). Tools in containers take a flag or a
  config line, listed in [§9](#9-building-images). A TLS port with a
  host-local CA is [open question 32](#open-questions).
- **`NO_PROXY`.** silod already puts `NO_PROXY` in every container's
  environment ([`local-backend.md`, The gateway, egress and the model
  proxy](local-backend.md#the-gateway-egress-and-the-model-proxy)). It
  now names `registry.silo.internal` explicitly, beside
  `silo.internal`, so that no client sends registry traffic to the
  egress proxy whatever its suffix-matching rules are. The egress proxy
  refuses `*.silo.internal` anyway, because it resolves to an internal
  address.
- **`Internal: true` networks don't reach it**, as they don't reach
  the gateway at all.
- **What is served** is the OCI Distribution API, v1.1: the `/v2/`
  base, manifests and blobs (`GET`, `HEAD`, `PUT`, `DELETE`), chunked
  and monolithic uploads, cross-repository mounts, tag lists, the
  referrers API, and `_catalog`, filtered to what the caller may read
  ([§6](#6-the-registry-service)).

### Repository names (call L57)

The names read like [`local-git.md`'s](local-git.md#repo-names-are-scoped-per-silo):
the silo's path, then the repo.

```
registry.silo.internal/<silo path>/<repo>[:tag|@digest]     by path:  registry.silo.internal/team-a/x/app:1
registry.silo.internal/id_<silo id>/<repo>[:tag|@digest]   by id:    registry.silo.internal/id_k7f3qa/app:1
registry.silo.internal/<upstream>/<upstream repo>          cache:    registry.silo.internal/docker.io/library/alpine:3.20
```

The first component decides which form a name is:

| First component | Form | Why it can't be confused |
| --- | --- | --- |
| Contains a `.` (`docker.io`, `gcr.io`) | **Upstream cache** ([§8](#8-the-pull-through-cache)) | Silo names are DNS labels, which can't contain a dot, and repo names can't either (below) |
| `id_` followed by a silo id | **A silo by id**. The id form survives a rename or move, as git's `-<id>` form does | A silo name can't contain `_`. git's leading `-` isn't valid in an OCI name, so the marker differs |
| Anything else | **A silo by path**. The **last** component is the repo, and everything before it is the silo's path from the root | Repo names are a single component, so the split is unambiguous |

- **A repo name is one component**: 1 to 63 characters matching
  `[a-z0-9]+((_|__|-+)[a-z0-9]+)*`. That is the OCI name grammar
  (distribution-spec, "Pulling manifests") without the `.`, and it
  doesn't start with `id_`. A dot would make a root repo's name look
  like an upstream. Multi-component names
  (`team-a/x/tools/builder`) are [open question 31](#open-questions).
- **Names are absolute.** A Dockerfile, compose file or manifest that
  names an image has to mean the same thing in every silo that reads
  it, so there is no `self` shorthand, as in
  [`local-git.md` §6](local-git.md#the-url-form-call-l45).
  `silo registry url app` prints the name for the caller.
- **The root silo's repos** are `registry.silo.internal/<repo>`.
- **Mirror requests** (`?ns=`): containerd, and everything built on its
  resolver (BuildKit, nerdctl), adds a query parameter `ns=<registry
  host>` when it pulls through a mirror (containerd `docs/hosts.md`). A
  request for `/v2/library/alpine/manifests/3.20?ns=docker.io` is the
  same as one for `/v2/docker.io/library/alpine/manifests/3.20`. An
  `ns` that names a configured upstream selects the cache. An `ns` of
  `registry.silo.internal`, or none, selects the path rules above. Any
  other `ns` is `NAME_UNKNOWN`.
- **Docker Hub's short names** are normalised as Docker does them:
  `docker.io/alpine` is `docker.io/library/alpine`, and
  `index.docker.io` and `registry-1.docker.io` are aliases of
  `docker.io` ([Configuration](#configuration)).

## 3. Repos as silo objects

### Created by the first push (call L58)

A registry repo belongs to exactly one silo, as a git repo does.
**Unlike a git repo (call L41), it is created by its first push.** Every
registry client (`docker push`, kaniko, BuildKit, crane, skopeo, oras)
expects push-to-create, and none of them can call an API first. A git
repo is made explicitly so that it can be seeded. An image repo has
nothing to seed: it is empty until something is pushed.

```json
{
  "name": "app",
  "id": "ir-3fq8zk",
  "silo": "/team-a/x",
  "ref": "registry.silo.internal/team-a/x/app",
  "ref_by_id": "registry.silo.internal/id_k7f3qa/app",
  "tags": 4,
  "manifests": 6,
  "charged_bytes": 412090368,
  "create_time": "2026-10-13T09:12:00Z",
  "last_push_time": "2026-10-13T10:40:51Z"
}
```

- **Names are per silo.** `/team-a/x` and `/team-b/y` can each have an
  `app`. A repo can't be renamed. A silo rename or move changes its
  repos' path names and not their id names, as for git
  ([`local-git.md`, Renames and moves](local-git.md#renames-and-moves-call-l53)).
  Image references by path inside existing images and compose files
  stop resolving, which is why the id form exists.
- **The id** (`ir-` and six base-32 characters) is what the store uses.
- **A new quota class, `image_repos`** (adding to call L19), handed down
  like `repos`. Its default at the root is unlimited. A push that would
  create a repo over the count is `DENIED`, with the class named.

### Operations

| Call | Does |
| --- | --- |
| `GET /silo/v1/silos/{silo}/imageRepos` | List. `?recursive=true` also lists descendants' repos, each with its silo path |
| `GET .../imageRepos/{repo}` | Get one |
| `GET .../imageRepos/{repo}/tags` | Tags and the digests they point at, and every manifest the repo holds, untagged ones included, with its charged size, its tags, and whether another manifest of the repo (an index) references it, which `prune` needs |
| `DELETE .../imageRepos/{repo}/tags/{tag}` | Untag ([§10](#10-garbage-collection-and-deletion)) |
| `DELETE .../imageRepos/{repo}/manifests/{digest}` | Delete a manifest and every tag on it |
| `DELETE .../imageRepos/{repo}` | Delete the repo and everything in it |

There is no create call. These follow
[`fence-api.md`](fence-api.md)'s conventions: errors, `request_id`,
`validate_only`, pagination, and `NOT_FOUND` for anything the caller
may not see. Who may call which is in [§5](#5-the-fence-on-the-registry).

`silo` gets a `registry` group. Paths are relative to the caller's own
silo unless they start with `/`:

```
silo registry list [--recursive] [/team-a]
silo registry get app
silo registry tags app
silo registry url app              # the name to push to and pull from, for this caller
silo registry delete app[:tag|@digest]
silo registry prune app --untagged # delete manifests no tag points at
silo registry cache status | set | evict
```

## 4. Storage, charging, and what is data

### One content store, records per repo (call L59)

- **Bytes:** every blob and manifest is in `silo-containerd`'s content
  store, by digest, once. The registry writes through containerd's
  content API with the expected digest, so containerd verifies it.
- **Records:** the **store** is the truth for repos, tags and which
  manifests a repo holds (`image_repos`, `image_tags`,
  `image_repo_manifests`). containerd holds what keeps the bytes alive:
  one image record per (repo, manifest) in a namespace of its own,
  **`silo-registry`**, named `<repo id>@<digest>`. Its target is the
  manifest, and containerd's `containerd.io/gc.ref.content` labels on
  the manifest reach its config, layers and child manifests, so
  containerd's GC keeps exactly what some repo references
  (containerd `docs/garbage-collection.md`).
- **Uploads in progress** are held by a containerd lease per upload
  session, with `containerd.io/gc.expire` one hour out
  ([§6](#uploads)).
- **The cache** keeps its entries the same way, in namespace
  **`silo-cache`** ([§8](#eviction-and-the-cache-budget-call-l74)).
- **Silos' image records** stay where they are, in `silo-<id>` (call L7).

### Pushed images are data (call L60)

[Part 2](local-rootless-storage.md#data-and-what-is-not-data) says
`cache/containerd/` content is never data, because it can always be
pulled again. A pushed image can't: the registry holds its only copy. So
**containerd's root moves out of `cache/`**:

```
<data-dir>/
  containerd/                     silo-containerd's root: the content store and its metadata (bolt)
  cache/
    snapshots/                    the overlayfs snapshotter's root_path: containers' writable layers
    buildkit/  images-wrapped/  runtimes/     unchanged
```

- containerd's overlayfs snapshotter takes a `root_path` of its own
  (`plugins/snapshots/overlay/plugin/plugin.go`), so writable layers
  stay under `cache/`, where part 2 puts them.
- **The data table gains a row:**

  | What | Data? | Why |
  | --- | --- | --- |
  | `containerd/` content referenced by a registry repo, and the store's repo and tag rows | **Yes** | The only copy of a pushed image. A later backup exports it per repo as an OCI image layout through the registry, never by copying the directory |
  | `containerd/` content referenced only by image records or the cache | No | Re-pulled on demand, as before (call L6) |

- **`cache/` stays safe to wipe.** That is the point of the move: an
  operator who empties `cache/` to get disk back loses writable layers
  and wraps, as part 2 says, and never a pushed image.
- **Layout version 2.** `pkg/backend/local/datadir` writes layout 2,
  with `containerd/` and `cache/snapshots/` (`Dir.Containerd` and
  `Dir.Snapshots`, the root and `root_path` the gVisor driver, plan
  item 7, gives silo-containerd). It landed before that driver, so no
  data directory had a containerd root yet, but the forward migration
  handles one anyway: it moves `cache/containerd/` to `containerd/`
  (one rename on one filesystem) and the overlayfs snapshotter's state
  in it, `io.containerd.snapshotter.v1.overlayfs/`, to
  `cache/snapshots/` (another).
- **Restore** (call L40): each `silo-registry` record carries the
  object sequence label (`dev.silo.seq`). A record newer than the
  restored store is an orphan: reported, never deleted, and not served
  until an operator decides. A repo row whose manifest content is
  missing is reported as `image_manifest_missing` and answered with 404
  `MANIFEST_UNKNOWN`. It isn't re-created.

### Charging (call L61)

**Sharing is silod's saving, not a silo's discount** (Images §4). So:

- **A repo charges its silo's `disk_gb` with the stored size of every
  distinct blob its manifests reference**, manifests included. A layer
  used by two tags of one repo is charged once. A layer used by two
  repos, of one silo or of two, is charged to each repo. That keeps
  each repo's charge independent of every other repo, so a delete
  releases exactly what it charged, and the hard partition stays exact.
- **The size charged is the stored (compressed) size**, because that is
  what a pushed blob takes on disk. A facade pull still charges the
  unpacked size of the image record it makes
  ([§7](#pulling-from-the-local-registry)).
- **Uploads are charged as they arrive**, to the silo whose repo the
  upload names, so a client can't fill the disk with blobs it never
  references. A blob that no manifest of the repo references an hour
  after its upload finished is dropped and refunded ([§6](#uploads)).
- **Enforced at push.** silod is the only writer, as for git
  ([`local-git.md` §5](local-git.md#quota)). An upload chunk that would
  take the silo over its `disk_gb` grant is refused with 413 and the
  OCI error `DENIED`: `silo: /team-a/x is over its disk_gb grant (12.1
  of 12 GiB)`. One upload overshoots by at most one chunk (the registry
  caps chunks at 64 MiB).
- **Whose grant pays.** The repo's silo, always. A manage-grant holder
  pushing into its target charges the target, as every object a holder
  makes is charged ([`fence-api.md`, What a holder may do](fence-api.md#what-a-holder-may-do)).
- **Pulling charges the puller**, unchanged: a facade pull of a local
  image charges the image record's unpacked size to the pulling silo.
- **The cache charges nobody** ([§8](#eviction-and-the-cache-budget-call-l74)).

## 5. The fence on the registry

### Who may do what (call L63)

| Caller, relative to the repo's silo | Pull (manifests, blobs, tag list) | Push (uploads, manifests, tags), delete tags and manifests | Delete the repo |
| --- | --- | --- | --- |
| The silo itself | Yes | Yes | Yes |
| A proper ancestor | **Yes** | No (as git, [open question 21](local-git.md#open-questions)) | Yes, as for any descendant object |
| The holder of an active `manage` grant on it or an ancestor, with `registry` in the scope | With `read` or `write` access | With `write` access | With `write` access |
| A silo holding an accepted import of a registry export that covers the repo | **Yes** | No | No |
| Anyone else, siblings included | No: 404 `NAME_UNKNOWN` | No: 404 | No: `NOT_FOUND` |

- **The same 404 for everything the caller can't read**, whether the
  repo exists or not, and whether the reference is a tag, a digest or a
  blob. An upload to a repo the caller can't write is also 404, unless
  the repo is in the caller's own silo and doesn't exist yet, in which
  case it is the first push ([§3](#created-by-the-first-push-call-l58)).
- **Every row is cut down by the caller's own effective control
  policy** ([below](#sealed-and-read-silos-call-l65)).
- **Manage grants: the `registry` resource type.** A manage grant's
  `facade.resources` gains `registry`, beside `containers`, `volumes`
  and `repos`. `["*"]` includes it. `read` lets the holder pull the
  target's repos and its descendants'. `write` also lets it push,
  untag, delete and create by pushing. **Never looser:** the holder gets
  no more than the scope, and no more than its own control policy
  allows. When the grant ends, the gateway closes the holder's open
  registry connections within one second, as it does for a revoked
  export, and an upload in progress is abandoned (its lease expires).
  Calls are audited under the target, `via=manage:<holder>`.
- **Ancestors delete through the fence API.** An ancestor's delete of
  a descendant's tag, manifest or repo is a fence API call
  ([§3](#operations)), as for any descendant object, and not a registry
  API request, which it can't make on a repo it can't write.
- **Ancestors don't push** in v1, for the same reason as git: a parent
  hands a child an image by pushing it to its own repo, which the child
  can't read, so it **exports** it to the child
  ([below](#registry-exports-call-l64)). Whether ancestors should push
  into descendants' repos is the same question as git's open question
  21, and is answered for both at once.

### Registry exports (call L64)

Anything sideways is an explicit grant. A registry export is a
[proxied export](fence-api.md#exports-proxied) with `protocol:
"registry"`, offered and accepted the same way:

```json
POST /silo/v1/self/exports
{"name": "base-images", "protocol": "registry",
 "repos": ["builder", "base-*"], "to": ["/team-b", "/team-c/ci"]}
```

- **`repos`** names repos of the exporter's own silo, by name or by a
  name prefix ending in `*`. A prefix covers repos created later, which
  is what makes an export of "my base images" useful. It never covers a
  descendant's repos: a descendant exports its own.
- **`to`** works as for every export. A path needn't be visible to the
  exporter, and nothing is revealed about whether it exists.
- **Accepting**, by the importer: `POST /silo/v1/self/imports`
  `{"export": "/team-a/base-images"}`. It returns the names the importer
  pulls by: `registry.silo.internal/team-a/<repo>`, the exporter's own
  names. There is no import-side alias, so an image name means the
  same thing in every silo.
- **Read only.** An import lets the importer pull, never push or
  delete. Write grants between silos are
  [open question 39](#open-questions).
- **Grant policy:** each side's effective grant policy must allow
  exports, as for every export (call 14). A policy change that stops
  allowing it suspends the grant.
- **Withdrawing**, by either side, stops further pulls at once, and the
  gateway closes the importer's open registry connections within one
  second. It can't take back what was pulled: an image the importer
  already pulled into its own records, or copied into its own repo, is
  its own. That is the same as a clone of a git repo.
- **Grant calls on a descendant's behalf** work as for other exports
  ([`fence-api.md`](fence-api.md#grant-calls-on-a-descendants-behalf)),
  so a sealed silo's images can be exported by an ancestor.

### Sealed and `read` silos (call L65)

The split is git's ([call L44](local-git.md#sealed-and-read-silos-call-l44)):

- **Registry traffic is a data path.** It is on port 80, which the
  fence keeps open for sealed silos. So a sealed or `read` silo keeps
  pulling from, pushing to, and untagging in its **existing** repos. A
  sealed cell's build bot keeps working.
- **Creating and deleting repos are control-plane calls.** A first push
  that would create a repo is refused to a sealed or `read` silo, with
  403 `DENIED` and `silo: creating a repo is a control-plane call; this
  silo's control policy is none`. Deleting a repo is on the fence API
  (port 8080), and refused twice to a sealed silo, as every fence API
  call is.
- **A sealed cell is changed from outside:** an ancestor deletes its
  repos, and a manage-grant holder with `registry` at `write` creates
  them by pushing.

### Who is calling (call L66)

- **Inside silo-net: origin identity, no tokens.** The caller is the
  silo that owns the connection's source address
  ([`gateway.md`, Origin identity](gateway.md#origin-identity)),
  trustworthy because of the bridge's anti-spoofing and `prerouting`'s
  interface check ([`local-backend.md` §9](local-backend.md#the-gateway-egress-and-the-model-proxy)).
  The registry answers `GET /v2/` with 200 and no challenge, so clients
  never try to log in to a local repo.
- **`Authorization` never affects identity.** On a local repo it is
  ignored. On an upstream path it is the caller's credential **for the
  upstream**, used for that request and nothing else
  ([§8](#private-upstream-images-call-l72)).
- **Agents are their silo.** An agent's registry calls come from its
  tool container's address, so they act as the agent's silo, and the
  audit also names the agent (`agent=<id>`), as for git.
- **The Docker facade** pushes and pulls as the facade's caller: by
  origin on `docker.silo.internal:2375`, by `SO_PEERCRED` on the
  per-silo host socket, and as the agent's silo on a session socket
  ([`local-backend.md`, Address](local-backend.md#address-and-how-a-request-names-its-silo)).
  `docker push registry.silo.internal/...` therefore works from the host
  with no listener at all, and is the recommended way.
- **The host front door for registry clients** (crane, skopeo, oras,
  a host BuildKit) is a TCP listener on **`127.0.0.1:5115`**, because
  those tools can't speak the API over a Unix socket. A TCP peer has no
  `SO_PEERCRED`. silod looks the peer's socket up instead with
  `NETLINK_SOCK_DIAG` (`inet_diag`, the interface `ss -e` uses), which
  reports the uid that owns a socket. On loopback, a process can only
  own sockets as its own uid, so the uid is the kernel's word, as
  `SO_PEERCRED`'s is. The uid is then mapped to a silo exactly as a
  front-door member is. A connection whose peer socket can't be found
  (it closed during the lookup) is refused.
  - On the host the name is `localhost:5115/team-a/x/app`. Most clients
    treat `localhost` as plain HTTP without configuration. The digests
    are the same as inside.
  - *Verify* (plan item 29): that `inet_diag` reports the owning uid of
    a loopback peer to an unprivileged silod, and in which kernel
    versions.
  - The port is set at install (`--registry-host-port`, `0` turns it
    off). Whether it should be on by default is
    [open question 37](#open-questions).
- **No registry credential is ever needed for a local repo**, inside or
  outside. `docker login registry.silo.internal` succeeds and stores
  nothing silod uses ([§7](#7-the-docker-facade)).

### What egress allowlists mean here

- **Local repos are not egress.** A silo with an empty egress policy
  can push to and pull from its repos, and pull what it may read of
  others'. Nothing leaves the host.
- **The cache is egress**, judged per request against the caller's
  own policy ([§8](#egress-on-every-request-call-l71)).
- **A container can still push to an external registry with its own
  tools**, through the egress proxy, when its egress allows that
  registry. That is ordinary tenant traffic, already governed by
  egress, and silod's registry has no part in it.

### Audit (call L67)

Every request is audited, as the calling silo, under the silo whose
repo it touches (an ancestor's or an importer's pull shows in both):

| Record | When | Fields beyond silo, caller, `via`, agent and time |
| --- | --- | --- |
| `registry.pull` | Each manifest or blob request on a local repo | repo, reference, digest, bytes, status |
| `registry.push` | Each manifest `PUT`, and each completed upload | repo, tag, digest, bytes, charged delta |
| `registry.delete` | Untag, manifest delete, repo delete | repo, reference, refunded bytes |
| `registry.cache` | Each request on an upstream path, and each upstream fetch behind a facade pull | upstream, repo, reference, digest, `hit`/`miss`/`stale`, the egress decision and the hosts it was taken on, `auth=none`/`anonymous`/`credentials` |
| `registry.denied` | Each refusal: access, quota, control policy, egress | the reason |

- A facade pull or push writes one `image.pull`/`image.push` record,
  as the facade does today, and the registry and cache records it
  causes carry its `pull_id`.
- **No credential is ever in a record**, nor any value derived from
  one, such as a token or a hash of it. `auth=credentials` says only
  that some were presented.

## 6. The registry service

### Requests

1. The gateway resolves the caller's silo by origin (or the front door
   by uid) and passes `Silo-Origin` as it does for every handler.
2. silod parses the name ([call L57](#repository-names-call-l57)) and
   sends upstream paths to the cache ([§8](#8-the-pull-through-cache)).
3. For a local repo, it checks the access table
   ([§5](#who-may-do-what-call-l63)). Reads need read, and writes need
   write. Everything the caller may not read is 404.
4. It serves the request from the store and the content store, holding
   a short containerd lease for the request so that GC can't remove a
   blob mid-read.

The handler is silod's own Go code over containerd's content API. The
distribution project's registry isn't used: its storage drivers keep
their own blob tree rather than containerd's content store, and its
proxy mode holds one upstream and one stored credential for it
(distribution, "Registry as a pull through cache"), which is the
opposite of [§8](#8-the-pull-through-cache).

### The registry never answers from global content (call L62)

A blob on disk is not a blob the caller may have. Two registry
features would leak that if they were answered from the content store:

- **Blob existence.** `HEAD /v2/<name>/blobs/<digest>` is what clients
  ask before uploading, to skip blobs the registry has. It answers 200
  only if the digest is referenced by a manifest in **that repo** (or is
  an upload of that repo still within its grace hour). Otherwise it is
  404, even when the blob is on disk for someone else.
- **Cross-repository mounts.** `POST
  /v2/<name>/blobs/uploads/?mount=<digest>&from=<other>` mounts the
  blob only if the caller may **read** `<other>`, and `<other>`
  references the digest. Otherwise it answers 202 with an upload
  session, which the spec allows ("if a registry ... is unable to mount
  the requested blob, it SHOULD return a 202"). A mount without
  `from` is never answered from global content.

So a client proves it has a blob by uploading it, **proof of
possession**, which is containerd's own `isolated` content sharing
policy ("clients prove they have access to the content by providing
all of the content to the ingest", containerd `docs/ops.md`). silod
enforces it in front of containerd, rather than setting
`content_sharing_policy = "isolated"`, because the facade's own pulls
still need dedup across namespaces (Images §4). An upload of a blob that
is already on disk is hashed in full, written to the ingest as usual,
and dropped at commit in favour of the existing copy. The client sees
the same latency either way.

### Uploads

- **Sessions** are per repo and per caller silo, with a containerd lease
  each. A session idle for 15 minutes is cancelled and its bytes
  refunded.
- **Limits:** 64 MiB a chunk; 16 open sessions per silo; a manifest of
  at most 4 MiB, parsed with a strict JSON decoder and validated against
  its media type before it is stored. A manifest must reference only
  blobs and manifests already in the repo, or it is
  `MANIFEST_BLOB_UNKNOWN`, as the spec requires. The one exception is
  `subject`, which may name a manifest not yet present (distribution-
  spec, "Pushing Manifests with Subject").
- **The grace hour:** a completed blob that no manifest references is
  kept, charged, for an hour after its upload, so that a client can push
  blobs and then the manifest. Then its lease expires.
- **Tags** are moved by a manifest `PUT` to a tag, and silod keeps the
  old target as an untagged manifest of the repo, as registries do.
  Untagged manifests stay, and stay charged, until deleted or pruned
  ([§10](#10-garbage-collection-and-deletion)).

### Listing

- `GET /v2/<name>/tags/list` follows the access table.
- `GET /v2/_catalog` lists what the caller may read: its own repos, its
  descendants', and those of its accepted imports, by path name. The
  cache is never listed.
- The referrers API (`GET /v2/<name>/referrers/<digest>`) is served
  over the repo's own manifests, so signatures and SBOMs pushed with
  `subject` work.
- A push to an upstream path is 405 `UNSUPPORTED` with
  `silo: the cache is read-only; push to registry.silo.internal/<silo
  path>/<repo>`.

## 7. The Docker facade

### What changes (call L68)

| Endpoint | Before | Now |
| --- | --- | --- |
| `POST /images/{name}/tag` | Served | Unchanged. Tagging an image `registry.silo.internal/...` is only a record in the caller's namespace, as in Docker |
| `POST /images/{name}/push` | 501 | **Served for `registry.silo.internal/...` names.** Any other registry is 501 `silo: pushes go to registry.silo.internal only; this host copies nothing to external registries` |
| `POST /images/create` (pull) | Through the egress proxy | A local repo: **no egress check**, read access required. An upstream: through the cache ([§8](#8-the-pull-through-cache)) |
| `GET /images/{name}/get`, `GET /images/get` (`docker save`) | 501 | **Served**, for images the caller has records for |
| `POST /images/load` (`docker load`) | 501, open question 6 | **Served** ([below](#load-and-save)). This answers open question 6 |
| `POST /commit` | 501 | **Served for `sandbox` containers.** 501 for `vm` ([below](#commit)) |
| `POST /auth` (`docker login`) | 501 | **Served, validate-only** ([below](#docker-login)) |
| `POST /build`, `/build/prune`, `/session` | 501 | Still 501. The message now names the way that works: `silo: image builds are not served by the facade; build in a container with kaniko or BuildKit and push to registry.silo.internal` ([§9](#9-building-images)) |
| `POST /images/create?fromSrc=…` (`docker import`), `GET /containers/{id}/export`, `GET /containers/{id}/changes`, `GET /images/search`, `GET /distribution/{name}/json` | 501 | Still 501. `fromSrc` may be a URL, which would make silod fetch from the network on a tenant's behalf. Export and changes need the same rootfs access as commit, and nothing asked for them. Search is Docker Hub's catalogue |

The allowlists for the new endpoints live in
`pkg/facade/local/docker/tables.go` with the rest. `push`, `load` and
`commit` mutate, and `save` is a read, under `pkg/control`.

### Pushing

- `docker push registry.silo.internal/team-a/x/app:1` sends `POST
  /images/registry.silo.internal/team-a/x/app/push?tag=1`. silod finds
  the image record in the caller's namespace, checks write access to
  the repo as the caller ([§5](#who-may-do-what-call-l63)), and writes
  the repo's records over the same content. No bytes are copied,
  because the record already proves the caller has them.
- It charges the repo ([call L61](#charging-call-l61)) and audits
  `image.push` and `registry.push`.
- **For `vm`**, the tenant's image is pushed, never its wrap. The wrap is
  made again from the same layers wherever the image runs.
- The progress stream is Docker's (`{"status": "Pushed"}` per layer, and
  `{"aux": {"Tag": …, "Digest": …, "Size": …}}` at the end), so
  clients that read the digest from it work.
- `X-Registry-Auth` on a push is ignored.

### Pulling from the local registry

- `docker pull registry.silo.internal/team-a/x/app:1` (or `docker run`
  of it) needs read access as the caller. It never touches egress or
  the network: the content is already on the host. The record is made
  in the caller's namespace and charged the unpacked size, as every
  pull is (Images §4).
- A sibling that can't read the repo gets Docker's `manifest unknown`
  shape (404), the same as for an image that doesn't exist.
- **Revoking access later** (an export withdrawn, a grant ended) leaves
  the caller's record and its containers alone. The record is a pull
  that already happened.

### `load` and `save`

- **`docker save`** streams an OCI image layout tar of images the
  caller holds records for, read from the content store. It reads only
  what the caller already has.
- **`docker load`** was held back by open question 6 because silod
  would parse an untrusted archive on the host. It is served now
  because silod **never extracts it**:
  - The tar is read as a stream. Each entry under `blobs/sha256/` is
    written to a content ingest under the digest its path names, and
    containerd refuses it if the bytes don't match. No path from the
    archive is ever created on a filesystem.
  - Both forms are read: the OCI layout (`index.json`, `oci-layout`,
    `blobs/`), and the legacy `docker save` form (`manifest.json`, with
    `<id>/layer.tar` or `blobs/` entries), whose layers are hashed as
    they stream to get their digests.
  - The manifests and configs are validated as a pushed manifest is
    ([§6](#uploads)), and every blob they name must be in the archive.
  - The archive is capped at the caller's free `disk_gb`, and charged
    as a pull is.
  - The layer tars are unpacked later by containerd's snapshotter when a
    container is made, which is what happens to every pulled layer
    already. `load` adds no new parser.
- `load` makes records in the caller's namespace, not in the registry.
  `docker push` puts them there.

### `commit`

- **`sandbox`: served.** gVisor's root overlay is in the sandbox
  (`--overlay2=root:self`), so the writes aren't in the containerd
  snapshot on the host. `runsc tar rootfs-upper <container id>`
  "extracts the upper layer of a container's rootfs into a tar archive"
  (`runsc/cmd/tar.go`). silod runs it with the pinned `runsc`, pausing
  the container first unless `pause=false`, as Docker does. The tar is
  stored as a blob, hashed on the way in, and never unpacked on the
  host. The new image's config is the container's, with the request's
  `changes` (`CMD`, `ENTRYPOINT`, `ENV`, `EXPOSE`, `LABEL`, `USER`,
  `WORKDIR`, `VOLUME`) applied. Its layer is charged as a pull is.
  *Unverified:* how `rootfs-upper` represents deletions (whiteouts),
  which plan item 30 checks against an image that removes a file.
- **`vm`: still 501**, `silo: commit of a vm container is not served:
  its writable layer is a guest disk`. The writable layer is granule's
  overlay over a guest disk image, an ext4 filesystem that rootless
  silod can't mount and shouldn't parse. It needs granule to produce the
  diff of the wrapped container's `RootfsDir` inside the guest. That is
  added to plan item 9's granule list.

### `docker login`

The facade refused `/auth`, but the docker CLI's `docker login` calls
`/auth` and saves the credential in the client's `config.json` only if
it succeeds. With `/auth` refused, the CLI never saved a credential, so
`X-Registry-Auth` never carried one, and a private upstream image
couldn't be pulled through the facade at all. So `/auth` is served, and
**validates only**:

- For an upstream, silod tries the credential against the upstream's
  token endpoint, through the egress proxy **as the caller**, and
  answers `Login Succeeded` or 401. It stores nothing, and the
  credential stays in the client's own config, as with any daemon.
- For `registry.silo.internal`, it answers `Login Succeeded` without
  looking. The credential the CLI then saves is never used.

## 8. The pull-through cache

### What it is (call L70)

- **One cache for the whole host**, for the configured upstreams. It is
  reached two ways:
  - **by path**, `registry.silo.internal/<upstream>/<repo>`, or by a
    mirror request with `?ns=<upstream>`, from tools in containers
    ([§2](#repository-names-call-l57));
  - **behind every facade pull.** `docker pull alpine` from a silo goes
    through the same fetcher and the same rules. That replaces the
    direct pull of Images §4: L6's egress rule is now applied here.
    A facade pull from a registry that isn't a configured upstream also
    goes through the fetcher, with the same rules. It just isn't
    addressable through `registry.silo.internal`.
- **Tags are revalidated, digests aren't.** A pull by tag asks the
  upstream for the tag's current digest (a manifest `HEAD`) on every
  request, through the egress proxy as the caller, as distribution's
  proxy does ("When a pull is attempted with a tag, the Registry checks
  the remote"). A pull by digest is served from the cache without
  contacting the upstream, unless the content is private
  ([below](#private-upstream-images-call-l72)).
- **When the upstream is unreachable**, a tag is served from the cache
  if it was last revalidated less than `stale_tag_max` ago (24 hours by
  default), audited as `stale`. Otherwise it is 503.
- **One fetch per object** for public content: concurrent misses on the
  same manifest or blob, from any silos allowed to have it, wait on one
  upstream fetch. Private content is never shared in flight
  ([below](#private-upstream-images-call-l72)).

### Egress on every request (call L71)

**A silo gets a cached object only if its own egress would have let it
fetch that object from the upstream.** Images §4 phrases that as "the
registry host, and every host the registry redirects a blob to". So:

- **Each fetch records the hosts it used:** the upstream's API host,
  the token realm's host (Docker Hub's is `auth.docker.io`), and every
  host a blob `GET` was redirected through to reach the bytes. They are
  stored with the cache entry.
- **A hit is checked against the caller's effective egress policy**
  for every recorded host of that object: API and token hosts for a
  manifest; those plus the blob's redirect hosts for a blob. Any denied
  host makes the request a 403 `DENIED` (`silo: egress to
  production.cloudflare.docker.com is not allowed for /team-a/x`),
  audited with the host that failed.
- **Redirect hosts change** (CDNs move). A hit refused only because of
  a recorded redirect host is re-checked once: silod asks the upstream
  for the blob through the egress proxy as the caller, follows its
  redirects without reading the body, and serves the cached bytes if
  the caller's policy allows the hosts it uses now. The new hosts
  replace the recorded ones.
- **A miss is the caller's own fetch.** silod dials the upstream
  through the gateway's egress proxy **as the calling silo**, exactly
  as Images §4 pulls do, so the proxy applies the caller's policy to
  every hop, and its egress audit shows the connection.
- **The upstream list is not an allowlist.** Configuring `docker.io`
  as an upstream lets nobody reach it. Each silo's egress still
  decides, so a silo whose ancestors deny Docker Hub gets 403 from the
  cache as it would from the proxy.

### Private upstream images (call L72)

Credentials arrive per request: in `X-Registry-Auth` for a facade pull,
and in `Authorization` (Basic) on an upstream path from a tool in a
container. They are used for that request and dropped. **They are never
written to the store, the audit, a log, a file or a cache entry**, and
neither is any token silod gets for them.

- **Classifying content.** When silod fetches a manifest, it first asks
  the upstream for it **anonymously** (a `HEAD`, through the egress
  proxy as the caller). If that succeeds, the manifest and everything
  it references are `public`. If it needs credentials, they are
  `private`, and silod fetches with the caller's credentials. A `HEAD`
  is used because registries such as Docker Hub don't count it as a
  pull (*verify* per upstream, plan item 32). Classification is
  re-checked anonymously when it is older than `public_ttl` (24 hours),
  so an image made private upstream stops being public here.
- **A private manifest is served only after revalidation**, on every
  request: silod asks the upstream for that manifest **by digest**
  (`HEAD`) with the caller's credentials, through the egress proxy as
  the caller. 200 means the caller may read it, and silod serves the
  cached manifest. Anything else is the upstream's answer, passed back.
  A caller that presents no credentials gets 401 with `WWW-Authenticate:
  Basic realm="registry.silo.internal"` on an upstream path, so that
  registry clients send the ones they have, and Docker's
  `unauthorized: authentication required` through the facade.
- **Blobs follow the manifest.** A blob of a private repo is served
  only if the caller holds an **authorisation** for a manifest in the
  same upstream repo that references it. An authorisation is a
  revalidation that succeeded: (silo, upstream, repo, manifest digest),
  held **in silod's memory only**, for `auth_ttl` (5 minutes by
  default). It contains no credential. A restart forgets them all, and
  the next manifest request revalidates. An index's authorisation
  covers the manifests it lists.
- **A blob is served in the repo it was asked for, through a manifest of
  that repo** ([call L62](#the-registry-never-answers-from-global-content-call-l62)).
  Asking for a private layer's digest under a public repo's path is a
  404, because no manifest of the public repo references it. A blob in
  both a public and a private image is public.
- **Misses fetch with the caller's credentials**, and private fetches
  are not shared in flight: two silos missing on the same private blob
  each fetch it, each with their own credentials. The content store
  still keeps one copy.
- **The cost:** a pull of a private image costs one upstream round trip
  per manifest (two with the token exchange), even on a hit. Blobs,
  which are most of the bytes, come from the cache. Holding an
  authorisation longer, or not revalidating digests at all, is
  [open question 35](#open-questions).

### The timing channel, and `images.sharing` (call L73)

Images §4 already notes that a fast pull shows another silo pulled the
same image ([`local-backend.md` §14](local-backend.md#14-limitations)).
The cache widens it: it now covers images pulled by tools inside
containers too, and a configured upstream's whole catalogue, and its
size and revalidation behaviour can be measured from inside a silo. Two
things are done about it.

- **Uploads show nothing.** Proof of possession
  ([call L62](#the-registry-never-answers-from-global-content-call-l62))
  makes a push take the same time whether a blob is on disk or not, and
  local repos are read only through the access table. The channel is
  the cache's alone.
- **A policy turns sharing off for a subtree.** This answers
  `local-backend.md`'s [open question 7](local-backend.md#open-questions):

  ```json
  "images": { "sharing": "shared" }      // or "isolated"
  ```

  - It is handed down never-looser, like `egress`: `isolated` is
    stricter. If any ancestor has `isolated`, the silo is isolated, and
    a child can't set `shared` back (`LOOSER_THAN_PARENT`). The root's
    default is `shared`. `describe` shows the effective value and the
    silo it comes from.
  - **A sharing domain** is the nearest silo, the caller or an
    ancestor, that sets `isolated` itself, or `shared` (the whole host)
    when none does. Nested isolation works: a silo inside an isolated
    subtree can isolate itself further.
  - **A hit is decided by the cache index of the caller's domain, never
    by the content store.** Each cache entry records the domain that
    fetched it. A caller in domain D gets a hit only on an entry D
    fetched. Everything else is a miss: fetched in full from the
    upstream, through the egress proxy, as the caller, and verified,
    even if the blob is already on disk. At commit, the duplicate is
    dropped for the existing copy, so disk still holds one copy, but
    the timing is a real fetch's.
  - **Facade pulls follow the same rule**, so an isolated subtree's
    `docker pull` no longer skips blobs another domain fetched.
  - An isolated domain's fetches don't make hits for anyone outside it,
    in either direction.
  - **The cost** is upstream bandwidth, upstream rate limits (an
    isolated subtree pulls from Docker Hub as if it had its own host),
    and pull latency. Disk is unchanged.
- **What stays visible** even when isolated: total host disk use isn't
  shown to tenants (`/info` and `/system/df` describe the silo), but
  write throughput and timing of a shared disk are an ordinary noisy-
  neighbour channel, outside this page. Padding hit latency to look
  like a miss, for shared silos, is [open question 36](#open-questions).

### Configuration

The upstream list is the root's, set by the operator. It is in the
store, changed through the fence API on the root
(`PUT /silo/v1/registry/cache`, root members with `full` control only),
and shown by `silo registry cache status`:

```json
{
  "upstreams": [
    {"name": "docker.io", "endpoint": "https://registry-1.docker.io",
     "aliases": ["index.docker.io", "registry-1.docker.io"]},
    {"name": "gcr.io",    "endpoint": "https://gcr.io"},
    {"name": "ghcr.io",   "endpoint": "https://ghcr.io"},
    {"name": "quay.io",   "endpoint": "https://quay.io"}
  ],
  "size_limit_gb": 50,
  "stale_tag_max": "24h",
  "public_ttl": "24h",
  "auth_ttl": "300s"
}
```

- **`name`** is how references name the upstream, and must contain a
  dot. **`aliases`** are other host names that mean the same upstream,
  for Docker's normalisation and for `ns`. **`endpoint`** is where
  silod sends API requests. These are the mirror mappings: a reference
  to `index.docker.io/library/alpine` is the cache entry
  `docker.io/library/alpine`.
- **There is no credential field**, by design. The cache never holds
  one. An upstream that rate-limits anonymous pulls by host IP is
  [open question 40](#open-questions).
- Install writes the four upstreams above. Removing an upstream makes
  its path a `NAME_UNKNOWN` and its entries cache-only, so they age out
  ([below](#eviction-and-the-cache-budget-call-l74)). Facade pulls from
  it still go through the fetcher, as pulls from any registry do.

### Tools in containers are not repointed (call L75)

**Only the facade's pulls go through the cache automatically.** silod
doesn't rewrite configuration inside containers:

- Every tool keeps registry configuration in its own place and format:
  containerd's `hosts.toml`, `buildkitd.toml`, kaniko's flags,
  `/etc/containers/registries.conf` for skopeo and podman, crane's
  flags. silod would have to know each, and write into the image's
  filesystem, which it otherwise never does beyond `/etc/hosts` and
  `/etc/resolv.conf`.
- A tool pointed nowhere still works: it pulls from the upstream
  directly through the egress proxy, under the same egress policy. It
  only misses the cache.
- What silod does provide is the name, in DNS, and `NO_PROXY`. The
  per-tool lines are in [§9](#using-the-registry-and-the-cache-from-a-container),
  and the image `silo-tools` uses for an agent's tool container carries
  them. Writing them for well-known tools automatically is
  [open question 34](#open-questions).

### Eviction and the cache budget (call L74)

- **A cache entry** is an image record in containerd namespace
  `silo-cache`, named `<domain>/<upstream>/<repo>@<digest>`, with its
  recorded hosts, classification and last access time in the store.
  Entries keep their manifests' blobs alive through containerd's GC
  labels, as registry records do.
- **Cache-only bytes** are blobs that no silo's image record, no
  registry repo and no container snapshot references, and that some
  cache entry does. Only those count against the cache.
- **They are charged to nobody.** They come out of a **cache budget**
  that install carves from the root's disk capacity:

  ```
  root disk_gb = (data filesystem − reserve − cache budget) × 1.0
  ```

  adding to call L20. The budget is `--image-cache-size` (default 10% of
  the data filesystem, at least 10 GiB). The hard partition stays exact:
  every byte on disk is either charged to some silo, possibly more than
  once, or inside the budget.
- **Eviction** is least-recently-used, by entry, when cache-only bytes
  pass the budget, down to 90% of it. An entry whose blobs a silo or a
  repo also references isn't cache-only and isn't counted, so evicting
  it frees nothing and it is left. Evicting an entry never removes a
  blob something else references: containerd's GC removes only what
  nothing references.
- **`silo registry cache evict [<upstream>/<repo>[@digest]]`**, for the
  root, drops entries now.
- **A blob that a silo's image record pins** (a pulled image) is charged
  to that silo and is not the cache's, until the record goes. Then it
  becomes cache-only if a cache entry references it, and is evicted in
  turn.

## 9. Building images

### The supported path (call L69)

`docker build` stays refused in the facade (`local-backend.md` keeps
the facade-run build under Later). The supported way is to **build in a
container of the silo's own class, then push** to
`registry.silo.internal`. The build is then a workload like any other:
fenced, charged and audited.

| Builder | `sandbox` (gVisor) | `vm` (granule) |
| --- | --- | --- |
| **kaniko** | **Supported.** kaniko's README has "Running kaniko in gVisor", which needs only `--force`, "since currently there isn't a way to determine whether or not a container is running in gVisor". Its `run_in_docker.sh` runs it with Docker's default privileges, which is what the facade gives every container. *Unverified* on this backend until plan item 35 runs it | **Supported, *unverified*.** kaniko unpacks the base image into its own root and runs `RUN` steps there with no namespaces or mounts, which the wrapped container's chroot allows. It probably needs `--force` here too, because granule-container's chroot looks like no container runtime kaniko recognises |
| **BuildKit, rootless** | ***Unverified*, with known obstacles**, below | ***Unverified*, likely to work** with one change, below |

**kaniko** is the default this page documents. Two facts about it
matter:

- `GoogleContainerTools/kaniko` was **archived in June 2025**. The
  maintained fork is `chainguard-dev/kaniko`, which publishes no images
  of its own beyond Chainguard's, and says that "Running kaniko in any
  Docker image other than the official kaniko image is not supported".
  So the operator decides where the executor image comes from: built
  from the fork and pushed to a local repo, or pulled through the cache
  from Chainguard's registry, whose host the silo's egress must then
  allow.
- kaniko "by itself **does not** make it safe to run untrusted builds",
  and "relies on the security features of your container runtime"
  (its README, "Security"). Here that runtime is the silo's class,
  which is the point of building in a container.

**Rootless BuildKit** (`moby/buildkit:rootless`) is what BuildKit's own
container recipe runs, with `--security-opt seccomp=unconfined`,
`apparmor=unconfined` and `systempaths=unconfined` (BuildKit
`docs/rootless.md`, "Docker"). Against this backend:

- **seccomp and AppArmor:** the facade refuses both options. In
  `sandbox` it doesn't matter: `runsc` loads an OCI seccomp profile only
  with `--oci-seccomp`, which defaults to false ("Enables loading OCI
  seccomp filters inside the sandbox", `runsc/config/flags.go`), and the
  Sentry, not the host, answers the `unshare` and `mount` calls
  BuildKit needs. *Unverified:* that runsc ignores the AppArmor profile
  name as well.
- **`systempaths=unconfined`:** the facade has no such option. BuildKit
  offers `--oci-worker-no-process-sandbox` instead, which it discourages
  ("it cannot terminate processes that did not exit during an
  `ExecOp`", and lets `ExecOp`s signal the daemon), but uses in its own
  Kubernetes examples. Inside one container of one silo, that cost is
  the builder's own. Use the flag.
- **The overlay snapshotter:** gVisor's "Docker in gVisor" guide says
  overlay-on-overlay fails and that "gVisor currently requires tmpfs as
  an upper layer for overlayfs". In `sandbox`, run BuildKit with
  `--oci-worker-snapshotter=native`, or with its state directory on a
  `tmpfs` mount (which is charged to memory).
- **`newuidmap` and `no-new-privileges`:** rootlesskit maps a range of
  subordinate ids with the setuid `newuidmap` helper. The facade sets
  `no-new-privileges` on every container, so a setuid helper gains
  nothing, and rootlesskit can map only its own uid. Builds whose
  `RUN` steps create files owned by other users then fail. This is the
  obstacle most likely to stop rootless BuildKit in **both** classes.
  *Unverified* which way it goes. Plan item 35 finds out.
- **In `vm`**, the guest kernel is a full Linux kernel, so overlay and
  user namespaces work if granule's guest kernel enables them. The
  `newuidmap` question is the same, and depends on whether
  granule-container applies `no-new-privileges` inside the guest
  (*unverified*).

**Where a builder doesn't work, do this:**

- **Use kaniko** in that class. It covers Dockerfile builds.
- **If BuildKit is needed** (multi-platform output, cache mounts,
  build secrets), and it fails in `sandbox` but works in `vm`, run the
  builder in the `vm` class, if the silo's `isolation` policy allows
  it.
- **If it fails in both**, the reason will be privilege the facade
  doesn't grant. Granting in-sandbox or in-guest privileges to a
  declared builder container would not weaken the fence, because in
  both classes those privileges are perceived only inside the boundary
  (gVisor's tutorial: "gVisor never runs with capabilities on the host
  Linux kernel"; a guest's capabilities are the guest kernel's). But it
  is a facade change, so it is [open question 33](#open-questions), not
  part of this page.

### Using the registry and the cache from a container

Each tool needs to be told that `registry.silo.internal` is plain HTTP.

```sh
# kaniko: build, pull the base image through the cache, push locally
/kaniko/executor --force --context dir:///workspace \
  --destination registry.silo.internal/team-a/x/app:1 \
  --insecure-registry registry.silo.internal \
  --registry-map index.docker.io=registry.silo.internal/docker.io
```

```toml
# buildkitd.toml: the cache as Docker Hub's mirror (BuildKit adds ?ns=docker.io),
# and the local registry over plain HTTP
[registry."docker.io"]
  mirrors = ["registry.silo.internal"]
  http = true
[registry."registry.silo.internal"]
  http = true
```

```sh
# crane and skopeo
crane push --insecure image.tar registry.silo.internal/team-a/x/app:1
skopeo copy --dest-tls-verify=false oci-archive:image.tar docker://registry.silo.internal/team-a/x/app:1
```

- BuildKit's `mirrors` also takes a path form
  (`"core.harbor.domain/proxy.docker.io"` in its own example), so
  `registry.silo.internal/docker.io` works there too.
- *Verify* (plan item 35) that kaniko's `--registry-map` accepts a
  target with a path, as BuildKit's mirrors do. If it doesn't, name the
  base image in full in the Dockerfile:
  `FROM registry.silo.internal/docker.io/library/alpine:3.20`.

## 10. Garbage collection and deletion

### Deleting tags, manifests and repos (call L76)

- **Untag** (`DELETE /v2/<name>/manifests/<tag>`, or the fence API):
  removes the tag. The manifest stays, untagged, pullable by digest and
  charged, as in other registries.
- **Delete a manifest** (`DELETE /v2/<name>/manifests/<digest>`):
  removes it and every tag on it, refunds the bytes no other manifest
  of the repo references, and removes its `silo-registry` record. An
  index's child manifests go with it if nothing else in the repo
  references them.
- **`silo registry prune app --untagged`** deletes every untagged
  manifest. Nothing deletes untagged manifests automatically in v1. A
  retention policy is [open question 38](#open-questions).
- **Delete a repo** (fence API only): marks it `DELETING`, which makes
  every registry request on it a 404, then deletes its manifests as
  above and its row. Exports that name it by name stop covering
  anything; a prefix export keeps covering later repos.
- **`docker rmi registry.silo.internal/...`** removes the caller's own
  image record only, as in Docker. It never touches the registry.
- **A manifest being pulled** keeps its blobs until the pull finishes,
  through the request's lease ([§6](#requests)).

### Garbage collection

There is one collector: **containerd's**. silod never deletes a blob
itself. It removes **roots**, and containerd's mark-and-sweep removes
what nothing reaches:

| Root | Namespace | Removed when |
| --- | --- | --- |
| A silo's image record, and its containers' snapshots | `silo-<id>` | `docker rmi`, container removal, silo deletion |
| A repo's manifest | `silo-registry` | Manifest delete, repo delete, silo deletion |
| A cache entry | `silo-cache` | Eviction, `cache evict` |
| An upload session, or a request in flight | a lease with `gc.expire` | Session end, the grace hour, the request's end |
| A wrapped image for `vm` | `silo-<id>` | As today |

- **Charging follows the store, not the collector.** A refund happens
  when the store's rows change. A blob's bytes may stay on disk a little
  longer, until containerd collects, and are then inside the cache
  budget if the cache references them, or gone.
- **Collection is triggered** by containerd after deletions, as today,
  and every 10 minutes by silod's reconcile, which also compares the
  store's repo rows with the `silo-registry` records and reports
  differences (`registry_drift`). It re-creates a missing record when
  the content exists, and reports `image_manifest_missing` when it
  doesn't.

### Deleting a silo

Deleting a silo with `force` adds a step to
[Teardown](local-backend.md#teardown), before its namespace goes:

1. Its **exports end**, so importers stop pulling, and its imports of
   others' exports end.
2. **Its repos are deleted** as above, deepest silo first, with their
   charges. There is no outbound copy, so nothing is archived: pushed
   images that exist nowhere else are gone. `delete` without `force`
   is `FAILED_PRECONDITION` for a silo with repos, as for any non-empty
   silo, and its message counts them.
3. **What other silos pulled stays.** Their image records and
   containers are theirs, and keep their blobs alive.
4. **Its cache entries stay.** The cache is the host's, not the silo's.
   An isolated domain whose root is deleted leaves entries that no
   caller can hit, which age out by eviction.
5. A manage grant it held, or that targeted it, ends.

## 11. What a bug would expose

| Bug | Exposed |
| --- | --- |
| The access check reads the wrong silo, or treats a sibling as an ancestor | Another silo's pushed images, read. With a write bug, overwriting its tags. Its images are its code and data, so this is the registry's equivalent of a fence breach |
| A blob or `HEAD` answered from the content store instead of through a manifest of the named repo ([call L62](#the-registry-never-answers-from-global-content-call-l62)) | Any blob on the host by digest: another silo's private layers, a private upstream image's layers, or just their existence. This is the property the design leans on most, and every blob path goes through one function so it can be tested alone |
| A cross-repository mount without the read check on `from` | The same, by mount: a silo copies another's blob into its own repo |
| Proof of possession skipped (an upload deduplicated before its bytes are hashed) | Existence of a blob, by timing or by a short-circuited response |
| The name parser splits a path wrongly | Requests reaching a different silo's repo than named. The access check still applies to the repo it reached, so the exposure is wrong 404s or writes into the caller's own wrong repo, not another silo's |
| The egress check skipped or wrong on a hit | Upstream images a silo's ancestors denied, as content only. No connection is made, so nothing else leaves or enters |
| A private manifest served without revalidation, or an authorisation keyed by the wrong silo or repo | A private upstream image to a silo that never presented credentials for it |
| A credential written to a log, the audit, the store or a cache entry | One silo's upstream credential readable by whoever reads that: the operator, a later backup, anyone with silod's account |
| The sharing domain computed wrongly | The timing channel across an `isolated` boundary. Content is still never served without the access and egress checks |
| A charge not made or not refunded | The disk partition: a silo past its `disk_gb`, possibly filling the data filesystem for every silo, or a silo charged for nothing |
| A GC root missing for a repo manifest | **Data loss**: a pushed image's blobs collected while its tag still exists. The reconcile reports it (`image_manifest_missing`), but can't restore it |
| An export revocation or grant expiry not applied | The importer or holder keeps pulling, or pushing, after consent ended |
| The front-door `inet_diag` lookup matches the wrong socket | A host user acting as another user's silo on the registry, including pushes |
| `Host` routing on port 80 crossed with `/git/` or the export paths | A request handled by the wrong service. Each service still checks access itself, so this exposes nothing it wouldn't to that caller |

The structural defence is that **nothing is served because it is on
disk**. Every byte the registry or the cache serves is reached through
(caller, repo, manifest), each checked, and disk dedup is invisible to
the API.

## 12. Conformance cases

In the split of [`local-backend.md` §13](local-backend.md#two-kinds-of-cases-call-l24).
The registry exists only on the local backend for now, so the neutral
cases run against `local/sandbox`, `local/vm` and `local/fake`. A
`conformance.Target` reports whether it offers a registry, and the GCP
target reports these cases as "not offered", not as passing.

**Backend-neutral fence properties** (`pkg/conformance/fence`, against
the fence API and the OCI API through a probe):

- **Access:** a sibling gets 404 on pull, push, `HEAD` and tag list,
  identical for a repo that exists and one that doesn't, by path name
  and by id name; an ancestor pulls and can't push; a descendant can't
  read its parent's repos.
- **No global answers:** a silo that pushed blob B can't learn that
  another silo has B, by `HEAD`, by `mount` without `from`, by `mount`
  with a `from` it can't read, or by pushing B (the push uploads in
  full).
- **Exports:** offered but not accepted gives nothing; accepted gives
  pull and never push; a prefix covers a repo created after the
  accept; withdrawal closes an in-flight pull within a second;
  grant-policy suspension.
- **Manage grants:** `registry: read` pulls and can't push; `write`
  pushes and charges the target; revocation closes open connections.
- **Sealed and `read` silos:** push and pull to existing repos; a first
  push to a new repo is refused; repo delete refused.
- **Never-looser:** `images.sharing` can't be loosened by a child.
- **Egress through the cache:** a silo whose egress denies the upstream
  gets 403 on a hit for an image another silo pulled; denying only the
  blob redirect host refuses blobs and not manifests; a silo with empty
  egress still pulls its own local repo.
- **Private upstream images:** against a test upstream with token auth,
  a silo with credentials pulls; then a silo without credentials gets
  401 on the manifest and 404 on each blob by digest, under the private
  repo's path and under a public repo's path.
- **Isolation:** after a shared-domain pull of an image, an isolated
  silo's pull of it makes upstream requests for every blob (counted at
  the test upstream).
- **Audit:** every request of a pull, hit and miss, produces a record
  as the calling silo, and no record, log line or store row contains a
  canary credential the test presented.
- **Quota:** a push over `disk_gb` is refused mid-upload; deleting the
  repo refunds exactly what the push charged; `image_repos` limits
  creation.
- **OCI conformance:** the distribution-spec conformance suite
  (`opencontainers/distribution-spec/conformance`) passes against a
  silo's own repo, for pull, push, content discovery and management.

**Facade-specific cases** (`pkg/conformance/docker`):

- `docker tag` and `docker push` to `registry.silo.internal/<own
  path>/app`, then `docker pull` and `docker run` of it from another
  container of the same silo and from an importer;
- a push to any other registry is 501 with the message;
- a pull from a local repo in a silo with an empty egress policy works;
- `docker save` then `docker load` round-trips an image with the same
  digests; `load` of an archive with a mismatched digest, a path
  traversal entry, or a size past the silo's free disk is refused and
  writes nothing;
- `docker commit` of a `sandbox` container that created, changed and
  deleted files gives an image with those three changes; `commit` of a
  `vm` container is 501;
- `docker build` is still 501, and its message names kaniko and
  BuildKit;
- `docker login` to an upstream with good and bad credentials gives
  `Login Succeeded` and 401, and stores nothing on the host;
- `docker pull` of a private upstream image with `X-Registry-Auth`
  works, and without it fails with Docker's `unauthorized`;
- every new refusal in [§7](#what-changes-call-l68)'s table.

**Local-only structural cases** (`pkg/conformance/local`):

- the cache stays within its budget under a pull loop, evicts least
  recently used, and never evicts a blob a record or repo references;
- after deleting a repo, its blobs are gone from the content store once
  nothing else references them, and a blob another repo references
  stays;
- wiping `cache/` loses no pushed image;
- a store restored to before a push reports the pushed manifest as an
  orphan and doesn't serve or delete it;
- the host front door maps `localhost:5115` connections from two uids
  to their own silos;
- the builder recipes in [§9](#9-building-images): kaniko in both
  classes, and whatever plan item 35 finds for BuildKit.

## 13. Staged implementation plan

These are items **25 to 36** of
[`local-backend.md` §15](local-backend.md#15-staged-implementation-plan),
numbered after part 3's, so existing references stay valid. Nothing
earlier depends on them. Each can be filed on its own, in this order.

25. **Image repos in the fence API and the store.**
    - The `imageRepos` object, its list, get and delete calls, and
      `silo registry list|get|tags|url|delete|prune`;
    - the `image_repos` quota class;
    - the `registry` type in manage-grant scopes;
    - registry exports (`protocol: "registry"`, names and prefixes) and
      their imports;
    - the `images.sharing` policy, never-looser.

    Uses the fake backend. Depends on 4.

    *Landed in task 3450:* the object and its calls, the tables,
    `silo registry` (all but `cache`, which is step 31's), the
    `image_repos` class, the `registry` scope type, and
    `fenceapi.CreateImageRepo`, the first push's half that step 27
    calls. Each manifest row carries the digests and sizes it
    references, from which `charged_bytes` is worked out; the
    `disk_gb` charge itself is made by step 27's push path, which has
    the bytes. *Left:* registry exports and their imports, and the
    `images.sharing` policy, filed as step 25b.

    *Landed in step 25b (task 3478):* registry exports, `protocol:
    "registry"` with `repos` and no target, stored in `exports.repos`
    (migration 0024), offered, accepted, suspended and withdrawn as
    every export is, and left alone by the fence and the gateway, which
    carry nothing for them; `silo export offer --protocol registry
    --repos`. The registry's questions are
    `fenceapi.MayReadImported` (and `ImportOfImageRepo`, for the audit
    record's import) and `fenceapi.ImportedImageRepos`, which step 27's
    `GET /v2/_catalog` lists beside the caller's own and descendants'
    repos. The fence API's `GET .../imageRepos` stays per silo, as
    [Listing](#listing) puts imports in the catalog only. And
    `images.sharing`, `{"sharing": "shared"|"isolated"}` on create,
    PATCH and bootstrap, shown own and effective with
    `effective_from`, the sharing domain step 31's cache index is
    keyed by; `--images-sharing` on `silo create` and `silo patch`. A
    new silo's own value is `shared`, so it is in its parent's domain
    until it isolates itself.
26. **containerd's root out of `cache/`** (call L60): layout 2,
    `containerd/` and `cache/snapshots/`, the migration from layout 1,
    and the data table. Depends on 14. Best landed before 7, so that
    the gVisor driver creates containerd in the right place first.

    *Landed in task 3479*, before 7.
27. **The registry service for local repos.**
    - `registry.silo.internal` in the gateway's DNS and in `NO_PROXY`;
    - `Host` routing on port 80;
    - the name parser, with a fuzz test;
    - the access table and the 404 rule;
    - manifests, blobs, uploads with leases, mounts, tag lists,
      referrers and the filtered catalog;
    - proof of possession, as one blob-access function every path uses;
    - charging and enforcement at push;
    - first-push creation and its control-policy check;
    - `silo-registry` records and the audit.

    Tested with the distribution-spec conformance suite, crane, skopeo
    and oras against silod in-process. Depends on 25, 26, and on 15 for
    the listener in `silo-net`.

    *Landed in task 3480, over a fake content store:* `pkg/registry`,
    the handler, over `registry.Content`, an interface shaped like
    containerd's content API (blobs by digest, ingests, leases, and
    `silo-registry` image records named `<repo id>@<digest>` with
    `dev.silo.seq` and the manifest's references), with an in-memory
    `Memory` that every test runs on. The access table is the fence
    API's `RegistryAccess` (imports included, through step 25b's
    `ImportOfImageRepo`), and the catalog its `RegistryCatalog`.
    `Service.blob` is the one blob-access function: HEAD and GET of a
    blob, a mount's `from` and a manifest's references all go through
    it. A repo charges `local/disk_gb` on its `image_repo:<id>` ledger
    object (the one its `image_repos` count is on, so a repo delete
    refunds both), in millionths of a GiB, rounded up: its manifests'
    distinct blobs, its grace-hour blobs and its uploads in progress. A
    chunk the silo has no room for is truncated back out of the ingest
    and refused with 413 `DENIED`, so an upload never overshoots. The
    gateway routes port 80 by `Host` (`IsRegistryHost`) to silod's
    `RegistrySilodPath`, `registry.silo.internal` answers with
    `silo.internal`'s address, and `silo-tools`' default `NO_PROXY`
    names it. A `registry` CI job runs the conformance suite (all of it
    passes but blob delete, which is 405 here, and a mount without
    `from`, which is a session by design), crane, skopeo and oras
    against the handler.

    *Left, filed as step 27b:* containerd's implementation of
    `registry.Content` in silod and wiring the service into silod's
    `SilodOptions.Registry` (until then silod answers the registry's
    path with 404); a short read lease per request; the grace hour's
    blobs survive only as long as silod's process, since they are kept
    in memory (their leases expire on their own, and the next push
    re-reckons the repo's charge). A fence API manifest delete refunds
    the bytes the next time the registry reckons the repo, which step
    34's deletion work settles. The Docker facade puts no proxy
    variables in containers' environment yet, so there was no
    `NO_PROXY` there to add the name to; whatever adds them names
    `registry.silo.internal` beside `silo.internal`.
28. **Grants on the registry:** exports and imports in the access path,
    manage-grant scopes, and closing connections within a second on
    revocation and expiry. Depends on 27.

    *Landed in task 3481.* Step 27's `RegistryAccess` already gave
    imports their pulls and manage grants their scopes; a grant whose
    scope leaves out `registry` now answers the same 404 as no grant
    (it was a 500). The gateway's tables carry `RegistryImports`, the
    ACTIVE imports of registry exports, and when one leaves them
    (withdrawn, suspended, or its silo gone) the gateway cuts off the
    importer's port 80 registry calls to the exporter's repos, whose
    names it parses with `imagerepos.SplitPath`; calls to other silos'
    repos go on. A manage grant that ends cuts off its holder's port 80
    calls, git's and the registry's, as git's did. silod looks for
    expired grants every 500ms, not every 10s, so that expiry is
    within the second too. The §12 cases for exports and manage grants
    are in `pkg/registry` and, through the gateway, in
    `pkg/gateway/registrygrants_test.go`.
29. **The host front door for registry clients:** the loopback listener,
    the `inet_diag` uid lookup and its verification across kernels, and
    `--registry-host-port`. Depends on 27.
30. **The Docker facade:**
    - `push` to the local registry;
    - pulls of local repos without egress;
    - `save`, and `load` without extraction (both archive forms);
    - `commit` for `sandbox` through `runsc tar rootfs-upper`, with its
      whiteout check;
    - `/auth` validate-only;
    - the updated refusals.

    Depends on 6, 7 and 27.

    *Landed in task 3483, against the fake driver and `registry.Memory`:*
    the facade's `imagePush`, `imageSave`, `imageLoad`, `commit` and
    `auth` (`pkg/facade/local/docker/registry.go` and `archive.go`), and
    pulls of `registry.silo.internal` names through the registry rather
    than `Options.Images`. The registry gained `Service.PushHeld` and
    `Service.Pull`, which serve the facade's push and pull in process as
    the caller, as a client's manifest PUT and GET are served, except
    that a facade push proves possession by the caller's image record:
    each blob is checked in the content store, not through
    `Service.blob`. Image records now carry their manifest's digest and
    media type. `load` commits no ingest until the whole archive has
    been read and its manifests checked, so a refused load leaves
    nothing in the content store. `commit` reads the writable layer
    through `runtime.Committer` and turns overlayfs whiteouts (a 0/0
    character device) and opaque directories (`trusted.overlay.opaque`)
    into OCI's `.wh.` entries, refusing devices and paths outside the
    root. The cases of [§12](#12-conformance-cases)'s facade list run
    as the facade package's tests (`registry_test.go`), but for the
    private upstream ones, which need step 32.

    *Left:* the gVisor driver's `RootfsUpper` (`runsc tar rootfs-upper`
    through `silod runsc`) and the run that settles this page's
    *unverified* whiteout claim against real runsc; silod's wiring of
    the facade's `Content`, `Registry` and `Login` (with step 27b's
    containerd `registry.Content`); and moving the facade cases into
    `pkg/conformance/docker`, so that step 36 runs them against
    `local/sandbox` too.
31. **The pull-through cache, public content:**
    - the upstream configuration and `silo registry cache`;
    - path and `ns` addressing, Docker Hub normalisation;
    - recorded hosts and the per-request egress check, with the
      redirect re-check;
    - misses through the egress proxy as the caller;
    - single-flight fetches;
    - tag revalidation and stale tags;
    - facade pulls moved onto the fetcher;
    - `registry.cache` audit.

    Tested against a local test upstream with redirects to a second
    host. Depends on 27 and 30.

    *Landed in task 3484, over `registry.Memory`:* the configuration
    in settings as `registry.cache` (install's four upstreams until
    set), `GET`/`PUT /silo/v1/registry/cache` for root members (`PUT`
    with full control, audited as `registry.cache.set`), and `silo
    registry cache status|set`. The cache is `pkg/registry/cache.go`:
    upstream paths and `?ns=` through `imagerepos.ParseName`, its index
    in `registry_cache_objects` and `registry_cache_tags` (migration
    0027), keyed by upstream repo, so a blob is served only in the repo
    it was fetched for. Each fetch does the anonymous token exchange
    and follows redirects itself, recording the API host, the token
    realm's host and every redirect host with the object. Every
    request, hit or miss, is checked against the caller's effective
    egress (`fenceapi.EffectiveEgress`) for each of them: a denied host
    is 403 `DENIED` naming it, and a blob refused only for a redirect
    host is re-checked once by a GET whose body isn't read. Misses go
    through `Options.Egress`, a round tripper per calling silo, and
    concurrent misses on one object share one fetch, a waiter whose
    shared fetch failed making its own. Tags are revalidated by a
    manifest `HEAD` on every request, served `stale` up to
    `stale_tag_max` when the upstream can't be reached and 503 after;
    digests aren't revalidated. Each request writes `registry.cache`
    as the calling silo (with `hit`/`miss`/`stale`, the egress
    decision, the hosts and `auth=none`/`anonymous`), and a refusal
    `registry.denied` too. The facade's pulls from any other registry
    go through `registry.Service.PullThrough` and `FetchThrough`
    (facade `Options.Fetcher`), with the pull's `pull_id` on both the
    `facade.call` record and the `registry.cache` records; the old
    `Options.Images` puller is used only where there is no fetcher, as
    in the fake driver. An upstream that wants credentials is 404
    `NAME_UNKNOWN` until step 32.

    *Left:* the production `registry.Egress`. The gateway's egress
    proxy names its caller by source address, so silod can't yet send
    a request through it as a silo; the gateway needs an authenticated
    way for silod to say which silo a request is for (on its silod
    socket, as `Silo-Origin` is), and silod's wiring of the cache with
    step 27b's. The cache's `silo-cache` image records and GC labels
    are step 34's, with eviction; the cache index per sharing domain is
    step 33's.
32. **Private upstream content:**
    - anonymous classification and `public_ttl`;
    - revalidation by digest with the caller's credentials;
    - in-memory authorisations;
    - the Basic challenge on upstream paths;
    - the canary-credential test over logs, audit, store and the data
      directory;
    - checking which upstreams count a `HEAD` as a pull.

    Depends on 31.
33. **Sharing domains:** the cache index per domain, full fetches for
    misses in an isolated domain, and facade pulls on the same rule.
    Depends on 25 and 31.
34. **Cache budget, eviction, GC and deletion:**
    - the budget in root capacity, `--image-cache-size`;
    - LRU eviction of cache-only entries, `cache evict`;
    - tag, manifest and repo deletion;
    - silo deletion's registry step;
    - the reconcile's `registry_drift` and `image_manifest_missing`;
    - restore handling for `silo-registry` records.

    Depends on 27 and 31.
35. **Builders, run for real.** kaniko (from the Chainguard fork) and
    rootless BuildKit, in `sandbox` and in `vm`, each building a
    Dockerfile with a `RUN` step that `chown`s a file to another user,
    and pushing to the local registry with the base image through the
    cache. It settles this page's *unverified* builder claims, the
    `newuidmap` question, and kaniko's `--registry-map` with a path,
    and writes the results back into [§9](#9-building-images). Depends
    on 27, 30 and 31, and on 10 for the `vm` half.
36. **Registry and cache conformance cases** in `local-conformance`,
    as listed in [§12](#12-conformance-cases). Depends on 12 and on
    28 to 35.

Plan item 9's granule list gains one entry: **the diff of a wrapped
container's root, as a tar**, for `docker commit` in the `vm` class.

## 14. Later

- **Outbound copy to an external registry.** Mirroring pushed images to
  an operator's registry, as [`local-git.md`](local-git.md#8-the-outbound-mirror)
  mirrors repos into the uber repo: one external registry, a namespaced
  repository per silo repo, one credential under `keys/`, never a
  silo's, and a never-looser policy like `repos.mirror` for keeping a
  subtree's images on the host. The no-credential cache and the
  per-silo names were chosen so that this can be added without
  renaming anything.
- **`docker build` in the facade**, as `local-backend.md`'s Later
  already says: run as a builder container in the caller's silo, with
  the push to the local registry as its output.
- **The GCP backend.** The registry carries over as a gateway path, with
  its content on the control VM's disk or in a bucket. Artifact
  Registry is the other option, at the cost of the per-silo fence being
  IAM rather than silod.
- **Signature policy:** a never-looser policy that a silo may only run
  images signed by given keys, checked at create, using the referrers
  the registry already serves.
- **Cache warming:** the operator lists images the cache fetches ahead
  of time, as the root's own pulls.
- **Pinning a silo's images**, which part 2's Later asks for, is now a
  push to the silo's own repo.

## Calls made on this page

L56. **silod serves an OCI Distribution API registry at
     `registry.silo.internal`,** on the gateway's port 80, selected by
     `Host`, over plain HTTP, inside `silo-net`. It speaks v1.1,
     referrers included.

L57. **Names are `<silo path>/<repo>`,** with single-component repo
     names without dots, `id_<silo id>/<repo>` by id, and a first
     component with a dot for an upstream in the cache. A mirror's
     `?ns=` selects an upstream the same way.

L58. **Image repos are silo objects created by their first push,**
     unlike git repos. They are listed and deleted through the fence
     API, and counted in a new `image_repos` quota class.

L59. **Blobs are stored once in `silo-containerd`'s content store.**
     The store holds repos and tags, and per-repo containerd records in
     `silo-registry` keep their content alive.

L60. **containerd's root moves to `<data-dir>/containerd/`,** out of
     `cache/`, because pushed images are data. Writable layers stay in
     `cache/snapshots/`.

L61. **A repo charges its silo's `disk_gb` with the stored size of the
     distinct blobs its manifests reference,** per repo, enforced at
     push. Uploads are charged as they arrive. A manage-grant holder's
     push charges the target.

L62. **The registry never answers from global content.** Uploads prove
     possession, and blob existence, reads and mounts go through a
     manifest of a repo the caller may read.

L63. **Access is git's:** a silo reads and writes its own repos,
     ancestors read and delete, manage grants give what the `registry`
     type and access allow, importers read, and everyone else gets 404.

L64. **Sideways access is a registry export,** `protocol: "registry"`,
     of repo names or name prefixes, offered and accepted like any
     export, read-only.

L65. **Pushing, pulling and untagging are a data path** that sealed and
     `read` silos keep on existing repos. Creating a repo by push and
     deleting a repo are control-plane calls.

L66. **Identity is origin inside, and the uid on the host:**
     `SO_PEERCRED` on the facade's sockets, and `inet_diag` on the
     loopback registry port. `Authorization` never identifies anyone.

L67. **Every registry and cache request is audited** as the calling
     silo, and no credential, or anything derived from one, is ever
     recorded.

L68. **The facade serves `push` to the local registry, `save`, `load`,
     `commit` for `sandbox`, and validate-only `/auth`.** Pulls from
     the local registry skip egress. `build`, `import` and `vm`
     `commit` stay 501. `load` never extracts, which answers
     `local-backend.md`'s open question 6.

L69. **Builds run in a container of the silo's class and push.** kaniko
     is the supported builder in both classes. Rootless BuildKit is
     documented with its obstacles, and v1 grants no extra privilege
     for it.

L70. **One pull-through cache per host** for configured upstreams,
     addressable by path or `ns`, and behind every facade pull. Tags are
     revalidated per request, and digests aren't.

L71. **Every cache request is held to the caller's egress policy,** on
     the upstream's API and token hosts and the blob's recorded
     redirect hosts. A miss is the caller's own fetch through the
     egress proxy.

L72. **Private upstream content is served only after the caller's own
     credentials revalidate the manifest,** with brief in-memory
     authorisations for its blobs. Credentials and tokens are never
     stored or logged.

L73. **`images.sharing: shared | isolated`, never-looser, sets sharing
     domains.** A hit is decided by the caller's domain's index, never
     by the content store. This answers `local-backend.md`'s open
     question 7.

L74. **Cache-only blobs are charged to nobody,** inside a cache budget
     carved from the root's disk capacity, and evicted least recently
     used.

L75. **Only the facade's pulls use the cache automatically.** Tools in
     containers are pointed at it by their own configuration, which
     silod documents and doesn't write.

L76. **containerd's collector is the only one.** silod removes roots
     (records, entries, leases), charges follow the store, and deleting
     a silo deletes its repos with no archive.

## Open questions

31. **Multi-component repo names.** `registry.silo.internal/team-a/x/tools/builder`
    can't be told apart from repo `builder` of a silo `/team-a/x/tools`.
    A marker component that no silo name can be, because it contains
    `_` (for example `.../team-a/x/img_r/tools/builder`), would allow
    it. Is that worth an uglier name for the cases that want it?
32. **TLS.** Should the registry also listen on 443 with a certificate
    from a host-local CA under `keys/`, so tools work without an
    insecure-registry flag? The CA would have to reach every image's
    trust store, which silod doesn't write.
33. **Privileges for a declared builder container.** In both classes,
    capabilities, seccomp and AppArmor apply only inside the boundary.
    Should the facade accept them for a container labelled as a builder,
    in a silo whose policy allows builders, if plan item 35 shows
    rootless BuildKit can't run without them?
34. **Pointing tools at the cache automatically.** Should silod set
    well-known configuration (`/etc/containers/registries.conf.d/`,
    a `buildkitd.toml`, containerd's `hosts.toml`) in containers that
    opt in with a label?
35. **Private revalidation.** Every pull of a private image costs an
    upstream round trip per manifest. Should an authorisation last
    longer than 5 minutes, at the cost of serving an image for that
    long after the upstream revoked the caller's access?
36. **Padding hits.** Instead of, or beside, `isolated`, should hits for
    shared silos be delayed to look like misses? It costs every pull's
    speed, and timing padding is hard to get right.
37. **The host registry port.** Should `127.0.0.1:5115` be on by default?
    `docker push` through the facade's socket needs no port at all.
38. **Retention.** Should a repo have a policy that deletes untagged
    manifests after a time, or keeps only the last N tags? v1 keeps
    everything until deleted.
39. **Write grants between silos.** Should a registry export be able to
    grant push, so that a CI silo can publish into a release silo's
    repo? Today it pushes to its own repo and the release silo pulls.
40. **Upstream rate limits.** Every silo's anonymous Docker Hub pulls
    leave from one host address. Should the operator be able to give
    the cache a pull credential used only for public content, stored in
    `keys/`? It conflicts with "the cache holds no credential", which
    this page keeps.
