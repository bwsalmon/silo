#!/usr/bin/env python3
"""Regenerate tools/grainfiles/testdata/bad: one minimal fixture per lint code.
Run: python3 tools/grainfiles/testdata/mkfixtures.py"""
import pathlib, shutil

B = pathlib.Path(__file__).resolve().parent
for d in ("bad", "good"):
    shutil.rmtree(B / d, ignore_errors=True)

DEP = "https://github.com/acme/grain-eu"
USR = "Bruce Salmon <bsalmon@google.com>"


def task(extra="", body="Run the task.\n", title="T", requestor=USR, recipient=DEP):
    req_line = 'requestor: "%s"\n' % requestor if requestor is not None else ""
    return ('---\ntitle: %s\n%srecipient: "%s"\n%s---\n%s'
            % (title, req_line, recipient, extra, body))


def process(extra='"on":\n  manual: {}\n', body="Run the process.\n", title="P", requestor=USR, recipient=DEP):
    req_line = 'requestor: "%s"\n' % requestor if requestor is not None else ""
    return ('---\ntitle: %s\n%srecipient: "%s"\n%s---\n%s'
            % (title, req_line, recipient, extra, body))


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

case("gf001-bad-name", ["GF001"], {"r/tasks/Big-Name.task.md": A})
case("gf003-no-front-matter", ["GF003"], {"r/tasks/a.task.md": "Just text.\n"})
case("gf003-unclosed-front-matter", ["GF003"], {"r/tasks/a.task.md": "---\ntitle: T\n"})
case("gf004-duplicate-key", ["GF004"], {"r/tasks/a.task.md": task("title: Again\n")})
case("gf004-yaml-anchor", ["GF004"], {
    "r/tasks/a.task.md": '---\ntitle: &t T\nrequestor: "%s"\nrecipient: "%s"\n---\nBody.\n' % (USR, DEP),
})
case("gf005-unknown-suffix", ["GF005"], {"r/notes/x.widget.md": "---\ntitle: x\n---\n"})
case("gf006-case-collision", ["GF006"], {
    "r/docs/note.md": "# Note\n",
    "r/docs/Note.md": "# Note\n",
    "r/tasks/a.task.md": A,
})
case("gf020-missing-recipient", ["GF020"], {
    "r/tasks/a.task.md": '---\ntitle: T\nrequestor: "%s"\n---\nDo it.\n' % USR,
})
case("gf020-user-recipient", ["GF020"], {
    "r/tasks/a.task.md": task(recipient=USR),
})
case("gf020-bad-requestor", ["GF020"], {
    "r/tasks/a.task.md": task(requestor="@alice"),
})
case("gf020-process-without-trigger", ["GF020"], {
    "r/processes/p.process.md": process(extra="gate: true\n"),
})
case("gf020-unknown-source", ["GF020"], {
    "r/processes/p.process.md": process(extra='"on":\n  task-state: {}\n'),
})
case("gf020-bad-cron", ["GF020"], {
    "r/processes/p.process.md": process(extra='"on":\n  schedule:\n    cron: "0 2 * *"\n'),
})
case("gf020-push-without-filter", ["GF020"], {
    "r/processes/p.process.md": process(extra='"on":\n  push:\n    ignore: ["docs/**"]\n'),
})
case("gf020-bad-draft", ["GF020"], {
    "r/tasks/a.task.md": task('draft: "yes"\n'),
})
case("gf020-bad-not-before", ["GF020"], {
    "r/tasks/a.task.md": task('not_before: "2026-02-30"\n'),
})
case("gf020-bad-not-before-format", ["GF020"], {
    "r/processes/p.process.md": process(extra='"on":\n  manual: {}\nnot_before: "2026-10-25T09:00:00+02:00"\n'),
})
case("gf022-unknown-field", ["GF022"], {
    "r/tasks/a.task.md": task("colour: blue\nx_note: allowed\n", requestor=None),
})
case("gf030-dangling-depends", ["GF030"], {
    "r/tasks/a.task.md": task("depends_on: [./missing.task.md]\n"),
})
case("gf030-missing-anchor", ["GF030"], {
    "r/docs/design.md": "# Overview\n\nSome text.\n",
    "r/tasks/a.task.md": task("references: [/docs/design.md#nonexistent-section]\n"),
})
case("gf032-depends-on-asset", ["GF032"], {
    "r/docs/design.md": "# Design\n",
    "r/tasks/a.task.md": task("depends_on: [/docs/design.md]\n"),
})
case("gf032-trigger-with-asset", ["GF032"], {
    "r/docs/design.md": "# Design\n",
    "r/processes/p.process.md": process(extra="trigger_with: /docs/design.md\n"),
})
case("gf032-recipient-is-file", ["GF032"], {
    "r/tasks/b.task.md": A,
    "r/tasks/a.task.md": task(recipient="./b.task.md"),
})
case("gf034-depends-cycle", ["GF034"], {
    "r/tasks/a.task.md": task("depends_on: [./b.task.md]\n"),
    "r/tasks/b.task.md": task("depends_on: [./a.task.md]\n"),
})
case("gf034-trigger-with-cycle", ["GF034"], {
    "r/processes/a.process.md": process(extra="trigger_with: [./b.process.md]\n"),
    "r/processes/b.process.md": process(extra="trigger_with: [./a.process.md]\n"),
})
case("gf035-malformed-ref", ["GF035"], {
    "r/tasks/a.task.md": task('references: ["git::https://github.com/acme/widgets.git//docs/d.md"]\n'),
})
case("gf035-query-parameter", ["GF035"], {
    "r/tasks/a.task.md": task('references: ["https://github.com/acme/widgets/blob/main/docs/d.md?ref=main"]\n'),
})
case("gf036-escapes-repo", ["GF036"], {
    "r/tasks/a.task.md": task("depends_on: [../../../outside.task.md]\n"),
})
print("fixtures written")
