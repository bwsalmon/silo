#!/usr/bin/env python3
"""grainlint: the reference linter for the Grainfile Format (docs/grainfiles-spec.md).

  grainlint.py [--var KEY=VALUE ...] TREE...
                             lint each tree (a directory of repositories, or one repository)
  grainlint.py --self-test   lint the examples (no errors or warnings), every fixture under
                             testdata/bad (exactly the codes in its expect.txt), and
                             check the spec, schemas and linter agree

A TREE is a directory of repositories, one per child directory (or a single
repository). Remote references (https://…) are resolved through stubs.yaml
at the tree's top, mapping a repository URL to a child directory; an unmapped
repository is external and is not resolved.

Needs python3-yaml, python3-jsonschema, and python3-jinja2.
"""
import datetime, json, pathlib, posixpath, re, sys, urllib.parse, zoneinfo
import jinja2, jinja2.meta, jinja2.nodes, jinja2.sandbox
import jsonschema, yaml

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parent.parent
SCHEMAS = ROOT / "schemas" / "grainfiles"
SPEC = ROOT / "docs" / "grainfiles-spec.md"
SPEC_SUMMARY = ROOT / "docs" / "grainfile-v1.md"

DEFAULT_VERSION = "v1"
VERSIONS = {
    "v1": {
        "grainfile": "v1",
        "spec_summary_path": "docs/grainfile-v1.md",
        "spec_path": "docs/grainfiles-spec.md",
        "schemas": {
            "common": "schemas/grainfiles/common.schema.json",
            "task": "schemas/grainfiles/task.schema.json",
            "process": "schemas/grainfiles/process.schema.json",
        },
    },
}


def resolve_version(version=None):
    v = DEFAULT_VERSION if version is None else version
    if not isinstance(v, str) or v not in VERSIONS:
        return None
    return dict(VERSIONS[v])


KINDS = ["task", "process"]
SOURCES = ["schedule", "push", "merge", "pull_request", "workflow", "manual", "scenario"]
SEVERITY = {"GF005": "warning", "GF022": "warning"}
CODES = ["GF001", "GF003", "GF004", "GF005", "GF006", "GF020", "GF022",
         "GF030", "GF032", "GF033", "GF034", "GF035", "GF036"]
STEM = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")
REPO_SLUG_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._-]+$")
USER_IDENTITY = re.compile(r"^[^<>\s][^<>]*\s+<[^<>\s@]+@[^<>\s@]+\.[^<>\s@]+>$")
ANCHOR_RE = re.compile(r"^[A-Za-z0-9_.-]+$")
TIME_RE = re.compile(r"^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|\+(?:[01]\d|2[0-3]):[0-5]\d|-(?!00:00)(?:[01]\d|2[0-3]):[0-5]\d)$")
DATE_RE = re.compile(r"^\d{4}-\d{2}-\d{2}$")
IANA_TZ_RE = re.compile(r"^(UTC|Etc/(?:UTC|GMT)|[A-Z][A-Za-z_+-]+(?:/[A-Za-z_+-]+)+)$")
WORKFLOW_EVENTS = {
    "triggered": "triggered",
    "started": "triggered",
    "succeeded": "succeeded",
    "success": "succeeded",
    "failed": "failed",
    "failure": "failed",
}


def parse_time_utc(s, allow_date=False):
    if not isinstance(s, str):
        return None
    if allow_date and DATE_RE.match(s):
        try:
            d = datetime.date.fromisoformat(s)
            return datetime.datetime(d.year, d.month, d.day, 0, 0, 0, tzinfo=datetime.timezone.utc)
        except ValueError:
            return None
    m = TIME_RE.match(s)
    if not m:
        return None
    try:
        dt = datetime.datetime.strptime(m.group(1) + "T" + m.group(2), "%Y-%m-%dT%H:%M:%S")
    except ValueError:
        return None
    frac = m.group(3)
    if frac:
        dt = dt.replace(microsecond=int((frac + "000000")[:6]))
    tz_part = m.group(4)
    if tz_part == "Z":
        tz = datetime.timezone.utc
    else:
        sign = 1 if tz_part[0] == "+" else -1
        hh = int(tz_part[1:3])
        mm = int(tz_part[4:6])
        tz = datetime.timezone(datetime.timedelta(minutes=sign * (hh * 60 + mm)))
    return dt.replace(tzinfo=tz).astimezone(datetime.timezone.utc)


def valid_time_or_date(s, allow_date=False):
    return parse_time_utc(s, allow_date=allow_date) is not None


def valid_iana_timezone(tz):
    if not isinstance(tz, str) or not tz:
        return False
    if tz not in ("UTC", "Etc/UTC", "Etc/GMT"):
        if tz.startswith(("Etc/", "SystemV/")):
            return False
        if not IANA_TZ_RE.match(tz):
            return False
        for seg in tz.split("/"):
            if not seg or seg.startswith((".", "+", "-")) or seg.endswith((".", "+", "-")):
                return False
    try:
        zoneinfo.ZoneInfo(tz)
        return True
    except (zoneinfo.ZoneInfoNotFoundError, KeyError, ValueError):
        return False


EXPECTS = {
    ("task", "depends_on"): ("task",),
}
MAX_FILE_BYTES = 1024 * 1024
MAX_FM_BYTES = 64 * 1024

FORBIDDEN_JINJA2_NODES = (
    jinja2.nodes.Include,
    jinja2.nodes.Extends,
    jinja2.nodes.Import,
    jinja2.nodes.FromImport,
)
ALLOWED_JINJA2_FILTERS = frozenset(("default", "d", "lower", "upper", "trim", "replace"))


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
for _ch in "yYnNoO":  # §5.3: YAML 1.2 booleans only (preserve null resolver on n/N)
    Loader.yaml_implicit_resolvers[_ch] = [
        r for r in Loader.yaml_implicit_resolvers.get(_ch, [])
        if r[0] != "tag:yaml.org,2002:bool"
    ]
for _ch, _rs in list(Loader.yaml_implicit_resolvers.items()):
    Loader.yaml_implicit_resolvers[_ch] = [r for r in _rs if r[0] != "tag:yaml.org,2002:timestamp"]


def _strip_yaml_quoted_and_comments(src):
    """Replace contents of YAML quoted strings and comments with spaces for GF004 scanning."""
    out = []
    in_single = False
    in_double = False
    in_comment = False
    i = 0
    n = len(src)
    while i < n:
        ch = src[i]
        if ch == "\n":
            in_comment = False
            out.append("\n")
            i += 1
            continue
        if in_comment:
            out.append(" ")
            i += 1
            continue
        if in_single:
            if ch == "'":
                if i + 1 < n and src[i + 1] == "'":
                    out.append("  ")
                    i += 2
                    continue
                in_single = False
                out.append("'")
            else:
                out.append(" ")
            i += 1
            continue
        if in_double:
            if ch == "\\" and i + 1 < n:
                out.append("  ")
                i += 2
                continue
            if ch == '"':
                in_double = False
                out.append('"')
            else:
                out.append(" ")
            i += 1
            continue
        if ch == "'":
            in_single = True
            out.append("'")
        elif ch == '"':
            in_double = True
            out.append('"')
        elif ch == "#" and (i == 0 or src[i - 1] in (" ", "\t", "\n", "\r")):
            in_comment = True
            out.append(" ")
        else:
            out.append(ch)
        i += 1
    return "".join(out)


def render_template(raw_text, path="<template>", cli_vars=None):
    """Render a sandboxed .grain.md.jinja2 template -> (rendered_text, template_vars) or raises Finding."""
    if isinstance(raw_text, bytes):
        if len(raw_text) > MAX_FILE_BYTES:
            raise Finding("GF006", path, "grainfile template exceeds 1 MiB")
        try:
            raw_text = raw_text.decode("utf-8")
        except UnicodeDecodeError:
            raise Finding("GF003", path, "not UTF-8")
    elif len(raw_text.encode("utf-8")) > MAX_FILE_BYTES:
        raise Finding("GF006", path, "grainfile template exceeds 1 MiB")
    raw_text = raw_text.replace("\r\n", "\n")
    env = jinja2.sandbox.ImmutableSandboxedEnvironment(loader=None, keep_trailing_newline=True)
    try:
        ast = env.parse(raw_text)
    except jinja2.TemplateSyntaxError as e:
        raise Finding("GF033", path, "Jinja2 syntax error: %s" % e)
    for node in ast.find_all(FORBIDDEN_JINJA2_NODES):
        raise Finding("GF034", path, "forbidden Jinja2 tag %s in sandboxed grainfile template" % type(node).__name__.lower())
    for fnode in ast.find_all(jinja2.nodes.Filter):
        if fnode.name not in ALLOWED_JINJA2_FILTERS:
            raise Finding("GF033", path, "unsupported Jinja2 filter %r" % fnode.name)
    for mnode in ast.find_all((
        jinja2.nodes.Add,
        jinja2.nodes.Sub,
        jinja2.nodes.Mul,
        jinja2.nodes.Div,
        jinja2.nodes.FloorDiv,
        jinja2.nodes.Mod,
        jinja2.nodes.Pow,
        jinja2.nodes.Concat,
    )):
        raise Finding("GF033", path, "unsupported Jinja2 operator %s" % type(mnode).__name__.lower())
    try:
        template_vars = sorted(jinja2.meta.find_undeclared_variables(ast))
        rendered = env.from_string(raw_text).render(**(cli_vars or {}))
    except jinja2.TemplateError as e:
        raise Finding("GF033", path, "Jinja2 render error: %s" % e)
    except Exception as e:
        raise Finding("GF033", path, "Jinja2 evaluation error: %s" % e)
    if len(rendered.encode("utf-8")) > MAX_FILE_BYTES:
        raise Finding("GF006", path, "rendered grainfile template exceeds 1 MiB")
    return rendered, template_vars


def parse_text(text, path):
    """Parse decoded Grainfile text -> (front matter, body) or raises Finding."""
    text = text.replace("\r\n", "\n")
    lines = text.split("\n")
    if lines[0].rstrip() != "---":
        raise Finding("GF003", path, "no front matter")
    end = next((i for i in range(1, len(lines)) if lines[i].rstrip() == "---"), None)
    if end is None:
        raise Finding("GF003", path, "unclosed front matter")
    for fm_line in lines[1:end]:
        if fm_line.strip() == "...":
            raise Finding("GF004", path, "multiple YAML documents are not allowed in front matter")
    src = "\n".join(lines[1:end])
    if len(src.encode("utf-8")) > MAX_FM_BYTES:
        raise Finding("GF006", path, "front matter exceeds 64 KiB")
    stripped_src = _strip_yaml_quoted_and_comments(src)
    if re.search(r"(^|[\s\[{,:])[&*][A-Za-z0-9_-]+|<<\s*:|(^|[\s\[{,:])!", stripped_src, re.M):
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


def parse_grainfile(path, is_template=False, cli_vars=None):
    """-> (front matter, body, template_vars) or raises Finding."""
    raw = pathlib.Path(path).read_bytes()
    if len(raw) > MAX_FILE_BYTES:
        raise Finding("GF006", path, "grainfile exceeds 1 MiB")
    if raw.startswith(b"version https://git-lfs"):
        raise Finding("GF003", path, "a grainfile may not be an LFS pointer")
    if b"\0" in raw or raw.startswith(b"\xef\xbb\xbf"):
        raise Finding("GF003", path, "NUL or byte-order mark")
    try:
        text = raw.decode("utf-8").replace("\r\n", "\n")
    except UnicodeDecodeError:
        raise Finding("GF003", path, "not UTF-8")
    template_vars = []
    if is_template:
        text, template_vars = render_template(text, path, cli_vars=cli_vars)
    fm, body = parse_text(text, path)
    return fm, body, template_vars


def parse(path, is_template=False, cli_vars=None):
    """-> (front matter, body) or raises Finding."""
    fm, body, _ = parse_grainfile(path, is_template=is_template, cli_vars=cli_vars)
    return fm, body


def heading_slug(text):
    return re.sub(r"\s", "-", re.sub(r"[^\w\s-]", "", text.strip().lower()))


def sections(body):
    """Markdown headings -> [(level, text, start line, end line)]."""
    lines = body.split("\n")
    heads = [(i, len(m.group(1)), m.group(2)) for i, l in enumerate(lines)
             for m in [re.match(r"^(#{1,6})\s+(.*?)\s*#*\s*$", l)] if m]
    out = []
    for n, (i, lvl, text) in enumerate(heads):
        end = next((j for j, l2, _ in heads[n + 1:] if l2 <= lvl), len(lines))
        out.append((lvl, text, i, end))
    return out, lines


# ---------------------------------------------------------------- references (§8)

class Ref:
    """A parsed reference: form is remote, rooted or relative."""
    def __init__(self, form, repo=None, path="", gitref=None, anchor=None, mode=None):
        self.form, self.repo, self.path, self.gitref, self.anchor, self.mode = (
            form, repo, path, gitref, anchor, mode
        )


def canon_repo(url):
    u = urllib.parse.urlsplit(url)
    host = (u.hostname or "").lower()
    if u.port and not (u.scheme == "https" and u.port == 443):
        host += ":%d" % u.port
    path = re.sub(r"\.git$", "", u.path.rstrip("/"))
    return "%s://%s%s" % (u.scheme, host, path)


REPO_SEG_RE = re.compile(r"^[A-Za-z0-9_.-]+$")


def _has_encoded_path_hazard(s):
    low = s.lower()
    return "%2e" in low or "%2f" in low or "%5c" in low or "%00" in low


def _valid_repo_seg(seg):
    return bool(seg) and seg not in (".", "..") and bool(REPO_SEG_RE.match(seg))


def parse_ref(s):
    """-> Ref, or raises Finding('GF035') / Finding('GF036') (§8.1)."""
    if not isinstance(s, str) or not s or re.search(r"[\s\\]", s):
        raise Finding("GF035", "", "a reference must be a non-empty string without whitespace or backslashes")
    if _has_encoded_path_hazard(s):
        raise Finding("GF035", "", "encoded path traversal or separator sequences are not allowed in references (%r)" % s)
    if "#" in s:
        s, _, anchor = s.partition("#")
        if not anchor or not ANCHOR_RE.match(anchor):
            raise Finding("GF035", "", "malformed anchor in %r" % s)
    else:
        anchor = None
    if not s:
        raise Finding("GF035", "", "empty reference path before anchor")
    if s.startswith("git::") or "::" in s:
        raise Finding("GF035", "", "go-getter git:: syntax is not allowed; use a plain https:// URL (%r)" % s)
    if "?" in s:
        raise Finding("GF035", "", "query parameters are not allowed in references (%r)" % s)
    if s.startswith("git@"):
        raise Finding("GF035", "", "SSH references are not allowed; use https:// (%r)" % s)
    if "://" in s or re.match(r"^[a-zA-Z][a-zA-Z0-9+.-]*:", s):
        if not (s.startswith("https://") or s.startswith("file://")):
            raise Finding("GF035", "", "unsupported reference scheme in %r (only https:// is allowed)" % s)
        u = urllib.parse.urlsplit(s)
        if u.username or u.password or "@" in (u.netloc or ""):
            raise Finding("GF035", "", "credentials are not allowed in reference URLs (%r)" % s)
        if "//" in u.path:
            raise Finding("GF035", "", "'//' path separators are not allowed; use standard forge /blob/<ref>/ or /tree/<ref>/ URLs (%r)" % s)
        if u.scheme == "https":
            if not u.hostname or "." not in u.hostname:
                raise Finding("GF035", "", "invalid host in %r" % s)
            trailing = "/" if u.path.endswith("/") else ""
            segs = [seg for seg in u.path.split("/") if seg]
            if len(segs) < 2:
                raise Finding("GF035", "", "repository URL must have at least <owner>/<repo>: %r" % s)
            if "-" in segs:
                idx = segs.index("-")
                if idx < 2 or idx + 2 >= len(segs) or segs[idx + 1] not in ("blob", "tree"):
                    raise Finding("GF035", "", "malformed GitLab reference URL: %r" % s)
                repo_segs, mode, gitref, sub_segs = segs[:idx], segs[idx + 1], segs[idx + 2], segs[idx + 3:]
            elif "+" in segs:
                idx = segs.index("+")
                if idx == 0 or idx + 1 >= len(segs):
                    raise Finding("GF035", "", "malformed Gitiles reference URL: %r" % s)
                repo_segs, mode, gitref, sub_segs = segs[:idx], None, segs[idx + 1], segs[idx + 2:]
            elif len(segs) >= 3 and segs[2] in ("blob", "tree"):
                if len(segs) < 4:
                    raise Finding("GF035", "", "missing ref in %s URL: %r" % (segs[2], s))
                repo_segs, mode, gitref, sub_segs = segs[:2], segs[2], segs[3], segs[4:]
            else:
                if any(seg.endswith(".git") for seg in segs[:-1]):
                    raise Finding("GF035", "", "sub-path in repository URL must use /blob/<ref>/ or /tree/<ref>/: %r" % s)
                repo_segs, mode, gitref, sub_segs = segs, None, None, []
            if any(not _valid_repo_seg(seg) for seg in repo_segs):
                raise Finding("GF035", "", "invalid repository path segment in %r" % s)
            if gitref is not None and (not gitref or gitref in (".", "..") or "@" in gitref or "\\" in gitref):
                raise Finding("GF035", "", "invalid git ref in %r" % s)
            if mode == "blob" and (not sub_segs or trailing):
                raise Finding("GF035", "", "blob URL must name a file path: %r" % s)
            if any(seg == ".." for seg in sub_segs):
                raise Finding("GF036", "", "reference path climbs above repository root: %r" % s)
            if any(seg == "." for seg in sub_segs):
                raise Finding("GF035", "", "dot path segments are not allowed in remote reference URLs: %r" % s)
            host = u.hostname.lower()
            if u.port and u.port != 443:
                host += ":%d" % u.port
            repo_url = canon_repo("https://%s/%s" % (host, "/".join(repo_segs)))
            sub = "/".join(sub_segs) + (trailing if sub_segs else "")
            return Ref("remote", repo=repo_url, path=sub, gitref=gitref, anchor=anchor, mode=mode)
        else:
            raw = s[7:].lstrip("/")
            if not raw or "//" in raw:
                raise Finding("GF035", "", "no repository path: %r" % s)
            trailing = "/" if raw.endswith("/") else ""
            segs = [seg for seg in raw.split("/") if seg]
            if not segs:
                raise Finding("GF035", "", "no repository path: %r" % s)
            if len(segs) >= 2 and segs[1] in ("blob", "tree"):
                if len(segs) < 3:
                    raise Finding("GF035", "", "missing ref in %s URL: %r" % (segs[1], s))
                repo_name, mode, gitref, sub_segs = segs[0], segs[1], segs[2], segs[3:]
            else:
                repo_name, mode, gitref, sub_segs = segs[0], None, None, segs[1:]
            if mode == "blob" and (not sub_segs or trailing):
                raise Finding("GF035", "", "blob URL must name a file path: %r" % s)
            if any(seg == ".." for seg in sub_segs):
                raise Finding("GF036", "", "reference path climbs above repository root: %r" % s)
            if any(seg == "." for seg in sub_segs):
                raise Finding("GF035", "", "dot path segments are not allowed in remote reference URLs: %r" % s)
            sub = "/".join(sub_segs) + (trailing if sub_segs else "")
            return Ref("remote", repo="file://" + repo_name, path=sub, gitref=gitref, anchor=anchor, mode=mode)
    if "//" in s:
        raise Finding("GF035", "", "'//' path separators are not allowed in references (%r)" % s)
    if s.startswith("/"):
        return Ref("rooted", path=s[1:], anchor=anchor)
    return Ref("relative", path=s, anchor=anchor)


# ---------------------------------------------------------------- the tree

class Repo:
    def __init__(self, d, url):
        self.dir, self.url = d, url
        self.files = sorted(p.relative_to(d).as_posix() for p in d.rglob("*")
                            if p.is_file() and ".git" not in p.relative_to(d).parts)


class Item:
    """One grainfile (<name>.task.grain.md[.jinja2] or <name>.process.grain.md[.jinja2])."""
    def __init__(self, repo, rel, kind, name, is_template=False):
        self.repo, self.rel, self.kind, self.name = repo, rel, kind, name
        self.is_template = bool(is_template)
        self.template_vars = []
        self.fm, self.body, self.ok = {}, "", False
        self.grainfile = DEFAULT_VERSION
        self.spec_summary_path = VERSIONS[DEFAULT_VERSION]["spec_summary_path"]
        self.spec_path = VERSIONS[DEFAULT_VERSION]["spec_path"]
        self.recipient = "/" if kind == "task" else None
        self.targets = {}

    @property
    def path(self):
        return self.repo.dir / self.rel

    def __repr__(self):
        return self.rel


def classify(rel):
    """Repo-relative path -> ('grain', kind, stem, is_template) | ('asset',) | ('asset', 'GF005')."""
    leaf = rel.rsplit("/", 1)[-1]
    for k in KINDS:
        suf_j2 = ".%s.grain.md.jinja2" % k
        if leaf.endswith(suf_j2):
            return ("grain", k, leaf[:-len(suf_j2)], True)
        suf = ".%s.grain.md" % k
        if leaf.endswith(suf):
            return ("grain", k, leaf[:-len(suf)], False)
    if re.match(r"^(.+)\.([A-Za-z][A-Za-z0-9-]*)\.grain\.md(?:\.jinja2)?$", leaf):
        return ("asset", "GF005")
    return ("asset",)


def valid_repo_slug_or_url(val):
    if not isinstance(val, str) or not val or re.search(r"[\s\\]", val):
        return False
    if "://" in val or val.startswith(("http:", "https:", "ssh:", "git@", "file:")):
        try:
            r = parse_ref(val)
        except Finding:
            return False
        return r.form == "remote" and not r.path and r.gitref is None and r.mode is None and r.anchor is None
    cleaned = val[:-4] if val.endswith(".git") else val
    if not REPO_SLUG_RE.match(cleaned):
        return False
    owner, repo = cleaned.split("/", 1)
    return owner not in (".", "..") and repo not in ("", ".", "..")


class Schemas:
    def __init__(self):
        store = {}
        for p in SCHEMAS.glob("*.schema.json"):
            s = json.loads(p.read_text())
            store[s["$id"]] = s
        self.store = store
        self.by_kind = {s["$id"].rsplit("/", 1)[-1][:-12]: s for s in store.values()}

    def validate(self, kind, fm, path):
        s = self.by_kind[kind]
        props = s.get("properties", {})
        out, data = [], dict(fm)
        for k in list(fm):
            if k not in props and not k.startswith("x_"):
                if k == "trigger_with":
                    out.append(Finding("GF020", path, "trigger_with is not allowed; use depends_on on .task.grain.md"))
                elif kind == "process" and k in ("recipient", "requestor", "depends_on"):
                    out.append(Finding("GF020", path, "%s is only valid on .task.grain.md files" % k))
                elif kind == "task" and k in ("when", "on", "gate"):
                    out.append(Finding("GF020", path, "%s is only valid on .process.grain.md files" % k))
                else:
                    out.append(Finding("GF022", path, "unknown field %r" % k))
                del data[k]
        if kind == "process" and isinstance(data.get("when"), dict):
            when = dict(data["when"])
            for k in list(when):
                if k not in SOURCES:
                    out.append(Finding("GF020", path, "unknown trigger source %r in when" % k))
                    del when[k]
            if not when and "when" in data:
                # Avoid a duplicate minProperties error when the only source was unknown
                del data["when"]
                return out
            else:
                data["when"] = when
        v = jsonschema.Draft202012Validator(s, resolver=jsonschema.RefResolver.from_schema(s, store=self.store))
        for e in sorted(v.iter_errors(data), key=lambda e: list(e.path)):
            p = list(e.path)
            if p and p[0] in ("references", "depends_on") and isinstance(e.instance, str) and e.instance and not re.search(r"[\s\\]", e.instance):
                continue
            if p == ["from_template"] and isinstance(e.instance, str):
                continue
            if p in (["recipient"], ["requestor"]) and isinstance(e.instance, str) and e.instance and not any(ch in e.instance for ch in "<>@\\ "):
                continue
            where = ".".join(str(x) for x in p) or "(top)"
            if p == [] and "'grainfile' is a required property" in e.message:
                out.append(Finding("GF020", path, 'grainfile: required field missing (expected "v1")'))
            else:
                out.append(Finding("GF020", path, "%s: %s" % (where, e.message)))
        return out


# ---------------------------------------------------------------- the linter

class Linter:
    def __init__(self, top, cli_vars=None):
        self.top = pathlib.Path(top)
        self.cli_vars = dict(cli_vars or {})
        self.findings = []
        self.schemas = Schemas()
        stubs = self.top / "stubs.yaml"
        self.stubs = {canon_repo(k): v for k, v in (yaml.safe_load(stubs.read_text()) or {}).items()} if stubs.exists() else {}
        children = sorted(p for p in self.top.iterdir() if p.is_dir() and not p.name.startswith("."))
        if (stubs.exists() or (self.top / "expect.txt").exists() or
                (not (self.top / ".git").exists() and any((p / ".git").exists() for p in children))):
            dirs = children
        else:
            dirs = [self.top]
        back = {v: k for k, v in self.stubs.items()}
        self.repos = [Repo(d, back.get(d.name, "file://" + d.name)) for d in dirs]
        self.items = {}  # (repo dir, repo-relative path) -> Item

    def add(self, code, path, msg, severity=None):
        self.findings.append(Finding(code, path, msg, severity))

    def show(self, repo, rel):
        return "%s/%s" % (repo.dir.name, rel) if repo.dir != self.top else rel

    def run(self):
        for repo in self.repos:
            self.scan(repo)
        for it in list(self.items.values()):
            if it.ok:
                self.check_item(it)
        self.check_graphs()
        return self.findings

    def scan(self, repo):
        lower = {}
        for x in repo.files:
            if x.lower() in lower:
                self.add("GF006", self.show(repo, x), "differs only in case from %s" % lower[x.lower()])
            lower[x.lower()] = x
            if len(x.encode("utf-8")) > 1024 or any(len(s.encode("utf-8")) > 255 for s in x.split("/")):
                self.add("GF006", self.show(repo, x), "path too long")
            c = classify(x)
            if c[0] == "asset" and len(c) > 1:
                self.add(c[1], self.show(repo, x), "unregistered kind suffix")
            elif c[0] == "grain":
                is_tmpl = c[3] if len(c) > 3 else False
                it = Item(repo, x, c[1], c[2], is_template=is_tmpl)
                self.items[(repo.dir, x)] = it
                if not STEM.match(c[2]):
                    self.add("GF001", self.show(repo, x), "bad grainfile name %r" % c[2])
                try:
                    it.fm, it.body, it.template_vars = parse_grainfile(
                        it.path, is_template=is_tmpl, cli_vars=self.cli_vars
                    )
                    it.ok = True
                except Finding as e:
                    e.path = self.show(repo, x)
                    self.findings.append(e)

    # ---- resolution (§8.2)
    def resolve(self, it, s, field):
        """-> the target Item, 'asset', 'folder', None (external), or raises Finding."""
        try:
            r = parse_ref(s)
        except Finding as e:
            raise Finding(e.code, "", "%s: %s" % (field, e.msg))
        repo = it.repo
        if r.form == "remote":
            stub = self.stubs.get(r.repo)
            if stub is None and r.gitref is None:
                for stub_url in self.stubs:
                    if r.repo.startswith(stub_url + "/"):
                        raise Finding("GF035", "", "%s: sub-path in repository URL %r must use /blob/<ref>/ or /tree/<ref>/" % (field, s))
            if stub is None and r.repo.startswith("file://") and (self.top / r.repo[7:].strip("/")).is_dir():
                stub = r.repo[7:].strip("/")
            target_repo = next((x for x in self.repos if x.dir.name == stub), None)
            if target_repo is None:
                return None
            if r.path and posixpath.normpath(r.path).startswith(".."):
                raise Finding("GF036", "", "%s: %r climbs above the repository root" % (field, s))
            path = posixpath.normpath(r.path.strip("/")) if r.path.strip("/") else ""
            trailing_slash = r.path.endswith("/") or not r.path or r.mode == "tree"
        else:
            target_repo = repo
            if r.form == "rooted":
                path = posixpath.normpath(r.path) if r.path else ""
            else:
                path = posixpath.normpath(posixpath.join(posixpath.dirname(it.rel), r.path))
            trailing_slash = r.path.endswith("/")
            if path == ".." or path.startswith("../"):
                raise Finding("GF036", "", "%s: %r climbs above the repository root" % (field, s))
        if path == ".":
            path = ""
        is_dir = (path == "") or (target_repo.dir / path).is_dir() or \
            any(f.startswith(path.rstrip("/") + "/") for f in target_repo.files)
        if trailing_slash:
            if not is_dir:
                raise Finding("GF030", "", "%s: no folder %r" % (field, s))
            if r.anchor:
                raise Finding("GF030", "", "%s: folder reference %r may not carry an anchor" % (field, s))
            return "folder"
        if path in target_repo.files:
            target = self.items.get((target_repo.dir, path))
            if target is not None:
                if r.anchor and target.ok:
                    slugs = [heading_slug(t) for _, t, _, _ in sections(target.body)[0]]
                    if r.anchor not in slugs:
                        raise Finding("GF030", "", "%s: no heading #%s in %s" % (field, r.anchor, path))
                return target
            if r.anchor and path.endswith(".md"):
                body = (target_repo.dir / path).read_text(encoding="utf-8", errors="replace")
                slugs = [heading_slug(t) for _, t, _, _ in sections(body)[0]]
                if r.anchor not in slugs:
                    raise Finding("GF030", "", "%s: no heading #%s in %s" % (field, r.anchor, path))
            return "asset"
        if is_dir and r.mode != "blob":
            if r.anchor:
                raise Finding("GF030", "", "%s: folder reference %r may not carry an anchor" % (field, s))
            return "folder"
        raise Finding("GF030", "", "%s: %r does not resolve" % (field, s))

    def check_deployment_ref(self, it, s, field, where):
        try:
            r = parse_ref(s)
        except Finding as e:
            self.add(e.code, where, "%s: %s" % (field, e.msg))
            return
        if r.anchor:
            self.add("GF035", where, "%s: deployment identity %r may not carry an anchor" % (field, s))
            return
        if r.mode == "blob":
            self.add("GF032", where, "%s: %r is a file reference (/blob/), expected a repository or folder" % (field, s))
            return
        for k in KINDS:
            for suf in (".%s.grain.md" % k, ".%s.grain.md.jinja2" % k):
                if r.path.endswith(suf) or (r.repo and r.repo.endswith(suf)):
                    self.add("GF032", where, "%s: %r is a %s grainfile, expected a repository or folder" % (field, s, k))
                    return
        # Canonical Hub deployment reference (/deployments/<id> or https://<hub-host>/deployments/<id>)
        if r.form == "rooted" and re.match(r"^deployments/[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$", r.path.strip("/")):
            return
        try:
            t = self.resolve(it, s, field)
        except Finding as e:
            self.add(e.code, where, e.msg)
            return
        if t is not None and t != "folder":
            kind = t.kind if isinstance(t, Item) else t
            self.add("GF032", where, "%s: %r resolves to a %s, expected a repository or folder" % (field, s, kind))

    def check_from_template(self, it, s, where):
        if not isinstance(s, str):
            return
        if not s or not s.strip() or re.search(r"[\s\\]", s):
            self.add("GF035", where, "from_template: must be a non-empty URL or path without whitespace")
            return
        try:
            r = parse_ref(s)
        except Finding as e:
            self.add("GF035", where, "from_template: %s" % e.msg)
            return
        if r.anchor:
            self.add("GF035", where, "from_template: %r may not carry an anchor" % s)
            return
        if r.mode == "tree" or s.endswith("/"):
            self.add("GF035", where, "from_template: %r must reference a .%s.grain.md.jinja2 file, not a directory" % (s, it.kind))
            return
        want_suf = ".%s.grain.md.jinja2" % it.kind
        if not r.path or not r.path.endswith(want_suf):
            self.add("GF035", where, "from_template: %r on a %s grainfile must end with %s" % (s, it.kind, want_suf))
            return
        try:
            t = self.resolve(it, s, "from_template")
        except Finding as e:
            self.add(e.code, where, e.msg)
            return
        it.targets.setdefault("from_template", []).append((s, t))

    def check_when_filter(self, ev_key, flt, where):
        if not isinstance(flt, dict):
            return
        raw_repo = flt.get("repo")
        if isinstance(raw_repo, str) and not valid_repo_slug_or_url(raw_repo):
            self.add("GF020", where, "when.%s.repo: invalid repository %r" % (ev_key, raw_repo))
        raw_repos = flt.get("repos")
        if isinstance(raw_repos, list):
            for rp in raw_repos:
                if isinstance(rp, str) and not valid_repo_slug_or_url(rp):
                    self.add("GF020", where, "when.%s.repos: invalid repository %r" % (ev_key, rp))
        raw_paths = flt.get("paths")
        if isinstance(raw_paths, list):
            for p in raw_paths:
                if not isinstance(p, str):
                    continue
                if "\\" in p:
                    self.add("GF020", where, "when.%s.paths: backslashes are not allowed in %r" % (ev_key, p))
                elif "://" in p or p.startswith(("http:", "https:", "ssh:", "git@", "file:")):
                    try:
                        r = parse_ref(p)
                        if r.form != "remote" or r.anchor:
                            self.add("GF020", where, "when.%s.paths: invalid forge URL %r" % (ev_key, p))
                    except Finding:
                        self.add("GF020", where, "when.%s.paths: invalid forge URL %r" % (ev_key, p))
                elif ".." in p.strip("/").split("/"):
                    self.add("GF020", where, "when.%s.paths: '..' segment not allowed in %r" % (ev_key, p))
        raw_ignore = flt.get("ignore")
        if isinstance(raw_ignore, list):
            for p in raw_ignore:
                if not isinstance(p, str):
                    continue
                if "\\" in p or ".." in p.strip("/").split("/"):
                    self.add("GF020", where, "when.%s.ignore: invalid path %r" % (ev_key, p))

    def check_schedule_item(self, sch, where):
        if not isinstance(sch, dict):
            return
        cron = sch.get("cron")
        if isinstance(cron, str) and len(cron.split()) != 5:
            self.add("GF020", where, "schedule.cron must be a five-field cron expression")
        tz = sch.get("timezone")
        if isinstance(tz, str) and not valid_iana_timezone(tz):
            self.add("GF020", where, "when.schedule.timezone: invalid IANA timezone %r" % tz)

    def check_workflow_filter(self, flt, where):
        if not isinstance(flt, dict):
            return
        raw_wf = flt.get("workflow")
        if isinstance(raw_wf, str) and not raw_wf.strip():
            self.add("GF020", where, "when.workflow.workflow: must be a non-empty string")
        raw_wfs = flt.get("workflows")
        if isinstance(raw_wfs, list):
            for w in raw_wfs:
                if isinstance(w, str) and not w.strip():
                    self.add("GF020", where, "when.workflow.workflows: must be non-empty strings")
        raw_repo = flt.get("repo")
        if isinstance(raw_repo, str) and not valid_repo_slug_or_url(raw_repo):
            self.add("GF020", where, "when.workflow.repo: invalid repository %r" % raw_repo)
        raw_repos = flt.get("repos")
        if isinstance(raw_repos, list):
            for rp in raw_repos:
                if isinstance(rp, str) and not valid_repo_slug_or_url(rp):
                    self.add("GF020", where, "when.workflow.repos: invalid repository %r" % rp)
        raw_branches = flt.get("branches")
        if isinstance(raw_branches, list):
            for br in raw_branches:
                if isinstance(br, str) and not br.strip():
                    self.add("GF020", where, "when.workflow.branches: must be non-empty strings")

    def refs_of(self, it):
        fm, out = it.fm, []
        def add(field, v):
            if isinstance(v, str):
                out.append((field, v))
        fields = ("depends_on", "references") if it.kind == "task" else ("references",)
        for f in fields:
            val = fm.get(f)
            if isinstance(val, list):
                for v in val:
                    add(f, v)
        return out

    def check_item(self, it):
        where = self.show(it.repo, it.rel)
        self.findings += self.schemas.validate(it.kind, it.fm, where)
        fm = it.fm
        vinfo = resolve_version(fm["grainfile"]) if "grainfile" in fm else None
        if vinfo is not None:
            it.grainfile = vinfo["grainfile"]
            it.spec_summary_path = vinfo["spec_summary_path"]
            it.spec_path = vinfo["spec_path"]
        title = fm.get("title")
        if isinstance(title, str) and "\n" in title:
            self.add("GF020", where, "title must be a single line")
        nb = fm.get("not_before")
        if isinstance(nb, str) and (DATE_RE.match(nb) or TIME_RE.match(nb)) and not valid_time_or_date(nb, allow_date=True):
            self.add("GF020", where, "not_before: invalid calendar date or UTC time %r" % nb)
        if "from_template" in fm and isinstance(fm["from_template"], str):
            self.check_from_template(it, fm["from_template"], where)
        if it.kind == "task":
            req = fm.get("requestor")
            if isinstance(req, str) and not USER_IDENTITY.match(req):
                if not any(ch in req for ch in "<>@\\ ") and req:
                    self.check_deployment_ref(it, req, "requestor", where)
            rec = fm.get("recipient", "/")
            it.recipient = rec if isinstance(rec, str) and rec else "/"
            if isinstance(rec, str) and not any(ch in rec for ch in "<>@\\ ") and rec:
                self.check_deployment_ref(it, rec, "recipient", where)
        if it.kind == "process":
            when = fm.get("when")
            if isinstance(when, dict):
                sch = when.get("schedule")
                if isinstance(sch, dict):
                    self.check_schedule_item(sch, where)
                elif isinstance(sch, list):
                    for item in sch:
                        self.check_schedule_item(item, where)
                for ev_key in ("push", "pull_request", "merge"):
                    raw_ev = when.get(ev_key)
                    if isinstance(raw_ev, dict):
                        self.check_when_filter(ev_key, raw_ev, where)
                    elif isinstance(raw_ev, list):
                        for flt in raw_ev:
                            self.check_when_filter(ev_key, flt, where)
                raw_wf = when.get("workflow")
                if isinstance(raw_wf, dict):
                    self.check_workflow_filter(raw_wf, where)
                elif isinstance(raw_wf, list):
                    for flt in raw_wf:
                        self.check_workflow_filter(flt, where)
        it.targets = {}
        for field, s in self.refs_of(it):
            try:
                t = self.resolve(it, s, field)
            except Finding as e:
                self.add(e.code, where, e.msg)
                continue
            want = EXPECTS.get((it.kind, field))
            if want and isinstance(t, Item) and t.is_template:
                self.add("GF032", where, "%s: %r is an unrendered .jinja2 template, expected a concrete .task.grain.md file" % (field, s))
            elif want and isinstance(t, Item) and t.kind not in want:
                self.add("GF032", where, "%s: %r is a %s, expected %s" % (field, s, t.kind, " or ".join(want)))
            elif want and t in ("asset", "folder"):
                self.add("GF032", where, "%s: %r is a %s, expected %s" % (field, s, t, " or ".join(want)))
            elif want and t is None:
                r = parse_ref(s)
                if r.mode == "tree" or not any(r.path.endswith(".%s.grain.md" % k) for k in want):
                    self.add("GF032", where, "%s: %r is not a concrete %s grainfile" % (field, s, " or ".join(want)))
            it.targets.setdefault(field, []).append((s, t))

    def check_graphs(self):
        graph = {}
        for it in self.items.values():
            if it.ok and it.kind == "task":
                graph[it] = [t for _, t in it.targets.get("depends_on", []) if isinstance(t, Item)]
        for cyc in cycles(graph):
            self.add("GF034", self.show(cyc[0].repo, cyc[0].rel),
                     "depends_on cycle: %s" % (" -> ".join(x.rel for x in cyc)))


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


# ---------------------------------------------------------------- entry points

def lint(top, cli_vars=None):
    return Linter(top, cli_vars=cli_vars).run()


def self_test():
    ok = True
    def fail(msg):
        nonlocal ok
        ok = False
        print("FAIL", msg)
    v1 = resolve_version("v1")
    if not v1 or v1["spec_summary_path"] != "docs/grainfile-v1.md" or v1["spec_path"] != "docs/grainfiles-spec.md":
        fail("resolve_version('v1') mismatch: %r" % (v1,))
    if not SPEC_SUMMARY.exists():
        fail("missing %s" % SPEC_SUMMARY)
    ex = ROOT / "examples" / "grainfiles"
    ex_linter = Linter(ex)
    for f in ex_linter.run():
        fail("examples: %s" % f)
    # Verify template discovery on examples/grainfiles
    tmpl_items = {it.rel: it for it in ex_linter.items.values() if it.is_template}
    if ".grain/tasks/rollout-region.task.grain.md.jinja2" not in tmpl_items:
        fail("missing rollout-region.task.grain.md.jinja2 in examples")
    else:
        t_it = tmpl_items[".grain/tasks/rollout-region.task.grain.md.jinja2"]
        if t_it.template_vars != ["dry_run", "environment", "recipient_url", "region", "requestor"]:
            fail("unexpected template_vars on rollout-region: %r" % (t_it.template_vars,))
    if ".grain/processes/branch-audit.process.grain.md.jinja2" not in tmpl_items:
        fail("missing branch-audit.process.grain.md.jinja2 in examples")
    else:
        p_it = tmpl_items[".grain/processes/branch-audit.process.grain.md.jinja2"]
        if p_it.template_vars != ["enforce_gate", "target_branch"]:
            fail("unexpected template_vars on branch-audit: %r" % (p_it.template_vars,))
    # Verify --var override rendering on examples/grainfiles
    for f in lint(ex, cli_vars={"region": "us-east-1", "environment": "production", "dry_run": "true", "target_branch": "release/v2"}):
        fail("examples with --var overrides: %s" % f)
    seen = set()
    bad = HERE / "testdata" / "bad"
    for case in sorted(p for p in bad.iterdir() if p.is_dir()):
        want = set((case / "expect.txt").read_text().split())
        seen |= want
        got = {f.code for f in lint(case)}
        if got != want:
            fail("%s: want %s, got %s" % (case.name, sorted(want), sorted(got)))
            for f in lint(case):
                print("     ", f)
        else:
            print("ok  ", case.name, sorted(got))
    spec = SPEC.read_text()
    spec_codes = sorted(set(re.findall(r"^\| (GF\d{3}) \|", spec, re.M)))
    if spec_codes != sorted(CODES):
        fail("spec lint table %s != linter codes %s" % (spec_codes, sorted(CODES)))
    for c in CODES:
        sev = re.search(r"^\| %s \| (\w+) \|" % c, spec, re.M)
        if sev and sev.group(1) != SEVERITY.get(c, "error"):
            fail("%s: spec says %s, linter %s" % (c, sev.group(1), SEVERITY.get(c, "error")))
    m = re.search(r"12\.1\. \*\*Kinds:\*\*(.*?)\n\n", spec, re.S)
    if not m or sorted(re.findall(r"`([a-z-]+)`", m.group(1))) != sorted(KINDS):
        fail("spec §12.1 kinds disagree with the linter")
    have = sorted(p.name[:-12] for p in SCHEMAS.glob("*.schema.json") if p.name != "common.schema.json")
    if have != sorted(KINDS):
        fail("schemas %s != kinds %s" % (have, sorted(KINDS)))
    m = re.search(r"12\.2\. \*\*Trigger sources:\*\*(.*?)\n\n", spec, re.S)
    if not m or sorted(re.findall(r"`([a-z_-]+)`", m.group(1))) != sorted(SOURCES):
        fail("spec §12.2 trigger sources disagree with the linter")
    common = json.loads((SCHEMAS / "common.schema.json").read_text())["$defs"]
    if sorted(common["when"]["properties"]) != sorted(SOURCES):
        fail("common.schema.json trigger sources disagree with the linter")
    uncovered = set(CODES) - seen
    if uncovered:
        fail("codes with no broken fixture: %s" % sorted(uncovered))
    for url, want_repo, want_path, want_ref, want_mode, want_anchor in [
        ("https://github.com/acme/grain-eu", "https://github.com/acme/grain-eu", "", None, None, None),
        ("https://github.com/acme/deployments/tree/main/grain-eu", "https://github.com/acme/deployments", "grain-eu", "main", "tree", None),
        ("https://github.com/acme/widgets/blob/main/docs/widget-schema.md#req-1", "https://github.com/acme/widgets", "docs/widget-schema.md", "main", "blob", "req-1"),
        ("https://gitlab.com/acme/group/widgets/-/blob/main/docs/widget-schema.md", "https://gitlab.com/acme/group/widgets", "docs/widget-schema.md", "main", "blob", None),
        ("https://gitlab.com/acme/group/widgets/-/tree/main/src", "https://gitlab.com/acme/group/widgets", "src", "main", "tree", None),
        ("https://gerrit.googlesource.com/widgets/+/main/docs/widget-schema.md", "https://gerrit.googlesource.com/widgets", "docs/widget-schema.md", "main", None, None),
    ]:
        r = parse_ref(url)
        if (r.form, r.repo, r.path, r.gitref, r.mode, r.anchor) != ("remote", want_repo, want_path, want_ref, want_mode, want_anchor):
            fail("parse_ref(%r) -> %r" % (url, (r.form, r.repo, r.path, r.gitref, r.mode, r.anchor)))
    for bad_ref in [
        "git::https://github.com/acme/widgets.git//docs/d.md",
        "http://github.com/acme/widgets/blob/main/docs/d.md",
        "ssh://git@github.com/acme/widgets.git",
        "git@github.com:acme/widgets.git",
        "https://github.com/acme/widgets/blob/main/docs/d.md?ref=main",
        "https://github.com/acme/widgets//docs/d.md",
    ]:
        try:
            parse_ref(bad_ref)
            fail("parse_ref(%r) should have raised GF035" % bad_ref)
        except Finding as e:
            if e.code != "GF035":
                fail("parse_ref(%r) raised %s, want GF035" % (bad_ref, e.code))
    print("self-test", "passed" if ok else "FAILED")
    return 0 if ok else 1


def main(argv):
    if argv[:1] == ["--self-test"]:
        return self_test()
    cli_vars = {}
    trees = []
    i = 0
    while i < len(argv):
        arg = argv[i]
        if arg == "--var":
            if i + 1 >= len(argv) or "=" not in argv[i + 1]:
                print("grainlint: --var requires KEY=VALUE", file=sys.stderr)
                return 2
            k, _, v = argv[i + 1].partition("=")
            cli_vars[k] = v
            i += 2
        elif arg.startswith("--var="):
            kv = arg[len("--var="):]
            if "=" not in kv:
                print("grainlint: --var requires KEY=VALUE", file=sys.stderr)
                return 2
            k, _, v = kv.partition("=")
            cli_vars[k] = v
            i += 1
        else:
            trees.append(arg)
            i += 1
    fs = [f for t in trees for f in lint(t, cli_vars=cli_vars)]
    for f in fs:
        print(f)
    errors = [f for f in fs if f.severity == "error"]
    print("%d error(s), %d warning(s)" % (len(errors), len(fs) - len(errors)))
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
