---
kind: repo
schema: grain/repo@v1
role: shared
title: Namespace resolution, in miniature
owner:
  user: bsalmon@google.com
host: local
default_ref: main
writers: ["user:bsalmon@google.com"]
---

# Namespace resolution, in miniature

The cases of docs/task-repos.md §3.3, as a tree `check.py` resolves and
checks. Every include here is relative, so it reads the same commit.

| Folder | Holds | Contributes |
| --- | --- | --- |
| `y/` | Y, `y/templates/greeting.md` | Y |
| `x/` | `x/templates/note.md`; includes `y/`; excludes Y | `note.md`, and not Y |
| `w/` | includes Y and excludes Y | no Y |
| `z/` | a Grain root: includes `x/`, and includes Y directly | `note.md` **and Y**: (X's contribution − Y) ∪ {Y} |
| `v/` | a Grain root: includes `x/`, and excludes `x/include-y.md` | `note.md`; X's include of `y/` is disabled, which here loses nothing more, since X excluded Y anyway |

Include and exclude objects are names too, and each folder's are listed
with what it contributes, but they are configuration: never run, ranked
or placed.
