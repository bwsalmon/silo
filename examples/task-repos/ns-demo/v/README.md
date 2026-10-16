---
kind: repo
schema: grain/repo@v1
role: grain-root
grain: {id: demo-v}
title: Root of Grain "demo-v"
owner:
  user: bsalmon@google.com
host: local
default_ref: main
writers: ["user:bsalmon@google.com"]
---

V includes X, and disables one branch of it by excluding X's include
object, `x/include-y.md`. What that include contributed drops out, and so
does its own name. X's own objects stay.
