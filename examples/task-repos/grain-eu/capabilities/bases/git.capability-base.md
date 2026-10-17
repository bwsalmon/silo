---
kind: capability-base
schema: grain/capability-base@v1
type: git@v1
title: Git access through eu's git proxy
grain: eu
provider: {code: "https://github.com/bwsalmon/grain.git//pkg/capability/git/", rev: "illustrative"}
dimensions:
  repos:
    kind: set
    maximum: ["https://github.com/bwsalmon/*", "https://silo.example.com/git/*"]
    narrow: [use]
    required: true
    note: The repos eu's own credentials reach
  folders:
    kind: set
    maximum: ["**"]
    narrow: [use]
    note: For a folder entry the proxy serves blobs only under these paths (§11.20)
  operations:
    kind: set
    maximum: [fetch, push]
    narrow: [use]
    required: true
  push_refs:
    kind: set
    maximum: ["refs/heads/grain/eu/*"]
    narrow: [use]
  lifetime:
    kind: quantity
    unit: seconds
    maximum: 86400
    narrow: [credential]
materialises:
  files:
    - {path: .git-credentials, holds: "a run-scoped token for eu's git proxy, and the proxy's address"}
    - {path: .gitconfig, holds: "insteadOf rules sending each repo's URL to the proxy"}
implies:
  repos: {from: repos}
  egress: []
  tools: []
proxies: [git-proxy]
approval: none
aka: ["capability:repo-read", "capability:repo-write"]
declared_at: "2026-10-16T08:00:00Z"
---

Written by Grain `eu` from its provider code (docs/task-repos.md §5.7).
Every repo and folder entry of a task's `context:` is a use of this base,
narrowed to that entry's repo, folders, commit and mode; the proxy's
scope in §5.6 is exactly the effective capability. A workflow's
`repo-read:<owner/name>` and `repo-write:<owner/name>` are shorthand for
the same base with built-in reducers (§5.7, CI).

Its only egress is the proxy itself, which sits in `eu`. What it implies
is the repos it reaches, its `repos` dimension as narrowed, and those are
checked against `eu`'s `repos:` policy like any context entry.
