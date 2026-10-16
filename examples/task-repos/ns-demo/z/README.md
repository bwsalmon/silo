---
kind: repo
schema: grain/repo@v1
role: grain-root
grain: {id: demo-z}
title: Root of Grain "demo-z"
owner:
  user: bsalmon@google.com
host: local
default_ref: main
writers: ["user:bsalmon@google.com"]
---

Z includes X, which excludes Y, and includes Y directly. Z's namespace
contains Y: X's exclude subtracted from X's contribution, and Z's
direct include is a new contribution of its own.
