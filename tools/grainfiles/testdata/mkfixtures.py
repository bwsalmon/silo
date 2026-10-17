import pathlib, textwrap, shutil
B = pathlib.Path(__file__).resolve().parent
for d in ("bad", "good"):
    shutil.rmtree(B / d, ignore_errors=True)
LIB = "---\nkind: config\nschema: grain/config@v1\nrole: library\nowner: {team: hub}\n---\n"
DEP = "---\nkind: config\nschema: grain/config@v1\nrole: deployment\ndeployment: {id: eu}\nowner: {user: a@example.com}\nbacklog:\n  lanes:\n    - {name: own, path: backlog/}\n---\n"
RC = "schema: grain/repo-config@v1\n"
def task(state="proposed", extra=""):
    return f'---\nkind: task\nschema: grain/task@v1\ntitle: T\nstate: {state}\nsubmitter: user:a@example.com\ncreated_at: "2026-10-16T09:00:00Z"\n{extra}---\nDo it.\n'
def case(name, codes, files, base=None, actors=None):
    root = B / ("bad" if codes else "good") / name
    def put(top, fs):
        for p, s in fs.items():
            f = top / p
            f.parent.mkdir(parents=True, exist_ok=True)
            f.write_text(textwrap.dedent(s) if not s.startswith("---") else s)
    if base is not None:
        put(root / "base", base)
        put(root / "head", files)
        (root / "authorize.yaml").write_text("actors: %s\n" % actors)
    else:
        put(root, files)
    if codes:
        (root / "expect.txt").write_text(" ".join(codes) + "\n")
def lib(fs):
    d = {"r/.grainconfig": RC, "r/grain.config.md": LIB}
    d.update(fs); return d

case("gf001-bad-name", ["GF001"], lib({"r/tasks/Big-Name.task.md": task()}))
case("gf002-kind-mismatch", ["GF002"], lib({"r/tasks/a.task.md": task().replace("kind: task", "kind: ref")}))
case("gf003-no-front-matter", ["GF003"], lib({"r/tasks/a.task.md": "Just text.\n"}))
case("gf004-duplicate-key", ["GF004"], lib({"r/tasks/a.task.md": task(extra="title: Again\n")}))
case("gf005-unknown-suffix", ["GF005"], lib({"r/notes/x.widget.md": "---\nkind: widget\n---\n"}))
case("gf007-reserved-folder", ["GF007"], lib({"r/tasks/a.task.md": task(), "r/tasks/a.thread/b.task.md": task()}))
case("gf010-nested-folder", ["GF010"], lib({"r/sub/grain.config.md": LIB}))
case("gf011-config-in-template", ["GF010", "GF011"], lib({
    "r/templates/t/t.template.md": "---\nkind: template\nschema: grain/template@v1\nform: folder\ntitle: T\n---\n",
    "r/templates/t/grain.config.md": LIB}))
case("gf020-schema", ["GF020"], lib({"r/tasks/a.task.md": task().replace("title: T\n", "")}))
case("gf022-unknown-field", ["GF022"], lib({"r/tasks/a.task.md": task(extra="colour: blue\nx_note: fine\n")}))
case("gf023-schema-kind", ["GF023"], lib({"r/tasks/a.task.md": task().replace("grain/task@v1", "grain/ref@v1")}))
case("gf030-dangling", ["GF030"], lib({"r/tasks/a.task.md": task(extra="depends_on: [missing.task.md]\n")}))
case("gf031-redirect-loop", ["GF031"], lib({
    "r/tasks/a.task.md": task(extra="depends_on: [x.task.md]\n"),
    "r/tasks/x.task.moved.md": '---\nkind: moved\nschema: grain/moved@v1\nto: y.task.md\nmoved_by: user:a@example.com\nmoved_at: "2026-10-16T09:00:00Z"\n---\n',
    "r/tasks/y.task.moved.md": '---\nkind: moved\nschema: grain/moved@v1\nto: x.task.md\nmoved_by: user:a@example.com\nmoved_at: "2026-10-16T09:00:00Z"\n---\n'}))
case("gf032-wrong-kind", ["GF032"], lib({"r/tasks/a.task.md": task(extra="depends_on: [../memory/m.memory.md]\n"),
    "r/memory/m.memory.md": "---\nkind: memory\nschema: grain/memory@v1\ntitle: M\n---\n"}))
case("gf033-alias", ["GF033"], lib({"r/tasks/a.task.md": task(extra='depends_on: ["nope:tasks/b.task.md"]\n')}))
case("gf034-cycle", ["GF034"], lib({"r/tasks/a.task.md": task(extra="depends_on: [b.task.md]\n"),
                                     "r/tasks/b.task.md": task(extra="depends_on: [a.task.md]\n")}))
case("gf035-malformed-ref", ["GF035", "GF020"], lib({"r/tasks/a.task.md": task(extra='depends_on: ["two words.task.md"]\n')}))
case("gf036-escapes-repo", ["GF036"], lib({"r/tasks/a.task.md": task(extra="depends_on: [../../../x.task.md]\n")}))
case("gf040-orphan", ["GF040"], {"r/.grainconfig": RC, "r/grain.config.md": DEP, "r/backlog/.keep": "",
                                  "r/tasks/lonely.task.md": task()})
case("gf041-misplaced-comment", ["GF041"], lib({"r/tasks/a.task.md": task(), "r/tasks/b.task.md": task(),
    "r/tasks/a.thread/20261016T090000Z-user-a-example-com.comment.md":
    '---\nkind: comment\nschema: grain/comment@v1\ntask: ../b.task.md\nauthor: user:a@example.com\nat: "2026-10-16T09:00:00Z"\n---\nHi.\n'}))
case("gf042-stray-comment", ["GF042"], lib({"r/tasks/a.task.md": task(),
    "r/notes/20261016T090000Z-user-a-example-com.comment.md":
    '---\nkind: comment\nschema: grain/comment@v1\ntask: ../tasks/a.task.md\nauthor: user:a@example.com\nat: "2026-10-16T09:00:00Z"\n---\nHi.\n'}))
case("gf044-tombstone-unlisted", ["GF044"], lib({"r/tasks/new.task.md": task(),
    "r/tasks/old.task.moved.md": '---\nkind: moved\nschema: grain/moved@v1\nto: new.task.md\nmoved_by: user:a@example.com\nmoved_at: "2026-10-16T09:00:00Z"\n---\n'}))
LC = '---\nkind: lifecycle\nschema: grain/lifecycle@v1\ntitle: L\ninitial: new\nstates:\n  new: {category: proposed}\n  go: {category: ready}\n  ok: {category: done}\n  no: {category: cancelled}\ntransitions:\n  - {from: new, to: [go, no]}\n  - {from: go, to: [ok]}\n%s---\n'
case("gf050-bad-lifecycle", ["GF050"], lib({"r/lifecycles/l.lifecycle.md": LC.replace("{from: go, to: [ok]}", "{from: go, to: [gone]}") % ""}))
case("gf051-unknown-state", ["GF051"], lib({"r/lifecycles/l.lifecycle.md": LC % "", "r/tasks/a.task.md": task("sec-approved", "lifecycle: ../lifecycles/l.lifecycle.md\n")}))
case("gf054-alias-collision", ["GF054"], lib({"r/lifecycles/l.lifecycle.md": LC % "aliases: {go: ok}\n"}))
case("gf055-cancelled-dependency", ["GF055"], lib({"r/tasks/a.task.md": task(extra="depends_on: [b.task.md]\n"),
    "r/tasks/b.task.md": task("rejected")}))
FT = "---\nkind: template\nschema: grain/template@v1\nform: folder\ntitle: T\n%s---\n"
case("gf061-template-escape", ["GF061"], lib({"r/templates/t/t.template.md": FT % "",
    "r/templates/t/a.task.md": task(extra="depends_on: [../../tasks/x.task.md]\n"), "r/tasks/x.task.md": task()}))
case("gf062-template-cycle", ["GF062"], lib({"r/templates/t/t.template.md": FT % "",
    "r/templates/t/a.task.md": task(extra="depends_on: [b.task.md]\n"), "r/templates/t/b.task.md": task(extra="depends_on: [a.task.md]\n")}))
case("gf063-missing-input", ["GF063"], lib({"r/templates/t.template.md":
    "---\nkind: template\nschema: grain/template@v1\ntitle: T\ninputs:\n  - {name: version, type: string, required: true}\n---\n{{ version }}\n"}))
case("gf064-template-reached-by-root", ["GF064"], {"r/.grainconfig": RC, "r/grain.config.md": DEP,
    "r/backlog/rq/rq.template.md": FT % "", "r/backlog/rq/a.ref.md":
    '---\nkind: ref\nschema: grain/ref@v1\nto: a.task.md\nrank: "m"\nadded_by: hub\nadded_at: "2026-10-16T09:00:00Z"\n---\n',
    "r/backlog/rq/a.task.md": task()})
case("gf065-template-subset", ["GF065"], lib({"r/templates/t.template.md":
    "---\nkind: template\nschema: grain/template@v1\ntitle: T\n---\n{% extends 'x' %}\n"}))
case("gf066-nested-template", ["GF066"], lib({"r/templates/t/t.template.md": FT % "", "r/templates/t/u/u.template.md": FT % ""}))
case("gf067-instance-provenance", ["GF067"], lib({
    "r/tasks/rq-2026-10-17/a.task.md": task(extra='instantiated_from: "https://x.example.com/r.git//t/t.template.md@%s"\n' % ("a" * 40)),
    "r/tasks/rq-2026-10-17/b.task.md": task()}))
case("gf068-manifest-name", ["GF068"], lib({"r/templates/t/other.template.md": FT % ""}))
case("gf070-owners-syntax", ["GF070"], lib({"r/GRAINOWNERS": "*\n"}))
case("gf071-owners-principal", ["GF071"], lib({"r/GRAINOWNERS": "* @robot:x\n"}))
case("gf090-suite", ["GF090"], lib({"r/suites/s.suite.md": "---\nkind: suite\n---\n"}))
# authorisation mode (§16.4)
OWN = "*  @team:hub#admin\n/tasks/  @authenticated(create) @team:hub#member\n/tasks/**/*.task.md:state(*->go)  @team:sec#approver\n"
BASEFS = lib({"r/GRAINOWNERS": OWN, "r/lifecycles/l.lifecycle.md": LC % "", "r/tasks/a.task.md": task("new", "lifecycle: ../lifecycles/l.lifecycle.md\n")})
HEADGO = dict(BASEFS); HEADGO["r/tasks/a.task.md"] = task("go", "lifecycle: ../lifecycles/l.lifecycle.md\napproved_by: user:s@example.com\napproved_at: \"2026-10-16T10:00:00Z\"\n")
case("gf074-unauthorised-transition", ["GF074"], HEADGO, BASEFS, ["user:dev@example.com", "team:hub#member"])
case("sec-approver-may-transition", [], HEADGO, BASEFS, ["user:s@example.com", "team:sec#approver", "team:hub#member"])
HEADBAD = dict(BASEFS); HEADBAD["r/tasks/a.task.md"] = task("ok", "lifecycle: ../lifecycles/l.lifecycle.md\n")
case("gf052-transition-not-in-lifecycle", ["GF052", "GF074"], HEADBAD, BASEFS, ["user:dev@example.com"])
HEADT = dict(BASEFS); HEADT["r/tasks/a.task.md"] = task("new", "lifecycle: ../lifecycles/l.lifecycle.md\n").replace("Do it.", "Do something else.")
case("gf073-unauthorised-field", ["GF073"], HEADT, BASEFS, ["user:dev@example.com"])
