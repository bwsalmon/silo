#!/usr/bin/env python3
"""Regenerate tools/grainfiles/testdata/bad: minimal fixtures covering all lint codes.
Run: python3 tools/grainfiles/testdata/mkfixtures.py"""
import pathlib, shutil

B = pathlib.Path(__file__).resolve().parent
for d in ("bad", "good"):
    shutil.rmtree(B / d, ignore_errors=True)

DEP = "https://github.com/acme/grain-eu"
USR = "Bruce Salmon <bsalmon@google.com>"


def task(extra="", body="Run the task.\n", title="T", requestor=USR, recipient=DEP):
    req_line = 'requestor: "%s"\n' % requestor if requestor is not None else ""
    rec_line = 'recipient: "%s"\n' % recipient if recipient is not None else ""
    return ('---\ngrainfile: v1\ntitle: %s\n%s%s%s---\n%s'
            % (title, req_line, rec_line, extra, body))


def process(extra="when:\n  manual: {}\n", body="Run the process.\n", title="P"):
    return ('---\ngrainfile: v1\ntitle: %s\n%s---\n%s'
            % (title, extra, body))


def put(top, fs):
    for p, s in fs.items():
        f = top / p
        f.parent.mkdir(parents=True, exist_ok=True)
        f.write_text(s)


def case(name, codes, files):
    root = B / "bad" / name
    put(root, files)
    (root / "expect.txt").write_text(" ".join(codes) + "\n")


A = task()

case("gf001-bad-name", ["GF001"], {"r/tasks/Big-Name.task.grain.md": A})
case("gf001-compact-utc-stem", ["GF001"], {"r/tasks/20260417T143000Z-fix.task.grain.md": A})
case("gf003-no-front-matter", ["GF003"], {"r/tasks/a.task.grain.md": "Just text.\n"})
case("gf003-unclosed-front-matter", ["GF003"], {"r/tasks/a.task.grain.md": "---\ntitle: T\n"})
case("gf004-duplicate-key", ["GF004"], {"r/tasks/a.task.grain.md": task("title: Again\n")})
case("gf004-yaml-anchor", ["GF004"], {
    "r/tasks/a.task.grain.md": '---\ntitle: &t T\nrequestor: "%s"\nrecipient: "%s"\n---\nBody.\n' % (USR, DEP),
})
case("gf005-unknown-suffix", ["GF005"], {"r/notes/x.widget.grain.md": "---\ntitle: x\n---\n"})
case("gf006-case-collision", ["GF006"], {
    "r/docs/note.md": "# Note\n",
    "r/docs/Note.md": "# Note\n",
    "r/tasks/a.task.grain.md": A,
})
case("gf020-bad-grainfile-version", ["GF020"], {
    "r/tasks/a.task.grain.md": "---\ngrainfile: v2\ntitle: T\n---\nDo it.\n",
})
case("gf020-user-recipient", ["GF020"], {
    "r/tasks/a.task.grain.md": task(recipient=USR),
})
case("gf020-bad-requestor", ["GF020"], {
    "r/tasks/a.task.grain.md": task(requestor="@alice"),
})
case("gf020-process-recipient", ["GF020"], {
    "r/processes/p.process.grain.md": process(extra='recipient: "%s"\nwhen:\n  manual: {}\n' % DEP),
})
case("gf020-process-depends-on", ["GF020"], {
    "r/tasks/b.task.grain.md": A,
    "r/processes/p.process.grain.md": process(extra="when:\n  manual: {}\ndepends_on: [../tasks/b.task.grain.md]\n"),
})
case("gf020-trigger-with-removed", ["GF020"], {
    "r/processes/p.process.grain.md": process(extra="when:\n  manual: {}\ntrigger_with: [./b.process.grain.md]\n"),
})
case("gf020-process-without-trigger", ["GF020"], {
    "r/processes/p.process.grain.md": process(extra="gate: true\n"),
})
case("gf020-unknown-source", ["GF020"], {
    "r/processes/p.process.grain.md": process(extra="when:\n  task-state: {}\n"),
})
case("gf020-bad-cron", ["GF020"], {
    "r/processes/p.process.grain.md": process(extra='when:\n  schedule:\n    cron: "0 2 * *"\n'),
})
case("gf020-push-without-filter", ["GF020"], {
    "r/processes/p.process.grain.md": process(extra='when:\n  push:\n    ignore: ["docs/**"]\n'),
})
case("gf020-bad-draft", ["GF020"], {
    "r/tasks/a.task.grain.md": task('draft: "yes"\n'),
})
case("gf020-bad-not-before", ["GF020"], {
    "r/tasks/a.task.grain.md": task('not_before: "2026-02-30"\n'),
})
case("gf020-bad-not-before-format", ["GF020"], {
    "r/processes/p.process.grain.md": process(extra='when:\n  manual: {}\nnot_before: "2026-10-25T09:00:00+02:00"\n'),
})
case("gf022-unknown-field", ["GF022"], {
    "r/tasks/a.task.grain.md": task("colour: blue\nx_note: allowed\n", requestor=None),
})
case("gf030-dangling-depends", ["GF030"], {
    "r/tasks/a.task.grain.md": task("depends_on: [./missing.task.grain.md]\n"),
})
case("gf030-missing-anchor", ["GF030"], {
    "r/docs/design.md": "# Overview\n\nSome text.\n",
    "r/tasks/a.task.grain.md": task("references: [/docs/design.md#nonexistent-section]\n"),
})
case("gf032-depends-on-asset", ["GF032"], {
    "r/docs/design.md": "# Design\n",
    "r/tasks/a.task.grain.md": task("depends_on: [/docs/design.md]\n"),
})
case("gf032-task-depends-on-process", ["GF032"], {
    "r/processes/p.process.grain.md": process(),
    "r/tasks/a.task.grain.md": task("depends_on: [../processes/p.process.grain.md]\n"),
})
case("gf032-depends-on-template", ["GF032"], {
    "r/tasks/tmpl.task.grain.md.jinja2": task(title='Rollout {{ region | default("eu") }}'),
    "r/tasks/a.task.grain.md": task("depends_on: [./tmpl.task.grain.md.jinja2]\n"),
})
case("gf032-recipient-is-file", ["GF032"], {
    "r/tasks/b.task.grain.md": A,
    "r/tasks/a.task.grain.md": task(recipient="./b.task.grain.md"),
})
case("gf033-jinja2-syntax", ["GF033"], {
    "r/tasks/a.task.grain.md.jinja2": task(body="Run {{ unclosed\n"),
})
case("gf034-depends-cycle", ["GF034"], {
    "r/tasks/a.task.grain.md": task("depends_on: [./b.task.grain.md]\n"),
    "r/tasks/b.task.grain.md": task("depends_on: [./a.task.grain.md]\n"),
})
case("gf034-jinja2-include", ["GF034"], {
    "r/tasks/a.task.grain.md.jinja2": task(body='{% include "other.md" %}\n'),
})
case("gf035-malformed-ref", ["GF035"], {
    "r/tasks/a.task.grain.md": task('references: ["git::https://github.com/acme/widgets.git//docs/d.md"]\n'),
})
case("gf035-query-parameter", ["GF035"], {
    "r/tasks/a.task.grain.md": task('references: ["https://github.com/acme/widgets/blob/main/docs/d.md?ref=main"]\n'),
})
case("gf036-escapes-repo", ["GF036"], {
    "r/tasks/a.task.grain.md": task("depends_on: [../../../outside.task.grain.md]\n"),
})
print("fixtures written")
