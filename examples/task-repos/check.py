#!/usr/bin/env python3
"""Check the example task repos against docs/task-repos.md.

This is a reviewer's aid, not the implementation. It:

- finds every object by its file name, <name>.<kind>.md (§3.2), never by
  scanning front matter, and refuses a file whose front matter's kind
  differs from its suffix, or a kind: file with no suffix; reads each
  repo's .grainconfig for its aliases and settings (§3.4);
- validates each object against its kind's JSON Schema, and each
  .grainconfig against grainconfig.schema.json;
- resolves every reference -- relative, by alias, and by git URL, mapping
  this folder's illustrative Silo URLs to the sibling directories -- with
  the path as the identity, following a tombstone beside the old path
  (<old>.moved.md) with a hop limit and cycle detection, then the archive
  mapping (§3.8);
- checks the path rules: a tombstone's target lists it in moved_from, and
  the moved object's moved_from entries have tombstones pointing back;
- lists each grain.config.md's typed root entries from the tree alone
  (§3.3, §3.7): the entry's folder subtree, its kind suffix only, less its
  ignore: patterns (gitignore syntax) and any archive/ folder, the most
  specific entry of one type winning an overlap, under limits that fail
  loudly; and checks what the examples say each Grain's active set holds;
- checks backlogs: every reference resolves to a task, no lane holds two
  references to one task, and no reference outlives its task's close;
- checks execution records: at most one open run per task;
- checks code repos (host: other): a task there came from an intake
  (moved_from) and is not a trigger's firing, and a template there carries
  no reads, writes, grants or target; a gate there names no other repos,
  and a workflow trigger's workflow exists in the same repo;
- checks that no active set holds a gate, template or capability, since
  gates are read by the merged-into repo's merge queue and the rest are
  reached by reference only (§3.1, §6.3);
- checks context: entries (§5.6): write only on a repo or folder at a
  branch, depth only on files, input: only in templates, no overlapping
  sandbox paths, and, in a code repo, only that repo; prints each task's
  sandbox layout, following depth through Markdown body links;
- treats any file that is not an object as an asset (§2.2), and checks
  each kind: asset sidecar's asset, sha256 and bytes;
- derives each task's status from its runs, and prints each lane's
  effective order, and each Grain's next picks across its lanes;
- resolves capability objects (§5.7): each chain of bases to a base type,
  with cycle detection and a depth limit; applies the built-in reducers to
  the Grain's published base declaration; checks the chain against an
  illustrative local policy (sanctioned bases and definition sources,
  implied egress and repos); reports Anselm reducers as not evaluable,
  which refuses the request until Anselm is adopted;
- evaluates GRAINOWNERS (§3.5): every object has an owner, the nearest
  file and its last matching line win, field lines override for their
  fields; and replays the design's rejected push;
- runs the orphan check (§3.9), printing each orphan as a warning;
- recomputes requirement hashes;
- renders each template fixture with Python Jinja2 under StrictUndefined.

Needs python3-yaml, python3-jsonschema and python3-jinja2.
"""
import hashlib
import json
import pathlib
import posixpath
import re
import sys
from collections import Counter

import jinja2
import jsonschema
import yaml

HERE = pathlib.Path(__file__).resolve().parent
# The examples' illustrative hosts, mapped to the folders that stand in for them.
HOSTED = {
    "https://silo.example.com/git/hub/intake.git": "hub-intake",
    "https://silo.example.com/git/hub/shared.git": "hub-shared",
    "https://silo.example.com/git/grains/eu.git": "grain-eu",
    "https://silo.example.com/git/grains/us.git": "grain-us",
    "https://git.example.com/hub/widgets.git": "widgets-code",
    "https://silo.example.com/git/hub/placements.git": "hub-placements",
    "https://silo.example.com/git/org/templates.git": "org-templates",
    "https://silo.example.com/git/org/capabilities.git": "org-capabilities",
}
EXTERNAL = ("https://github.com/",)  # real repos: referenced, not fetched here
REPOS = sorted(set(HOSTED.values()))
MAX_HOPS = 8
OPEN_RUN = ("accepted", "running", "waiting")
CLOSED_RUN = ("done", "failed", "cancelled")
PRIORITY = ["urgent", "high", "normal", "low"]
# Kind suffixes (§3.2), longest first so capability-base wins over capability.
KINDS = sorted(["task", "ref", "run", "comment", "template", "trigger", "gate", "suite", "design",
                "memory", "moved", "asset", "capability", "capability-base"], key=len, reverse=True)
ROOT_TYPES = {"backlog": "ref", "triggers": "trigger", "memory": "memory"}
ASSET = "(asset)"
CONTEXT_LIMITS = {"entries": 64, "depth": 3, "reached_files": 256, "reached_bytes": 64 << 20, "file_bytes": 32 << 20}
LIMITS = {"objects_per_entry": 5000, "objects": 50000, "entries": 64}
CAPABILITY_DEPTH = 8
# Grain eu's local policy for capabilities, as §5.7 sketches it. A real
# policy is the Grain's own configuration, not a task-repo file.
POLICY = {
    "eu": {
        "bases": ["git@v1", "gcp-service-account@v1"],
        "sources": [
            {"source": "https://silo.example.com/git/grains/eu.git//", "bases": "*"},
            {"source": "https://silo.example.com/git/org/capabilities.git//", "bases": ["gcp-service-account@v1"]},
            {"source": "https://silo.example.com/git/hub/shared.git//capabilities/", "bases": ["gcp-service-account@v1"]},
        ],
        "egress": ["oauth2.googleapis.com", "sts.googleapis.com", "*.googleapis.com"],
        "repos": ["https://github.com/bwsalmon/*", "https://silo.example.com/git/hub/*", "https://git.example.com/hub/*"],
        "anselm": None,  # no Anselm evaluator yet
    },
}
errors = []


def err(msg):
    errors.append(msg)


def split(path):
    text = path.read_text()
    if not text.startswith("---\n"):
        return None, text
    fm, body = text[4:].split("\n---\n", 1)
    return yaml.safe_load(fm), body


def load_schemas():
    store, schemas = {}, {}
    for p in (HERE / "schemas").glob("*.json"):
        s = json.loads(p.read_text())
        store[s["$id"]] = s
        store[p.name] = s
        schemas[p.name.split(".")[0]] = s
    return store, schemas


def grainconfig(repo):
    """A repo's .grainconfig (§3.4); the defaults when it has none."""
    p = HERE / repo / ".grainconfig"
    return yaml.safe_load(p.read_text()) if p.is_file() else {}


def kind_of(path):
    """The kind a file name declares (§3.2), or None for an asset."""
    name = posixpath.basename(path)
    if name == "grain.config.md":
        return "config"
    for k in KINDS:
        if name.endswith("." + k + ".md"):
            return k
    return None


def suffix_problem(path, fm):
    """Why a file's name and front matter disagree, or None (§3.2)."""
    k = kind_of(path)
    fk = fm.get("kind") if isinstance(fm, dict) else None
    if k and fk != k:
        return f"the name says kind {k}, the front matter says kind {fk}"
    if not k and fk:
        return f"front matter says kind {fk}, but the name has no kind suffix"
    return None


def objects():
    objs = {}
    for repo in REPOS:
        for p in sorted((HERE / repo).rglob("*.md")):
            path = str(p.relative_to(HERE / repo))
            fm, body = split(p)
            why = suffix_problem(path, fm)
            if why:
                err(f"{repo}/{path}: {why}")
                continue
            if kind_of(path):
                objs[(repo, path)] = (fm, body)
    return objs


def aliases(repo, src):
    return grainconfig(repo).get("aliases") or {}


def short(ident):
    return f"{ident[0]}/{ident[1]}"


def canonical(repo, src, ref):
    """Return the identity (repo, path) a ref string names, or (None, reason)."""
    target = ref.split("#", 1)[0]
    if "://" in target:
        scheme_end = target.index("://") + 3
        sep = target.find("//", scheme_end)
        url, inpath = (target[:sep], target[sep + 2:]) if sep >= 0 else (target, "")
        inpath = inpath.rsplit("@", 1)[0] if "@" in inpath else inpath
        if sep < 0 and "@" in url[scheme_end:]:
            url = url.rsplit("@", 1)[0]  # a whole repo at a ref
        if url.startswith(EXTERNAL):
            return None, "external"
        if url not in HOSTED:
            return None, f"unknown repo {url}"
        return HOSTED[url], posixpath.normpath(inpath.lstrip("/")) if inpath else ""
    m = re.match(r"^([a-z][a-z0-9-]*):(.*)$", target)
    if m:
        al = aliases(repo, src)
        if m.group(1) not in al:
            return None, f"undeclared alias {m.group(1)}"
        return canonical(repo, src, al[m.group(1)] + "//" + m.group(2))
    target = target.rsplit("@", 1)[0] if "@" in target else target
    if target.startswith("/"):
        return repo, posixpath.normpath(target.lstrip("/"))
    return repo, posixpath.normpath(posixpath.join(posixpath.dirname(src), target))


def stone_of(path):
    """Where a tombstone for path sits (§2.1): its .md replaced by .moved.md."""
    return path.removesuffix(".md") + ".moved.md"


def old_of(stone):
    return stone.removesuffix(".moved.md") + ".md"


def archive_path(path):
    """The archive mapping (§3.8): <top>/<rest> is stored at <top>/archive/<rest>."""
    top, _, rest = path.partition("/")
    return f"{top}/archive/{rest}" if rest else None


def resolve(objs, repo, src, ref):
    """Follow a reference, and any tombstones, to an object (§4).

    Returns (identity, front matter, hops) or (None, reason, hops)."""
    ident = canonical(repo, src, ref)
    if ident[0] is None:
        return None, ident[1], 0
    seen, hops = [], 0
    while True:
        if ident in seen:
            return None, "redirect loop: " + " -> ".join(short(i) for i in seen + [ident]), hops
        seen.append(ident)
        if ident[1] == "" or (HERE / ident[0] / ident[1]).is_dir():
            return ident, None, hops  # a repo or folder
        if ident in objs and objs[ident][0]["kind"] != "moved":
            return ident, objs[ident][0], hops
        if ident not in objs and (HERE / ident[0] / ident[1]).is_file():
            return ident, {"kind": ASSET}, hops  # an asset: any file that is not an object (§2.2)
        stone = ident if ident in objs else (ident[0], stone_of(ident[1]))
        if stone in objs:
            hops += 1
            if hops > MAX_HOPS:
                return None, f"more than {MAX_HOPS} redirects", hops
            ident = canonical(stone[0], stone[1], objs[stone][0]["to"])
            if ident[0] is None:
                return None, ident[1], hops
            continue
        arch = archive_path(ident[1])
        if arch and (ident[0], arch) in objs:
            return (ident[0], arch), objs[(ident[0], arch)][0], hops  # not a hop
        return None, "dangling", hops


def edges(fm):
    """Every (field, ref string, expected kind, pinned) a file holds."""
    out = []

    def add(field, value, kind=None, pinned=False):
        if isinstance(value, dict):
            value = value.get("ref")
        if value:
            out.append((field, value, kind, pinned))

    kind = fm["kind"]
    for e in context_of(fm) if kind in ("task", "template", "trigger") else []:
        e = entry(e)
        if e.get("ref"):
            add("context", e["ref"], "capability" if e["mode"] == "use" else None)
    if kind == "task":
        add("template", fm.get("template"), "template")
        add("trigger", fm.get("trigger"), "trigger")
        for r in fm.get("depends_on") or []:
            add("depends_on", r, "task")
        for r in fm.get("implements") or []:
            add("implements", r, "design")
        add("supersedes", fm.get("supersedes"), "task")
        add("filed_by.run", (fm.get("filed_by") or {}).get("run"), "run")
    elif kind == "ref":
        add("to", fm.get("to"), "trigger" if fm.get("fired_at") else "task")
        add("previous", fm.get("previous"), "run")
    elif kind == "run":
        add("task", fm.get("task"), "task")
        # from is pinned to the commit it was read at: Silo removes a
        # reference once its run closes, and these example repos have no
        # history to read it back from.
        add("from", fm.get("from"), "ref", pinned=True)
        add("previous", fm.get("previous"), "run")
        for r in (fm.get("closed") or {}).get("follow_ups") or []:
            add("closed.follow_ups", r, "task")
    elif kind == "comment":
        add("run", fm.get("run"), "run")
        add("task", fm.get("task"), "task")
        add("in_reply_to", fm.get("in_reply_to"), "comment")
        add("supersedes", fm.get("supersedes"), "comment")
        for r in fm.get("attachments") or []:
            add("attachments", r, ASSET)
    elif kind == "asset":
        add("asset", fm.get("asset"), ASSET)
    elif kind == "trigger":
        fires = fm.get("fires") or {}
        add("fires.template", fires.get("template"), "template")
        add("fires.review.template", (fires.get("review") or {}).get("template"), "template")
        add("derived_from", fm.get("derived_from"), "trigger")
        for r in ((fm.get("on") or {}).get("design-change") or {}).get("designs") or []:
            if not r.endswith("/"):
                add("on.design-change.designs", r, "design")
    elif kind == "gate":
        req = fm.get("requires") or {}
        for k in ("security_review", "second_review"):
            add(f"requires.{k}.template", (req.get(k) or {}).get("template"), "template")
    elif kind == "capability":
        add("base.capability", (fm.get("base") or {}).get("capability"), "capability")
    elif kind == "design":
        for r in fm.get("applies_to") or []:
            add("applies_to", r)
        for r in fm.get("supersedes") or []:
            add("supersedes", r, "design")
    return out


def context_of(fm):
    """A task's or template's context:, or a trigger's fires.context."""
    if fm["kind"] == "trigger":
        return (fm.get("fires") or {}).get("context") or []
    return fm.get("context") or []


def entry(e):
    """A context: entry, with a bare string read as {ref, mode: read} (§5.6)."""
    return {"ref": e, "mode": "read"} if isinstance(e, str) else {"mode": "read", **e}


def target_type(ref):
    """repo, folder or file, from the reference's form (§5.6)."""
    path = ref.split("#", 1)[0]
    if "://" in path:
        sep = path.find("//", path.index("://") + 3)
        path = path[sep + 2:] if sep >= 0 else ""
    elif re.match(r"^[a-z][a-z0-9-]*:", path):
        path = path.split(":", 1)[1]
    path = path.rsplit("@", 1)[0] if "@" in path else path
    return "repo" if path in ("", "/") else "folder" if path.endswith("/") else "file"


def url_of(repo, src, ref):
    """The repo URL, in-repo path and @ref a reference names, without resolving it."""
    target = ref.split("#", 1)[0]
    m = re.match(r"^([a-z][a-z0-9-]*):(?!//)(.*)$", target)
    if m:
        target = aliases(repo, src)[m.group(1)] + "//" + m.group(2)
    at = ""
    if "://" in target:
        sep = target.find("//", target.index("://") + 3)
        url, path = (target[:sep], target[sep + 2:]) if sep >= 0 else (target, "")
        if "@" in path:
            path, at = path.rsplit("@", 1)
        elif "@" in url.split("://", 1)[1]:
            url, at = url.rsplit("@", 1)
        return url, path.lstrip("/"), at
    if "@" in target:
        target, at = target.rsplit("@", 1)
    url = next(u for u, d in HOSTED.items() if d == repo)
    path = target.lstrip("/") if target.startswith("/") else posixpath.normpath(posixpath.join(posixpath.dirname(src), target))
    return url, path, at


def default_path(url, path, kind, at, write_base):
    """The sandbox layout's default place for an entry (§5.6)."""
    name = url.rstrip("/").rsplit("/", 1)[-1].removesuffix(".git")
    if kind in ("repo", "folder"):
        return f"work/{name}/" if at in ("", write_base.get(url, at)) else f"work/{name}@{at.replace('/', '-')}/"
    host, _, rpath = url.split("://", 1)[1].partition("/")
    return f"context/{host.replace(':', '_')}/{rpath.removesuffix('.git')}/{path}"


LINK = re.compile(r"!?\[[^\]]*\]\(\s*<?([^)\s>]+)>?(?:\s+\"[^\"]*\")?\s*\)|^\[[^\]]+\]:\s*(\S+)", re.M)


def body_links(repo, src, body):
    """The links in a Markdown body that are references (§5.6): relative or
    /-rooted paths, declared aliases, and git URLs with the // separator."""
    out = []
    for m in LINK.finditer(body):
        link = m.group(1) or m.group(2)
        if link.startswith("#"):
            continue
        if "://" in link:
            if link.find("//", link.index("://") + 3) < 0:
                continue  # an ordinary web link
        elif re.match(r"^[a-z][a-z0-9+.-]*:", link) and link.split(":", 1)[0] not in aliases(repo, src):
            continue  # mailto: and the like
        out.append(link)
    return out


def materialise(objs, repo, path, fm):
    """The listed and reached files of a task's context:, as §5.6's resolver
    plans them (the PEP is the examples' own: everything readable here)."""
    plan, skipped, queue, budget = [], [], [], {"files": 0, "bytes": 0}
    for e in map(entry, fm.get("context") or []):
        if target_type(e["ref"]) != "file" or e["mode"] == "use":
            continue
        ident, t, _ = resolve(objs, repo, path, e["ref"])
        if ident is None:
            continue
        plan.append(ident)
        if e.get("depth"):
            queue.append((ident, min(e["depth"], CONTEXT_LIMITS["depth"])))
    reached = []
    while queue:
        (frepo, fpath), d = queue.pop(0)
        if not fpath.endswith(".md"):
            continue
        for link in body_links(frepo, fpath, split(HERE / frepo / fpath)[1]):
            if target_type(link) != "file":
                skipped.append((link, (frepo, fpath), "not a file"))
                continue
            ident, t, _ = resolve(objs, frepo, fpath, link)
            if ident is None:
                skipped.append((link, (frepo, fpath), t))
                continue
            if ident in plan:
                continue
            size = (HERE / ident[0] / ident[1]).stat().st_size
            if (budget["files"] + 1 > CONTEXT_LIMITS["reached_files"] or size > CONTEXT_LIMITS["file_bytes"]
                    or budget["bytes"] + size > CONTEXT_LIMITS["reached_bytes"]):
                skipped.append((link, (frepo, fpath), "limit"))
                continue
            budget["files"] += 1
            budget["bytes"] += size
            plan.append(ident)
            reached.append((ident, (frepo, fpath)))
            if d > 1:
                queue.append((ident, d - 1))
    return reached, skipped


def check_context(objs, repo, path, fm, report):
    """A task's, template's or trigger's context: entries (§5.6)."""
    kind = fm["kind"]
    code = grainconfig(repo).get("host") == "other"
    inputs = {i["name"]: i for i in fm.get("inputs") or []}
    entries = [entry(e) for e in context_of(fm)]
    if len(entries) > CONTEXT_LIMITS["entries"]:
        err(f"{repo}/{path}: more than {CONTEXT_LIMITS['entries']} context entries")
    own = next(u for u, d in HOSTED.items() if d == repo)
    for i in inputs.values():
        if i["type"] == "ref" and i.get("default") and code and url_of(repo, path, i["default"])[0] != own:
            err(f"{repo}/{path}: input {i['name']}: a code repo's ref input may name only its own repo")
    resolved, write_base = [], {}
    for n, e in enumerate(entries, 1):
        if "input" in e:
            i = inputs.get(e["input"])
            if kind != "template":
                err(f"{repo}/{path}: context entry {n}: input: is for templates; a {kind} records the value")
            elif not i or not (i["type"] == "ref" or (i["type"] == "list" and i.get("of") == "ref")):
                err(f"{repo}/{path}: context entry {n}: input {e['input']} is not a declared input of type ref")
            elif e["mode"] == "write" and i.get("ref_kind") not in ("repo", "folder"):
                err(f"{repo}/{path}: context entry {n}: write needs an input with ref_kind repo or folder")
            continue
        what = target_type(e["ref"])
        url, ipath, at = url_of(repo, path, e["ref"])
        if code and url != own:
            err(f"{repo}/{path}: context entry {n}: a code repo's grain folder may name only its own repo (§12.1)")
        if e["mode"] == "write":
            if what == "file":
                err(f"{repo}/{path}: context entry {n}: write is only for a repo or a folder")
            if not at or re.match(r"^[0-9a-f]{40}$", at) or at.startswith("v"):
                err(f"{repo}/{path}: context entry {n}: write needs @<branch>, its base")
            if url in write_base and write_base[url] != at:
                err(f"{repo}/{path}: context entry {n}: two write bases for {url}")
            write_base[url] = at
        if e.get("depth") and what != "file":
            err(f"{repo}/{path}: context entry {n}: depth is for files only")
        if e.get("reducers") and e["mode"] != "use":
            err(f"{repo}/{path}: context entry {n}: reducers are for use entries only")
        if e["mode"] == "use":
            if what != "file" or e.get("depth") or e.get("path"):
                err(f"{repo}/{path}: context entry {n}: use names one capability object, with no depth or path")
            continue  # a request: nothing of the file lands (§5.7)
        resolved.append((e, what, url, ipath, at))
    if kind != "task":
        return
    paths = {}
    for e, what, url, ipath, at in resolved:
        p = e.get("path") or default_path(url, ipath, what, at, write_base)
        if p in ("task.md", "run.md") or p.startswith("thread/") or ".." in p.split("/"):
            err(f"{repo}/{path}: context path {p}: reserved or escapes the sandbox")
        key = (url, at) if what != "file" else (url, at, ipath)
        for q, k in paths.items():
            if k != key and (p == q or (q.endswith("/") and p.startswith(q) and what != "file")
                             or (p.endswith("/") and q.startswith(p) and len(k) == 2)):
                err(f"{repo}/{path}: context paths {q} and {p} overlap")
        paths.setdefault(p, key)
    reached, skipped = materialise(objs, repo, path, fm)
    report.append(f"sandbox of {repo}/{path}:")
    for e, what, url, ipath, at in resolved:
        p = e.get("path") or default_path(url, ipath, what, at, write_base)
        report.append(f"  {p} <- {e['ref']} ({what}, {e['mode']})")
    for ident, via in reached:
        url = next(u for u, d in HOSTED.items() if d == ident[0])
        report.append(f"  {default_path(url, ident[1], 'file', '', {})} <- {short(ident)} (via {short(via)}, depth)")
    for link, via, why in skipped:
        report.append(f"  skipped {link} (via {short(via)}): {why}")
    return reached


def seconds(v):
    """A limit's max: a number of seconds, or a duration such as 30m or 1h."""
    if isinstance(v, int):
        return v
    m = re.match(r"^(\d+)([smhd])$", v)
    return int(m.group(1)) * {"s": 1, "m": 60, "h": 3600, "d": 86400}[m.group(2)]


def narrow_set(current, allow):
    """subset: keep what both the current set and allow admit (globs either side)."""
    import fnmatch
    out = [v for v in current if any(fnmatch.fnmatchcase(v, a) for a in allow)]
    out += [a for a in allow if a not in out and any(fnmatch.fnmatchcase(a, c) for c in current)]
    return out


def bases_of(objs, grain):
    """The base declarations a Grain publishes in its root (§5.7)."""
    return {fm["type"]: ((r, p), fm) for (r, p), (fm, _) in objs.items()
            if fm["kind"] == "capability-base" and fm["grain"] == grain}


def source_ok(ident, base_type, policy):
    url = next(u for u, d in HOSTED.items() if d == ident[0])
    for s in policy["sources"]:
        sep = s["source"].find("//", s["source"].index("://") + 3)
        surl, spath = s["source"][:sep], s["source"][sep + 2:]
        if url == surl and ident[1].startswith(spath) and (s["bases"] == "*" or base_type in s["bases"]):
            return True
    return False


def chain_of(objs, repo, src, ref):
    """Follow a capability's base references to its base type (§5.7): the
    chain base-most first, or (None, reason)."""
    chain, seen = [], []
    while True:
        ident, fm, _ = resolve(objs, repo, src, ref)
        if ident is None:
            return None, f"{ref}: {fm}"
        if fm is None or fm.get("kind") != "capability":
            return None, f"{ref}: not a capability"
        if ident in seen:
            return None, "capability cycle: " + " -> ".join(short(i) for i in seen + [ident])
        seen.append(ident)
        chain.insert(0, (ident, fm))
        if len(chain) > CAPABILITY_DEPTH:
            return None, f"a chain longer than {CAPABILITY_DEPTH}"
        if "type" in fm["base"]:
            return chain, fm["base"]["type"]
        repo, src, ref = ident[0], ident[1], fm["base"]["capability"]


def effective(objs, grain, chain, base_type, extra):
    """Apply every reducer, base-most first, to the Grain's declared maximum.
    Returns (dimensions, implies, pending, refusal)."""
    policy = POLICY[grain]
    if base_type not in policy["bases"]:
        return None, None, [], f"base {base_type} is not in {grain}'s policy"
    decl = bases_of(objs, grain).get(base_type)
    if not decl:
        return None, None, [], f"{grain} does not implement {base_type}"
    for ident, fm in chain:
        if not source_ok(ident, base_type, policy):
            return None, None, [], f"{short(ident)}: not a sanctioned source for {base_type}"
    dims = {k: (list(v["maximum"]) if v["kind"] == "set" else v["maximum"]) for k, v in decl[1]["dimensions"].items()}
    pending = []
    steps = [(short(i), n, r) for i, fm in chain for n, r in enumerate(fm.get("reducers") or [], 1)]
    steps += [("the task's entry", n, r) for n, r in enumerate(extra, 1)]
    for where, n, r in steps:
        if r["language"] == "anselm":
            if policy["anselm"] is None:
                pending.append(f"{where} reducer {n} ({r.get('id', 'anselm')}, on {r['on']})")
            continue
        if r["type"] in ("subset", "limit"):
            d = decl[1]["dimensions"].get(r["dimension"])
            if d is None:
                return None, None, [], f"{where} reducer {n}: {base_type} has no dimension {r['dimension']}"
            if r.get("on") and r["on"] not in d["narrow"]:
                return None, None, [], f"{where} reducer {n}: {r['dimension']} cannot be narrowed on {r['on']}"
            if r["type"] == "subset":
                dims[r["dimension"]] = narrow_set(dims[r["dimension"]], r["allow"])
                if not dims[r["dimension"]] and d.get("required"):
                    return None, None, [], f"{where} reducer {n}: {r['dimension']} is empty"
            else:
                dims[r["dimension"]] = min(dims[r["dimension"]], seconds(r["max"]))
        elif r["type"] == "not_after":
            dims["not_after"] = min(dims.get("not_after", r["at"]), r["at"])
    if "accounts" in dims and "projects" in dims:
        # Provider logic, not a reducer: the GCP base mints only for an
        # account in a remaining project (its declaration's note).
        dims["accounts"] = [a for a in dims["accounts"]
                            if any(a.endswith(f"@{p}.iam.gserviceaccount.com") for p in dims["projects"])]
        if not dims["accounts"]:
            return None, None, [], "no account qualifies"
    implies = {}
    for k, v in decl[1]["implies"].items():
        implies[k] = dims.get(v["from"], []) + v.get("plus", []) if isinstance(v, dict) else v
    import fnmatch
    for host in implies.get("egress", []):
        if not any(fnmatch.fnmatchcase(host, a) for a in policy["egress"]):
            return None, None, [], f"implies egress to {host}, not in {grain}'s network policy"
    for r in implies.get("repos", []):
        if not any(fnmatch.fnmatchcase(r, a) for a in policy["repos"]):
            return None, None, [], f"implies repo {r}, not in {grain}'s repos policy"
    return dims, implies, pending, None


def section_hashes(text):
    body = text.split("\n---\n", 1)[1]
    lines = body.split("\n")
    heads = [(i, len(re.match(r"^(#+)", l).group(1))) for i, l in enumerate(lines) if re.match(r"^#+\s", l)]
    out = {}
    for i, lvl in heads:
        m = re.search(r"\{#([A-Z][A-Z0-9]*-\d+)\}\s*$", lines[i])
        if not m:
            continue
        end = next((j for j, l2 in heads if j > i and l2 <= lvl), len(lines))
        sec = "\n".join(lines[i:end])
        sec = re.sub(r"\s*\{#[A-Z][A-Z0-9]*-\d+\}", "", sec)
        sec = "\n".join(l.rstrip() for l in sec.split("\n"))
        sec = re.sub(r"\n{2,}", "\n\n", sec).strip() + "\n"
        out[m.group(1)] = "sha256:" + hashlib.sha256(sec.encode()).hexdigest()
    return out


def glob_re(g):
    """A glob: * and ? within one path segment, ** across segments (a
    leading or inner **/ also matches no folders at all)."""
    out, i = "", 0
    while i < len(g):
        if g.startswith("**/", i):
            out, i = out + "(?:.*/)?", i + 3
        elif g.startswith("**", i):
            out, i = out + ".*", i + 2
        elif g[i] == "*":
            out, i = out + "[^/]*", i + 1
        elif g[i] == "?":
            out, i = out + "[^/]", i + 1
        else:
            out, i = out + re.escape(g[i]), i + 1
    return re.compile("^" + out + "$")


def ignored(rel, patterns):
    """gitignore semantics for one entry's ignore: (§3.3): the last matching
    pattern wins, ! negates, a trailing / matches folders, a pattern with no
    inner / matches at any depth, and a matched folder prunes its subtree.
    rel is relative to the entry's folder."""
    parts = rel.split("/")
    out = False
    for pat in patterns:
        neg = pat.startswith("!")
        pat = pat[1:] if neg else pat
        dir_only = pat.endswith("/")
        core = pat.strip("/")
        anchored = pat.startswith("/") or "/" in core
        rx = glob_re(core)
        hit = False
        # every prefix that is a folder, then the file itself
        for n in range(1, len(parts) + 1):
            cand = "/".join(parts[:n]) if anchored else parts[n - 1]
            is_dir = n < len(parts)
            if (is_dir or not dir_only) and rx.match(cand):
                hit = True
                break
        if hit:
            out = not neg
    return out


def configs():
    """Every grain.config.md in the examples: (repo, folder, front matter)."""
    out = []
    for repo in REPOS:
        for p in sorted((HERE / repo).rglob("grain.config.md")):
            fm, _ = split(p)
            out.append((repo, posixpath.dirname(str(p.relative_to(HERE / repo))), fm))
    return out


def list_entry(objs, repo, cfg_path, e, kind, limit):
    """The objects one root entry activates (§3.3, §3.7), from the tree alone:
    files under its folder with the kind's suffix, less its ignore: and any
    archive/ folder. Returns (identities, None) or (None, why): an entry
    over its limit is unknown, never truncated."""
    loc = canonical(repo, cfg_path, e["path"])
    if loc[0] is None:
        return None, loc[1]
    trepo, folder = loc
    base = HERE / trepo / folder
    if base.is_file():
        rel = [folder] if kind_of(folder) == kind else []
        return [(trepo, r) for r in rel], None
    if not base.is_dir():
        return [], None  # a folder not created yet lists empty
    names = []
    for f in sorted(base.rglob("*")):
        if not f.is_file():
            continue
        rel = str(f.relative_to(base))
        if "archive" in rel.split("/")[:-1]:
            continue
        if kind_of(rel) != kind or ignored(rel, e.get("ignore") or []):
            continue
        names.append((trepo, posixpath.normpath(posixpath.join(folder, rel))))
        if len(names) > limit:
            return None, f"over objects_per_entry ({limit})"
    return names, None


def active_set(objs, repo, home, fm, limits=None):
    """A grain.config.md's active set: {type: [(entry, identities or None, why)]},
    each file belonging to the most specific entry of its type."""
    limits = {**LIMITS, **(fm.get("limits") or {}), **(limits or {})}
    cfg_path = posixpath.join(home, "grain.config.md") if home else "grain.config.md"
    out, total = {}, 0
    for rtype, kind in ROOT_TYPES.items():
        raw = fm.get(rtype) or []
        entries = raw.get("lanes", []) if isinstance(raw, dict) else raw
        listed = [(e, *list_entry(objs, repo, cfg_path, e, kind, limits["objects_per_entry"])) for e in entries]
        spec = {}
        for n, (e, ids, why) in enumerate(listed):
            loc = canonical(repo, cfg_path, e["path"])
            for i in ids or []:
                if i not in spec or len(loc[1]) > spec[i][1]:
                    spec[i] = (n, len(loc[1]))
        out[rtype] = [(e, None if ids is None else [i for i in ids if spec[i][0] == n], why)
                      for n, (e, ids, why) in enumerate(listed)]
        total += sum(len(ids or []) for _, ids, _ in out[rtype])
    if total > limits["objects"]:
        return None, f"over objects ({limits['objects']})"
    return out, None


def owners_files(repo):
    """Every GRAINOWNERS in a repo: {folder: [(regex, fields or None, owners)]}."""
    out = {}
    for p in sorted((HERE / repo).rglob("GRAINOWNERS")):
        home = posixpath.dirname(str(p.relative_to(HERE / repo)))
        rules = []
        for line in p.read_text().splitlines():
            line = re.sub(r"\s+#.*$", "", line).strip()
            if not line or line.startswith("#"):
                continue
            pat, *who = line.split()
            fields = None
            if ":" in pat:
                pat, f = pat.rsplit(":", 1)
                fields = f.split(",")
            anchored = pat.startswith("/") or "/" in pat.rstrip("/")
            core = pat.lstrip("/")
            if core.endswith("/"):
                core += "**"
            rx = glob_re(core if anchored or core == "*" else "**/" + core)
            if core == "*":
                rx = re.compile("^.*$")
            rules.append((rx, fields, who))
        out[home] = rules
    return out


def owners_of(repo, path, field=None):
    """Who may write a file, or one field of it (§3.5): the nearest
    GRAINOWNERS with a matching line; in it, the last matching line,
    a field line overriding the whole-file owners for its fields."""
    files = owners_files(repo)
    d = posixpath.dirname(path)
    while True:
        rules = files.get(d)
        if rules is not None:
            rel = path[len(d) + 1:] if d else path
            whole = next((w for rx, f, w in reversed(rules) if f is None and rx.match(rel)), None)
            if field:
                fl = next((w for rx, f, w in reversed(rules) if f and field in f and rx.match(rel)), None)
                if fl is not None:
                    return fl
            if whole is not None:
                return whole
        if not d:
            return None
        d = posixpath.dirname(d)


def may(owners, who, verb):
    for o in owners or []:
        m = re.match(r"^@?([^()]+)(?:\(([^)]*)\))?$", o)
        name, verbs = m.group(1), m.group(2)
        if name == who or (name == "authenticated" and who.startswith("user:")):
            if verbs is None or verb in verbs.split(","):
                return True
    return False


def picks(lanes, conf):
    """The order a Grain takes runnable references across its lanes (§5.2):
    ranks are compared within a lane only, and grain.config.md's backlog
    decides between lanes, in list order. lanes is [(weight, [entries])]."""
    queues = {n: list(es) for n, (w, es) in enumerate(lanes) if es}
    weight = {n: (1 if conf.get("mode") == "round_robin" else lanes[n][0]) for n in queues}
    out, credit = [], {n: 0 for n in queues}
    while any(queues.values()):
        live = sorted(n for n in queues if queues[n])
        if conf.get("mode", "strict") == "strict":
            lane = live[0]
        else:
            for l in live:
                credit[l] += weight[l]
            lane = max(live, key=lambda l: (credit[l], -l))
            credit[lane] -= sum(weight[l] for l in live)
        out.append(queues[lane].pop(0))
    return out


def main():
    store, schemas = load_schemas()
    objs = objects()
    report = []

    # Schemas.
    for (repo, path), (fm, body) in objs.items():
        kind = fm["kind"]
        if kind not in schemas:
            err(f"{repo}/{path}: unknown kind {kind}")
            continue
        resolver = jsonschema.RefResolver.from_schema(schemas[kind], store=store)
        v = jsonschema.Draft202012Validator(schemas[kind], resolver=resolver)
        for e in v.iter_errors(json.loads(json.dumps(fm, default=str))):
            err(f"{repo}/{path}: {'/'.join(map(str, e.path))}: {e.message}")

    for repo in REPOS:
        cfg = HERE / repo / ".grainconfig"
        if cfg.is_file():
            v = jsonschema.Draft202012Validator(schemas["grainconfig"])
            for e in v.iter_errors(yaml.safe_load(cfg.read_text())):
                err(f"{repo}/.grainconfig: {'/'.join(map(str, e.path))}: {e.message}")
    bad = suffix_problem("backlog/2026-10-17-fix-dns.task.md", {"kind": "ref"})
    if not bad or "kind task" not in bad:
        err("a suffix mismatch was not detected")
    report.append(f"suffix mismatch (§8.15): backlog/2026-10-17-fix-dns.task.md: {bad}")

    # References: the path is the identity; tombstones redirect (§2.1, §4).
    for (repo, path), (fm, body) in objs.items():
        for field, ref, want, pinned in edges(fm):
            ident, target, hops = resolve(objs, repo, path, ref)
            if ident is None:
                if target == "external" or (pinned and target == "dangling"):
                    continue
                err(f"{repo}/{path}: {field} {ref}: {target}")
                continue
            if target is None:
                continue
            if want and target["kind"] != want:
                err(f"{repo}/{path}: {field} {ref}: is a {target['kind']}, not a {want}")
            if hops:
                report.append(f"redirect: {repo}/{path} {field} {ref} -> {short(ident)} ({hops} hop)")
            anchor = ref.split("#", 1)[1] if "#" in ref else None
            if anchor and re.match(r"^[A-Z][A-Z0-9]*-\d+$", anchor):
                text = (HERE / ident[0] / ident[1]).read_text()
                if "{#" + anchor + "}" not in text:
                    err(f"{repo}/{path}: {field} {ref}: no requirement {anchor}")
                elif field == "implements":
                    imp = next(r for r in fm["implements"] if r["ref"] == ref)
                    if section_hashes(text).get(anchor) != imp["hash"]:
                        err(f"{repo}/{path}: implements {anchor}: hash differs (changed since implemented)")

    # Tombstones and moved_from agree, both ways (§2.1, §8.1). A tombstone
    # is the old file name plus .moved.md, beside the old path.
    for (repo, path), (fm, body) in objs.items():
        if fm["kind"] == "moved":
            ident, target, _ = resolve(objs, repo, path, fm["to"])
            if ident is None:
                err(f"{repo}/{path}: tombstone to {fm['to']}: {target}")
                continue
            back = [canonical(ident[0], ident[1], r) for r in target.get("moved_from") or []]
            if (repo, old_of(path)) not in back:
                err(f"{repo}/{path}: tombstone target {short(ident)} does not list its old path in moved_from")
        for r in fm.get("moved_from") or []:
            old = canonical(repo, path, r)
            stone = (old[0], stone_of(old[1]))
            if stone not in objs or objs[stone][0]["kind"] != "moved":
                err(f"{repo}/{path}: moved_from {r}: no tombstone beside it")

    # Code repos' grain folders (§5.5, §6.2, §8.13).
    for (repo, path), (fm, body) in objs.items():
        gfm = grainconfig(repo)
        if gfm.get("host") != "other":
            continue
        if fm["kind"] == "task":
            th = gfm.get("task_home") or "intake"
            folder = th.get("path", ".grain/tasks/") if isinstance(th, dict) else ".grain/tasks/"
            if not path.startswith(folder):
                err(f"{repo}/{path}: a task outside {folder}, the folder task_home names")
            if fm.get("trigger") or fm.get("schedule"):
                err(f"{repo}/{path}: a trigger's firing in a code repo; firings live in an intake or a Grain root")
            homes = [canonical(repo, path, r)[0] for r in fm.get("moved_from") or []]
            if not any(h and "team" in (split(HERE / h / "grain.config.md")[0] or {}) for h in homes if (HERE / str(h) / "grain.config.md").is_file()):
                err(f"{repo}/{path}: a task in a code repo with no intake in moved_from; Silo will not place it")
        if fm["kind"] == "template":
            for k in ("reads", "writes", "grants", "target"):
                if k in fm:
                    err(f"{repo}/{path}: a code repo's template may not carry {k}")
        if fm["kind"] == "gate" and (fm.get("match") or {}).get("repos"):
            err(f"{repo}/{path}: a gate in a code repo governs that repo only; match.repos is for shared gate sets")
        wf = ((fm.get("fires") or {}).get("workflow") if fm["kind"] == "trigger" else None)
        if wf and not (HERE / repo / wf.lstrip("/")).is_file():
            err(f"{repo}/{path}: fires.workflow {wf} is not a file in this repo")

    # context: entries, the sandbox layout and depth (§5.6); sidecars (§2.2).
    reached = {}
    for (repo, path), (fm, body) in objs.items():
        if fm["kind"] in ("task", "template", "trigger") and context_of(fm):
            reached[(repo, path)] = check_context(objs, repo, path, fm, report)
        if fm["kind"] == "asset" and fm.get("asset"):
            ident, t, _ = resolve(objs, repo, path, fm["asset"])
            if ident and t["kind"] == ASSET:
                data = (HERE / ident[0] / ident[1]).read_bytes()
                if "sha256" in fm and hashlib.sha256(data).hexdigest() != fm["sha256"]:
                    err(f"{repo}/{path}: sha256 does not match {short(ident)}")
                if "bytes" in fm and len(data) != fm["bytes"]:
                    err(f"{repo}/{path}: bytes does not match {short(ident)}")
    want = {("hub-shared", "designs/img/claims-flow.png")}
    got = {i for i, via in reached.get(("hub-intake", "tasks/2026-10-16-reject-dangling-refs.task.md")) or []}
    if got != want:
        err(f"depth: 1 reached {sorted(got)}, expected {sorted(want)}")

    # Execution records, by task (§5.3).
    runs = {}
    for (repo, path), (fm, body) in objs.items():
        if fm["kind"] == "run":
            ident, task, _ = resolve(objs, repo, path, fm["task"]["ref"])
            if ident:
                runs.setdefault(ident, []).append(((repo, path), fm))
    for task, rs in runs.items():
        live = [r for r in rs if r[1]["state"] in OPEN_RUN]
        if len(live) > 1:
            err(f"{short(task)}: {len(live)} open runs: " + ", ".join(short(r[0]) for r in live))
        for (repo, path), fm in rs:
            if fm.get("previous"):
                prev, pfm, _ = resolve(objs, repo, path, fm["previous"])
                if prev and pfm["state"] != "superseded":
                    err(f"{repo}/{path}: previous {short(prev)} is {pfm['state']}, not superseded")

    def done(task):
        return any(r[1]["state"] == "done" for r in runs.get(task, []))

    def order_key(e):
        ref, task, fm, tfm = e
        ready = all(done(canonical(task[0], task[1], d)) for d in tfm.get("depends_on") or [])
        return (not ready, not fm.get("expedite", False), PRIORITY.index(fm.get("priority", "normal")),
                fm["rank"], str(tfm["created_at"]), ref[1])

    # Backlogs: folders of ranked references, one lane each (§5.2).
    entries = {}
    for (repo, path), (fm, body) in objs.items():
        if fm["kind"] != "ref":
            continue
        ident, task, _ = resolve(objs, repo, path, fm["to"])
        if ident:
            entries.setdefault((repo, posixpath.dirname(path)), []).append(((repo, path), ident, fm, task))
    for lane, es in entries.items():
        seen = {}
        for ref, task, fm, tfm in es:
            if task in seen:
                err(f"{short(ref)} and {short(seen[task])} both reference {short(task)} in one lane")
            seen[task] = ref
            if tfm["state"] in ("cancelled", "rejected"):
                err(f"{short(ref)}: references a {tfm['state']} task; its writer removes it")
            if any(r[1]["state"] in CLOSED_RUN for r in runs.get(task, [])):
                err(f"{short(ref)}: references a closed task; its writer removes it")

        es.sort(key=order_key)

    # Active sets: each grain.config.md's typed root entries (§3.3, §3.7).
    spaces, team_triggers = {}, {}
    for repo, home, fm in configs():
        who = (fm.get("grain") or {}).get("id") or "team:" + str(fm.get("team"))
        act, why = active_set(objs, repo, home, fm)
        if act is None:
            err(f"{repo}/{home or '.'}grain.config.md: {why}")
            continue
        for rtype, es in act.items():
            for e, ids, why in es:
                cross = "://" in e["path"] or re.match(r"^[a-z][a-z0-9-]*:", e["path"])
                if cross and ("@" not in e["path"] or "ref_mode" not in e):
                    err(f"{repo}/grain.config.md: {rtype} entry {e['path']}: another repo needs @<ref> and ref_mode")
                if not cross and ("@" in e["path"] or "ref_mode" in e):
                    err(f"{repo}/grain.config.md: {rtype} entry {e['path']}: a relative entry reads this commit, and takes no @<ref>")
                if ids is None:
                    err(f"{repo}/grain.config.md: {rtype} entry {e['path']}: unknown: {why}")
        writable = [e for e, _, _ in act["memory"] if e.get("write")]
        if len(writable) > 1 or any("@" in e["path"] for e in writable):
            err(f"{repo}/grain.config.md: at most one write: true memory entry, in this repo")
        spaces[who] = (repo, home, fm, act)
        names = {i for es in act.values() for _, ids, _ in es for i in ids or []}
        report.append(f"active set {who} ({repo}/{home or '.'}): " + ", ".join(
            f"{len([i for _, ids, _ in act[t] for i in ids or []])} {t}" for t in ROOT_TYPES))
        for i in names:
            if objs[i][0]["kind"] in ("gate", "template", "capability", "task", "design"):
                err(f"active set {who}: {short(i)} is a {objs[i][0]['kind']}, which no root lists")

    def active(who, rtype):
        return [i for _, ids, _ in spaces[who][3][rtype] for i in ids or []]

    def queued_on(task):
        return [g for g in spaces if any(objs[r][0]["to"] and resolve(objs, r[0], r[1], objs[r][0]["to"])[0] == task
                                         for r in active(g, "backlog"))]

    def status(task):
        """The derived status of §5.1."""
        fm = objs[task][0]
        rs = runs.get(task, [])
        open_ = [r for r in rs if r[1]["state"] in OPEN_RUN]
        if open_:
            return f"{open_[0][1]['state']} on {open_[0][1]['grain']}"
        if fm["state"] == "cancelled":
            return "cancelled"
        latest = [r for r in rs if r[1]["state"] != "superseded"]
        if latest:
            return f"{latest[-1][1]['state']} on {latest[-1][1]['grain']}"
        queued = queued_on(task)
        if queued:
            return "queued on " + ", ".join(queued)
        return "unplaced" if fm["state"] == "approved" else fm["state"]

    for lane, es in sorted(entries.items()):
        report.append(f"lane {lane[0]}/{lane[1]}/:")
        for ref, task, fm, tfm in es:
            flags = [f"rank {fm['rank']}"]
            if fm.get("priority"):
                flags.append(fm["priority"])
            if fm.get("expedite"):
                flags.append("expedite")
            report.append(f"  {posixpath.basename(ref[1])} -> {short(task)} [{', '.join(flags)}] ({status(task)})")

    # Each Grain picks across its lanes; a ref is runnable if its task has
    # no open or closed run, and is not cancelled.
    by_ref = {e[0]: e for es in entries.values() for e in es}
    for who, (repo, home, fm, act) in sorted(spaces.items()):
        if not act["backlog"]:
            continue
        conf = fm.get("backlog") or {}
        lanes = []
        for e, ids, _ in act["backlog"]:
            es = sorted((by_ref[i] for i in ids or [] if i in by_ref), key=lambda x: order_key(x))
            lanes.append((e.get("weight", 1), [x for x in es if not runs.get(x[1]) and x[3]["state"] == "approved"]))
        order = picks(lanes, conf)
        report.append(f"next on {who} ({conf.get('mode', 'strict')}): " +
                      ", ".join(posixpath.basename(x[1][1].removesuffix(".task.md")) for x in order))

    # What the examples say each active set holds, by root type.
    expect = [
        ("eu", "backlog", "hub-placements", "grains/eu/backlog/2026-10-16-approval-flow.ref.md", True),
        ("eu", "backlog", "hub-placements", "grains/us/backlog/2026-10-16-refuse-self-approved.ref.md", False),
        ("eu", "backlog", "grain-eu", "backlog/2026-10-16-tidy-run-notes.ref.md", True),
        ("eu", "triggers", "hub-shared", "triggers/granule-pull.trigger.md", True),       # a team trigger root
        ("eu", "triggers", "hub-shared", "triggers/plan-designs.trigger.md", True),       # listed; only Silo fires it
        ("eu", "triggers", "hub-shared", "triggers/archive/legacy-triage-sweep.trigger.md", False),  # retired
        ("eu", "triggers", "grain-eu", "triggers/monthly-tidy.trigger.md", True),
        ("eu", "memory", "grain-eu", "memory/deploy-notes.memory.md", True),
        ("eu", "memory", "hub-shared", "memory/merge-style.memory.md", True),
        ("us", "triggers", "hub-placements", "grains/us/triggers/granule-pull.trigger.md", False),  # its entry's ignore:
        ("us", "triggers", "grain-us", "triggers/granule-pull-weekly.trigger.md", True),
        ("us", "triggers", "hub-shared", "triggers/granule-pull.trigger.md", False),      # us names no team folder
        ("us", "triggers", "widgets-code", ".grain/triggers/nightly-ci.trigger.md", True),  # one file named
        ("us", "backlog", "hub-placements", "grains/us/backlog/2026-10-16-refuse-self-approved.ref.md", True),
        ("team:hub", "triggers", "widgets-code", ".grain/triggers/weekly-release-notes.trigger.md", True),
        ("team:hub", "triggers", "widgets-code", ".grain/triggers/nightly-ci.trigger.md", False),
    ]
    for who, rtype, repo, path, want in expect:
        if ((repo, path) in active(who, rtype)) != want:
            err(f"active set {who} {rtype}: {repo}/{path} {'missing' if want else 'present'}")
    mem = active("eu", "memory")
    if mem.index(("grain-eu", "memory/deploy-notes.memory.md")) > mem.index(("hub-shared", "memory/merge-style.memory.md")):
        err("eu's memory: its own memory must take precedence over the team's")
    report.append("memory on eu, in precedence order: " + ", ".join(short(i) for i in mem))
    # ignore: semantics, and limits that fail loudly.
    for rel, pats, want in [("granule-pull.trigger.md", ["granule-pull.trigger.md"], True),
                            ("x/granule-pull.trigger.md", ["granule-pull.trigger.md"], True),
                            ("x/granule-pull.trigger.md", ["/granule-pull.trigger.md"], False),
                            ("drafts/a.ref.md", ["drafts/"], True),
                            ("a.trigger.md", ["*.trigger.md", "!a.trigger.md"], False),
                            ("drafts", ["drafts/"], False)]:
        if ignored(rel, pats) != want:
            err(f"ignore {pats} on {rel}: expected {want}")
    small, why = active_set(objs, "grain-eu", "", spaces["eu"][2], {"objects_per_entry": 1})
    if any(ids is not None for e, ids, _ in small["triggers"] if "shared:" in e["path"]):
        err("an entry over objects_per_entry was truncated, not failed")
    else:
        report.append("limits: eu's team trigger entry at objects_per_entry 1 is unknown (" +
                      next(w for e, ids, w in small["triggers"] if "shared:" in e["path"]) + "), never truncated")

    # GRAINOWNERS (§3.5): every object has an owner; and the design's examples.
    for (repo, path), (fm, body) in objs.items():
        if owners_of(repo, path) is None:
            err(f"{repo}/{path}: no GRAINOWNERS line owns it")
    for repo, path, field, who, verb, want in [
            ("grain-eu", "backlog/urgent-thing.ref.md", None, "user:alex@example.com", "create", False),  # §8.15
            ("grain-eu", "grain.config.md", None, "silo", "update", False),
            ("grain-eu", "grain.config.md", None, "grain:eu", "update", False),
            ("grain-eu", "grain.config.md", None, "user:bsalmon@google.com", "update", True),
            ("grain-eu", "runs/4310-carry-approver/20261016T135830Z.run.md", None, "grain:eu", "update", True),
            ("hub-placements", "grains/eu/backlog/x.ref.md", None, "silo", "create", True),
            ("hub-intake", "tasks/2026-10-16-approval-by-push.task.md", "approved_by", "user:mallory@example.com", "update", False),
            ("hub-intake", "tasks/2026-10-16-approval-by-push.task.md", "approved_by", "team:hub#approver", "update", True),
            ("hub-intake", "tasks/2026-10-18-new.task.md", None, "user:alex@example.com", "create", True),
            ("hub-intake", "tasks/2026-10-16-approval-by-push.task.md", "body", "user:alex@example.com", "update", False),
            ("hub-intake", "backlog/2026-10-16-approval-flow.ref.md", "rank", "team:hub#member", "update", True)]:
        got = may(owners_of(repo, path, field), who, verb)
        if got != want:
            err(f"GRAINOWNERS {repo}/{path}{':' + field if field else ''}: {who} {verb}: got {got}, expected {want}")
    report.append("GRAINOWNERS: grain-eu/backlog/urgent-thing.ref.md: user:alex@example.com may not create it; owners "
                  + " ".join(owners_of("grain-eu", "backlog/urgent-thing.ref.md")))

    # Orphans (§3.9): objects under no known root entry and reached by no
    # known reference. Warnings, unless a repo's .grainconfig says refuse.
    known, todo = set(), []
    for who, (repo, home, fm, act) in spaces.items():
        todo += [i for es in act.values() for _, ids, _ in es for i in ids or []]
        todo.append((repo, posixpath.join(home, "grain.config.md") if home else "grain.config.md"))
        if fm.get("grain"):
            todo += [i for i in objs if i[0] == repo and i[1].startswith(posixpath.join(home, "runs/") if home else "runs/")]
    for (repo, path), (fm, body) in objs.items():
        gf = grainconfig(repo).get("grain_folder", ".grain/")
        if fm["kind"] == "gate" and path.startswith(gf):
            todo.append((repo, path))
        if fm["kind"] in ("ref", "task"):  # an intake's tasks, and its ranking of its unplaced work
            if (HERE / repo / "grain.config.md").is_file() and "team" in split(HERE / repo / "grain.config.md")[0]:
                todo.append((repo, path))
    while todo:
        i = todo.pop()
        if i in known or i not in objs:
            continue
        known.add(i)
        fm = objs[i][0]
        for field, ref, want, pinned in edges(fm):
            ident, t, _ = resolve(objs, i[0], i[1], ref)
            if ident and t is not None:
                todo.append(ident)
            stone = canonical(i[0], i[1], ref)
            if stone[0] and (stone[0], stone_of(stone[1])) in objs:
                todo.append((stone[0], stone_of(stone[1])))
        if fm["kind"] == "template":  # its {% include %} and {% import %} partials
            for r in re.findall(r"\{%-?\s*(?:include|import)\s+\"([^\"]+)\"", objs[i][1]):
                todo.append(canonical(i[0], i[1], r))
        if fm["kind"] in ("task", "run", "moved"):  # its thread, beside it
            folder = (i[1].removesuffix(".task.md") if fm["kind"] == "task" else
                      old_of(i[1]).removesuffix(".task.md") if fm["kind"] == "moved" else posixpath.dirname(i[1])) + "/"
            todo += [j for j in objs if j[0] == i[0] and j[1].startswith(folder) and objs[j][0]["kind"] == "comment"]
        if fm["kind"] == "trigger":
            for d in (((fm.get("on") or {}).get("design-change") or {}).get("designs") or []):
                c = canonical(i[0], i[1], d)
                todo += [j for j in objs if c[0] == j[0] and j[1].startswith(c[1]) and objs[j][0]["kind"] == "design"]
        if fm["kind"] == "moved" or fm.get("moved_from"):
            for r in fm.get("moved_from") or []:
                c = canonical(i[0], i[1], r)
                todo.append((c[0], stone_of(c[1])))
    for (repo, path), (fm, body) in sorted(objs.items()):
        if (repo, path) in known or "archive" in path.split("/") or fm["kind"] in ("capability-base", "asset"):
            continue
        if grainconfig(repo).get("orphans") == "refuse":
            err(f"orphan: {repo}/{path}")
        else:
            report.append(f"orphan (warning): {repo}/{path}")

    # Capabilities (§5.7): every chain resolves to a base type, and each
    # request is checked against the Grain's base declaration and policy.
    for (repo, path), (fm, body) in objs.items():
        if fm["kind"] == "capability":
            chain, why = chain_of(objs, repo, path, "/" + path)
            if chain is None:
                err(f"{repo}/{path}: {why}")
        if fm["kind"] == "capability-base":
            for k, v in (fm.get("implies") or {}).items():
                if isinstance(v, dict) and v["from"] not in fm["dimensions"]:
                    err(f"{repo}/{path}: implies {k} from {v['from']}, which is not a dimension")
    loop = dict(objs)
    for a, b in (("a", "b"), ("b", "a")):
        loop[("hub-shared", f"capabilities/{a}.capability.md")] = ({"kind": "capability", "base": {"capability": f"{b}.capability.md"}}, "")
    if "capability cycle" not in (chain_of(loop, "hub-shared", "x.md", "capabilities/a.capability.md")[1] or ""):
        err("a capability cycle was not detected")
    got_caps = {}
    for (repo, path), (fm, body) in sorted(objs.items()):
        if fm["kind"] != "task":
            continue
        for n, e in enumerate(map(entry, fm.get("context") or []), 1):
            if e["mode"] != "use":
                continue
            chain, base_type = chain_of(objs, repo, path, e["ref"])
            if chain is None:
                err(f"{repo}/{path}: context entry {n}: {base_type}")
                continue
            for grain in sorted(POLICY):
                dims, implies, pending, refusal = effective(objs, grain, chain, base_type, e.get("reducers") or [])
                head = f"capability {repo}/{path} entry {n} on {grain}: "
                report.append(head + " -> ".join(short(i) for i, _ in chain) + f" over {base_type}")
                if refusal:
                    report.append(f"  refused: {refusal}")
                    got_caps[(repo, path, n, grain)] = ("refused", refusal)
                    continue
                report.append("  effective: " + ", ".join(f"{k}={v}" for k, v in dims.items()))
                report.append("  implies: " + ", ".join(f"{k}={v}" for k, v in implies.items()))
                for p in pending:
                    report.append(f"  not evaluable here: {p}; {grain} refuses the request until it can evaluate Anselm")
                got_caps[(repo, path, n, grain)] = (dims, pending)
    want = got_caps.get(("hub-intake", "tasks/2026-10-17-widgets-preview-deploy.task.md", 2, "eu"))
    if not want or want[0] == "refused" or want[0]["projects"] != ["acme-widgets"] \
            or want[0]["roles"] != ["roles/run.developer", "roles/iam.serviceAccountUser"] \
            or want[0]["token_lifetime"] != 1800 or len(want[1]) != 1:
        err(f"widgets-preview-deploy's capability on eu: {want}, expected acme-widgets, two roles, 1800s and one Anselm reducer pending")

    report.append("tasks:")
    for ident, (fm, body) in sorted(objs.items()):
        if fm["kind"] == "task":
            report.append(f"  {short(ident)}: {status(ident)}")

    # Templates render under Python Jinja2, StrictUndefined (§6.1).
    class Loader(jinja2.BaseLoader):
        def get_source(self, env, name):
            fm, body = split(HERE / "hub-shared" / "templates" / name)  # name: <name>.template.md
            return body.lstrip("\n"), name, lambda: True
    env = jinja2.Environment(loader=Loader(), undefined=jinja2.StrictUndefined,
                             keep_trailing_newline=True, autoescape=False)
    for fx in sorted((HERE / "fixtures").glob("*.inputs.json")):
        fname = fx.name.replace(".inputs.json", "")
        got = env.get_template(fname + ".template.md").render(**json.loads(fx.read_text()))
        want = (HERE / "fixtures" / (fname + ".out.md")).read_text()
        if got != want:
            err(f"fixtures/{fname}: render differs")

    for line in report:
        print(line)
    for e in errors:
        print(e)
    print(f"{len(objs)} objects, {len(errors)} problems")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
