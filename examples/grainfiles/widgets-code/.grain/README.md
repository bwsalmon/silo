# The widgets repo's grain folder

This folder holds the widgets repo's own grain objects, beside the code
they are about:

- `templates/` and `triggers/`: the standing work on this repo, in the
  same Markdown format as any grain repo's. A trigger files work when
  something happens: a time, a push, a design change, a request;
- `gates/`: the repo's merge gates, which hold a change for review and
  file nothing else;
- `workflows/`: CI, which stays GitHub-Actions YAML;
- `tasks/`: long-lived and design-linked tasks, promoted here from the
  hub intake. Each one's old intake path is a tombstone pointing here.

The design docs in `/docs/design/` are objects in place, named
`*.design.md` (docs/grainfiles.md §2.1, §9.5).

**Nothing here is active by itself.** A trigger fires only for a Grain or
team whose `grain.config.md` names it as a trigger root. The gates are
read by this repo's own merge queue, from `.grain/gates/`, when a change
merges into it (§6.7).

**Task home.** `task_home: promote` in the repo's `.grainconfig`: approved tasks stay in the hub
intake unless someone promotes them. A trigger's firings never come
here: the weekly release-notes trigger files its tasks into the intake
(docs/grainfiles.md §9.2, §6.6).

**Write authority** is this repo's review and merge process, not Silo's
hook. The repo's `GRAINOWNERS` says what belongs here, and readers check
objects against it, and against the design's invariants, when they read
them (§9.6).
