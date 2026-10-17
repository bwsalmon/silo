---
kind: include
schema: grain/include@v1
target: "shared:capabilities/@main"
ref_mode: track
kinds: [capability]
added_by: "user:bsalmon@google.com"
added_at: "2026-10-16T12:00:00Z"
---

The hub team's capability objects, tracking `main`. They join `eu`'s
namespace as capabilities **available to request**. Including them
grants nothing: a task still has to request one in `context:` with
`mode: use`, and `eu`'s local policy still has to sanction the base and
every source in the capability's chain, and allow everything it implies
(docs/task-repos.md §5.7).

Their bases need not be in the namespace. `widgets-deployer.md` is built
on the org's `gcp/acme-widgets.md`, which `eu` reads when it resolves the
chain and checks against its policy's sources, but does not offer on its
own.
