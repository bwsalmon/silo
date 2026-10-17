#!/usr/bin/env python3
"""grainlint: the reference linter for the Grainfile Format (docs/grainfiles-spec.md).

  grainlint.py TREE...                 lint each tree (a directory holding repos, or one repo)
  grainlint.py --authorize BASE HEAD --actor PRINCIPAL [--actor ...]
                                       authorise HEAD against BASE's GRAINOWNERS (§16.4)
  grainlint.py --self-test             lint the examples (no errors), every fixture under
                                       testdata/bad (exactly the codes in its expect.txt), and
                                       check the spec, schemas and linter agree

A TREE is a directory of repositories: each child holding a .grainconfig, a
grain.config.md or a .git marker file is a repository. Locator references
(https://…) are resolved through stubs.yaml at the tree's top, mapping a URL
to a child directory; an unmapped URL is treated as external and stubbed.

Needs python3-yaml, python3-jsonschema and python3-jinja2.
"""
import fnmatch, json, pathlib, posixpath, re, sys
import jinja2, jsonschema, yaml

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parent.parent
SCHEMAS = ROOT / "schemas" / "grainfiles"
SPEC = ROOT / "docs" / "grainfiles-spec.md"
KINDS = ["config", "task", "ref", "run", "comment", "template", "trigger", "gate", "capability",
         "capability-base", "design", "memory", "moved", "lifecycle", "placement", "asset"]
CATEGORIES = ["proposed", "ready", "active", "waiting", "done", "cancelled"]
SEVERITY = {"GF005": "warning", "GF022": "warning", "GF040": "warning", "GF044": "warning",
            "GF055": "warning", "GF067": "warning", "GF072": "warning"}
CODES = ["GF001", "GF002", "GF003", "GF004", "GF005", "GF006", "GF007", "GF010", "GF011", "GF020",
         "GF021", "GF022", "GF023", "GF030", "GF031", "GF032", "GF033", "GF034", "GF035", "GF036",
         "GF040", "GF041", "GF042", "GF044", "GF050", "GF051", "GF052", "GF053", "GF054", "GF055",
         "GF061", "GF062", "GF063", "GF064", "GF065", "GF066", "GF067", "GF068", "GF070", "GF071",
         "GF072", "GF073", "GF074", "GF080", "GF090"]
STEM = re.compile(r"^(\d{8}T\d{6}Z|[a-z0-9])[a-z0-9._-]*$")
RESERVED_DIRS = {".thread": {"comment"}, ".runs": {"run", "comment"}, ".placements": {"placement"}, ".attachments": set()}
EXPECTS = {  # field -> expected kind (§9.6)
    ("ref", "to"): "task", ("task", "depends_on"): "task", ("task", "supersedes"): "task",
    ("comment", "task"): "task", ("placement", "task"): "task", ("task", "template"): "template",
    ("trigger", "fires.template"): "template", ("task", "lifecycle"): "lifecycle",
    ("config", "lifecycle"): "lifecycle", ("template", "lifecycle"): "lifecycle", ("run", "task"): "task",
    ("run", "from"): "ref", ("comment", "run"): "run", ("comment", "in_reply_to"): "comment",
    ("capability", "base.capability"): "capability", ("placement", "ref"): "ref", ("ref", "previous"): "run",
}
DEFAULT_LIFECYCLE = "grain/default"
PRINCIPAL = re.compile(r"^(user:[^@\s]+@[^@\s]+|team:[a-z0-9-]+#(member|approver|admin|maintainer)|(deployment|grain):[a-z][a-z0-9-]*|hub(:trigger)?|silo(:trigger)?|authenticated|none)$")


class Finding:
    def __init__(self, code, path, msg):
        self.code, self.path, self.msg = code, str(path), msg

    def __str__(self):
        return "%s %s %s: %s" % (SEVERITY.get(self.code, "error"), self.code, self.path, self.msg)


def kind_of(name):
    """§4.1.1: parse a file name from the right; None for an asset."""
    if name == "grain.config.md":
        return "config"
    if not name.endswith(".md"):
        return None
    base = name[:-3]
    if base.endswith(".moved"):
        return "moved"
    for k in sorted(KINDS, key=len, reverse=True):
        if base.endswith("." + k) and len(base) > len(k) + 1:
            return k
    return None


def stem_of(name, kind):
    if kind == "moved":
        return name[:-len(".moved.md")]
    return name[:-len("." + kind + ".md")]


class YamlSubsetLoader(yaml.SafeLoader):
    pass


def _no_dupes(loader, node, deep=False):
    keys = set()
    for k, _ in node.value:
        if k.tag != "tag:yaml.org,2002:str" and not isinstance(loader.construct_object(k), str):
            raise yaml.YAMLError("non-string key")
        key = loader.construct_object(k)
        if key in keys:
            raise yaml.YAMLError("duplicate key %r" % key)
        keys.add(key)
    return loader.construct_mapping(node, deep=deep)


YamlSubsetLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _no_dupes)
# YAML 1.2: a bare on/off/yes/no is a string (§5.3).
YamlSubsetLoader.yaml_implicit_resolvers = {k: [(t, r) for t, r in v if t != "tag:yaml.org,2002:bool"]
                                            for k, v in yaml.SafeLoader.yaml_implicit_resolvers.items()}
YamlSubsetLoader.add_implicit_resolver("tag:yaml.org,2002:bool", re.compile(r"^(?:true|false)$"), list("tf"))


def parse(path):
    """§5: returns (front matter, body) or raises (code, message)."""
    raw = path.read_bytes()
    if b"\x00" in raw:
        raise ValueError(("GF003", "NUL in file"))
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError:
        raise ValueError(("GF003", "not UTF-8"))
    if text.startswith("version https://git-lfs"):
        raise ValueError(("GF003", "a grainfile may not be an LFS pointer"))
    lines = text.replace("\r\n", "\n").split("\n")
    if not lines or lines[0].rstrip() != "---":
        raise ValueError(("GF003", "no front matter"))
    for i in range(1, len(lines)):
        if lines[i].rstrip() == "---":
            fm, body = "\n".join(lines[1:i]), "\n".join(lines[i + 1:])
            break
    else:
        raise ValueError(("GF003", "front matter is not closed"))
    if re.search(r"(^|[\s\[{,:-])[&*][A-Za-z0-9_]|<<\s*:|(^|\s)!", fm, re.M):
        raise ValueError(("GF004", "anchors, aliases, merge keys and tags are outside the YAML subset"))
    try:
        data = yaml.load(fm, Loader=YamlSubsetLoader)
    except yaml.YAMLError as e:
        msg = str(e).split("\n")[0]
        raise ValueError(("GF004" if "key" in msg else "GF003", msg))
    if not isinstance(data, dict):
        raise ValueError(("GF004", "front matter is not a mapping"))
    return data, body


class Schemas:
    def __init__(self):
        store = {}
        for p in SCHEMAS.glob("*.schema.json"):
            d = json.loads(p.read_text())
            store[d["$id"]] = d
        self.store = store
        self.by_kind = {k: store["https://grain.dev/schemas/grainfiles/%s.schema.json" % k] for k in KINDS}
        self.repo = store["https://grain.dev/schemas/grainfiles/grainconfig.schema.json"]
        from jsonschema import RefResolver  # noqa: deprecated but available everywhere we run
        self.resolver = lambda s: RefResolver(s["$id"], s, store=store)

    def check(self, schema, data):
        v = jsonschema.Draft202012Validator(schema, resolver=self.resolver(schema))
        out = []
        for e in v.iter_errors(data):
            if e.validator == "additionalProperties" and e.validator_value is False:
                extra = [k for k in e.instance if k not in e.schema.get("properties", {})
                         and not k.startswith("x_")]
                out.append(("GF022", "unknown field(s) %s at /%s" % (extra, "/".join(map(str, e.path)))))
            elif e.validator == "false" or (e.schema is False):
                out.append(("GF020", "refused field /%s" % "/".join(map(str, e.path))))
            else:
                out.append(("GF020", "/%s: %s" % ("/".join(map(str, e.path)), e.message[:200])))
        return out


class Repo:
    def __init__(self, name, path):
        self.name, self.path = name, path
        self.settings = {}
        self.folders = []  # grain folder tops, repo-relative ("" for the root)


class Linter:
    def __init__(self, top, schemas=None):
        self.top = pathlib.Path(top).resolve()
        self.schemas = schemas or Schemas()
        self.findings = []
        self.files = {}  # abs path -> {"kind","data","body","repo","folder"}
        self.stubs = {}
        sp = self.top / "stubs.yaml"
        if sp.exists():
            self.stubs = yaml.safe_load(sp.read_text()) or {}
        self.repos = self.find_repos()
        self.default_lifecycle = self.load_default_lifecycle()

    def add(self, code, path, msg):
        try:
            path = pathlib.Path(path).resolve().relative_to(self.top)
        except ValueError:
            pass
        self.findings.append(Finding(code, path, msg))

    # ---- discovery -------------------------------------------------------
    def find_repos(self):
        kids = [p for p in sorted(self.top.iterdir()) if p.is_dir() and not p.name.startswith(".")]
        repos = []
        is_repo = lambda p: any((p / m).exists() for m in (".grainconfig", "grain.config.md", ".git"))
        if is_repo(self.top):
            repos.append(Repo(self.top.name, self.top))
        else:
            for k in kids:
                repos.append(Repo(k.name, k))
        return repos

    def load_default_lifecycle(self):
        p = SCHEMAS / "lifecycles" / "default.lifecycle.md"
        data, _ = parse(p)
        return data

    def walk(self, repo):
        out = []
        for p in sorted(repo.path.rglob("*")):
            rel = p.relative_to(repo.path)
            if any(part in (".git",) for part in rel.parts) or p.is_dir():
                continue
            out.append(p)
        return out

    def run(self):
        for repo in self.repos:
            self.lint_repo(repo)
        self.check_references()
        self.check_relations()
        self.check_lifecycles()
        self.check_templates()
        self.check_roots()
        return self.findings

    def lint_repo(self, repo):
        gc = repo.path / ".grainconfig"
        if gc.exists():
            try:
                repo.settings = yaml.safe_load(gc.read_text()) or {}
                for code, msg in self.schemas.check(self.schemas.repo, repo.settings):
                    self.add("GF080" if code == "GF020" else code, gc, msg)
            except yaml.YAMLError as e:
                self.add("GF080", gc, str(e).split("\n")[0])
        files = self.walk(repo)
        configs = sorted(p.parent for p in files if p.name == "grain.config.md")
        for c in configs:
            outer = [o for o in configs if o != c and o in c.parents]
            if outer:
                self.add("GF010", c / "grain.config.md", "grain folder nested inside %s" % outer[0].relative_to(repo.path))
        tops = [c for c in configs if not any(o != c and o in c.parents for o in configs)]
        repo.folders = tops
        for p in files:
            if p.name == ".grainconfig" and p.parent != repo.path:
                self.add("GF007", p, ".grainconfig is only allowed at a repository root")
            folder = next((t for t in sorted(tops, key=lambda t: -len(t.parts)) if t == p.parent or t in p.parents), None)
            if folder is None:
                continue
            self.lint_file(repo, folder, p)

    def lint_file(self, repo, folder, p):
        name = p.name
        k = kind_of(name)
        for d in p.relative_to(folder).parent.parts:
            for suf, allowed in RESERVED_DIRS.items():
                if d.endswith(suf):
                    if suf == ".attachments" and k is None:
                        continue
                    if k not in allowed and not (k is None and suf != ".placements"):
                        self.add("GF007", p, "a %s file in a reserved %s/ folder" % (k or "asset", suf))
        if name.endswith(".suite.md"):
            self.add("GF090", p, "kind: suite is removed; convert it to a folder template (spec §13.8)")
            return
        if k is None:
            if name.endswith(".md") and re.match(r".+\.[a-z][a-z0-9-]*\.md$", name) and not name.endswith(".design.md"):
                self.add("GF005", p, "unknown kind suffix; read as an asset")
            return
        if k == "config" and name == "grain.config.md" and p.parent != folder:
            self.add("GF010", p, "nested grain.config.md")
        stem = stem_of(name, k) if name != "grain.config.md" else "grain"
        if k != "asset" and not STEM.match(stem):
            self.add("GF001", p, "name %r is not lower-case [a-z0-9._-]" % stem)
        if len(name.encode()) > 255:
            self.add("GF006", p, "path segment over 255 bytes")
        try:
            data, body = parse(p)
        except ValueError as e:
            code, msg = e.args[0]
            self.add(code, p, msg)
            return
        if data.get("kind") != k:
            self.add("GF002", p, "kind %r does not match the file's suffix .%s.md" % (data.get("kind"), k))
            return
        sch = str(data.get("schema", ""))
        m = re.match(r"^grain/([a-z-]+)@v(\d+)$", sch)
        if m and m.group(1) != k:
            self.add("GF023", p, "schema %s names another kind" % sch)
            return
        if m and m.group(2) != "1":
            self.add("GF021", p, "unknown schema version %s" % sch)
            return
        for code, msg in self.schemas.check(self.schemas.by_kind[k], data):
            self.add(code, p, msg)
        if k == "comment" and not re.match(r"^\d{8}T\d{6}Z-[a-z0-9-]+$", stem):
            self.add("GF001", p, "a comment is named <compact UTC>-<author slug>")
        if k == "placement" and not re.match(r"^\d{8}T\d{6}Z-[a-z0-9-]+$", stem):
            self.add("GF001", p, "a placement is named <compact UTC>-<deployment>")
        self.files[p] = {"kind": k, "data": data, "body": body, "repo": repo, "folder": folder, "stem": stem}

    # ---- references ------------------------------------------------------
    def repo_of_url(self, url):
        u = url.lower().rstrip("/")
        u = u[:-4] if u.endswith(".git") else u
        for k, v in self.stubs.items():
            kk = k.lower().rstrip("/")
            kk = kk[:-4] if kk.endswith(".git") else kk
            if kk == u:
                return next((r for r in self.repos if r.name == v), None)
        return None

    def resolve(self, ref, src, expect=None, field=""):
        """§9.3. Returns the target path, None if stubbed external, or reports and returns False."""
        info = self.files.get(src)
        repo = info["repo"] if info else None
        if not isinstance(ref, str) or not ref or re.search(r"[\s\\]", ref) or re.match(r"^[^/:@]+@[^/:]+:", ref):
            self.add("GF035", src, "%s: malformed reference %r" % (field, ref))
            return False
        s = ref
        anchor = None
        if "#" in s:
            s, anchor = s.split("#", 1)
        m = re.match(r"^([a-z]+://[^/]+)(/.*)$", s)
        target_repo = repo
        if m:
            rest = m.group(2)
            if "//" in rest:
                url_path, path = rest.split("//", 1)
            else:
                url_path, path = rest, ""
            url = m.group(1) + url_path
            if "@" in path:
                path = path.rsplit("@", 1)[0]
            elif "@" in url and "//" not in rest:
                url = url.rsplit("@", 1)[0]
            target_repo = self.repo_of_url(url)
            if target_repo is None:
                return None
            base = target_repo.path
            full = base / path
        else:
            am = re.match(r"^([a-z][a-z0-9-]*):(?!//)(.*)$", s)
            if am:
                aliases = (repo.settings.get("aliases") or {}) if repo else {}
                if am.group(1) not in aliases:
                    self.add("GF033", src, "%s: undeclared alias %r" % (field, am.group(1)))
                    return False
                return self.resolve(aliases[am.group(1)].rstrip("/") + "//" + am.group(2) + (("#" + anchor) if anchor else ""), src, expect, field)
            if "@" in s:
                s = s.rsplit("@", 1)[0]
            if s.startswith("/"):
                full = info["folder"] / s.lstrip("/")
            else:
                full = src.parent / s
            base = repo.path
        norm = pathlib.Path(posixpath.normpath(str(full)))
        try:
            norm.relative_to(target_repo.path)
        except ValueError:
            self.add("GF036", src, "%s: %r climbs above its repository root" % (field, ref))
            return False
        target = self.follow(norm, src, field, ref)
        if target is False:
            return False
        if expect == "folder":
            if not target.is_dir():
                return target  # an absent declared child folder is simply empty
            return target
        if expect and target.is_file():
            tk = kind_of(target.name)
            if tk != expect:
                self.add("GF032", src, "%s: %r is a %s, expected a %s" % (field, ref, tk or "asset", expect))
                return False
        if anchor and target.is_file() and target in self.files:
            t = self.files[target]
            ids = {r.get("id") for r in t["data"].get("requirements", []) if isinstance(r, dict)}
            slugs = {re.sub(r"[^a-z0-9 -]", "", h.lower()).strip().replace(" ", "-")
                     for h in re.findall(r"^#+\s+(.*)$", t["body"], re.M)}
            if anchor not in ids and anchor.lower() not in slugs:
                self.add("GF030", src, "%s: anchor #%s not found" % (field, anchor))
        return target

    def follow(self, path, src, field, ref):
        seen = set()
        hops = 0
        while True:
            if path.exists():
                return path
            if path in seen or hops >= 8:
                self.add("GF031", src, "%s: redirect loop or more than 8 hops from %r" % (field, ref))
                return False
            seen.add(path)
            tomb = path.with_name(path.name[:-3] + ".moved.md") if path.name.endswith(".md") else None
            if tomb and tomb.exists() and tomb in self.files:
                to = str(self.files[tomb]["data"].get("to"))
                if re.match(r"^[a-z]+://|^[a-z][a-z0-9-]*:", to):
                    t = self.resolve(to, tomb, None, "moved.to")
                    return t if t else False
                path = pathlib.Path(posixpath.normpath(str(tomb.parent / to)))
                hops += 1
                continue
            info = self.files.get(src)
            folder = None
            for r in self.repos:
                for f in r.folders:
                    if f in path.parents:
                        folder = f
            if folder:
                rel = path.relative_to(folder)
                if len(rel.parts) > 1 and rel.parts[1] != "archive":
                    arch = folder / rel.parts[0] / "archive" / pathlib.Path(*rel.parts[1:])
                    if arch.exists():
                        return arch
            self.add("GF030", src, "%s: %r does not resolve" % (field, ref))
            return False

    def refs_of(self, k, d):
        """Yield (field, value, expected kind) for every reference a grainfile holds."""
        def one(f, v, exp=None):
            if v is not None:
                yield f, v, exp if exp is not None else EXPECTS.get((k, f))
        if k == "task":
            for v in d.get("depends_on") or []:
                yield from one("depends_on", v)
            if isinstance(d.get("template"), dict):
                yield from one("template", d["template"].get("ref"))
            yield from one("lifecycle", d.get("lifecycle"))
            yield from one("supersedes", d.get("supersedes"))
            for i in d.get("implements") or []:
                if isinstance(i, dict):
                    yield from one("implements", i.get("ref"), "design")
        elif k == "ref":
            yield from one("to", d.get("to"), "trigger" if d.get("fired_at") else "task")
            yield from one("previous", d.get("previous"))
        elif k == "run":
            for f in ("task", "from"):
                if isinstance(d.get(f), dict):
                    yield from one(f, d[f].get("ref"))
        elif k == "comment":
            for f in ("task", "run", "in_reply_to"):
                yield from one(f, d.get(f))
        elif k == "placement":
            yield from one("task", d.get("task"))
            yield from one("ref", d.get("ref"))
        elif k == "trigger":
            if isinstance(d.get("fires"), dict):
                yield from one("fires.template", d["fires"].get("template"))
        elif k == "moved":
            yield from one("to", d.get("to"), "")
        elif k == "asset":
            yield from one("asset", d.get("asset"), "")
        elif k in ("config", "template"):
            yield from one("lifecycle", d.get("lifecycle"))
        elif k == "capability":
            if isinstance(d.get("base"), dict):
                yield from one("base.capability", d["base"].get("capability"))
        for e in (d.get("context") or []) if k in ("task", "template") else []:
            if isinstance(e, dict) and "ref" in e:
                yield from one("context", e["ref"], "")

    def in_template(self, p):
        for parent in p.parents:
            m = parent / (parent.name + ".template.md")
            if m.exists() and m in self.files and self.files[m]["data"].get("form") == "folder":
                return parent
        return None

    def check_references(self):
        self.edges = {}
        for p, info in list(self.files.items()):
            tdir = self.in_template(p)
            for field, v, exp in self.refs_of(info["kind"], info["data"]):
                if v == DEFAULT_LIFECYCLE:
                    continue
                if tdir and not re.match(r"^[a-z]+://|^[a-z][a-z0-9-]*:", str(v)):
                    tgt = pathlib.Path(posixpath.normpath(str(p.parent / str(v).split("@")[0].split("#")[0])))
                    if field == "depends_on" and tdir not in tgt.parents:
                        self.add("GF061", p, "depends_on %r leaves the template folder" % v)
                        continue
                t = self.resolve(v, p, exp or None, field)
                if tdir and field == "depends_on" and t is False:
                    self.findings[-1].code = "GF061"
                if t:
                    self.edges.setdefault(p, []).append((field, t))
        # depends_on cycles (§9.4)
        graph = {p: [t for f, t in self.edges.get(p, []) if f == "depends_on"] for p in self.files}
        for code, nodes in (("GF034", [p for p in graph if not self.in_template(p)]),
                            ("GF062", [p for p in graph if self.in_template(p)])):
            for cyc in cycles(graph, set(nodes)):
                self.add(code, cyc[0], "depends_on cycle: %s" % " -> ".join(c.name for c in cyc))
        # tombstones (§8.3)
        for p, info in self.files.items():
            if info["kind"] == "moved":
                t = dict(self.edges.get(p, [])).get("to")
                if t and t in self.files:
                    mf = self.files[t]["data"].get("moved_from") or []
                    old = p.with_name(p.name.replace(".moved.md", ".md"))
                    if not any(pathlib.Path(posixpath.normpath(str(t.parent / m))) == old or str(m).endswith(str(old.relative_to(info["folder"]))) for m in mf):
                        self.add("GF044", p, "the moved grainfile does not list this tombstone in moved_from")

    # ---- one-to-many (§11) -----------------------------------------------
    def declared(self, p, field, default_suffix):
        d = self.files[p]["data"]
        v = d.get(field)
        if v is None:
            return (p.parent / (self.files[p]["stem"] + default_suffix)).resolve()
        t = self.resolve(v.rstrip("/") + "/" if not v.endswith("/") else v, p, "folder", field)
        return t.resolve() if t else None

    def check_relations(self):
        parents = {}  # folder -> (parent path, back-pointer field)
        for p, info in self.files.items():
            if self.in_template(p) and info["kind"] not in ("task", "ref"):
                pass
            if info["kind"] == "task":
                for field, suf in (("thread", ".thread"), ("placements", ".placements")):
                    f = self.declared(p, field, suf)
                    if f:
                        parents[(f, field)] = p
            elif info["kind"] == "ref":
                f = self.declared(p, "runs", ".runs")
                if f:
                    parents[(f, "runs")] = p
        open_runs = {}
        for p, info in self.files.items():
            k, d = info["kind"], info["data"]
            if k not in ("comment", "run", "placement"):
                continue
            folder = p.parent.resolve()
            owner = None
            if k == "comment":
                owner = parents.get((folder, "thread")) or parents.get((folder, "runs"))
            elif k == "run":
                owner = parents.get((folder, "runs"))
            else:
                owner = parents.get((folder, "placements"))
            if owner is None:
                self.add("GF042", p, "a %s in no declared folder: no parent lists %s/" % (k, p.parent.name))
                continue
            back = {"comment": d.get("task") or d.get("run"), "run": (d.get("from") or {}).get("ref") if isinstance(d.get("from"), dict) else None,
                    "placement": d.get("task")}[k]
            target = dict(self.edges.get(p, [])).get({"comment": "task" if d.get("task") else "run", "run": "from", "placement": "task"}[k])
            okind = self.files[owner]["kind"]
            if k == "comment" and d.get("run"):
                # a run comment's parent is the run record, inside the ref's runs folder
                if target is None or target.parent.resolve() != folder:
                    self.add("GF041", p, "run: %r is not a record in this runs folder" % back)
                continue
            if target is not None and target.resolve() != owner.resolve():
                self.add("GF041", p, "back-pointer %r names %s, but the folder is declared by %s" % (back, target.name, owner.name))
            if k == "run" and d.get("status") in ("accepted", "running", "awaiting_reply", "held"):
                key = (str((d.get("task") or {}).get("ref")), d.get("deployment"))
                if key in open_runs:
                    self.add("GF053", p, "a second open run of one task on one deployment (%s)" % open_runs[key].name)
                open_runs[key] = p

    # ---- lifecycles (§12) ------------------------------------------------
    def lifecycle_ok(self, p, d):
        states = d.get("states") or {}
        ok = True
        if not isinstance(states, dict):
            return False
        cats = {}
        for s, v in states.items():
            c = (v or {}).get("category") if isinstance(v, dict) else None
            if c not in CATEGORIES:
                self.add("GF050", p, "state %r has no category from %s" % (s, CATEGORIES))
                ok = False
            cats[s] = c
        init = d.get("initial")
        if init not in states or cats.get(init) != "proposed":
            self.add("GF050", p, "initial %r is not a proposed-category state" % init)
            ok = False
        if "done" not in cats.values():
            self.add("GF050", p, "no state has category done")
            ok = False
        for t in d.get("transitions") or []:
            for s in ([t.get("from")] if t.get("from") != "*" else []) + list(t.get("to") or []):
                if s not in states:
                    self.add("GF050", p, "transition names unknown state %r" % s)
                    ok = False
        for ev, s in (d.get("on_outcome") or {}).items():
            if s not in states:
                self.add("GF050", p, "on_outcome %s names unknown state %r" % (ev, s))
                ok = False
        for a, s in (d.get("aliases") or {}).items():
            if a in states or s in (d.get("aliases") or {}):
                self.add("GF054", p, "alias %r collides with a state or another alias" % a)
                ok = False
            elif s not in states:
                self.add("GF050", p, "alias %r names unknown state %r" % (a, s))
                ok = False
        return ok

    def lifecycle_for(self, p):
        d = self.files[p]["data"]
        cand = d.get("lifecycle")
        src = p
        if not cand:
            cfg = self.files.get(self.files[p]["folder"] / "grain.config.md")
            if cfg and cfg["data"].get("lifecycle"):
                cand, src = cfg["data"]["lifecycle"], self.files[p]["folder"] / "grain.config.md"
        if not cand or cand == DEFAULT_LIFECYCLE:
            return self.default_lifecycle
        t = dict(self.edges.get(src, [])).get("lifecycle")
        if t and t in self.files:
            return self.files[t]["data"]
        return None

    def check_lifecycles(self):
        self.lifecycle_ok(SCHEMAS / "lifecycles" / "default.lifecycle.md", self.default_lifecycle)
        for p, info in self.files.items():
            if info["kind"] == "lifecycle":
                self.lifecycle_ok(p, info["data"])
        for p, info in self.files.items():
            if info["kind"] != "task" or self.in_template(p):
                continue
            lc = self.lifecycle_for(p)
            if lc is None:
                continue
            st = info["data"].get("state")
            states = lc.get("states") or {}
            if st not in states and st not in (lc.get("aliases") or {}):
                self.add("GF051", p, "state %r is not a state of its lifecycle %r" % (st, lc.get("title")))
                continue
            for f, t in self.edges.get(p, []):
                if f == "depends_on" and t in self.files:
                    dl = self.lifecycle_for(t)
                    ds = self.files[t]["data"].get("state")
                    if dl and ((dl.get("states") or {}).get(ds) or {}).get("category") == "cancelled":
                        self.add("GF055", p, "dependency %s is cancelled; this task stays blocked" % t.name)

    # ---- templates (§13) -------------------------------------------------
    def check_templates(self):
        for p, info in self.files.items():
            if info["kind"] != "template":
                continue
            form = info["data"].get("form", "task")
            if form == "folder":
                if p.name != p.parent.name + ".template.md":
                    self.add("GF068", p, "a folder manifest is named <dir>/<dir>.template.md")
                    continue
                tdir = p.parent
                for q in tdir.rglob("*"):
                    if q.name == "grain.config.md":
                        self.add("GF011", q, "grain.config.md inside template folder %s" % tdir.name)
                    if q != p and q.name.endswith(".template.md") and q in self.files and self.files[q]["data"].get("form") == "folder":
                        self.add("GF066", q, "nested folder templates are deferred (spec §13.2)")
                self.dry_run(p, tdir)
            else:
                if p.name == p.parent.name + ".template.md" and form == "task":
                    self.add("GF068", p, "a template at a folder's top named for it must be form: folder")
                self.render_check(p, info, self.default_inputs(info["data"], p))
        # an instance folder: a sibling was materialised, so every grainfile here was (§13.3)
        for p, info in self.files.items():
            if info["data"].get("instantiated_from"):
                continue
            if any(q.parent == p.parent and i["data"].get("instantiated_from") for q, i in self.files.items()):
                self.add("GF067", p, "a grainfile in an instance folder without instantiated_from")

    def default_inputs(self, d, p):
        vals = {}
        for i in d.get("inputs") or []:
            if not isinstance(i, dict):
                continue
            if "default" in i:
                vals[i["name"]] = i["default"]
            elif i.get("required", True) and i.get("type") != "ref":
                self.add("GF063", p, "required input %r has no default and no value (dry run)" % i["name"])
                vals[i["name"]] = {"int": 1, "bool": True, "list": []}.get(i.get("type"), "x")
        return vals

    def render_check(self, p, info, vals):
        env = jinja2.Environment(undefined=jinja2.StrictUndefined, keep_trailing_newline=True, autoescape=False)
        for text in (info["body"], str(info["data"].get("title", ""))):
            if re.search(r"{%-?\s*(extends|block)\b|\.(items|format|keys|values)\(\)", text):
                self.add("GF065", p, "construct outside the Jinja subset")
                return
            try:
                env.from_string(text).render(**vals)
            except jinja2.TemplateError as e:
                self.add("GF065", p, "render: %s" % e)
                return
        for k in info["data"]:
            if k in ("title",):
                continue
            if "{{" in json.dumps(info["data"][k]) and k not in ("inputs",):
                self.add("GF065", p, "substitution in field %r; only body and title are templated (spec §13.5)" % k)

    def dry_run(self, manifest, tdir):
        vals = self.default_inputs(self.files[manifest]["data"], manifest)
        members = [q for q in self.files if tdir in q.parents and q != manifest]
        if len(members) > 500:
            self.add("GF006", manifest, "more than 500 members")
        for q in members:
            self.render_check(q, self.files[q], vals)
            for field, v, _ in self.refs_of(self.files[q]["kind"], self.files[q]["data"]):
                if field == "depends_on" and isinstance(v, str) and not re.match(r"^[a-z]+:", v):
                    tgt = pathlib.Path(posixpath.normpath(str(q.parent / v)))
                    if tdir not in tgt.parents and not any(f.code == "GF061" and f.path.endswith(q.name) for f in self.findings):
                        self.add("GF061", q, "depends_on %r leaves the template folder" % v)

    # ---- roots (§10.2) ---------------------------------------------------
    def check_roots(self):
        reached = set()
        for p, info in self.files.items():
            if info["kind"] != "config" or info["data"].get("role") != "deployment":
                continue
            d = info["data"]
            entries = [("ref", e) for e in (d.get("backlog") or {}).get("lanes", [])]
            entries += [("trigger", e) for e in d.get("triggers") or []]
            entries += [("memory", e) for e in d.get("memory") or []]
            for k, e in entries:
                path = e.get("path", "")
                if re.match(r"^[a-z]+://|^[a-z][a-z0-9-]*:", path):
                    t = self.resolve(path.rsplit("@", 1)[0] if "@" in path.split("//")[-1] else path, p, "folder", "root entry")
                    if not t:
                        continue
                    base = t
                else:
                    base = (info["folder"] / path).resolve()
                if not base.exists():
                    continue
                for q in sorted(base.rglob("*." + k + ".md")):
                    rel = q.relative_to(base)
                    if "archive" in rel.parts or any(fnmatch.fnmatch(str(rel), g.rstrip("/") + "*") for g in e.get("ignore") or []):
                        continue
                    if any(part.endswith(tuple(RESERVED_DIRS)) for part in rel.parts[:-1]):
                        continue
                    if self.in_template(q):
                        self.add("GF064", q, "template content listed by root entry %r" % path)
                        continue
                    reached.add(q.resolve())
                    for f, t in self.edges.get(q, []):
                        reached.add(t.resolve())
                if k == "trigger":
                    for g in base.rglob("*.gate.md"):
                        pass
        # orphans: deployment folders only (§10.2.5)
        for p, info in self.files.items():
            cfg = self.files.get(info["folder"] / "grain.config.md")
            if not cfg or cfg["data"].get("role") != "deployment":
                continue
            if info["kind"] in ("config", "template", "lifecycle", "design", "capability", "capability-base",
                                "gate", "asset", "moved", "comment", "run", "placement"):
                continue
            if self.in_template(p) or "archive" in p.relative_to(info["folder"]).parts:
                continue
            if p.resolve() not in reached and not any(p.resolve() == t.resolve() for es in self.edges.values() for _, t in es):
                self.add("GF040", p, "orphan: no root entry activates it and nothing reached references it")


def cycles(graph, nodes):
    out, color = [], {}
    def visit(n, stack):
        color[n] = 1
        stack.append(n)
        for m in graph.get(n, []):
            if m not in nodes:
                continue
            if color.get(m) == 1:
                out.append(stack[stack.index(m):] + [m])
            elif not color.get(m):
                visit(m, stack)
        stack.pop()
        color[n] = 2
    for n in sorted(nodes):
        if not color.get(n):
            visit(n, [])
    return out


# ---- GRAINOWNERS (§16.2) ---------------------------------------------------
RULE = re.compile(r"^(\S+?)(?::(\S+))?\s+(.+)$")
TRANS = re.compile(r"^state\(([a-z0-9_:*-]+)->([a-z0-9_:*-]+)\)$")
OWNER = re.compile(r"^@([^()\s]+)(?:\(([a-z,]+)\))?$")


def parse_owners(path):
    rules, errs = [], []
    for n, line in enumerate(path.read_text().splitlines(), 1):
        line = line.split(" #")[0].strip() if not line.lstrip().startswith("#") else ""
        if not line:
            continue
        m = RULE.match(line)
        if not m:
            errs.append(("GF070", "line %d: not <pattern>[:fields] <owners>" % n))
            continue
        pat, fields, owners = m.group(1), m.group(2), m.group(3).split()
        fl = []
        if fields:
            for f in re.split(r",(?![^(]*\))", fields):
                if not (TRANS.match(f) or re.match(r"^[a-z][a-z0-9_.]*$", f)):
                    errs.append(("GF070", "line %d: bad field %r" % (n, f)))
                fl.append(f)
        ol = []
        for o in owners:
            om = OWNER.match(o)
            if not om:
                errs.append(("GF070", "line %d: bad owner %r" % (n, o)))
                continue
            if not PRINCIPAL.match(om.group(1)):
                errs.append(("GF071", "line %d: unknown principal %r" % (n, om.group(1))))
            verbs = om.group(2).split(",") if om.group(2) else None
            if verbs and not set(verbs) <= {"create", "update", "delete", "move", "archive"}:
                errs.append(("GF070", "line %d: unknown verb in %r" % (n, o)))
            ol.append((om.group(1), verbs))
        rules.append((pat, fl, ol, n))
    return rules, errs


def gitmatch(pat, rel):
    anchored = pat.startswith("/")
    p = pat.lstrip("/")
    if p in ("*", "**"):
        return True
    if p.endswith("/"):
        return rel.startswith(p) if anchored else ("/" + rel).find("/" + p) >= 0
    rx = re.escape(p).replace(r"\*\*/", "(.*/)?").replace(r"\*\*", ".*").replace(r"\*", "[^/]*")
    if anchored or "/" in p:
        return re.fullmatch(rx + "(/.*)?", rel) is not None
    return any(re.fullmatch(rx, part) for part in rel.split("/"))


def lint_owners(linter):
    for repo in linter.repos:
        for folder in repo.folders:
            for g in folder.rglob("GRAINOWNERS"):
                _, errs = parse_owners(g)
                for code, msg in errs:
                    linter.add(code, g, msg)


def selector(sel, state, lc):
    if sel == "*":
        return True
    if sel.startswith("category:"):
        return ((lc.get("states") or {}).get(state) or {}).get("category") == sel[9:]
    return sel == state


def owners_for(folder, rel, field, trans, lc):
    """§16.2.3: nearest GRAINOWNERS with a matching line; last matching line wins."""
    d = (folder / rel).parent
    while True:
        g = d / "GRAINOWNERS"
        if g.exists():
            rules, _ = parse_owners(g)
            sub = str((folder / rel).relative_to(d))
            match = [r for r in rules if gitmatch(r[0], sub)]
            if match:
                whole = [r for r in match if not r[1]]
                best = whole[-1][2] if whole else None
                if field:
                    fr = [r for r in match if field in r[1]]
                    if fr:
                        best = fr[-1][2]
                if trans:
                    tr = []
                    for r in match:
                        for f in r[1]:
                            m = TRANS.match(f)
                            if m and selector(m.group(1), trans[0], lc) and selector(m.group(2), trans[1], lc):
                                tr.append(r)
                    if tr:
                        best = tr[-1][2]
                if best is not None:
                    return best, g
        if d == folder:
            return None, None
        d = d.parent


def authorize(base, head, actors):
    """§16.4, steps 3-5, for two trees of one repository (BASE and HEAD directories)."""
    base, head = pathlib.Path(base).resolve(), pathlib.Path(head).resolve()
    hl = Linter(head)
    hl.run()
    findings = [f for f in hl.findings if SEVERITY.get(f.code, "error") == "error"]
    bl = Linter(base)
    bl.run()
    principals = set(actors)
    def allowed(owners, verb):
        for name, verbs in owners or []:
            if name == "none":
                continue
            if name in principals or name == "authenticated" or ("grain:" + name[11:]) in principals:
                if verbs is None and verb != "delete":
                    return True
                if verbs and verb in verbs:
                    return True
        return False
    folders = [f for r in bl.repos for f in r.folders] or [base]
    for hp in sorted(head.rglob("*")):
        if hp.is_dir() or ".git" in hp.parts:
            continue
        rel = hp.relative_to(head)
        bp = base / rel
        folder = next((f for f in folders if f == bp.parent or f in bp.parents), None)
        if folder is None:
            continue
        frel = bp.relative_to(folder)
        if not bp.exists():
            ow, _ = owners_for(folder, frel, None, None, {})
            if not allowed(ow, "create"):
                findings.append(Finding("GF073", rel, "%s may not create this file" % sorted(principals)))
            continue
        if bp.read_bytes() == hp.read_bytes():
            continue
        if kind_of(hp.name) is None:
            ow, _ = owners_for(folder, frel, None, None, {})
            if not allowed(ow, "update"):
                findings.append(Finding("GF073", rel, "%s may not change this file" % sorted(principals)))
            continue
        bd, bb = parse(bp)
        hd, hb = parse(hp)
        changed = [k for k in set(bd) | set(hd) if bd.get(k) != hd.get(k)] + (["body"] if bb != hb else [])
        lc = {}
        if hp in hl.files:
            lc = hl.lifecycle_for(hp) or {} if hl.files[hp]["kind"] == "task" else {}
        for f in sorted(changed):
            if f == "state":
                trans = (bd.get("state"), hd.get("state"))
                allowedt = any((t.get("from") in ("*", trans[0])) and trans[1] in (t.get("to") or []) for t in lc.get("transitions") or [])
                if lc and not allowedt:
                    findings.append(Finding("GF052", rel, "transition %s -> %s is not in the lifecycle" % trans))
                ow, g = owners_for(folder, frel, "state", trans, lc)
                if not allowed(ow, "update"):
                    findings.append(Finding("GF074", rel, "%s may not move this task %s -> %s" % (sorted(principals), trans[0], trans[1])))
                continue
            ow, g = owners_for(folder, frel, f, None, lc)
            if not allowed(ow, "update"):
                findings.append(Finding("GF073", rel, "%s may not change field %r" % (sorted(principals), f)))
    for bp in sorted(base.rglob("*")):
        if bp.is_file() and ".git" not in bp.parts and not (head / bp.relative_to(base)).exists():
            folder = next((f for f in folders if f in bp.parents), None)
            if folder is None:
                continue
            ow, _ = owners_for(folder, bp.relative_to(folder), None, None, {})
            if not bp.name.endswith(".ref.md") or not allowed(ow, "delete"):
                findings.append(Finding("GF073", bp.relative_to(base), "%s may not delete this file" % sorted(principals)))
    return findings


def lint(top):
    l = Linter(top)
    l.run()
    lint_owners(l)
    return l.findings


def self_test():
    ok = True
    def fail(msg):
        nonlocal ok
        ok = False
        print("FAIL", msg)
    # 1. the examples lint with no errors
    ex = ROOT / "examples" / "grainfiles"
    errs = [f for f in lint(ex) if SEVERITY.get(f.code, "error") == "error"]
    for f in errs:
        fail("examples: %s" % f)
    # 2. each broken fixture produces exactly its expected codes
    bad = HERE / "testdata" / "bad"
    seen = set()
    for case in sorted(p for p in bad.iterdir() if p.is_dir()):
        want = set((case / "expect.txt").read_text().split())
        seen |= want
        if (case / "authorize.yaml").exists():
            a = yaml.safe_load((case / "authorize.yaml").read_text())
            got = {f.code for f in authorize(case / "base", case / "head", a["actors"])}
        else:
            got = {f.code for f in lint(case)}
        if got != want:
            fail("%s: want %s, got %s" % (case.name, sorted(want), sorted(got)))
        else:
            print("ok  ", case.name, sorted(got))
    # 3. good authorisation cases pass
    good = HERE / "testdata" / "good"
    for case in sorted(p for p in good.iterdir() if p.is_dir()) if good.exists() else []:
        a = yaml.safe_load((case / "authorize.yaml").read_text())
        got = [str(f) for f in authorize(case / "base", case / "head", a["actors"])]
        if got:
            fail("%s: want no findings, got %s" % (case.name, got))
        else:
            print("ok  ", case.name, "authorised")
    # 4. drift: the spec, the schemas and this linter agree
    spec = SPEC.read_text()
    spec_codes = sorted(set(re.findall(r"^\| (GF\d{3}) \|", spec, re.M)))
    if spec_codes != sorted(CODES):
        fail("spec lint table %s != linter codes %s" % (spec_codes, sorted(CODES)))
    for c in CODES:
        sev = re.search(r"^\| %s \| (\w+) \|" % c, spec, re.M)
        if sev and sev.group(1) != SEVERITY.get(c, "error"):
            fail("%s: spec says %s, linter %s" % (c, sev.group(1), SEVERITY.get(c, "error")))
    m = re.search(r"22\.1\. \*\*Kinds:\*\*(.*?)\(`suite`", spec, re.S)
    spec_kinds = sorted(re.findall(r"`([a-z-]+)`", m.group(1))) if m else []
    if spec_kinds != sorted(KINDS):
        fail("spec §22.1 kinds %s != linter kinds" % spec_kinds)
    have = sorted(p.name[:-12] for p in SCHEMAS.glob("*.schema.json") if p.name not in ("common.schema.json", "grainconfig.schema.json"))
    if have != sorted(KINDS):
        fail("schemas %s != kinds" % have)
    m = re.search(r"22\.3\. \*\*State categories:\*\*(.*?)\n\n", spec, re.S)
    if not m or sorted(re.findall(r"`([a-z]+)`", m.group(1))) != sorted(CATEGORIES):
        fail("spec §22.3 categories disagree with the linter")
    if json.loads((SCHEMAS / "common.schema.json").read_text())["$defs"]["category"]["enum"] != CATEGORIES:
        fail("common.schema.json categories disagree")
    uncovered = set(CODES) - seen - {"GF006", "GF021", "GF053", "GF080", "GF072"}
    if uncovered:
        fail("codes with no broken fixture: %s" % sorted(uncovered))
    print("self-test", "passed" if ok else "FAILED")
    return 0 if ok else 1


def main(argv):
    if argv[:1] == ["--self-test"]:
        return self_test()
    if argv[:1] == ["--authorize"]:
        actors = [argv[i + 1] for i, a in enumerate(argv) if a == "--actor"]
        fs = authorize(argv[1], argv[2], actors)
    else:
        fs = [f for t in argv for f in lint(t)]
    for f in fs:
        print(f)
    errors = [f for f in fs if SEVERITY.get(f.code, "error") == "error"]
    print("%d error(s), %d warning(s)" % (len(errors), len(fs) - len(errors)))
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
