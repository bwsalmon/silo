#!/usr/bin/env python3
"""grainlint: the reference linter for the Grainfile Format (docs/grainfiles-spec.md).

  grainlint.py TREE...                 lint each tree (a directory of repositories, or one repository)
  grainlint.py --authorize BASE HEAD --actor IDENTITY [--actor ...]
                                       authorise HEAD against BASE's GRAINOWNERS (§14.4); the
                                       actors are the pull request's author and its teams
  grainlint.py --render TREE TEMPLATE [NAME=VALUE ...]
                                       render a template in the sandbox (§11) and print the result
  grainlint.py --dirhash DIR           print the h1: hash of DIR, as an alias's sha256 (§8.2.2)
  grainlint.py --self-test             lint the examples (no errors), every fixture under
                                       testdata/bad (exactly the codes in its expect.txt), and
                                       check the spec, schemas and linter agree

A TREE is a directory of repositories, one per child directory (or a single
repository, if it holds grain.config.md or a .git marker at its top). Remote
references (git::https://…) are resolved through stubs.yaml at the tree's top,
mapping a repository URL to a child directory, at whatever ref they name; an
unmapped repository is external and is not resolved.

Needs python3-yaml, python3-jsonschema and python3-jinja2.
"""
import base64, hashlib, json, pathlib, posixpath, re, sys, urllib.parse
import jinja2, jinja2.sandbox, jsonschema, yaml

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parent.parent
SCHEMAS = ROOT / "schemas" / "grainfiles"
SPEC = ROOT / "docs" / "grainfiles-spec.md"
KINDS = ["config", "task", "comment", "template", "trigger", "gate", "capability", "design",
         "lifecycle", "moved", "record"]
CATEGORIES = ["proposed", "ready", "active", "waiting", "done", "cancelled"]
OUTCOMES = ["succeeded", "failed", "cancelled", "nothing_needed", "refused"]
SOURCES = ["schedule", "push", "merge", "pull_request", "design-change", "manual"]
SEVERITY = {"GF005": "warning", "GF022": "warning", "GF037": "warning", "GF038": "warning",
            "GF057": "warning", "GF072": "warning", "GF081": "warning"}
CODES = ["GF001", "GF003", "GF004", "GF005", "GF006", "GF007", "GF010", "GF011", "GF020", "GF021",
         "GF022", "GF030", "GF031", "GF032", "GF033", "GF034", "GF035", "GF036", "GF037", "GF038",
         "GF039", "GF043", "GF045", "GF050", "GF052", "GF056", "GF057", "GF061", "GF062", "GF063",
         "GF065", "GF066", "GF068", "GF070", "GF071", "GF072", "GF073", "GF081", "GF082", "GF083"]
RETIRED = ["GF002", "GF023", "GF040", "GF041", "GF042", "GF044", "GF046", "GF047", "GF048", "GF051",
           "GF053", "GF054", "GF055", "GF064", "GF067", "GF069", "GF074", "GF080", "GF090"]
FORMATS = [1]
STEM = re.compile(r"^(\d{8}T\d{6}Z|[a-z0-9])[a-z0-9._-]*$")
COMMENT_NAME = re.compile(r"^(\d{8}T\d{6}Z)-([a-z0-9][a-z0-9-]*)$")
CONTAINER = re.compile(r"^(.+)\.(task|capability|record)$")
CHILDREN = {"task": "thread", "record": "comments", "capability": None}
SHA = re.compile(r"^[0-9a-f]{40}$")
TAG = re.compile(r"^(refs/tags/.+|v?\d+(\.\d+)*([-+][0-9A-Za-z.+-]*)?)$")
IDENTITY = re.compile(r"^@(?:([a-z0-9.-]+):)?([A-Za-z0-9_.-]+)(/[A-Za-z0-9_.-]+)?$")
REQ_ID = re.compile(r"^[A-Z][A-Z0-9]*-[0-9]+$")
REFUSED = {
    "*": ["id"],
    "task": ["state", "placement", "runs", "thread", "approved_by"],
    "template": [],
    "trigger": ["fires_in", "overlap", "every", "daily", "weekly", "monthly", "gate", "human", "requires"],
    "capability": ["base", "reducers", "secrets", "approval", "grants"],
    "config": ["role", "backlog", "lanes", "triggers", "memory", "registry", "folders", "views", "lifecycle",
               "task_home", "moved_to", "archive_repo", "enforcement", "identity_map"],
}
EXPECTS = {  # (kind, field) -> expected kinds (§8.4)
    ("task", "depends_on"): ("task",), ("task", "supersedes"): ("task",), ("task", "capabilities"): ("capability",),
    ("template", "capabilities"): ("capability",), ("task", "lifecycle"): ("lifecycle",),
    ("template", "lifecycle"): ("lifecycle",), ("trigger", "fires.template"): ("template",),
    ("trigger", "fires.definition"): ("task",), ("trigger", "fires.review.template"): ("template",),
    ("task", "implements"): ("design",), ("record", "definition"): ("task", "template"),
    ("record", "trigger"): ("trigger",), ("comment", "in_reply_to"): ("comment",),
    ("comment", "supersedes"): ("comment",), ("comment", "record"): ("record",),
    ("config", "deployment.offers"): ("capability",), ("trigger", "on.design-change.designs"): ("design",),
    ("gate", "requires.security_review.template"): ("template",),
    ("gate", "requires.second_review.template"): ("template",),
}
MAX_HOPS = 8
MAX_OUTPUT = 256 * 1024


class Finding(Exception):
    def __init__(self, code, path, msg, severity=None):
        super().__init__(msg)
        self.code, self.path, self.msg = code, str(path), msg
        self.severity = severity or SEVERITY.get(code, "error")

    def __str__(self):
        return "%s %s %s: %s" % (self.code, self.severity, self.path, self.msg)


# ---------------------------------------------------------------- parsing (§5)

class Loader(yaml.SafeLoader):
    pass


def _mapping(loader, node, deep=False):
    keys = set()
    for k, _ in node.value:
        if k.tag != "tag:yaml.org,2002:str":
            raise yaml.constructor.ConstructorError(None, None, "non-string key", k.start_mark)
        if k.value in keys:
            raise yaml.constructor.ConstructorError(None, None, "duplicate key %r" % k.value, k.start_mark)
        keys.add(k.value)
    return loader.construct_mapping(node, deep)


Loader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping)
for _ch in "yYnN":  # §5.3: YAML 1.2 booleans only; a bare `on` key is the string "on"
    Loader.yaml_implicit_resolvers.pop(_ch, None)
Loader.yaml_implicit_resolvers["o"] = [r for r in Loader.yaml_implicit_resolvers.get("o", [])
                                       if r[0] != "tag:yaml.org,2002:bool"]
Loader.yaml_implicit_resolvers["O"] = [r for r in Loader.yaml_implicit_resolvers.get("O", [])
                                       if r[0] != "tag:yaml.org,2002:bool"]


def parse(path):
    """-> (front matter, body) or raises Finding."""
    raw = pathlib.Path(path).read_bytes()
    if raw.startswith(b"version https://git-lfs"):
        raise Finding("GF003", path, "a grainfile may not be an LFS pointer")
    if b"\0" in raw or raw.startswith(b"\xef\xbb\xbf"):
        raise Finding("GF003", path, "NUL or byte-order mark")
    try:
        text = raw.decode("utf-8").replace("\r\n", "\n")
    except UnicodeDecodeError:
        raise Finding("GF003", path, "not UTF-8")
    lines = text.split("\n")
    if lines[0].rstrip() != "---":
        raise Finding("GF003", path, "no front matter")
    end = next((i for i in range(1, len(lines)) if lines[i].rstrip() == "---"), None)
    if end is None:
        raise Finding("GF003", path, "unclosed front matter")
    src = "\n".join(lines[1:end])
    if re.search(r"(^|[\s\[{,:])[&*][A-Za-z0-9_-]+|<<\s*:|(^|\s)!", src, re.M):
        raise Finding("GF004", path, "anchors, aliases, merge keys and tags are not allowed")
    try:
        fm = yaml.load(src, Loader=Loader)
    except yaml.constructor.ConstructorError as e:
        raise Finding("GF004", path, str(e.problem))
    except yaml.YAMLError as e:
        raise Finding("GF003", path, "YAML: %s" % str(e).splitlines()[0])
    if fm is None:
        fm = {}
    if not isinstance(fm, dict):
        raise Finding("GF004", path, "front matter is not a mapping")
    return fm, "\n".join(lines[end + 1:])


def slug(identity):
    return re.sub(r"[^a-z0-9]+", "-", identity.lower()).strip("-")


def heading_slug(text):
    return re.sub(r"\s", "-", re.sub(r"[^\w\s-]", "", text.strip().lower()))


def sections(body):
    """Markdown headings -> [(level, text, start line, end line)] (§7.8)."""
    lines = body.split("\n")
    heads = [(i, len(m.group(1)), m.group(2)) for i, l in enumerate(lines)
             for m in [re.match(r"^(#{1,6})\s+(.*?)\s*#*\s*$", l)] if m]
    out = []
    for n, (i, lvl, text) in enumerate(heads):
        end = next((j for j, l2, _ in heads[n + 1:] if l2 <= lvl), len(lines))
        out.append((lvl, text, i, end))
    return out, lines


def requirement_hash(body, rid):
    secs, lines = sections(body)
    for lvl, text, start, end in secs:
        if text.startswith(rid) and (len(text) == len(rid) or not text[len(rid)].isalnum()):
            part = [l.rstrip() for l in lines[start:end]]
            while part and not part[-1]:
                part.pop()
            return "sha256:" + hashlib.sha256(("\n".join(part) + "\n").encode()).hexdigest()
    return None


def dirhash(top):
    """Go's dirhash Hash1 over the files under TOP, paths relative to it (§8.2.2)."""
    top = pathlib.Path(top)
    files = sorted(p.relative_to(top).as_posix() for p in top.rglob("*")
                   if p.is_file() and not p.is_symlink() and ".git" not in p.relative_to(top).parts)
    summary = "".join("%s  %s\n" % (hashlib.sha256((top / f).read_bytes()).hexdigest(), f) for f in files)
    return "h1:" + base64.b64encode(hashlib.sha256(summary.encode()).digest()).decode()


# ---------------------------------------------------------------- references (§8)

class Ref:
    """A parsed reference: form is remote, alias, rooted or relative."""
    def __init__(self, form, repo=None, path="", gitref=None, anchor=None, alias=None):
        self.form, self.repo, self.path, self.gitref, self.anchor, self.alias = form, repo, path, gitref, anchor, alias


def canon_repo(url):
    u = urllib.parse.urlsplit(url)
    host = (u.hostname or "").lower()
    if u.port and not (u.scheme == "https" and u.port == 443):
        host += ":%d" % u.port
    path = re.sub(r"\.git$", "", u.path.rstrip("/"))
    return "%s://%s%s" % (u.scheme, host, path)


def parse_ref(s):
    """-> Ref, or raises Finding('GF035') (§8.1)."""
    if not isinstance(s, str) or not s:
        raise Finding("GF035", "", "a reference must be a non-empty string")
    s, _, anchor = s.partition("#")
    anchor = anchor or None
    if s.startswith("git::"):
        rest = s[5:]
        m = re.match(r"^(https|file)://([^?]*)(?:\?(.*))?$", rest)
        if not m:
            raise Finding("GF035", "", "only git::https:// and git::file:// sources are allowed: %r" % s)
        scheme, body, query = m.groups()
        q = urllib.parse.parse_qs(query or "", keep_blank_values=True)
        if set(q) - {"ref"} or any(len(v) != 1 or not v[0] for v in q.values()):
            raise Finding("GF035", "", "only one ?ref= parameter is allowed: %r" % s)
        host_end = body.find("/") if scheme == "https" else 0
        if scheme == "https" and host_end <= 0:
            raise Finding("GF035", "", "no repository path: %r" % s)
        i = body.find("//", host_end if scheme == "https" else 1)
        repo, sub = (body[:i], body[i + 2:]) if i >= 0 else (body, "")
        return Ref("remote", repo=canon_repo("%s://%s" % (scheme, repo)), path=sub,
                   gitref=q.get("ref", [None])[0], anchor=anchor)
    if "://" in s or s.startswith("git@") or "::" in s:
        raise Finding("GF035", "", "remote references are go-getter strings beginning git::https:// (%r)" % s)
    m = re.match(r"^([a-z][a-z0-9-]*):(.+)$", s)
    if m:
        return Ref("alias", alias=m.group(1), path=m.group(2), anchor=anchor)
    if s.startswith("/"):
        return Ref("rooted", path=s[1:], anchor=anchor)
    return Ref("relative", path=s, anchor=anchor)


def pinned(gitref):
    return bool(gitref) and (bool(SHA.match(gitref)) or bool(TAG.match(gitref)))


# ---------------------------------------------------------------- the tree

class Folder:
    def __init__(self, repo, top):
        self.repo, self.top = repo, top  # repo: Repo; top: repo-relative posix path ("" for the root)
        self.config = {}
        self.aliases = {}  # name -> Ref
        self.owners = {}  # folder-relative dir -> [(pattern, fields, owners)]

    @property
    def dir(self):
        return self.repo.dir / self.top if self.top else self.repo.dir

    def rel(self, repo_rel):
        return repo_rel[len(self.top) + 1:] if self.top else repo_rel


class Repo:
    def __init__(self, d, url):
        self.dir, self.url = d, url
        self.files = sorted(p.relative_to(d).as_posix() for p in d.rglob("*")
                            if p.is_file() and ".git" not in p.relative_to(d).parts)


class Item:
    """One grainfile."""
    def __init__(self, folder, rel, kind, name):
        self.folder, self.rel, self.kind, self.name = folder, rel, kind, name  # rel: repo-relative
        self.fm, self.body, self.ok = {}, "", False

    @property
    def path(self):
        return self.folder.repo.dir / self.rel

    def __repr__(self):
        return self.rel


def classify(frel):
    """Folder-relative path -> ('grain', kind, name) | ('asset',) | ('owners',) | ('error', code, msg)."""
    segs = frel.split("/")
    leaf = segs[-1]
    conts = [i for i, s in enumerate(segs[:-1]) if CONTAINER.match(s)]
    if len(conts) > 1:
        return ("error", "GF007", "a container inside a container")
    if leaf == "GRAINOWNERS":
        return ("owners",)
    if conts:
        i = conts[0]
        stem, ckind = CONTAINER.match(segs[i]).groups()
        rest = segs[i + 1:]
        if rest == ["%s.md" % ckind]:
            return ("grain", ckind, stem)
        if rest[0] == "assets" and len(rest) > 1:
            return ("asset",)
        if CHILDREN[ckind] and rest[0] == CHILDREN[ckind]:
            if len(rest) == 2 and rest[1].endswith(".comment.md"):
                return ("grain", "comment", rest[1][:-11])
            if len(rest) > 2 and rest[1].endswith(".attachments"):
                return ("asset",)
        return ("error", "GF007", "%s/ may not hold %s" % (segs[i], "/".join(rest)))
    if leaf == "grain.config.md":
        return ("grain", "config", "grain") if len(segs) == 1 else ("error", "GF010", "nested grain folder")
    if leaf in ("task.md", "capability.md", "record.md"):
        return ("error", "GF007", "%s outside its container" % leaf)
    if leaf.endswith(".record.md"):
        return ("error", "GF007", "a record is always a folder <name>.record/")
    if leaf.endswith(".comment.md"):
        return ("error", "GF007", "a comment outside every thread/ and comments/")
    if leaf.endswith(".moved.md"):
        return ("grain", "moved", leaf[:-9])
    m = re.match(r"^(.+)\.([a-z][a-z0-9-]*)\.md$", leaf)
    if m and m.group(2) in KINDS and m.group(2) != "config":
        return ("grain", m.group(2), m.group(1))
    if m and not leaf.lower().startswith("readme"):
        return ("asset", "GF005")
    return ("asset",)


class Schemas:
    def __init__(self):
        store = {}
        for p in SCHEMAS.glob("*.schema.json"):
            s = json.loads(p.read_text())
            store[s["$id"]] = s
        self.store = store
        self.by_kind = {s["$id"].rsplit("/", 1)[-1][:-12]: s for s in store.values()}
        res = jsonschema.RefResolver.from_schema(self.by_kind["common"], store=store) if hasattr(jsonschema, "RefResolver") else None
        self.resolver = res

    def validate(self, kind, fm, path):
        s = self.by_kind[kind]
        props = s.get("properties", {})
        out, data = [], dict(fm)
        for k in list(fm):
            if k in REFUSED["*"] or k in REFUSED.get(kind, []):
                out.append(Finding("GF020", path, "refused field %r" % k))
                del data[k]
            elif k not in props and not k.startswith("x_"):
                out.append(Finding("GF022", path, "unknown field %r" % k))
                del data[k]
        v = jsonschema.Draft202012Validator(s, resolver=jsonschema.RefResolver.from_schema(s, store=self.store))
        for e in sorted(v.iter_errors(data), key=lambda e: list(e.path)):
            where = ".".join(str(x) for x in e.path) or "(top)"
            out.append(Finding("GF020", path, "%s: %s" % (where, e.message)))
        return out


# ---------------------------------------------------------------- the linter

class Linter:
    def __init__(self, top):
        self.top = pathlib.Path(top)
        self.findings = []
        self.schemas = Schemas()
        stubs = self.top / "stubs.yaml"
        self.stubs = {canon_repo(k): v for k, v in (yaml.safe_load(stubs.read_text()) or {}).items()} if stubs.exists() else {}
        if (self.top / "grain.config.md").exists() or (self.top / ".git").exists():
            dirs = [self.top]
        else:
            dirs = sorted(p for p in self.top.iterdir() if p.is_dir() and not p.name.startswith("."))
        back = {v: k for k, v in self.stubs.items()}
        self.repos = [Repo(d, back.get(d.name, "file://" + d.name)) for d in dirs]
        self.items = {}  # (repo dir, repo-relative path) -> Item
        self.folders = []

    def add(self, code, path, msg, severity=None):
        self.findings.append(Finding(code, path, msg, severity))

    def show(self, repo, rel):
        return "%s/%s" % (repo.dir.name, rel) if repo.dir != self.top else rel

    # ---- discovery
    def run(self):
        for repo in self.repos:
            tops = sorted(posixpath.dirname(f) for f in repo.files if posixpath.basename(f) == "grain.config.md")
            outer = []
            for t in tops:
                parent = next((o for o in outer if o == "" or t.startswith(o + "/")), None)
                if parent is not None:
                    self.add("GF010", self.show(repo, t + "/grain.config.md"), "grain folder nested in %r" % (parent or "/"))
                    continue
                outer.append(t)
            for t in outer:
                f = Folder(repo, t)
                self.folders.append(f)
                self.scan(f)
        for f in self.folders:
            self.configure(f)
        for it in list(self.items.values()):
            if it.ok:
                self.check_item(it)
        self.check_graphs()
        for f in self.folders:
            self.check_owners(f)
        return self.findings

    def scan(self, folder):
        repo = folder.repo
        files = [x for x in repo.files if not folder.top or x.startswith(folder.top + "/")]
        lower = {}
        for x in files:
            if x.lower() in lower:
                self.add("GF006", self.show(repo, x), "differs only in case from %s" % lower[x.lower()])
            lower[x.lower()] = x
            frel = folder.rel(x)
            if len(frel.encode()) > 1024 or any(len(s.encode()) > 255 for s in frel.split("/")):
                self.add("GF006", self.show(repo, x), "path too long")
            c = classify(frel)
            if c[0] == "error":
                self.add(c[1], self.show(repo, x), c[2])
            elif c[0] == "asset" and len(c) > 1:
                self.add(c[1], self.show(repo, x), "unregistered kind suffix")
            elif c[0] == "owners":
                d = posixpath.dirname(frel)
                folder.owners[d] = self.parse_owners(repo, x)
            elif c[0] == "grain":
                it = Item(folder, x, c[1], c[2])
                self.items[(repo.dir, x)] = it
                if c[1] == "comment":
                    m = COMMENT_NAME.match(c[2])
                    if not m:
                        self.add("GF001", self.show(repo, x), "a comment is named <compact UTC time>-<author slug>")
                elif c[1] not in ("config",) and not STEM.match(c[2]):
                    self.add("GF001", self.show(repo, x), "bad grainfile name %r" % c[2])
                try:
                    it.fm, it.body = parse(it.path)
                    it.ok = True
                except Finding as e:
                    e.path = self.show(repo, x)
                    self.findings.append(e)
        # containers: fixed file present, not both forms (§4.3.2)
        conts = {}
        for x in files:
            segs = folder.rel(x).split("/")
            for i, s in enumerate(segs[:-1]):
                m = CONTAINER.match(s)
                if m:
                    conts["/".join(segs[:i + 1])] = m.group(2)
        for c, ck in conts.items():
            full = (folder.top + "/" if folder.top else "") + c
            if full + "/%s.md" % ck not in repo.files:
                self.add("GF045", self.show(repo, full), "container without its %s.md" % ck)
            if full + ".md" in repo.files:
                self.add("GF045", self.show(repo, full), "present both as a file and as a folder")
        # template folders (§11.2)
        mans = [x for x in files if x.endswith(".template.md") and (self.items.get((repo.dir, x)) or Item(0, 0, 0, 0)).fm.get("form") == "folder"]
        for x in mans:
            d = posixpath.dirname(x)
            if posixpath.basename(x) != posixpath.basename(d) + ".template.md":
                self.add("GF068", self.show(repo, x), "a folder template's manifest is <dir>/<dir>.template.md")
            for y in files:
                if y.startswith(d + "/") and y != x:
                    it = self.items.get((repo.dir, y))
                    if posixpath.basename(y) == "grain.config.md":
                        self.add("GF011", self.show(repo, y), "grain.config.md inside a template folder")
                    elif it and it.kind == "template":
                        if it.fm.get("form") == "folder":
                            self.add("GF066", self.show(repo, y), "a template folder inside a template folder")
                        elif posixpath.dirname(y) == d:
                            self.add("GF068", self.show(repo, y), "a single-task template at a template folder's top")

    def template_dir(self, it):
        for x, other in self.items.items():
            if other.kind == "template" and other.ok and other.fm.get("form") == "folder" and x[0] == it.folder.repo.dir:
                d = posixpath.dirname(other.rel)
                if it.rel.startswith(d + "/") and it.rel != other.rel:
                    return d
        return None

    def configure(self, folder):
        it = self.items.get((folder.repo.dir, (folder.top + "/" if folder.top else "") + "grain.config.md"))
        if not it or not it.ok:
            return
        folder.config = it.fm
        if it.fm.get("format") not in FORMATS:
            self.add("GF021", self.show(folder.repo, it.rel), "unknown format %r" % it.fm.get("format"))
        seen = {}
        for name, a in (it.fm.get("aliases") or {}).items():
            src = a.get("source") if isinstance(a, dict) else None
            try:
                r = parse_ref(src)
                if r.form != "remote" or r.anchor:
                    raise Finding("GF035", "", "an alias source is a remote reference without an anchor")
            except Finding as e:
                self.add("GF035", self.show(folder.repo, it.rel), "alias %s: %s" % (name, e.msg))
                continue
            if name in ("git", "https", "file"):
                self.add("GF020", self.show(folder.repo, it.rel), "alias name %r is reserved" % name)
            folder.aliases[name] = r
            self.pin_check(folder, it, r, "alias %s" % name)
            if r.repo in seen and seen[r.repo][1] != r.gitref:
                self.add("GF039", self.show(folder.repo, it.rel), "aliases %s and %s name %s at different refs"
                         % (seen[r.repo][0], name, r.repo))
            seen.setdefault(r.repo, (name, r.gitref))
            want = a.get("sha256")
            stub = self.stubs.get(r.repo)
            if want and stub:
                d = self.top / stub / r.path.strip("/")
                got = dirhash(d) if d.is_dir() else None
                if got != want:
                    self.add("GF043", self.show(folder.repo, it.rel), "alias %s: tree hash %s, expected %s" % (name, got, want))

    def pin_check(self, folder, it, r, what):
        if not pinned(r.gitref):
            req = (folder.config.get("check") or {}).get("pinning") == "required"
            self.add("GF037", self.show(folder.repo, it.rel), "%s is %s" % (what, "branch-pinned" if r.gitref else "unpinned"),
                     "error" if req else None)

    # ---- resolution (§8.3)
    def resolve(self, it, s, field, hops=0):
        """-> the target Item, an asset/folder marker string, None (external), or raises Finding."""
        try:
            r = parse_ref(s)
        except Finding as e:
            raise Finding("GF035", "", "%s: %s" % (field, e.msg))
        folder, repo = it.folder, it.folder.repo
        if r.form == "alias":
            if r.alias not in folder.aliases:
                raise Finding("GF033", "", "%s: undeclared alias %r" % (field, r.alias))
            a = folder.aliases[r.alias]
            r = Ref("remote", repo=a.repo, path=posixpath.join(a.path.strip("/"), r.path) if a.path else r.path,
                    gitref=a.gitref, anchor=r.anchor, alias=r.alias)
        if r.form == "remote":
            if r.alias is None:
                if any(a.repo == r.repo for a in folder.aliases.values()):
                    self.add("GF038", self.show(repo, it.rel), "%s: %s is aliased; use the alias" % (field, r.repo))
                if field not in ("context", "definition", "trigger"):
                    self.pin_check(folder, it, r, "%s %s" % (field, s))
            stub = self.stubs.get(r.repo)
            if stub is None and r.repo.startswith("file://") and (self.top / r.repo[7:].strip("/")).is_dir():
                stub = r.repo[7:].strip("/")
            target_repo = next((x for x in self.repos if x.dir.name == stub), None)
            if target_repo is None:
                return None
            path = posixpath.normpath(r.path) if r.path.strip("/") else ""
            folder_ref = r.path.endswith("/") or not r.path
        else:
            target_repo = repo
            if r.form == "rooted":
                path = posixpath.normpath(posixpath.join(folder.top, r.path))
            else:
                path = posixpath.normpath(posixpath.join(posixpath.dirname(it.rel), r.path))
            folder_ref = r.path.endswith("/")
            if path.startswith(".."):
                raise Finding("GF036", "", "%s: %r climbs above the repository root" % (field, s))
        if path in (".", ""):
            path = ""
        if folder_ref:
            if path and not any(f.startswith(path.rstrip("/") + "/") for f in target_repo.files):
                raise Finding("GF030", "", "%s: no folder %r" % (field, s))
            return "folder"
        while True:
            if path in target_repo.files:
                break
            m = re.match(r"^(.*)\.(task|capability)\.md$", path)
            if m and "%s.%s/%s.md" % (m.group(1), m.group(2), m.group(2)) in target_repo.files:
                path = "%s.%s/%s.md" % (m.group(1), m.group(2), m.group(2))
                break
            if path.endswith(".record") and path + "/record.md" in target_repo.files:
                path += "/record.md"
                break
            tomb = path[:-3] + ".moved.md" if path.endswith(".md") else None
            if tomb and tomb in target_repo.files:
                if hops >= MAX_HOPS:
                    raise Finding("GF031", "", "%s: tombstone loop or more than %d hops at %r" % (field, MAX_HOPS, s))
                t = self.items.get((target_repo.dir, tomb))
                if not t or not t.ok or "to" not in t.fm:
                    raise Finding("GF030", "", "%s: broken tombstone %s" % (field, tomb))
                return self.resolve(t, t.fm["to"], field, hops + 1)
            raise Finding("GF030", "", "%s: %r does not resolve" % (field, s))
        target = self.items.get((target_repo.dir, path))
        if target is None:
            return "asset"
        if r.anchor and target.ok:
            if target.kind == "design":
                ids = [q.get("id") for q in target.fm.get("requirements") or [] if isinstance(q, dict)]
                if r.anchor not in ids:
                    raise Finding("GF030", "", "%s: no requirement %s in %s" % (field, r.anchor, path))
            elif r.anchor not in [heading_slug(t) for _, t, _, _ in sections(target.body)[0]]:
                raise Finding("GF030", "", "%s: no heading #%s in %s" % (field, r.anchor, path))
        return target

    def canonical(self, item):
        rel = item.rel
        m = re.match(r"^(.*)\.(task|capability)/\2\.md$", rel)
        if m:
            rel = "%s.%s.md" % (m.group(1), m.group(2))
        return "git::%s//%s" % (item.folder.repo.url, rel)

    def refs_of(self, it):
        fm, out = it.fm, []
        def add(field, v):
            if isinstance(v, str):
                out.append((field, v))
        for e in fm.get("context") or []:
            if isinstance(e, dict):
                add("context", e.get("ref"))
        for f in ("capabilities", "depends_on"):
            for v in fm.get(f) or []:
                add(f, v)
        for e in fm.get("implements") or []:
            if isinstance(e, dict):
                add("implements", e.get("ref"))
        for f in ("lifecycle", "supersedes", "superseded_by", "in_reply_to", "record", "to", "trigger"):
            add(f, fm.get(f))
        if it.kind == "record":
            add("definition", fm.get("definition"))
        for v in fm.get("attachments") or []:
            add("attachments", v)
        fires = fm.get("fires") if isinstance(fm.get("fires"), dict) else {}
        add("fires.template", fires.get("template"))
        add("fires.definition", fires.get("definition"))
        if isinstance(fires.get("review"), dict):
            add("fires.review.template", fires["review"].get("template"))
        on = fm.get("on") if isinstance(fm.get("on"), dict) else {}
        if isinstance(on.get("design-change"), dict):
            for v in on["design-change"].get("designs") or []:
                add("on.design-change.designs", v)
        req = fm.get("requires") if isinstance(fm.get("requires"), dict) else {}
        for k in ("security_review", "second_review"):
            if isinstance(req.get(k), dict):
                add("requires.%s.template" % k, req[k].get("template"))
        if it.kind == "config":
            dep = fm.get("deployment") if isinstance(fm.get("deployment"), dict) else {}
            for v in dep.get("offers") or []:
                add("deployment.offers", v)
        return out

    # ---- per-item checks
    def check_item(self, it):
        where = self.show(it.folder.repo, it.rel)
        fs = self.schemas.validate(it.kind, it.fm, where)
        self.findings += fs
        fm = it.fm
        it.targets = {}
        tdir = self.template_dir(it)
        for field, s in self.refs_of(it):
            if field == "context" and it.kind == "config":
                continue
            try:
                t = self.resolve(it, s, field)
            except Finding as e:
                code = e.code
                if field == "deployment.offers" and code == "GF030":
                    code = "GF081"
                self.add(code, where, e.msg)
                continue
            if tdir and isinstance(t, Item) and t.folder.repo is it.folder.repo and not t.rel.startswith(tdir + "/") \
                    and field in ("depends_on",) :
                self.add("GF061", where, "%s: %r leaves the template folder" % (field, s))
            want = EXPECTS.get((it.kind, field))
            if want and isinstance(t, Item) and t.kind not in want:
                self.add("GF032", where, "%s: %r is a %s, expected %s" % (field, s, t.kind, " or ".join(want)))
            elif want and t in ("asset", "folder"):
                self.add("GF032", where, "%s: %r is not a %s" % (field, s, " or ".join(want)))
            it.targets.setdefault(field, []).append((s, t))
        getattr(self, "check_" + it.kind, lambda it, where: None)(it, where)
        if it.kind in ("task", "template"):
            self.check_inputs(it, where, fm.get("inputs"))
        if it.kind == "template":
            self.render_check(it, where)
        elif it.kind == "task" and tdir:
            man = self.items.get((it.folder.repo.dir, "%s/%s.template.md" % (tdir, posixpath.basename(tdir))))
            self.render_check(it, where, sample_values((man.fm if man and man.ok else {}).get("inputs")))

    def check_comment(self, it, where):
        m = COMMENT_NAME.match(it.name)
        a, at = it.fm.get("author"), it.fm.get("at")
        if m and isinstance(a, str) and m.group(2) != slug(a):
            self.add("GF001", where, "author slug %r does not match author %r" % (m.group(2), a))
        if m and isinstance(at, str) and re.sub(r"[-:]", "", at) != m.group(1):
            self.add("GF001", where, "name time %s does not match at %s" % (m.group(1), at))

    def check_design(self, it, where):
        for q in it.fm.get("requirements") or []:
            if not isinstance(q, dict) or not isinstance(q.get("id"), str):
                continue
            h = requirement_hash(it.body, q["id"])
            if h is None:
                self.add("GF030", where, "requirement %s has no heading" % q["id"])
            elif q.get("hash") and q["hash"] != h:
                self.add("GF056", where, "requirement %s: hash %s, computed %s" % (q["id"], q["hash"], h))

    def check_task(self, it, where):
        for e, (s, t) in zip([e for e in it.fm.get("implements") or [] if isinstance(e, dict) and isinstance(e.get("ref"), str)],
                             it.targets.get("implements", [])):
            anchor = s.partition("#")[2]
            if isinstance(t, Item) and t.kind == "design" and anchor and e.get("hash"):
                h = requirement_hash(t.body, anchor)
                if h and h != e["hash"]:
                    self.add("GF057", where, "implements %s: requirement changed since (%s, now %s)" % (s, e["hash"], h))

    def check_lifecycle(self, it, where):
        fm = it.fm
        states = fm.get("states") if isinstance(fm.get("states"), dict) else {}
        bad = []
        for n, st in states.items():
            if not isinstance(st, dict) or st.get("category") not in CATEGORIES:
                bad.append("state %s has no category of §17.3" % n)
        if fm.get("initial") not in states or (states.get(fm.get("initial")) or {}).get("category") != "proposed":
            bad.append("initial must be a proposed-category state")
        for t in fm.get("transitions") or []:
            if isinstance(t, dict):
                for s in ([t.get("from")] if t.get("from") != "*" else []) + list(t.get("to") or []):
                    if s not in states:
                        bad.append("transition names unknown state %r" % s)
        if not any(isinstance(s, dict) and s.get("category") == "done" for s in states.values()):
            bad.append("no done-category state")
        for b in bad:
            self.add("GF050", where, b)

    def check_trigger(self, it, where):
        on = it.fm.get("on")
        if isinstance(on, dict):
            if len(on) != 1:
                self.add("GF020", where, "on: holds exactly one source")
            for k in on:
                if k not in SOURCES:
                    self.add("GF020", where, "unknown or reserved trigger source %r: never fires" % k)
            sch = on.get("schedule")
            if isinstance(sch, dict) and isinstance(sch.get("cron"), str) and len(sch["cron"].split()) != 5:
                self.add("GF020", where, "schedule.cron is a five-field cron expression")
        fires = it.fm.get("fires") if isinstance(it.fm.get("fires"), dict) else {}
        for s, t in it.targets.get("fires.template", []) + it.targets.get("fires.definition", []):
            if isinstance(t, Item) and t.ok:
                self.check_values(where, t.fm.get("inputs"), fires.get("inputs") or {})

    def check_record(self, it, where):
        fm = it.fm
        has_d, has_t = "definition" in fm, "task" in fm
        if has_d == has_t:
            self.add("GF083", where, "a record has exactly one of definition or task")
        if has_d:
            try:
                r = parse_ref(fm["definition"])
                if r.form != "remote" or not SHA.match(r.gitref or ""):
                    raise Finding("GF083", "", "")
            except Finding:
                self.add("GF083", where, "definition must be a git:: reference pinned with ?ref=<40-hex sha>")
        s, f = fm.get("started_at"), fm.get("finished_at")
        if isinstance(s, str) and isinstance(f, str) and f < s:
            self.add("GF020", where, "finished_at is before started_at")
        dep = it.folder.config.get("deployment") if isinstance(it.folder.config.get("deployment"), dict) else None
        if dep and isinstance(fm.get("deployment"), str) and fm["deployment"].lower() == str(dep.get("account", "")).lower():
            offers = set()
            for o in dep.get("offers") or []:
                try:
                    t = self.resolve(self.items[(it.folder.repo.dir, (it.folder.top + "/" if it.folder.top else "") + "grain.config.md")], o, "offers")
                    if isinstance(t, Item):
                        offers.add(self.canonical(t))
                except Finding:
                    pass
            for s2, t in it.targets.get("definition", []):
                if not isinstance(t, Item) or not t.ok:
                    continue
                reqs = [q for q in t.fm.get("capabilities") or [] if isinstance(q, str)]
                for q in reqs:
                    try:
                        c = self.resolve(t, q, "capabilities")
                    except Finding:
                        continue
                    if isinstance(c, Item) and self.canonical(c) not in offers:
                        self.add("GF082", where, "the work requested %s, which %s does not offer" % (self.canonical(c), fm["deployment"]))

    # ---- inputs and rendering (§11)
    def check_inputs(self, it, where, decls):
        names = set()
        for d in decls or []:
            if not isinstance(d, dict):
                continue
            if d.get("name") in names:
                self.add("GF020", where, "input %r declared twice" % d.get("name"))
            names.add(d.get("name"))
            if d.get("type") == "enum" and not d.get("options"):
                self.add("GF020", where, "enum input %r without options" % d.get("name"))
            if "default" in d and not type_ok(d, d["default"]):
                self.add("GF063", where, "input %r: default %r is not a %s" % (d.get("name"), d["default"], d.get("type", "string")))

    def check_values(self, where, decls, vals):
        for d in decls or []:
            if not isinstance(d, dict):
                continue
            n = d.get("name")
            if n in vals:
                if not type_ok(d, vals[n]):
                    self.add("GF063", where, "input %r: %r is not a valid %s" % (n, vals[n], d.get("type", "string")))
            elif d.get("required") and "default" not in d:
                self.add("GF063", where, "required input %r missing" % n)

    def render_check(self, it, where, vals=None):
        try:
            render(it, vals if vals is not None else sample_values(it.fm.get("inputs")))
        except Finding as e:
            self.add(e.code, where, e.msg)

    # ---- graphs
    def check_graphs(self):
        graph = {}
        for it in self.items.values():
            if it.ok and it.kind == "task":
                graph[it] = [t for _, t in getattr(it, "targets", {}).get("depends_on", []) if isinstance(t, Item)]
        for cyc in cycles(graph):
            tdirs = {self.template_dir(x) for x in cyc}
            code = "GF062" if len(tdirs) == 1 and None not in tdirs else "GF034"
            self.add(code, self.show(cyc[0].folder.repo, cyc[0].rel), "depends_on cycle: %s" % " -> ".join(x.rel for x in cyc))

    # ---- GRAINOWNERS (§14.2)
    def parse_owners(self, repo, rel):
        rules = []
        for n, line in enumerate((repo.dir / rel).read_text().splitlines(), 1):
            line = line.split("#", 1)[0].strip() if not line.lstrip().startswith("#") else ""
            if not line:
                continue
            parts = line.split()
            if len(parts) < 2:
                self.add("GF070", "%s:%d" % (self.show(repo, rel), n), "a rule is a pattern and owners")
                continue
            pat, fields = parts[0], []
            if ":" in pat:
                pat, fl = pat.rsplit(":", 1)
                fields = fl.split(",")
                if not all(re.match(r"^[a-z][a-z0-9_.]*$", f) for f in fields):
                    self.add("GF070", "%s:%d" % (self.show(repo, rel), n), "bad field list %r" % fl)
                    continue
            owners = []
            for o in parts[1:]:
                create = o.endswith("(create)")
                base = o[:-8] if create else o
                if base == "@none" and len(parts) == 2:
                    pass
                elif base == "*" or (IDENTITY.match(base) and not IDENTITY.match(base).group(1)):
                    pass
                elif re.match(r"^@?[^\s()]+(\(.*\))?$", o):
                    self.add("GF071", "%s:%d" % (self.show(repo, rel), n), "%r is not @login, @org/team or *" % o)
                    continue
                else:
                    self.add("GF070", "%s:%d" % (self.show(repo, rel), n), "bad owner %r" % o)
                    continue
                owners.append((base.lower(), create))
            rules.append((pat, fields, owners))
        return rules

    def check_owners(self, folder):
        if not folder.owners:
            return
        for (rd, rel), it in self.items.items():
            if it.folder is folder and owners_for(folder, folder.rel(rel), None) is None:
                self.add("GF072", self.show(folder.repo, rel), "no GRAINOWNERS rule governs this file")


def type_ok(d, v):
    t = d.get("type", "string")
    if t == "integer":
        return isinstance(v, int) and not isinstance(v, bool)
    if t == "boolean":
        return isinstance(v, bool)
    if t == "list":
        return isinstance(v, list) and all(isinstance(x, str) for x in v)
    if t == "enum":
        return v in (d.get("options") or [])
    return isinstance(v, str)


def sample_values(decls):
    out = {}
    for d in decls or []:
        if not isinstance(d, dict) or not isinstance(d.get("name"), str):
            continue
        if "default" in d:
            out[d["name"]] = d["default"]
            continue
        t = d.get("type", "string")
        out[d["name"]] = {"integer": 1, "boolean": True, "list": ["a", "b"],
                          "enum": (d.get("options") or [""])[0]}.get(t, "sample")
    return out


class FolderLoader(jinja2.BaseLoader):
    """Includes, imports and extends name files relative to the grain folder's top (§11.3)."""
    def __init__(self, folder):
        self.folder = folder

    def get_source(self, env, name):
        p = posixpath.normpath(name.lstrip("/"))
        if p.startswith("..") or p == ".":
            raise Finding("GF061", "", "include %r leaves the grain folder" % name)
        f = self.folder.dir / p
        if not f.is_file():
            raise Finding("GF065", "", "include %r not found" % name)
        return f.read_text(), str(f), lambda: True


def environment(folder):
    env = jinja2.sandbox.ImmutableSandboxedEnvironment(
        loader=FolderLoader(folder), undefined=jinja2.StrictUndefined, autoescape=False,
        keep_trailing_newline=True)
    env.filters.pop("random", None)
    env.globals.pop("lipsum", None)
    jinja2.sandbox.MAX_RANGE = 10000
    return env


def render(it, vals):
    """Render a template's or member's title and body (§11) -> (title, body)."""
    env = environment(it.folder)
    out = []
    for src in (str(it.fm.get("title", "")), it.body):
        try:
            text = env.from_string(src).render(**vals)
        except Finding:
            raise
        except jinja2.exceptions.SecurityError as e:
            raise Finding("GF065", "", "sandbox: %s" % e)
        except jinja2.exceptions.TemplateError as e:
            raise Finding("GF065", "", "template: %s" % e)
        except Exception as e:  # noqa: a template that fails to render for any reason
            raise Finding("GF065", "", "render failed: %s" % e)
        if len(text.encode()) > MAX_OUTPUT:
            raise Finding("GF065", "", "rendered output over 256 KiB")
        out.append(text)
    return out[0], out[1]


def cycles(graph):
    out, state = [], {}
    def visit(n, stack):
        state[n] = 1
        stack.append(n)
        for m in graph.get(n, []):
            if state.get(m) == 1:
                out.append(stack[stack.index(m):])
            elif m not in state:
                visit(m, stack)
        stack.pop()
        state[n] = 2
    for n in list(graph):
        if n not in state:
            visit(n, [])
    return out


def gitmatch(pat, rel):
    """CODEOWNERS/gitignore matching of a folder-relative path."""
    anchored = pat.startswith("/") or "/" in pat.rstrip("/")
    p = pat.lstrip("/")
    if p.endswith("/"):
        p += "**"
    rx = ""
    i = 0
    while i < len(p):
        if p[i:i + 3] == "**/":
            rx += "(?:.*/)?"
            i += 3
        elif p[i:i + 2] == "**":
            rx += ".*"
            i += 2
        elif p[i] == "*":
            rx += "[^/]*"
            i += 1
        elif p[i] == "?":
            rx += "[^/]"
            i += 1
        else:
            rx += re.escape(p[i])
            i += 1
    rx = ("^" if anchored else "^(?:.*/)?") + rx + "(?:/.*)?$"
    return re.match(rx, rel) is not None


def owners_for(folder, frel, field):
    """The owners governing FREL (folder-relative), or FIELD of it; None if no rule (§14.2.3)."""
    d = posixpath.dirname(frel)
    while True:
        rules = folder.owners.get(d)
        if rules:
            sub = frel[len(d) + 1:] if d else frel
            whole = fieldo = None
            for pat, fields, owners in rules:
                if gitmatch(pat, sub):
                    if not fields:
                        whole = owners
                    elif field in fields:
                        fieldo = owners
            if whole is not None or fieldo is not None:
                return fieldo if fieldo is not None else whole
        if not d:
            return None
        d = posixpath.dirname(d)


# ---------------------------------------------------------------- the forge check (§14.4)

FIXED_AUTHOR = {"comment": "author", "record": "deployment", "moved": "moved_by"}


def authorize(base, head, actors):
    actors = {a.lower() for a in actors}
    lb, lh = Linter(base), Linter(head)
    lb.run()
    out = [f for f in lh.run() if f.severity == "error"]
    def allowed(owners, created):
        return any(o == "*" or o in actors for o, create in owners if not create or created) and \
            not any(o == "@none" for o, _ in owners)
    bfold = {(f.repo.dir.name, f.top): f for f in lb.folders}
    for hf in lh.folders:
        bf = bfold.get((hf.repo.dir.name, hf.top))
        if bf is None:
            continue
        brepo, hrepo = bf.repo, hf.repo
        bfiles = {bf.rel(x) for x in brepo.files if not bf.top or x.startswith(bf.top + "/")}
        hfiles = {hf.rel(x) for x in hrepo.files if not hf.top or x.startswith(hf.top + "/")}
        def read(repo, f, frel):
            return (repo.dir / ((f.top + "/") if f.top else "") / frel).read_bytes()
        folds = set()
        for p in bfiles - hfiles:
            m = re.match(r"^(.*)\.(task|capability)\.md$", p)
            q = "%s.%s/%s.md" % (m.group(1), m.group(2), m.group(2)) if m else None
            if q and q in hfiles - bfiles and read(brepo, bf, p) == read(hrepo, hf, q):
                folds |= {p, q}
        owner = str(bf.config.get("owner", "")).lower()
        for p in sorted((bfiles | hfiles) - folds):
            created = p not in bfiles
            if not created and p in hfiles and read(brepo, bf, p) == read(hrepo, hf, p):
                continue
            full = (hf.top + "/" if hf.top else "") + p
            c = classify(p)
            kind = c[1] if c[0] == "grain" else None
            show = lh.show(hrepo, full)
            if kind in ("comment", "record", "moved") and not created:
                out.append(Finding("GF052", show, "a %s never changes once committed" % kind))
                continue
            if created and p.endswith(".md") and p[:-3] + ".moved.md" in bfiles:
                out.append(Finding("GF052", show, "a tombstone holds this path"))
                continue
            hit = lh.items.get((hrepo.dir, full))
            if created and kind in FIXED_AUTHOR and hit and hit.ok:
                who = str(hit.fm.get(FIXED_AUTHOR[kind], "")).lower()
                if who not in actors:
                    out.append(Finding("GF073", show, "%s is %s, not the actor" % (FIXED_AUTHOR[kind], who)))
                    continue
            fields = [None]
            if not created and p in hfiles and kind:
                bit = lb.items.get((brepo.dir, (bf.top + "/" if bf.top else "") + p))
                if bit and bit.ok and hit and hit.ok:
                    fields = [k for k in set(bit.fm) | set(hit.fm) if bit.fm.get(k) != hit.fm.get(k)]
                    if bit.body != hit.body:
                        fields.append("body")
                    if kind == "config" and "owner" in fields and owner not in actors:
                        out.append(Finding("GF073", show, "only the folder's owner changes owner"))
                        continue
            for fld in fields:
                o = owners_for(bf, p, fld)
                ok = allowed(o, created) if o is not None else owner in actors
                if not ok:
                    out.append(Finding("GF073", show, "%s may not %s %s" % (
                        "/".join(sorted(actors)), "create" if created else ("delete" if p not in hfiles else "change"),
                        p + (":" + fld if fld else ""))))
                    break
    return out


# ---------------------------------------------------------------- entry points

def lint(top):
    return Linter(top).run()


def self_test():
    ok = True
    def fail(msg):
        nonlocal ok
        ok = False
        print("FAIL", msg)
    ex = ROOT / "examples" / "grainfiles"
    for f in lint(ex):
        if f.severity == "error":
            fail("examples: %s" % f)
    seen = set()
    bad = HERE / "testdata" / "bad"
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
            for f in (lint(case) if not (case / "authorize.yaml").exists() else []):
                print("     ", f)
        else:
            print("ok  ", case.name, sorted(got))
    good = HERE / "testdata" / "good"
    for case in sorted(p for p in good.iterdir() if p.is_dir()) if good.exists() else []:
        a = yaml.safe_load((case / "authorize.yaml").read_text())
        got = [str(f) for f in authorize(case / "base", case / "head", a["actors"])]
        if got:
            fail("%s: want no findings, got %s" % (case.name, got))
        else:
            print("ok  ", case.name, "authorised")
    spec = SPEC.read_text()
    spec_codes = sorted(set(re.findall(r"^\| (GF\d{3}) \|", spec, re.M)))
    if spec_codes != sorted(CODES):
        fail("spec lint table %s != linter codes %s" % (spec_codes, sorted(CODES)))
    for c in CODES:
        sev = re.search(r"^\| %s \| (\w+) \|" % c, spec, re.M)
        if sev and sev.group(1) != SEVERITY.get(c, "error"):
            fail("%s: spec says %s, linter %s" % (c, sev.group(1), SEVERITY.get(c, "error")))
    m = re.search(r"17\.1\. \*\*Kinds:\*\*(.*?)\(`suite`", spec, re.S)
    if not m or sorted(re.findall(r"`([a-z-]+)`", m.group(1))) != sorted(KINDS):
        fail("spec §17.1 kinds disagree with the linter")
    have = sorted(p.name[:-12] for p in SCHEMAS.glob("*.schema.json") if p.name != "common.schema.json")
    if have != sorted(KINDS):
        fail("schemas %s != kinds" % have)
    m = re.search(r"17\.3\. \*\*State categories:\*\*(.*?)\n\n", spec, re.S)
    if not m or sorted(re.findall(r"`([a-z]+)`", m.group(1))) != sorted(CATEGORIES):
        fail("spec §17.3 categories disagree with the linter")
    m = re.search(r"17\.2\. \*\*Trigger sources:\*\*(.*?); reserved", spec, re.S)
    if not m or sorted(re.findall(r"`([a-z_-]+)`", m.group(1))) != sorted(SOURCES):
        fail("spec §17.2 trigger sources disagree with the linter")
    m = re.search(r"17\.5\. \*\*Record outcomes:\*\*(.*?)\n\n", spec, re.S)
    if not m or sorted(re.findall(r"`([a-z_]+)`", m.group(1))) != sorted(OUTCOMES):
        fail("spec §17.5 outcomes disagree with the linter")
    common = json.loads((SCHEMAS / "common.schema.json").read_text())["$defs"]
    if common["category"]["enum"] != CATEGORIES:
        fail("common.schema.json categories disagree")
    if json.loads((SCHEMAS / "record.schema.json").read_text())["properties"]["outcome"]["enum"] != OUTCOMES:
        fail("record.schema.json outcomes disagree")
    m = re.search(r"17\.7\. \*\*Retired lint codes:\*\*(.*?)\n\n", spec, re.S)
    if not m or sorted(set(re.findall(r"`(GF\d{3})`", m.group(1)))) != sorted(RETIRED) or set(RETIRED) & set(CODES):
        fail("spec §17.7 retired codes disagree with the linter")
    uncovered = set(CODES) - seen - {"GF006"}
    if uncovered:
        fail("codes with no broken fixture: %s" % sorted(uncovered))
    print("self-test", "passed" if ok else "FAILED")
    return 0 if ok else 1


def main(argv):
    if argv[:1] == ["--self-test"]:
        return self_test()
    if argv[:1] == ["--dirhash"]:
        print(dirhash(argv[1]))
        return 0
    if argv[:1] == ["--render"]:
        l = Linter(argv[1])
        l.run()
        it = next(i for (d, r), i in l.items.items() if str(d / r).endswith(argv[2]))
        tdir = l.template_dir(it)
        man = l.items.get((it.folder.repo.dir, "%s/%s.template.md" % (tdir, posixpath.basename(tdir)))) if tdir else it
        vals = sample_values(man.fm.get("inputs"))
        vals.update(dict(a.split("=", 1) for a in argv[3:]))
        title, body = render(it, vals)
        print("# " + title)
        print(body)
        return 0
    if argv[:1] == ["--authorize"]:
        actors = [argv[i + 1] for i, a in enumerate(argv) if a == "--actor"]
        fs = authorize(argv[1], argv[2], actors)
    else:
        fs = [f for t in argv for f in lint(t)]
    for f in fs:
        print(f)
    errors = [f for f in fs if f.severity == "error"]
    print("%d error(s), %d warning(s)" % (len(errors), len(fs) - len(errors)))
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
