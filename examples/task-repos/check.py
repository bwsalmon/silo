#!/usr/bin/env python3
"""Check the example task repos against docs/task-repos.md.

This is a reviewer's aid, not the implementation. It:

- finds every object by scanning front matter (never by path);
- validates each against its kind's JSON Schema;
- resolves every reference -- relative, by alias, and by git URL, mapping
  this folder's illustrative Silo URLs to the sibling directories -- with
  the path as the identity, following tombstones with a hop limit and
  cycle detection;
- checks the path rules: a tombstone's target lists it in moved_from, and
  the moved object's moved_from entries are tombstones pointing back;
- checks backlogs: every reference resolves to a task, no backlog holds two
  references to one task, and no reference outlives its task's close;
- checks execution records: at most one open run per task;
- derives each task's status from its runs, and prints each backlog's
  effective order;
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
}
EXTERNAL = ("https://github.com/",)  # real repos: referenced, not fetched here
REPOS = sorted(set(HOSTED.values()))
MAX_HOPS = 8
OPEN_RUN = ("accepted", "running", "waiting")
CLOSED_RUN = ("done", "failed", "cancelled")
PRIORITY = ["urgent", "high", "normal", "low"]
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


def objects():
    objs = {}
    for repo in REPOS:
        for p in sorted((HERE / repo).rglob("*.md")):
            fm, body = split(p)
            if isinstance(fm, dict) and "kind" in fm:
                objs[(repo, str(p.relative_to(HERE / repo)))] = (fm, body)
    return objs


def aliases(repo):
    fm, _ = split(HERE / repo / "README.md")
    return fm.get("aliases", {})


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
        al = aliases(repo)
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
        add("to", fm.get("to"), "task")
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
        queued = [g for g, b in backlogs.items() if g.startswith("grain-") and any(e[1] == task for e in b)]
        if queued:
            return "queued on " + ", ".join(q.removeprefix("grain-") for q in queued)
        return "unplaced" if fm["state"] == "approved" else fm["state"]

    # Backlogs: folders of ranked references (§5.2).
    backlogs = {}
    for (repo, path), (fm, body) in objs.items():
        if fm["kind"] != "ref":
            continue
        folders = split(HERE / repo / "README.md")[0].get("backlog", ["backlog/"])
        if not any(path.startswith(f) for f in folders):
            err(f"{repo}/{path}: a ref outside the README's backlog folders {folders}")
        ident, task, _ = resolve(objs, repo, path, fm["to"])
        if ident:
            backlogs.setdefault(repo, []).append(((repo, path), ident, fm, task))
    for repo, entries in sorted(backlogs.items()):
        seen = {}
        for ref, task, fm, tfm in entries:
            if task in seen:
                err(f"{short(ref)} and {short(seen[task])} both reference {short(task)}")
            seen[task] = ref
            if tfm["state"] in ("cancelled", "rejected"):
                err(f"{short(ref)}: references a {tfm['state']} task; Silo removes it")
            if any(r[1]["state"] in CLOSED_RUN for r in runs.get(task, [])):
                err(f"{short(ref)}: references a closed task; Silo removes it")

        def key(e):
            ref, task, fm, tfm = e
            ready = all(done(canonical(task[0], task[1], d)) for d in tfm.get("depends_on") or [])
            return (not ready, not fm.get("expedite", False), PRIORITY.index(fm.get("priority", "normal")),
                    fm["rank"], str(tfm["created_at"]), ref[1])

        report.append(f"backlog {repo}:")
        for ref, task, fm, tfm in sorted(entries, key=key):
            flags = [f"rank {fm['rank']}"]
            if fm.get("priority"):
                flags.append(fm["priority"])
            if fm.get("expedite"):
                flags.append("expedite")
            report.append(f"  {ref[1]} -> {short(task)} [{', '.join(flags)}] ({status(task)})")

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
