---
kind: repo
schema: grain/repo@v1
role: code
title: The widgets repo's grain folder
owner:
  team: hub
  silo: https://silo.example.com
host: other
scope: [/.grain/, /docs/design/]
default_ref: main
backlog: []
task_home:
  mode: promote
  path: tasks/
aliases:
  intake: https://silo.example.com/git/hub/intake.git
  shared: https://silo.example.com/git/hub/shared.git
writers:
  - who: [authenticated]
    may: [create, update]
    kinds: [template, trigger, gate, suite, design]
    fields: "*"
    note: >-
      Definitions land through this repo's own review and merge, like the
      code. Readers cannot check who pushed; they validate on read
      (docs/task-repos.md §8.13).
  - who: [silo]
    may: [create, update]
    kinds: [task]
    paths: ["tasks/**"]
    where: {state: approved}
    fields: "*"
    note: >-
      Tasks arrive here only from the hub intake: promoted, or committed
      on approval in code-repo mode, each with moved_from naming its
      intake tombstone. Silo lands them through the merge queue. A task
      written here by hand is not one Silo will place (§5.5).
  - who: [silo, "team:hub#member"]
    may: update
    kinds: [task]
    paths: ["tasks/**"]
    transition: {field: state, from: [approved], to: [cancelled]}
    fields: [state, cancel]
    note: A cancel. Readers honour one from any committer, since it only stops work.
---

# The widgets repo's grain folder

This folder holds the widgets repo's own grain objects, beside the code
they are about:

- `templates/` and `triggers/`: the standing work on this repo, in the
  same Markdown format as any task repo's. A trigger files work when
  something happens: a time, a push, a design change, a request;
- `gates/`: the repo's merge gates, which hold a change for review and
  file nothing else;
- `workflows/`: CI, which stays GitHub-Actions YAML;
- `tasks/`: long-lived and design-linked tasks, promoted here from the
  hub intake. Each one's old intake path is a tombstone pointing here.

`scope` also takes in `/docs/design/`, so the design docs there are
`kind: design` objects in place.

**Task home.** `task_home: promote`: approved tasks stay in the hub
intake unless someone promotes them. A trigger's firings never come
here: the weekly release-notes trigger files its tasks into the intake
(docs/task-repos.md §5.5, §6.2).

**Write authority** is this repo's review and merge process, not Silo's
hook. The `writers` rules above say what belongs here, and readers check
objects against them, and against the design's invariants, when they read
them (§8.13).
