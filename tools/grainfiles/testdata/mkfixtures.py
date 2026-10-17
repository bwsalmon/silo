"""Regenerates testdata/bad and testdata/good: one broken tree per lint code (spec §15),
and changes the forge check must authorise. Run: python3 tools/grainfiles/testdata/mkfixtures.py"""
import pathlib, shutil, sys, textwrap

B = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(B.parent))
import grainlint  # noqa: E402

for d in ("bad", "good"):
    shutil.rmtree(B / d, ignore_errors=True)

CFG = '---\nformat: 1\nowner: "@owner"\n---\n'
SHA = "0c4e9a7b5d3f1e2a6b8c9d0e1f2a3b4c5d6e7f80"


def cfg(extra=""):
    return '---\nformat: 1\nowner: "@owner"\n%s---\n' % extra


def task(extra="", body="Do it.\n"):
    return '---\ntitle: T\nsubmitter: "@alice"\ncreated_at: "2026-10-16T09:00:00Z"\n%s---\n%s' % (extra, body)


def comment(author="@alice", at="2026-10-16T09:00:00Z", extra=""):
    return '---\nauthor: "%s"\nat: "%s"\n%s---\nHi.\n' % (author, at, extra)


def record(extra, dep="@bot"):
    return ('---\ntitle: R\n%soutcome: succeeded\ndeployment: "%s"\nstarted_at: "2026-10-17T02:00:00Z"\n'
            'finished_at: "2026-10-17T02:30:00Z"\n---\nDone.\n' % (extra, dep))


def put(top, fs):
    for p, s in fs.items():
        f = top / p
        f.parent.mkdir(parents=True, exist_ok=True)
        f.write_text(s)


def case(name, codes, files, base=None, actors=None):
    root = B / ("bad" if codes else "good") / name
    if base is not None:
        put(root / "base", base)
        put(root / "head", files)
        (root / "authorize.yaml").write_text("actors: %s\n" % actors)
    else:
        put(root, files)
    if codes:
        (root / "expect.txt").write_text(" ".join(codes) + "\n")


def lib(fs, config=CFG):
    d = {"r/grain.config.md": config}
    d.update(fs)
    return d


A = task()
STUBS = "https://github.com/acme/lib.git: lib\n"
LIBREPO = {"lib/grain.config.md": CFG, "lib/caps/k.capability.md": "---\ntitle: K\n---\n"}

case("gf001-bad-name", ["GF001"], lib({"r/tasks/Big-Name.task.md": A}))
case("gf001-comment-author", ["GF001"], lib({"r/tasks/a.task/task.md": A,
     "r/tasks/a.task/thread/20261016T090000Z-bob.comment.md": comment("@alice")}))
case("gf003-no-front-matter", ["GF003"], lib({"r/tasks/a.task.md": "Just text.\n"}))
case("gf004-duplicate-key", ["GF004"], lib({"r/tasks/a.task.md": task("title: Again\n")}))
case("gf005-unknown-suffix", ["GF005"], lib({"r/notes/x.widget.md": "---\ntitle: x\n---\n"}))
case("gf007-reserved-folder", ["GF007"], lib({"r/tasks/a.task/task.md": A, "r/tasks/a.task/b.task.md": A,
     "r/notes/20261016T090000Z-alice.comment.md": comment()}))
case("gf007-record-file", ["GF007"], lib({"r/records/x.record.md": record('task: {title: T, prompt: P}\n')}))
case("gf010-nested-folder", ["GF010"], lib({"r/sub/grain.config.md": CFG}))
case("gf011-config-in-template", ["GF010", "GF011"], lib({
    "r/templates/t/t.template.md": "---\nform: folder\ntitle: T\n---\n",
    "r/templates/t/grain.config.md": CFG}))
case("gf020-schema", ["GF020"], lib({"r/tasks/a.task.md": task("state: proposed\n")}))
case("gf020-unknown-source", ["GF020"], lib({"r/t.template.md": "---\ntitle: T\n---\n",
     "r/triggers/a.trigger.md": '---\n"on":\n  task-state: {}\nfires:\n  template: ../t.template.md\n---\n'}))
case("gf020-removed-config", ["GF020"], lib({}, cfg("role: deployment\nbacklog: {lanes: []}\n")))
case("gf021-format", ["GF021"], lib({}, '---\nformat: 2\nowner: "@owner"\n---\n'))
case("gf022-unknown-field", ["GF022"], lib({"r/tasks/a.task.md": task("colour: blue\nx_note: fine\nkind: task\n")}))
case("gf030-dangling", ["GF030"], lib({"r/tasks/a.task.md": task("depends_on: [missing.task.md]\n")}))
case("gf030-capability-request", ["GF030"], lib({"r/tasks/a.task.md": task("capabilities: [../caps/none.capability.md]\n")}))
case("gf031-tombstone-loop", ["GF031"], lib({
    "r/tasks/a.task.md": task("depends_on: [x.task.md]\n"),
    "r/tasks/x.task.moved.md": '---\nto: y.task.md\nmoved_by: "@alice"\nmoved_at: "2026-10-16T09:00:00Z"\n---\n',
    "r/tasks/y.task.moved.md": '---\nto: x.task.md\nmoved_by: "@alice"\nmoved_at: "2026-10-16T09:00:00Z"\n---\n'}))
case("gf032-wrong-kind", ["GF032"], lib({"r/tasks/a.task.md": task("depends_on: [../l.lifecycle.md]\n"),
     "r/l.lifecycle.md": "---\ntitle: L\ninitial: p\nstates: {p: {category: proposed}, d: {category: done}}\n"
                         "transitions: [{from: p, to: [d]}]\n---\n"}))
case("gf033-alias", ["GF033"], lib({"r/tasks/a.task.md": task('capabilities: ["nope:caps/k.capability.md"]\n')}))
case("gf034-cycle", ["GF034"], lib({"r/tasks/a.task.md": task("depends_on: [b.task.md]\n"),
                                    "r/tasks/b.task.md": task("depends_on: [a.task.md]\n")}))
case("gf035-malformed-ref", ["GF035"], lib({"r/tasks/a.task.md": task(
    'capabilities: ["https://github.com/acme/lib.git//caps/k.capability.md"]\n')}))
case("gf035-query-parameter", ["GF035"], lib({"r/tasks/a.task.md": task(
    'capabilities: ["git::https://github.com/acme/lib.git//caps/k.capability.md?ref=v1&depth=1"]\n')}))
case("gf036-escapes-repo", ["GF036"], lib({"r/tasks/a.task.md": task("depends_on: [../../../x.task.md]\n")}))
case("gf037-unpinned", ["GF037"], dict(lib({"r/tasks/a.task.md": task(
    'capabilities: ["git::https://github.com/acme/lib.git//caps/k.capability.md?ref=main"]\n')}),
    **{"stubs.yaml": STUBS}, **LIBREPO))
case("gf037-pinning-required", ["GF037"], dict(lib({
    "r/tasks/a.task.md": task('capabilities: ["lib:caps/k.capability.md"]\n')},
    cfg('aliases:\n  lib: {source: "git::https://github.com/acme/lib.git"}\ncheck: {pinning: required}\n')),
    **{"stubs.yaml": STUBS}, **LIBREPO))
case("gf038-raw-aliased", ["GF038"], dict(lib({"r/tasks/a.task.md": task(
    'capabilities: ["git::https://github.com/acme/lib.git//caps/k.capability.md?ref=v1.0.0"]\n')},
    cfg('aliases:\n  lib: {source: "git::https://github.com/acme/lib.git?ref=v1.0.0"}\n')),
    **{"stubs.yaml": STUBS}, **LIBREPO))
case("gf039-two-versions", ["GF039"], dict(lib({}, cfg(
    'aliases:\n  lib: {source: "git::https://github.com/acme/lib.git?ref=v1.0.0"}\n'
    '  lib2: {source: "git::https://github.com/acme/lib.git//caps?ref=v2.0.0"}\n')), **{"stubs.yaml": STUBS}, **LIBREPO))
case("gf043-hash-mismatch", ["GF043"], dict(lib({}, cfg(
    'aliases:\n  lib: {source: "git::https://github.com/acme/lib.git?ref=v1.0.0", '
    'sha256: "h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}\n')), **{"stubs.yaml": STUBS}, **LIBREPO))
case("gf045-container-missing", ["GF045"], lib({"r/tasks/a.task/thread/20261016T090000Z-alice.comment.md": comment()}))
case("gf045-both-forms", ["GF045"], lib({"r/tasks/a.task.md": A, "r/tasks/a.task/task.md": A}))
LC = "---\ntitle: L\ninitial: p\nstates: {p: {category: proposed}, r: {category: ready}}\ntransitions: [{from: p, to: [r, z]}]\n---\n"
case("gf050-bad-lifecycle", ["GF050"], lib({"r/l.lifecycle.md": LC}))
DESIGN = ('---\ntitle: D\nstatus: approved\nowners: ["@carol"]\nrequirements:\n  - {id: D-1, title: One%s}\n---\n'
          '# D\n\n## D-1 One\n\nDo one thing.\n')
case("gf056-requirement-hash", ["GF056"], lib({"r/d.design.md": DESIGN % (', hash: "sha256:' + "0" * 64 + '"')}))
case("gf057-stale-implements", ["GF057"], lib({"r/d.design.md": DESIGN % "",
     "r/tasks/a.task.md": task('implements:\n  - {ref: "../d.design.md#D-1", hash: "sha256:%s"}\n' % ("1" * 64))}))
MAN = "---\nform: folder\ntitle: T\n%s---\n"
case("gf061-template-escape", ["GF061"], lib({"r/templates/t/t.template.md": MAN % "",
     "r/templates/t/a.task.md": task("depends_on: [../../tasks/x.task.md]\n"), "r/tasks/x.task.md": A}))
case("gf061-include-escape", ["GF061"], lib({"r/t.template.md": '---\ntitle: T\n---\n{% include "../../etc/passwd" %}\n'}))
case("gf062-template-cycle", ["GF062"], lib({"r/templates/t/t.template.md": MAN % "",
     "r/templates/t/a.task.md": task("depends_on: [b.task.md]\n"),
     "r/templates/t/b.task.md": task("depends_on: [a.task.md]\n")}))
case("gf063-missing-input", ["GF063"], lib({
    "r/t.template.md": "---\ntitle: T {{ who }}\ninputs:\n  - {name: who, required: true}\n---\nHi {{ who }}.\n",
    "r/triggers/a.trigger.md": '---\n"on":\n  manual: {}\nfires:\n  template: ../t.template.md\n---\n'}))
case("gf065-undefined", ["GF065"], lib({"r/t.template.md": "---\ntitle: T\n---\nHi {{ nobody }}.\n"}))
case("gf065-sandbox", ["GF065"], lib({"r/t.template.md": "---\ntitle: T\n---\n{{ ''.__class__.__mro__ }}\n"}))
case("gf065-random", ["GF065"], lib({"r/t.template.md": "---\ntitle: T\n---\n{{ [1, 2] | random }}\n"}))
case("gf066-nested-template", ["GF066"], lib({"r/templates/t/t.template.md": MAN % "",
     "r/templates/t/u/u.template.md": MAN % ""}))
case("gf068-manifest-name", ["GF068"], lib({"r/templates/t/other.template.md": MAN % ""}))
case("gf070-owners-syntax", ["GF070"], lib({"r/GRAINOWNERS": "*  @owner\n/tasks/\n"}))
case("gf071-owners-identity", ["GF071"], lib({"r/GRAINOWNERS": "*  @owner\n/tasks/  user:alice@example.com\n"}))
case("gf072-no-rule", ["GF072"], lib({"r/GRAINOWNERS": "/tasks/  @owner\n", "r/notes/n.task.md": A}))
case("gf081-offer-unresolved", ["GF081"], lib({}, cfg(
    'deployment:\n  account: "@bot"\n  offers: [caps/missing.capability.md]\n')))
DEPCFG = cfg('deployment:\n  account: "@bot"\n  offers: [caps/a.capability.md]\n')
case("gf082-not-offered", ["GF082"], dict(lib({
    "r/caps/a.capability.md": "---\ntitle: A\n---\n", "r/caps/b.capability.md": "---\ntitle: B\n---\n",
    "r/tasks/x.task.md": task("capabilities: [../caps/b.capability.md]\n"),
    "r/records/20261017T020000Z-x.record/record.md": record(
        'definition: "git::https://github.com/acme/r.git//tasks/x.task.md?ref=%s"\n' % SHA)}, DEPCFG),
    **{"stubs.yaml": "https://github.com/acme/r.git: r\n"}))
case("gf083-unpinned-definition", ["GF083"], dict(lib({
    "r/tasks/x.task.md": A,
    "r/records/20261017T020000Z-x.record/record.md": record(
        'definition: "git::https://github.com/acme/r.git//tasks/x.task.md?ref=main"\n')}),
    **{"stubs.yaml": "https://github.com/acme/r.git: r\n"}))

# ---- the forge check (§14.4)
OWN = "*  @owner\n/tasks/  @acme/hub\n/tasks/**/thread/  *(create)\n/tasks/:capabilities  @acme/security\n/records/  @bot\n"
base = lib({"r/GRAINOWNERS": OWN, "r/tasks/a.task.md": A, "r/caps/k.capability.md": "---\ntitle: K\n---\n"})
REC = {"r/records/20261017T020000Z-a.record/record.md": record('task: {title: T, prompt: P}\n')}
case("gf073-unauthorised-field", ["GF073"], dict(base, **{"r/tasks/a.task.md": task("capabilities: [../caps/k.capability.md]\n")}),
     base=base, actors=["@dave", "@acme/hub"])
case("gf073-comment-author", ["GF073"], dict(base, **{"r/tasks/a.task.md": None} if False else dict(base, **{
    "r/tasks/b.task/task.md": A, "r/tasks/b.task/thread/20261016T090000Z-alice.comment.md": comment("@alice")})),
     base=dict(base, **{"r/tasks/b.task/task.md": A}), actors=["@mallory"])
case("gf052-record-changed", ["GF052"], dict(base, **{k: v.replace("Done.", "Edited.") for k, v in REC.items()}),
     base=dict(base, **REC), actors=["@bot"])
case("commenter-folds-definition", [], dict({k: v for k, v in base.items() if k != "r/tasks/a.task.md"}, **{
    "r/tasks/a.task/task.md": A, "r/tasks/a.task/thread/20261016T090000Z-erin.comment.md": comment("@erin")}),
     base=base, actors=["@erin"])
case("security-adds-capability", [], dict(base, **{"r/tasks/a.task.md": task("capabilities: [../caps/k.capability.md]\n")}),
     base=base, actors=["@sam", "@acme/security"])
case("deployment-writes-record", [], dict(base, **REC), base=base, actors=["@bot"])
case("comment-appended-to-record", [], dict(base, **REC, **{
    "r/records/20261017T020000Z-a.record/comments/20261017T090000Z-bot.comment.md": comment("@bot", "2026-10-17T09:00:00Z")}),
     base=dict(base, **REC), actors=["@bot"])
print("fixtures written")
