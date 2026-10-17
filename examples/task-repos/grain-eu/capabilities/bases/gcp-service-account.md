---
kind: capability-base
schema: grain/capability-base@v1
type: gcp-service-account@v1
title: GCP service account, through eu's token endpoint
grain: eu
provider: {code: "https://github.com/bwsalmon/grain.git//pkg/capability/gcp/", rev: "illustrative"}
dimensions:
  accounts:
    kind: set
    maximum:
      - grain-sb-agent-*@bsalmon-fun-sandbox.iam.gserviceaccount.com
      - widgets-preview@acme-widgets.iam.gserviceaccount.com
    narrow: [credential]
    required: true
    note: The accounts eu can mint tokens for. Every other dimension narrows which of these qualify
  projects:
    kind: set
    maximum: [bsalmon-fun-sandbox, acme-widgets]
    narrow: [credential]
    required: true
  roles:
    kind: set
    maximum: ["*"]
    narrow: [credential]
    note: >-
      Narrowed by selection: eu mints only for an account whose roles in
      the remaining projects are all within the set. If none qualifies,
      the capability is empty and the request is refused
  token_lifetime:
    kind: quantity
    unit: seconds
    maximum: 3600
    narrow: [credential]
  calls:
    kind: set
    maximum: ["*"]
    narrow: [use]
    note: Google API calls, checked per request at eu's Google API proxy
materialises:
  files:
    - {path: .gcp-credentials.json, holds: "an external-account credential config naming eu's token endpoint; no key"}
  env:
    GOOGLE_APPLICATION_CREDENTIALS: .gcp-credentials.json
    CLOUDSDK_CORE_PROJECT: "the one remaining project, if exactly one remains"
implies:
  egress: [oauth2.googleapis.com, sts.googleapis.com, "*.googleapis.com"]
  repos: []
  tools: []
proxies: [token-endpoint, google-api-proxy]
approval: none
aka: ["capability:gcp-key"]
declared_at: "2026-10-16T08:00:00Z"
---

Written by Grain `eu` from its provider code, as its declaration of the
`gcp-service-account@v1` base (docs/task-repos.md §5.7). It is published
for Silo, which places tasks only on Grains that implement what they
request, and for readers. `eu` never reads this file to decide anything:
its providers are its source of truth, and this file is regenerated from
them whenever they change.

The sandbox never holds a key. It gets a credential config that points
at `eu`'s token endpoint, which mints short-lived access tokens for the
selected account, and every Google API call passes `eu`'s proxy, where
per-use reducers apply. Grain's `gcp-key` today hands out a key file from
a pool of accounts (`~/.gcp-service-account.json`); in that legacy mode
`token_lifetime` cannot be narrowed, and a reducer on it is refused
(§13.2).
