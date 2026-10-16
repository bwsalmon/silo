#!/usr/bin/env python3
"""Check the example task repos against docs/task-repos.md.

This is a reviewer's aid, not the implementation: it finds every object by
scanning front matter (never by path), validates it against its kind's
JSON Schema, checks ids are unique, resolves every {ref, id} reference --
relative, by alias, and by git URL, mapping this folder's illustrative
Silo URLs to the sibling directories -- follows tombstones, recomputes
requirement hashes, and renders each template with Python Jinja2 under
StrictUndefined, comparing against fixtures/.

Needs python3-yaml, python3-jsonschema and python3-jinja2.
"""
import hashlib
import json
import pathlib
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
    objs = []
    for repo in REPOS:
        for p in sorted((HERE / repo).rglob("*.md")):
            fm, body = split(p)
            if isinstance(fm, dict) and "kind" in fm:
                objs.append((repo, p, fm, body))
    return objs


def aliases(repo):
    fm, _ = split(HERE / repo / "README.md")
    return fm.get("aliases", {})


def resolve(repo, src, ref):
    """Return (repo, path) for a ref string, or (None, reason)."""
    target = ref.split("#", 1)[0]
    if "//" in target and "://" in target:
        scheme_end = target.index("://") + 3
        sep = target.find("//", scheme_end)
        url, inpath = (target[:sep], target[sep + 2:]) if sep >= 0 else (target, "")
        inpath = inpath.rsplit("@", 1)[0] if "@" in inpath else inpath
        if url.startswith(EXTERNAL):
            return None, "external"
        if url not in HOSTED:
            return None, f"unknown repo {url}"
        return HOSTED[url], inpath.lstrip("/")
    m = re.match(r"^([a-z][a-z0-9-]*):(.*)$", target)
    if m:
        al = aliases(repo)
        if m.group(1) not in al:
            return None, f"undeclared alias {m.group(1)}"
        return resolve(repo, src, al[m.group(1)] + "//" + m.group(2))
    target = target.rsplit("@", 1)[0] if "@" in target else target
    if target.startswith("/"):
        return repo, target.lstrip("/")
    rel = (src.parent / target).resolve().relative_to(HERE / repo)
    return repo, str(rel)


def check_ref(repo, src, r, by_id, what):
    rrepo, rpath = resolve(repo, src, r["ref"])
    if rrepo is None:
        if rpath != "external":
            err(f"{src.relative_to(HERE)}: {what} {r['ref']}: {rpath}")
        return
    p = HERE / rrepo / rpath
    if p.is_dir() or rpath == "":
        return  # a repo or folder (applies_to)
    want = r.get("id")
    if p.exists():
        fm, _ = split(p)
        if want and fm.get("id") != want:
            err(f"{src.relative_to(HERE)}: {what} {r['ref']}: id {fm.get('id')} != {want}")
        anchor = r["ref"].split("#", 1)[1] if "#" in r["ref"] else None
        if anchor and re.match(r"^[A-Z][A-Z0-9]*-\d+$", anchor) and "{#" + anchor + "}" not in p.read_text():
            err(f"{src.relative_to(HERE)}: {what} {r['ref']}: no requirement {anchor}")
        return
    # The path is a hint; the id is the truth (§4).
    hits = [o for o in by_id.get(want, []) if o[0] == rrepo]
    if not hits:
        err(f"{src.relative_to(HERE)}: {what} {r['ref']}: dangling")


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
    by_id = {}
    for o in objs:
        if o[2]["kind"] != "repo":
            by_id.setdefault(o[2]["id"], []).append(o)
    for repo, p, fm, body in objs:
        kind = fm["kind"]
        resolver = jsonschema.RefResolver.from_schema(schemas[kind], store=store)
        v = jsonschema.Draft202012Validator(schemas[kind], resolver=resolver)
        for e in v.iter_errors(json.loads(json.dumps(fm, default=str))):
            err(f"{p.relative_to(HERE)}: {'/'.join(map(str, e.path))}: {e.message}")
    for i, group in by_id.items():
        repos = [g[0] for g in group]
        if len(set(repos)) != len(repos):
            err(f"id {i} appears twice in one repo")
        live = [g for g in group if g[2].get("state") not in ("placed", "moved")]
        if len(live) > 1:
            err(f"id {i} is live in {len(live)} repos")
    for repo, p, fm, body in objs:
        refs = []
        for key in ("template", "schedule", "task", "moved_to", "in_reply_to", "supersedes"):
            if isinstance(fm.get(key), dict) and "ref" in fm[key]:
                refs.append((key, fm[key]))
        for key in ("depends_on", "implements", "applies_to"):
            refs += [(key, r) for r in fm.get(key, []) or []]
        for key in ("intake", "previous"):
            if key in (fm.get("placement") or {}):
                refs.append(("placement." + key, fm["placement"][key]))
        if "placement" in fm and "ref" in fm["placement"]:
            refs.append(("placement", {"ref": fm["placement"]["ref"], "id": fm["id"]}))
        for key, r in refs:
            check_ref(repo, p, r, by_id, key)
        for r in fm.get("implements", []) or []:
            rrepo, rpath = resolve(repo, p, r["ref"])
            anchor = r["ref"].split("#", 1)[1]
            if rrepo and section_hashes((HERE / rrepo / rpath).read_text()).get(anchor) != r["hash"]:
                err(f"{p.relative_to(HERE)}: implements {anchor}: hash differs (changed since implemented)")
    # Templates render under Python Jinja2, StrictUndefined (§6.1).
    class Loader(jinja2.BaseLoader):
        def get_source(self, env, name):
            fm, body = split(HERE / "hub-shared" / "templates" / name)
            return body.lstrip("\n"), name, lambda: True
    env = jinja2.Environment(loader=Loader(), undefined=jinja2.StrictUndefined,
                             keep_trailing_newline=True, autoescape=False)
    for fx in sorted((HERE / "fixtures").glob("*.inputs.json")):
        name = fx.name.replace(".inputs.json", "")
        got = env.get_template(name + ".md").render(**json.loads(fx.read_text()))
        want = (HERE / "fixtures" / (name + ".out.md")).read_text()
        if got != want:
            err(f"fixtures/{name}: render differs")
    for e in errors:
        print(e)
    print(f"{len(objs)} objects, {len(errors)} problems")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
