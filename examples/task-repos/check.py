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
  (moved_from) and is not a scheduled firing, and a template there carries
  no reads, writes, grants or target;
- derives each task's status from its runs, and prints each lane's
  effective order, and each Grain's next picks across its lanes;
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
    "https://silo.example.com/git/demo/namespaces.git": "ns-demo",
}
EXTERNAL = ("https://github.com/",)  # real repos: referenced, not fetched here
REPOS = sorted(set(HOSTED.values()))
MAX_HOPS = 8
OPEN_RUN = ("accepted", "running", "waiting")
CLOSED_RUN = ("done", "failed", "cancelled")
PRIORITY = ["urgent", "high", "normal", "low"]
CONFIG = ("include", "exclude")
LIMITS = {"depth": 8, "includes_per_space": 64, "spaces": 256, "repos": 32, "objects": 50000}
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
    if kind == "task":
        add("template", fm.get("template"), "template")
        add("schedule", fm.get("schedule"), "schedule")
        for r in fm.get("depends_on") or []:
            add("depends_on", r, "task")
        for r in fm.get("implements") or []:
            add("implements", r, "design")
        add("supersedes", fm.get("supersedes"), "task")
        add("filed_by.run", (fm.get("filed_by") or {}).get("run"), "run")
    elif kind == "ref":
        add("to", fm.get("to"), "schedule" if fm.get("fired_at") else "task")
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
    elif kind == "schedule":
        add("template", fm.get("template"), "template")
    elif kind == "include":
        add("target", fm.get("target"))
    elif kind == "exclude":
        for r in fm.get("names") or []:
            add("names", r, pinned=True)  # may name what is not there (yet)
    elif kind == "schedule":
        add("derived_from", fm.get("derived_from"), "schedule")
    elif kind == "repo":
        for lane in (fm.get("lanes") or {}).get("order") or []:
            add("lanes", lane["lane"])
    elif kind == "design":
        for r in fm.get("applies_to") or []:
            add("applies_to", r)
        for r in fm.get("supersedes") or []:
            add("supersedes", r, "design")
    return out


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
            if fm.get("schedule"):
                err(f"{repo}/{path}: a scheduled firing in a code repo; firings live in an intake")
            homes = [canonical(repo, path, r)[0] for r in fm.get("moved_from") or []]
            if not any(h and split(HERE / h / "README.md")[0].get("role") == "intake" for h in homes):
                err(f"{repo}/{path}: a task in a code repo with no intake in moved_from; Silo will not place it")
        if fm["kind"] == "template":
            for k in ("reads", "writes", "grants", "target"):
                if k in fm:
                    err(f"{repo}/{path}: a code repo's template may not carry {k}")

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
        ("demo-z", "ns-demo", "y/templates/greeting.md", True),     # (X − Y) ∪ {Y}
        ("demo-z", "ns-demo", "x/templates/note.md", True),
        ("demo-z", "ns-demo", "x/exclude-y.md", True),             # configuration is listed
        ("demo-v", "ns-demo", "x/templates/note.md", True),
        ("demo-v", "ns-demo", "x/include-y.md", False),            # disabled, and its name cut
        ("demo-v", "ns-demo", "y/templates/greeting.md", False),
        ("eu", "org-templates", "templates/security-review.md", True),   # transitive
        ("eu", "org-templates", "templates/legacy-triage.md", False),    # the team's exclude
        ("eu", "hub-placements", "grains/eu/backlog/2026-10-16-approval-flow.md", True),
        ("eu", "hub-placements", "grains/us/backlog/2026-10-16-refuse-self-approved.md", False),
        ("eu", "hub-shared", "schedules/prune-packages.md", False),      # eu is assigned no schedules
        ("us", "hub-shared", "schedules/prune-packages.md", True),       # through Silo's folder
        ("us", "hub-shared", "schedules/granule-pull.md", False),        # the root's exclude
        ("us", "grain-us", "schedules/granule-pull-weekly.md", True),
        ("us", "hub-shared", "templates/granule-pull.md", False),        # referenced, never included
    ]
    for grain, repo, path, want in expect:
        if has(grain, repo, path) != want:
            err(f"namespace {grain}: {repo}/{path} {'missing' if want else 'present'}")
    for space, want in [(("ns-demo", "x", "folder"), {"x/templates/note.md", "x/include-y.md", "x/exclude-y.md"}),
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
