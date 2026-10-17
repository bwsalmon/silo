import hashlib, pathlib, textwrap, shutil
B = pathlib.Path(__file__).resolve().parent
for d in ("bad", "good"):
    shutil.rmtree(B / d, ignore_errors=True)
LIB = "---\nkind: config\nschema: grain/config@v1\nrole: library\nowner: {team: hub}\n---\n"
DEP = "---\nkind: config\nschema: grain/config@v1\nrole: deployment\ndeployment: {id: eu}\nowner: {user: a@example.com}\nbacklog:\n  lanes:\n    - {name: own, path: backlog/}\n---\n"
RC = "schema: grain/repo-config@v1\n"
def task(extra=""):
    return f'---\nkind: task\nschema: grain/task@v1\ntitle: T\nsubmitter: user:a@example.com\ncreated_at: "2026-10-16T09:00:00Z"\n{extra}---\nDo it.\n'
def sha(text):
    return "sha256:" + hashlib.sha256(text.encode()).hexdigest()
REV = "0c4e9a7b5d3f1e2a6b8c9d0e1f2a3b4c5d6e7f80"
def exec_md(definition, defn_text, state="queued", extra=""):
    return (f'---\nkind: exec\nschema: grain/exec@v1\ndefinition: "{definition}@{REV}"\ndefinition_sha256: "{sha(defn_text)}"\n'
            f'state: {state}\ncreated_by: user:a@example.com\ncreated_at: "2026-10-16T10:00:00Z"\nrank: "m"\n{extra}---\n')
def execution(at, definition, defn_text, state="queued", extra=""):
    """An execution folder at AT (folder-relative to r/) of the definition DEFINITION (relative to exec.md)."""
    return {f"r/{at}/exec.md": exec_md(definition, defn_text, state, extra), f"r/{at}/task.md": defn_text}
def dep(fs):
    d = {"r/.grainconfig": RC, "r/grain.config.md": DEP}
    d.update(fs); return d
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

A = task()
TRIG = '---\nkind: trigger\nschema: grain/trigger@v1\n"on":\n  manual: {}\nfires:\n  template: ../t.template.md\n---\n'
COMMENT = '---\nkind: comment\nschema: grain/comment@v1\nauthor: user:a@example.com\nat: "2026-10-16T09:00:00Z"\n---\nHi.\n'
case("gf001-bad-name", ["GF001"], lib({"r/tasks/Big-Name.task.md": A}))
case("gf002-kind-mismatch", ["GF002"], lib({"r/tasks/a.task.md": A.replace("kind: task", "kind: memory")}))
case("gf003-no-front-matter", ["GF003"], lib({"r/tasks/a.task.md": "Just text.\n"}))
case("gf004-duplicate-key", ["GF004"], lib({"r/tasks/a.task.md": task("title: Again\n")}))
case("gf005-unknown-suffix", ["GF005"], lib({"r/notes/x.widget.md": "---\nkind: widget\n---\n"}))
case("gf007-reserved-folder", ["GF007"], lib({"r/tasks/a.task/task.md": A, "r/tasks/a.task/b.task.md": A,
    "r/notes/20261016T090000Z-user-a-example-com.comment.md": COMMENT}))
case("gf010-nested-folder", ["GF010"], lib({"r/sub/grain.config.md": LIB}))
case("gf011-config-in-template", ["GF010", "GF011"], lib({
    "r/templates/t/t.template.md": "---\nkind: template\nschema: grain/template@v1\nform: folder\ntitle: T\n---\n",
    "r/templates/t/grain.config.md": LIB}))
case("gf020-schema", ["GF020"], lib({"r/tasks/a.task.md": task("state: proposed\n")}))
case("gf022-unknown-field", ["GF022"], lib({"r/tasks/a.task.md": task("colour: blue\nx_note: fine\n")}))
case("gf023-schema-kind", ["GF023"], lib({"r/tasks/a.task.md": A.replace("grain/task@v1", "grain/exec@v1")}))
case("gf030-dangling", ["GF030"], lib({"r/tasks/a.task.md": task("depends_on: [missing.task.md]\n")}))
case("gf031-redirect-loop", ["GF031"], lib({
    "r/tasks/a.task.md": task("depends_on: [x.task.md]\n"),
    "r/tasks/x.task.moved.md": '---\nkind: moved\nschema: grain/moved@v1\nto: y.task.md\nmoved_by: user:a@example.com\nmoved_at: "2026-10-16T09:00:00Z"\n---\n',
    "r/tasks/y.task.moved.md": '---\nkind: moved\nschema: grain/moved@v1\nto: x.task.md\nmoved_by: user:a@example.com\nmoved_at: "2026-10-16T09:00:00Z"\n---\n'}))
case("gf032-wrong-kind", ["GF032"], lib({"r/tasks/a.task.md": task("depends_on: [../memory/m.memory.md]\n"),
    "r/memory/m.memory.md": "---\nkind: memory\nschema: grain/memory@v1\ntitle: M\n---\n"}))
case("gf033-alias", ["GF033"], lib({"r/tasks/a.task.md": task('depends_on: ["nope:tasks/b.task.md"]\n')}))
case("gf034-cycle", ["GF034"], dep({"r/tasks/a.task.md": A,
    **execution("backlog/a.exec", "/tasks/a.task.md", A, extra="depends_on: [../b.exec/]\n"),
    **execution("backlog/b.exec", "/tasks/a.task.md", A, extra="depends_on: [../a.exec/]\n")}))
case("gf035-malformed-ref", ["GF035", "GF020"], lib({"r/tasks/a.task.md": task('depends_on: ["two words.task.md"]\n')}))
case("gf036-escapes-repo", ["GF036"], lib({"r/tasks/a.task.md": task("depends_on: [../../../x.task.md]\n")}))
case("gf040-orphan", ["GF040"], dep({"r/backlog/.keep": "", "r/t.template.md": "---\nkind: template\nschema: grain/template@v1\ntitle: T\n---\nT\n",
    "r/other/x.trigger.md": TRIG}))
case("gf044-tombstone-unlisted", ["GF044"], lib({"r/tasks/new.task.md": A,
    "r/tasks/old.task.moved.md": '---\nkind: moved\nschema: grain/moved@v1\nto: new.task.md\nmoved_by: user:a@example.com\nmoved_at: "2026-10-16T09:00:00Z"\n---\n'}))
case("gf045-snapshot-missing", ["GF045"], dep({"r/tasks/a.task.md": A,
    "r/backlog/a.exec/exec.md": exec_md("/tasks/a.task.md", A)}))
case("gf046-definition-edited", ["GF046"], dep({"r/tasks/a.task.md": A.replace("Do it.", "Do it differently."),
    **execution("backlog/a.exec", "/tasks/a.task.md", A)}))
case("gf047-exec-outside-lane", ["GF047"], lib({"r/tasks/a.task.md": A,
    **execution("tasks/a.exec", "/tasks/a.task.md", A)}))
case("gf048-duplicate-exec", ["GF048"], {"r/.grainconfig": RC, "r/tasks/a.task.md": A,
    "r/grain.config.md": DEP.replace("    - {name: own, path: backlog/}\n", "    - {name: own, path: backlog/}\n    - {name: ops, path: ops/}\n"),
    **execution("backlog/a.exec", "/tasks/a.task.md", A), **execution("ops/a.exec", "/tasks/a.task.md", A)})
LC = '---\nkind: lifecycle\nschema: grain/lifecycle@v1\ntitle: L\ninitial: new\nstates:\n  new: {category: proposed}\n  go: {category: ready}\n  ok: {category: done}\n  no: {category: cancelled}\ntransitions:\n  - {from: new, to: [go, no]}\n  - {from: go, to: [ok]}\n%s---\n'
case("gf050-bad-lifecycle", ["GF050"], lib({"r/lifecycles/l.lifecycle.md": LC.replace("{from: go, to: [ok]}", "{from: go, to: [gone]}") % ""}))
case("gf051-unknown-state", ["GF051"], dep({"r/lifecycles/l.lifecycle.md": LC % "", "r/tasks/a.task.md": A,
    **execution("backlog/a.exec", "/tasks/a.task.md", A, "sec-approved", "lifecycle: /lifecycles/l.lifecycle.md\n")}))
case("gf054-alias-collision", ["GF054"], lib({"r/lifecycles/l.lifecycle.md": LC % "aliases: {go: ok}\n"}))
case("gf055-cancelled-dependency", ["GF055"], dep({"r/tasks/a.task.md": A,
    **execution("backlog/a.exec", "/tasks/a.task.md", A, extra="depends_on: [../b.exec/]\n"),
    **execution("backlog/b.exec", "/tasks/a.task.md", A, "rejected")}))
FT = "---\nkind: template\nschema: grain/template@v1\nform: folder\ntitle: T\n%s---\n"
case("gf061-template-escape", ["GF061"], lib({"r/templates/t/t.template.md": FT % "",
    "r/templates/t/a.task.md": task("depends_on: [../../tasks/x.task.md]\n"), "r/tasks/x.task.md": A}))
case("gf062-template-cycle", ["GF062"], lib({"r/templates/t/t.template.md": FT % "",
    "r/templates/t/a.task.md": task("depends_on: [b.task.md]\n"), "r/templates/t/b.task.md": task("depends_on: [a.task.md]\n")}))
case("gf063-missing-input", ["GF063"], lib({"r/templates/t.template.md":
    "---\nkind: template\nschema: grain/template@v1\ntitle: T\ninputs:\n  - {name: version, type: string, required: true}\n---\n{{ version }}\n"}))
case("gf064-template-reached-by-root", ["GF064"], dep({"r/backlog/.keep": "",
    "r/grain.config.md": DEP[:-4] + "triggers:\n  - path: triggers/\n---\n",
    "r/triggers/t/t.template.md": FT % "", "r/triggers/t/x.trigger.md": TRIG.replace("../t.template.md", "t.template.md")}))
case("gf065-template-subset", ["GF065"], lib({"r/templates/t.template.md":
    "---\nkind: template\nschema: grain/template@v1\ntitle: T\n---\n{% extends 'x' %}\n"}))
case("gf066-nested-template", ["GF066"], lib({"r/templates/t/t.template.md": FT % "", "r/templates/t/u/u.template.md": FT % ""}))
case("gf067-instance-provenance", ["GF067"], dep({"r/tasks/a.task.md": A,
    **execution("backlog/rq-2026-10-17/a.exec", "/tasks/a.task.md", A,
                extra='instantiated_from: "https://x.example.com/r.git//t/t.template.md@%s"\n' % ("a" * 40)),
    **execution("backlog/rq-2026-10-17/b.exec", "/tasks/a.task.md", A)}))
case("gf068-manifest-name", ["GF068"], lib({"r/templates/t/other.template.md": FT % ""}))
B_ = task("depends_on: [a.task.md]\n")
case("gf069-instance-mismatch", ["GF069"], dep({"r/templates/t/t.template.md": FT % "",
    "r/templates/t/a.task.md": A, "r/templates/t/b.task.md": B_,
    **execution("backlog/t-2026-10-17/a.exec", "/templates/t/a.task.md", A,
                extra='instantiated_from: "/templates/t/t.template.md@%s"\n' % ("a" * 40)),
    **execution("backlog/t-2026-10-17/b.exec", "/templates/t/b.task.md", B_,
                extra='instantiated_from: "/templates/t/t.template.md@%s"\n' % ("a" * 40))}))
case("gf070-owners-syntax", ["GF070"], lib({"r/GRAINOWNERS": "*\n"}))
case("gf071-owners-principal", ["GF071"], lib({"r/GRAINOWNERS": "* @robot:x\n"}))
case("gf090-suite", ["GF090"], lib({"r/suites/s.suite.md": "---\nkind: suite\n---\n"}))
# authorisation mode (§16.4)
OWN = ("*  @user:a@example.com\n"
       "/backlog/**/*.exec/  @team:hub#member(create,move)\n"
       "/backlog/**/*.exec/exec.md:state(*->go),approved_by,approved_at  @team:sec#approver\n"
       "/backlog/**/*.exec/thread/  @authenticated(create)\n"
       "/backlog/**/*.exec/runs/  @deployment:eu\n"
       "/tasks/  @team:hub#member\n"
       "/tasks/**/thread/  @authenticated(create)\n")
LCX = "lifecycle: /lifecycles/l.lifecycle.md\n"
BASEFS = dep({"r/GRAINOWNERS": OWN, "r/lifecycles/l.lifecycle.md": LC % "", "r/tasks/a.task.md": A,
              **execution("backlog/a.exec", "/tasks/a.task.md", A, "new", LCX)})
HEADGO = dict(BASEFS); HEADGO["r/backlog/a.exec/exec.md"] = exec_md("/tasks/a.task.md", A, "go",
    LCX + 'approved_by: user:s@example.com\napproved_at: "2026-10-16T10:00:00Z"\n')
case("gf074-unauthorised-transition", ["GF073", "GF074"], HEADGO, BASEFS, ["user:dev@example.com", "team:hub#member"])
case("sec-approver-may-transition", [], HEADGO, BASEFS, ["user:s@example.com", "team:sec#approver", "team:hub#member"])
HEADBAD = dict(BASEFS); HEADBAD["r/backlog/a.exec/exec.md"] = exec_md("/tasks/a.task.md", A, "ok", LCX)
case("gf052-transition-not-in-lifecycle", ["GF052", "GF074"], HEADBAD, BASEFS, ["user:dev@example.com"])
HEADT = dict(BASEFS); HEADT["r/backlog/a.exec/exec.md"] = exec_md("/tasks/a.task.md", A, "new", LCX).replace('rank: "m"', 'rank: "a"')
case("gf073-unauthorised-field", ["GF073"], HEADT, BASEFS, ["user:dev@example.com"])
HEADS = dict(BASEFS); HEADS["r/backlog/a.exec/task.md"] = A.replace("Do it.", "Do something else.")
case("gf052-snapshot-changed", ["GF045", "GF052"], HEADS, BASEFS, ["user:a@example.com"])
HEADF = dict(BASEFS); del HEADF["r/tasks/a.task.md"]
HEADF["r/tasks/a.task/task.md"] = A
HEADF["r/tasks/a.task/thread/20261016T110000Z-user-c-example-com.comment.md"] = COMMENT.replace("user:a@example.com", "user:c@example.com")
case("commenter-folds-definition", [], HEADF, BASEFS, ["user:c@example.com"])
HEADM = {k.replace("r/backlog/a.exec/", "r/backlog/later/a.exec/"): v for k, v in BASEFS.items()}
case("member-moves-execution", [], HEADM, BASEFS, ["user:m@example.com", "team:hub#member"])
