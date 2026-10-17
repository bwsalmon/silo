---
kind: asset
schema: grain/asset@v1
asset: claims-flow.png
title: How a signed claim travels from Silo to a Grain
alt: >-
  Silo signs a placement's attestation; the Grain verifies it against
  silo_issuer before it accepts the run.
media_type: image/png
license: CC-BY-4.0
provenance:
  author: "user:bsalmon@google.com"
  created_at: "2026-10-16T19:10:00Z"
  source: drawn for this design
sha256: "765f72e13a95db7c6885934f7c3d84439279afa14f6134f7666897c413e4214d"
bytes: 250
---

The sidecar of `claims-flow.png`, beside it and named for it
(docs/task-repos.md §2.2). The image is an asset: `hub-intake.md` embeds
it by a relative link, and a task that lists the design with `depth: 1`
gets it in its sandbox (§5.6, §8.16). This file is optional metadata:
the alt text, the license and where the image came from. It is never
ranked, placed or run.
