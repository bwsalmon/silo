#!/usr/bin/env python3
"""Check the example task repos against docs/task-repos.md.

This is a reviewer's aid, not the implementation. It:

- finds every object by scanning front matter (never by path), and the
  README that governs it by nearest-README discovery: a grain folder's
  .grain/README.md as well as a whole repo's README.md;
- validates each against its kind's JSON Schema;
- resolves every reference -- relative, by alias, and by git URL, mapping
  this folder's illustrative Silo URLs to the sibling directories -- with
  the path as the identity, following tombstones with a hop limit and
  cycle detection;
- checks the path rules: a tombstone's target lists it in moved_from, and
  the moved object's moved_from entries are tombstones pointing back;
- resolves each Grain root's namespace bottom-up from its include and
  exclude objects (§3.3): kinds intersected along the path, a depth limit,
  cycles cut, excludes scoped to their own folder, an excluded include
  disabled; and checks the X/Y/Z cases of ns-demo/ and the examples'
  expected names, in two iteration orders;
- checks backlogs: every reference resolves to a task, no lane holds two
  references to one task, and no reference outlives its task's close;
- checks execution records: at most one open run per task;
- checks code repos' grain folders: a task there came from an intake
  (moved_from) and is not a trigger's firing, and a template there carries
  no reads, writes, grants or target; a gate there names no other repos,
  and a workflow trigger's workflow exists in the same repo;
- checks triggers and gates (§6.2, §6.3) by their schemas: a trigger
  never carries a gate's keys nor a gate a trigger's, and a workflow
  trigger fires in a Grain; and that the example Grains, which include
  triggers only, have no gate in their namespaces;
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
  which refuses the request until Anselm is adopted; and checks that a
  requested capability is in the Grain's namespace;
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
    "https://silo.example.com/git/demo/namespaces.git": "ns-demo",
}
EXTERNAL = ("https://github.com/",)  # real repos: referenced, not fetched here
REPOS = sorted(set(HOSTED.values()))
MAX_HOPS = 8
OPEN_RUN = ("accepted", "running", "waiting")
CLOSED_RUN = ("done", "failed", "cancelled")
PRIORITY = ["urgent", "high", "normal", "low"]
CONFIG = ("include", "exclude")
ASSET = "(asset)"
CONTEXT_LIMITS = {"entries": 64, "depth": 3, "reached_files": 256, "reached_bytes": 64 << 20, "file_bytes": 32 << 20}
LIMITS = {"depth": 8, "includes_per_space": 64, "spaces": 256, "repos": 32, "objects": 50000}
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


def readmes(repo):
    """Every README.md declaring kind: repo, with its scope as repo-rooted
    folder prefixes ("" is the whole repo)."""
    out = []
    for p in sorted((HERE / repo).rglob("README.md")):
        fm, _ = split(p)
        if not (isinstance(fm, dict) and fm.get("kind") == "repo"):
            continue
        rel = str(p.relative_to(HERE / repo))
        home = posixpath.dirname(rel)
        scope = []
        for s in fm.get("scope") or ["/" + home + "/" if home else "/"]:
            s = s.lstrip("/") if s.startswith("/") else posixpath.join(home, s)
            s = posixpath.normpath(s) if s.strip("/") else ""
            scope.append("" if s in ("", ".") else s + "/")
        out.append((rel, home, fm, scope))
    return out


def governor(repo, path):
    """The README governing a file: of those whose scope covers it, the one
    with the longest covering entry (§3.1). Two at one length is an error."""
    best, length = None, -1
    for rel, home, fm, scope in readmes(repo):
        for s in scope:
            if path.startswith(s) and len(s) >= length:
                if len(s) == length and best[0] != rel:
                    err(f"{repo}/{path}: governed by both {best[0]} and {rel}")
                best, length = (rel, home, fm), len(s)
    return best


def objects():
    objs = {}
    for repo in REPOS:
        for p in sorted((HERE / repo).rglob("*.md")):
            fm, body = split(p)
            if isinstance(fm, dict) and "kind" in fm:
                path = str(p.relative_to(HERE / repo))
                if governor(repo, path) is None:
                    err(f"{repo}/{path}: kind {fm['kind']} outside every README's scope")
                objs[(repo, path)] = (fm, body)
    return objs


def aliases(repo, src):
    gov = governor(repo, src)
    return gov[2].get("aliases", {}) if gov else {}


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
            return ident, None, hops  # a repo or folder (applies_to)
        if ident not in objs:
            if (HERE / ident[0] / ident[1]).is_file():
                return ident, {"kind": ASSET}, hops  # an asset: any file that is not an object (§2.2)
            return None, "dangling", hops
        fm = objs[ident][0]
        if fm["kind"] != "moved":
            return ident, fm, hops
        hops += 1
        if hops > MAX_HOPS:
            return None, f"more than {MAX_HOPS} redirects", hops
        ident = canonical(ident[0], ident[1], fm["to"])
        if ident[0] is None:
            return None, ident[1], hops


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
    elif kind == "include":
        add("target", fm.get("target"))
    elif kind == "capability":
        add("base.capability", (fm.get("base") or {}).get("capability"), "capability")
    elif kind == "exclude":
        for r in fm.get("names") or []:
            add("names", r, pinned=True)  # may name what is not there (yet)
    elif kind == "repo":
        for lane in (fm.get("lanes") or {}).get("order") or []:
            add("lanes", lane["lane"])
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
    gov = governor(repo, path)
    code = bool(gov) and gov[2].get("role") == "code"
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
    """A name glob: * within one path segment, ** across segments."""
    out, i = "", 0
    while i < len(g):
        if g.startswith("**", i):
            out, i = out + ".*", i + 2
        elif g[i] == "*":
            out, i = out + "[^/]*", i + 1
        elif g[i] == "?":
            out, i = out + "[^/]", i + 1
        else:
            out, i = out + re.escape(g[i]), i + 1
    return re.compile("^" + out + "$")


def exclude_matcher(ident, fm):
    """The names an exclude object matches, as (exact names, patterns).
    Returns None if the exclude cannot be read, which fails its folder
    closed: an exclude that cannot be applied must never widen."""
    repo, path = ident
    names, pats = set(), []
    for n in fm.get("names") or []:
        c = canonical(repo, path, n)
        if c[0] is None:
            return None
        names.add(c)
    for g in fm.get("patterns") or []:
        c = canonical(repo, path, g)
        if c[0] is None:
            return None
        pats.append((c[0], glob_re(c[1])))
    return names, pats


def matched(name, matchers):
    return any(name in ns or any(name[0] == r and g.match(name[1]) for r, g in ps) for ns, ps in matchers)


def members(objs, space):
    """The objects a space holds: one file, or every object under a folder.
    A root holds what its README governs. READMEs are metadata and
    tombstones redirect, so neither is ever a name in a namespace."""
    repo, path, kind = space
    if kind == "file":
        found = [(repo, path)] if (repo, path) in objs else []
    else:
        pre = path + "/" if path else ""
        found = [i for i in objs if i[0] == repo and i[1].startswith(pre)]
        if kind == "root":
            readme = posixpath.join(path, "README.md") if path else "README.md"
            found = [i for i in found if (governor(repo, i[1]) or ("",))[0] == readme]
    return [i for i in found if objs[i][0]["kind"] not in ("repo", "moved")]


def contribution(objs, space, kinds, depth, stack, disabled, notes, budget, order=1):
    """contribution(space) = (own objects ∪ ⋃ contribution(each include in
    it)) − names matched by its excludes (§3.3). kinds is None for every
    kind; disabled carries the excludes of every space above on this path,
    which disable any include object they name."""
    key = (space[0], space[1])
    if key in stack:
        notes.append("cycle cut: " + " -> ".join(short(s) for s in stack + [key]))
        return set()
    budget["spaces"] += 1
    budget["repos"].add(space[0])
    if budget["spaces"] > LIMITS["spaces"] or len(budget["repos"]) > LIMITS["repos"]:
        budget["failed"] = True
        return set()
    held = members(objs, space)[::order]
    excl = []
    for i in held:
        if objs[i][0]["kind"] == "exclude":
            m = exclude_matcher(i, objs[i][0])
            if m is None:
                notes.append(f"{short(i)}: an exclude that cannot be read; {short(key)} contributes nothing")
                return set()
            excl.append(m)
    got = {i for i in held if objs[i][0]["kind"] in CONFIG or kinds is None or objs[i][0]["kind"] in kinds}
    incs = [i for i in held if objs[i][0]["kind"] == "include"]
    if len(incs) > LIMITS["includes_per_space"]:
        notes.append(f"{short(key)}: more than {LIMITS['includes_per_space']} includes; it contributes nothing")
        return set()
    above = disabled + excl
    for inc in incs:
        fm = objs[inc][0]
        if matched(inc, above):
            notes.append(f"disabled: {short(inc)} (its name is excluded)")
            continue
        if depth == 0:
            notes.append(f"depth limit: {short(inc)} not followed")
            continue
        ident, target, _ = resolve(objs, inc[0], inc[1], fm["target"])
        if ident is None:
            notes.append(f"skipped: {short(inc)} target {fm['target']}: {target}")
            continue
        want = None if "*" in fm["kinds"] else set(fm["kinds"])
        sub_kinds = want if kinds is None else (kinds if want is None else kinds & want)
        sub_depth = depth - 1 if "max_depth" not in fm else min(depth - 1, fm["max_depth"])
        sub = (ident[0], ident[1], "folder" if target is None else "file")
        got |= contribution(objs, sub, sub_kinds, sub_depth, stack + [key], above, notes, budget, order)
    return {n for n in got if not matched(n, excl)}


def namespace(objs, repo, home, order=1):
    """namespace(root) = contribution(root): the root's excludes apply last."""
    notes, budget = [], {"spaces": 0, "repos": set(), "failed": False}
    names = contribution(objs, (repo, home, "root"), None, LIMITS["depth"], [], [], notes, budget, order)
    if budget["failed"] or len(names) > LIMITS["objects"]:
        notes.append(f"{repo}/{home}: over the resolver's limits; the Grain keeps its last good namespace")
    return names, notes


def roots(objs):
    """Every Grain root: a kind: repo README with role: grain-root."""
    out = []
    for repo in REPOS:
        for rel, home, fm, scope in readmes(repo):
            if fm.get("role") == "grain-root":
                out.append((repo, home, fm))
    return out


def lane_of(ref):
    return (ref[0], posixpath.dirname(ref[1]))


def picks(lanes, entries, conf, repo, src):
    """The order a Grain takes runnable references across its lanes (§5.2):
    ranks are compared within a lane only, and the root's lanes setting
    decides between lanes."""
    listed = []
    for item in conf.get("order") or []:
        c = canonical(repo, src, item["lane"])
        listed.append((c, item.get("weight", 1)))

    def slot(lane):
        best = None
        for n, (c, w) in enumerate(listed):
            if lane[0] == c[0] and (lane[1] == c[1] or lane[1].startswith(c[1] + "/")):
                if best is None or len(c[1]) > len(listed[best][0][1]):
                    best = n
        return best

    queues, weight, rank = {}, {}, {}
    for lane in lanes:
        n = slot(lane)
        if n is None and conf.get("unlisted") == "ignore":
            continue
        queues[lane] = [e for e in entries[lane]]
        weight[lane] = 1 if conf.get("mode") == "round_robin" or n is None else listed[n][1]
        rank[lane] = (n if n is not None else len(listed), lane)
    out, credit = [], {lane: 0 for lane in queues}
    while any(queues.values()):
        live = sorted((l for l in queues if queues[l]), key=lambda l: rank[l])
        if conf.get("mode", "strict") == "strict":
            lane = live[0]
        else:
            for l in live:
                credit[l] += weight[l]
            lane = max(live, key=lambda l: (credit[l], -rank[l][0]))
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

    # Tombstones and moved_from agree, both ways (§2.1, §8.1).
    for (repo, path), (fm, body) in objs.items():
        if fm["kind"] == "moved":
            ident, target, _ = resolve(objs, repo, path, fm["to"])
            if ident is None:
                err(f"{repo}/{path}: tombstone to {fm['to']}: {target}")
                continue
            back = [canonical(ident[0], ident[1], r) for r in target.get("moved_from") or []]
            if (repo, path) not in back:
                err(f"{repo}/{path}: tombstone target {short(ident)} does not list it in moved_from")
        for r in fm.get("moved_from") or []:
            old = canonical(repo, path, r)
            if old not in objs or objs[old][0]["kind"] != "moved":
                err(f"{repo}/{path}: moved_from {r}: not a tombstone")

    # Code repos' grain folders (§5.5, §6.2, §8.13).
    for (repo, path), (fm, body) in objs.items():
        gov = governor(repo, path)
        if not gov or gov[2].get("role") != "code":
            continue
        rel, home, gfm = gov
        if fm["kind"] == "task":
            th = gfm.get("task_home") or "intake"
            folder = posixpath.join(home, (th.get("path", "tasks/") if isinstance(th, dict) else "tasks/"))
            if not path.startswith(folder):
                err(f"{repo}/{path}: a task outside {folder}, the folder task_home names")
            if fm.get("trigger") or fm.get("schedule"):
                err(f"{repo}/{path}: a trigger's firing in a code repo; firings live in an intake or a Grain root")
            homes = [canonical(repo, path, r)[0] for r in fm.get("moved_from") or []]
            if not any(h and split(HERE / h / "README.md")[0].get("role") == "intake" for h in homes):
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

    # Backlogs: folders of ranked references, one lane each (§5.2).
    entries = {}
    for (repo, path), (fm, body) in objs.items():
        if fm["kind"] != "ref":
            continue
        rel, home, gfm = governor(repo, path) or ("", "", {})
        folders = [posixpath.join(home, f) for f in gfm.get("backlog", ["backlog/"])]
        if not any(glob_re(f.rstrip("/") + "/**").match(path) for f in folders):
            err(f"{repo}/{path}: a ref outside the README's backlog folders {folders}")
        ident, task, _ = resolve(objs, repo, path, fm["to"])
        if ident:
            entries.setdefault(lane_of((repo, path)), []).append(((repo, path), ident, fm, task))
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

        def key(e):
            ref, task, fm, tfm = e
            ready = all(done(canonical(task[0], task[1], d)) for d in tfm.get("depends_on") or [])
            return (not ready, not fm.get("expedite", False), PRIORITY.index(fm.get("priority", "normal")),
                    fm["rank"], str(tfm["created_at"]), ref[1])
        es.sort(key=key)

    # Namespaces: each Grain root's, bottom-up and order-independent (§3.3).
    spaces = {}
    for repo, home, fm in roots(objs):
        grain = fm["grain"]["id"]
        names, notes = namespace(objs, repo, home)
        again, _ = namespace(objs, repo, home, order=-1)
        if again != names:
            err(f"{repo}/{home}: the namespace depends on iteration order")
        spaces[grain] = (repo, home, fm, names)
        report.append(f"namespace {grain} ({repo}/{home}): " + ", ".join(
            f"{n} {k}" for k, n in sorted(Counter(objs[i][0]["kind"] for i in names).items())))
        report.extend("  " + n for n in notes)

    def queued_on(task):
        return [g for g, (_, _, _, names) in spaces.items()
                if any(e[1] == task for lane in entries for e in entries[lane] if e[0] in names)]

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
    for grain, (repo, home, fm, names) in sorted(spaces.items()):
        mine = {lane: [e for e in es if e[0] in names and not runs.get(e[1]) and e[3]["state"] == "approved"]
                for lane, es in entries.items()}
        mine = {lane: es for lane, es in mine.items() if es}
        if not mine:
            continue
        src = posixpath.join(home, "README.md") if home else "README.md"
        order = picks(sorted(mine), mine, fm.get("lanes") or {}, repo, src)
        report.append(f"next on {grain} ({(fm.get('lanes') or {}).get('mode', 'strict')}): " +
                      ", ".join(posixpath.basename(e[1][1].removesuffix("/task.md")) for e in order))

    # What the examples say each namespace holds.
    def has(grain, repo, path):
        return (repo, path) in spaces[grain][3]
    expect = [
        ("demo-z", "ns-demo", "y/templates/greeting.template.md", True),     # (X − Y) ∪ {Y}
        ("demo-z", "ns-demo", "x/templates/note.template.md", True),
        ("demo-z", "ns-demo", "x/exclude-y.md", True),             # configuration is listed
        ("demo-v", "ns-demo", "x/templates/note.template.md", True),
        ("demo-v", "ns-demo", "x/include-y.md", False),            # disabled, and its name cut
        ("demo-v", "ns-demo", "y/templates/greeting.template.md", False),
        ("eu", "org-templates", "templates/security-review.template.md", True),   # transitive
        ("eu", "org-templates", "templates/legacy-triage.template.md", False),    # the team's exclude
        ("eu", "hub-placements", "grains/eu/backlog/2026-10-16-approval-flow.ref.md", True),
        ("eu", "hub-placements", "grains/us/backlog/2026-10-16-refuse-self-approved.ref.md", False),
        ("eu", "hub-shared", "triggers/prune-packages.trigger.md", False),       # eu is assigned no triggers
        ("us", "hub-shared", "triggers/prune-packages.trigger.md", True),        # through Silo's folder
        ("us", "hub-shared", "triggers/plan-designs.trigger.md", True),          # listed; only Silo fires it
        ("us", "hub-shared", "triggers/granule-pull.trigger.md", False),         # the root's exclude
        ("us", "grain-us", "triggers/granule-pull-weekly.trigger.md", True),
        ("us", "hub-shared", "templates/granule-pull.template.md", False),        # referenced, never included
        ("eu", "hub-shared", "capabilities/widgets-deployer.capability.md", True),  # available to request
        ("eu", "org-capabilities", "gcp/acme-widgets.capability.md", False),        # a base in its chain, not offered
    ]
    for grain, (_, _, _, names) in spaces.items():
        for repo, path in names:
            if objs[(repo, path)][0]["kind"] == "gate":
                err(f"namespace {grain}: gate {repo}/{path} reached through includes that accept triggers only")
    for grain, repo, path, want in expect:
        if has(grain, repo, path) != want:
            err(f"namespace {grain}: {repo}/{path} {'missing' if want else 'present'}")
    for space, want in [(("ns-demo", "x", "folder"), {"x/templates/note.template.md", "x/include-y.md", "x/exclude-y.md"}),
                        (("ns-demo", "w", "folder"), {"w/include-y.md", "w/exclude-y.md"})]:
        got = {p for r, p in contribution(objs, space, None, LIMITS["depth"], [], [], [], {"spaces": 0, "repos": set(), "failed": False})}
        if got != want:
            err(f"contribution of {short(space[:2])}: {sorted(got)}, expected {sorted(want)}")

    # Include objects: a target in another repo names its ref and its mode.
    for (repo, path), (fm, body) in objs.items():
        if fm["kind"] != "include":
            continue
        cross = "://" in fm["target"] or re.match(r"^[a-z][a-z0-9-]*:", fm["target"])
        if cross and ("@" not in fm["target"] or "ref_mode" not in fm):
            err(f"{repo}/{path}: an include of another repo needs @<ref> and ref_mode")
        if not cross and "@" in fm["target"]:
            err(f"{repo}/{path}: a relative include reads its own commit, and takes no @<ref>")
        at = fm["target"].rsplit("@", 1)[-1] if "@" in fm["target"] else ""
        if fm.get("ref_mode") == "track" and re.match(r"^[0-9a-f]{40}$", at):
            err(f"{repo}/{path}: track names a branch, not a commit")

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
        loop[("hub-shared", f"capabilities/{a}.md")] = ({"kind": "capability", "base": {"capability": f"{b}.md"}}, "")
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
                listed = chain[-1][0] in spaces[grain][3]
                dims, implies, pending, refusal = effective(objs, grain, chain, base_type, e.get("reducers") or [])
                head = f"capability {repo}/{path} entry {n} on {grain}: "
                report.append(head + " -> ".join(short(i) for i, _ in chain) + f" over {base_type}")
                if not listed:
                    refusal = refusal or f"{short(chain[-1][0])} is not in {grain}'s namespace"
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
            fm, body = split(HERE / "hub-shared" / "templates" / name)
            return body.lstrip("\n"), name, lambda: True
    env = jinja2.Environment(loader=Loader(), undefined=jinja2.StrictUndefined,
                             keep_trailing_newline=True, autoescape=False)
    for fx in sorted((HERE / "fixtures").glob("*.inputs.json")):
        fname = fx.name.replace(".inputs.json", "")
        got = env.get_template(fname + ".md").render(**json.loads(fx.read_text()))
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
