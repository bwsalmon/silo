# Security Audit 2 — 2026-10-18

An audit of the **Grain Hub** and **Grainfile** implementation in `bwsalmon/silo` (building on Tasks #35 and #36), evaluated against [docs/threat-model.md](../../threat-model.md) and [docs/grainfiles-spec.md §11](../../grainfiles-spec.md). Unlike Audit 1 (which only reported findings), every High, Medium, and Low finding identified in this audit is remediated directly in this change and locked down by regression tests in `hub/security`, `ui/`, and `sdk/ts`.

- **Scope:** `hub/grainfiles` (`resolve.go`, `yaml.go`, `grainfile.go`, `store.go`, `http.go`), `hub/rendezvous` (`server.go`, `client.go`), `hub/server` (`server.go`), `hub/nodeui`, `sdk/ts` (`src/client.ts`), `ui/` (`src/components/Markdown.tsx`, `src/pages/Grainfiles.tsx`, `src/pages/Nodes.tsx`), and `hub/security`.
- **Method:** Line-by-line review of every package in scope against `docs/threat-model.md` (`INV-1` through `INV-28`) and `docs/grainfiles-spec.md §11`, followed by proof-of-concept and regression test construction in `hub/security/grainfiles_test.go`, `hub/security/routes_test.go`, `hub/security/isolation_test.go`, `hub/security/atrest_test.go`, `hub/security/hardening_test.go`, `ui/src/trust.test.tsx`, and `sdk/ts/test/client.test.ts`.

---

## Summary of Findings & Remediations

| ID | Severity | Finding | Invariant | Status |
| --- | --- | --- | --- | --- |
| SA2-H1 | high | Cross-tenant recipient and repository subscription hijacking: any authenticated node could claim another team's `recipient` identity or subscribe to another team's `repos`/`folders` (or overwrite another owner's `node_id` subscription row in `SetSubscriptions`), and then use `GET /v1/grainfiles/content` to exfiltrate referenced source files from the victim team's repository. | INV-24, INV-26 | Fixed (`hub/grainfiles/store.go`, `hub/grainfiles/http.go`, `hub/rendezvous/server.go`, `hub/server/server.go`) |
| SA2-H2 | high | Unauthenticated control-stream completion spoofing: `rendezvous.Server.completeGrainfile` and `Store.RecordDependencyCompleted` allowed any connected node to emit `grainfile_completed` / `dependency_completed` for any Grainfile URL (including another team's or a `draft: true` Grainfile, or with arbitrary state strings), prematurely unblocking downstream `depends_on` tasks across deployments. | INV-27 | Fixed (`hub/rendezvous/server.go`, `hub/grainfiles/store.go`) |
| SA2-H3 | high | Ref/commit confusion on `GET /v1/grainfiles/content`: `serveContent` and `GetIndexByURL` allowed a recipient node to supply an arbitrary `?commit=`/`?ref=` query parameter or a different branch in `grainfile_url`/`reference` and forwarded that unverified ref to `a.Fetcher.FetchFile`, reading files from branches or commits where the Grainfile was never indexed or had not authorized those references. | INV-24 | Fixed (`hub/grainfiles/http.go`, `hub/grainfiles/store.go`, `hub/grainfiles/resolve.go`) |
| SA2-M1 | medium | Fail-open webhook HMAC verification: `VerifyGitHubSignature` returned `true` when `secret == ""`, and `serveGitHubWebhook` accepted unsigned `POST /v1/webhooks/github` requests whenever `WebhookSecret` was unconfigured, allowing unauthenticated callers to trigger upstream fetches and inject synthetic Grainfile events. | INV-2, INV-25 | Fixed (`hub/grainfiles/http.go`, `hub/security/routes_test.go`) |
| SA2-M2 | medium | `GitHubFetcher` SSRF, redirect following, and unscoped token minting: `GitHubFetcher` did not verify that `repoURL` was hosted on `github.com` before calling `TokenFunc` when `BaseURL` was empty, did not block link-local/cloud-metadata `BaseURL`s (`169.254.169.254`) or embedded URL credentials, used `http.DefaultClient` (following up to 10 HTTP 302 redirects to internal hosts with no timeout), and did not bound base64-decoded payload sizes before decoding. | INV-25 | Fixed (`hub/grainfiles/http.go`) |
| SA2-M3 | medium | URL authority confusion, case-sensitive `IsSameRepo`, and encoded path traversal in reference resolution: `CanonicalRepoURL` and `parseHTTPSRef` accepted `@` in `<org>/<repo>` (`https://github.com/org/repo-a@evil.com`), `IsSameRepo` was case-sensitive, single `.git` stripping missed `.git.git`, and percent-encoded traversal sequences (`%2e%2e`, `%2f`, `%5c`, `%00`) or `..` segments in `ParseRef` were not rejected with `403 Forbidden` on `GET /v1/grainfiles/content`. | INV-24 | Fixed (`hub/grainfiles/resolve.go`, `hub/grainfiles/http.go`, `hub/grainfiles/store.go`) |
| SA2-L1 | low | Unbounded YAML nesting depth and flow-collection tag bypass in `grainfiles.Parse`: `yaml.go` had no recursion depth limit on nested flow/block collections (`[[[[...]]]]`) and `forbiddenYAMLRe` only matched `!` tags after whitespace, missing `[!custom x]` and `{k:!!str v}`. | INV-19, INV-25 | Fixed (`hub/grainfiles/yaml.go`) |
| SA2-L2 | low | Missing `X-Content-Type-Options: nosniff` and `Content-Security-Policy: sandbox` headers on `GET /v1/grainfiles/content` responses. | INV-4, INV-24 | Fixed (`hub/grainfiles/http.go`) |
| SA2-L3 | low | Protocol-relative URL (`//evil.com`, `/\evil.com`) bypass in UI `safeUrl` / `linkableHref` / `safeHref`, and missing `noopener` on external links in `Grainfiles.tsx` and `Nodes.tsx`. | INV-15, INV-28 | Fixed (`ui/src/components/Markdown.tsx`, `ui/src/pages/Grainfiles.tsx`, `ui/src/pages/Nodes.tsx`) |
| SA2-L4 | low | TypeScript SDK URL path builders (`taskPath`, `teamPath`, `repoPath`) allowed `.` and `..` path segments (`encodeURIComponent("..") === ".."`), permitting client-side path traversal across API endpoints. | INV-28 | Fixed (`sdk/ts/src/client.ts`) |
| SA2-L5 | low | Unbounded `reason` string in `RecordDependencyCompleted` and unquoted node-supplied fields in rendezvous completion logs. | INV-18, INV-19 | Fixed (`hub/grainfiles/store.go`, `hub/rendezvous/server.go`) |

---

## Detailed Findings

### SA2-H1 (high): cross-tenant recipient and repository subscription hijacking

**Where:**
- `hub/grainfiles/store.go:845–908` (`SetSubscriptions`)
- `hub/grainfiles/http.go:1001–1025` (`serveContent`, `isNodeAuthorizedForRecord`)
- `hub/rendezvous/server.go:348–363`, `665–692` (`serveNode`, `subscribeNode`)
- `hub/server/server.go:406–508` (`New`)

**What:**
1. When a node connected to `/v1/nodes/connect` or sent a `subscribe` message on its control stream, `rendezvous.Server` checked `s.Admit(owner, node)` for `node.Repos`, but performed no team-ownership check on `Subscriptions.Recipients`, `Subscriptions.Repos`, `Subscriptions.Folders`, or `Subscriptions.Grainfiles`.
2. In `Store.SetSubscriptions`, `INSERT INTO grainfile_subscriptions ... ON CONFLICT (node_id) DO UPDATE` overwrote any existing `node_id` row without checking whether `existingOwner != sub.Owner`, and allowed any user to claim a `recipient` URL already claimed by another team's user.
3. In `GET /v1/grainfiles/content`, `serveContent` checked only `sub.MatchesRecord(rec)`. A node in Team B could subscribe to `Repos: ["https://github.com/acme/app"]` or `Grainfiles: ["https://github.com/acme/app/blob/main/processes/release.process.md"]` (owned by Team A) or claim Team A's `recipient` URL, and then call `GET /v1/grainfiles/content` to read Team A's Grainfiles and referenced source files.

**Reproduction / PoC Test:** `TestTenantRecipientIsolationAndControlStreamSpoofing` and `TestGrainfileSameOriginContentProxy` in `hub/security/grainfiles_test.go`.

**Violates:** INV-24, INV-26, and `docs/grainfiles-spec.md §11.1`.

**Fix:**
1. Added `SameTeam` check in `Store.SetSubscriptions`: a node cannot overwrite a `node_id` subscription row owned by a different user, nor can it claim a `recipient` URL already claimed by another user who does not share a team.
2. Added `AdmitSubscriptions` on `rendezvous.Server` (wired in `hub/server/server.go` via `h.Directory.RepoOwner` and `h.Directory.Access`): if any URL in `Recipients`, `Repos`, or `Folders` targets a repository owned by a team in `h.Directory`, the node's owner must be a member of that team.
3. Added `isNodeAuthorizedForRecord` and `AuthorizeOwnerRepo` in `hub/grainfiles/http.go`: cross-deployment capability access on `GET /v1/grainfiles/content` is granted when the Grainfile explicitly names `rec.Recipient` and the node's `sub.Recipients` claims that recipient; otherwise (when matched via `Repos`, `Folders`, or `Grainfiles`), the node's owner must be authorized for `rec.Repo` in `h.Directory`.

---

### SA2-H2 (high): unauthenticated control-stream `grainfile_completed` spoofing

**Where:**
- `hub/rendezvous/server.go:750–790` (`completeGrainfile`)
- `hub/grainfiles/store.go:1082–1155` (`RecordDependencyCompleted`)

**What:**
`rendezvous.Server.completeGrainfile` accepted `TypeGrainfileCompleted` (`grainfile_completed` / `dependency_completed`) from any connected node and called `s.Grainfiles.RecordDependencyCompleted(ctx, msg.GrainfileURL, msg.CommitSHA, state, msg.Reason)` without verifying:
1. Whether the calling node was actually the `recipient` (or authorized subscriber) of `msg.GrainfileURL`.
2. Whether `msg.GrainfileURL` was an indexed, non-draft Grainfile at `msg.CommitSHA`.
3. Whether `msg.State` was a valid completion state (`done`, `completed`, `success`, `failed`, `cancelled`) and `msg.Reason` was bounded in length.

Because `Store.DependenciesSatisfied` only queried `grainfile_events` for any `dependency_completed` row with `dependency_url = ? AND dependency_state = 'done'`, a node in Team B could spoof completion of Team A's upstream task and immediately unblock dependent Grainfiles waiting on `depends_on`.

**Reproduction / PoC Test:** `TestTenantRecipientIsolationAndControlStreamSpoofing` in `hub/security/grainfiles_test.go`.

**Violates:** INV-27 and `docs/grainfiles-spec.md §9.3`.

**Fix:**
1. In `rendezvous.Server.completeGrainfile`, look up the indexed `IndexRecord` via `GetIndexByURL(ctx, msg.GrainfileURL, msg.CommitSHA)` and verify that the calling node's registered subscriptions claim `completedRec.Recipient` (or match `completedRec` when `Recipient` is empty) before calling `RecordDependencyCompleted`.
2. In `Store.RecordDependencyCompleted`, validate `state` against the allowed set (`done`, `completed`, `success`, `failed`, `cancelled`), cap `reason` at 1024 bytes, and require `completedRec` to exist in the index at the specified ref/commit and have `Draft == false`.

---

### SA2-H3 (high): ref/commit confusion on `GET /v1/grainfiles/content`

**Where:**
- `hub/grainfiles/http.go:1082–1215` (`serveContent`)
- `hub/grainfiles/store.go:691–765` (`GetIndex`, `GetIndexByURL`)
- `hub/grainfiles/resolve.go:395–465` (`CheckReferenceAccess`)

**What:**
1. In `Store.GetIndexByURL`, when a lookup with a specific `commitSHA` or URL `GitRef` found no matching row, it fell back to `s.GetIndex(ctx, parsed.Repo, parsed.Path, "")` (returning whatever branch/commit was latest in the index).
2. In `serveContent`, `fetchRef := firstNonEmpty(rawCommit, rec.CommitSHA, rec.Ref)` prioritized the caller-supplied `?commit=` / `?ref=` query parameter over the indexed `rec.CommitSHA` / `rec.Ref`. A legitimate recipient node authorized for a Grainfile on `main` could pass `?ref=secret-branch` or a `reference` URL pointing to `/blob/secret-branch/docs/spec.md` and read files from unindexed branches or commits.

**Reproduction / PoC Test:** `TestGrainfileSameOriginContentProxy` (`ref confusion via query param`, `commit confusion via query param`, `ref confusion in grainfile_url`, `ref confusion in reference URL`) in `hub/security/grainfiles_test.go`.

**Violates:** INV-24 and `docs/grainfiles-spec.md §11.1`.

**Fix:**
1. Hardened `GetIndexByURL` with `refMatchesIndexRecord` so any explicit `commitSHA` or URL `GitRef` must match `rec.CommitSHA` or `rec.Ref` and never falls back to a different commit or branch.
2. Hardened `serveContent` to reject (`403 Forbidden`) any request whose `rawCommit`, `grainfile_url` ref, or `reference` URL ref does not match the indexed `rec.CommitSHA` or `rec.Ref`, and pinned all upstream `streamRepoFile` fetches to `pinnedFetchRef(rec)` (`firstNonEmpty(rec.CommitSHA, rec.Ref)`).
3. Hardened `Grainfile.CheckReferenceAccess` to verify that a remote target URL's `GitRef` matches the Grainfile's ref/commit or the authorized reference's `GitRef`.

---

### SA2-M1 (medium): fail-open webhook HMAC verification

**Where:**
- `hub/grainfiles/http.go:568–579` (`VerifyGitHubSignature`), `649–659` (`serveGitHubWebhook`)

**What:**
`VerifyGitHubSignature` returned `true` when `secret == ""`, and `serveGitHubWebhook` only verified `X-Hub-Signature-256` when `a.WebhookSecret != ""`. A Hub started without `-webhook-secret` accepted unsigned webhook payloads from any caller (`POST /v1/webhooks/github` had `want: everyone(200)` in `hub/security/routes_test.go`).

**Reproduction / PoC Test:** `TestWebhookReceiverAndGitHubFetcherHardening` in `hub/security/grainfiles_test.go` and `TestRouteMatrix` in `hub/security/routes_test.go`.

**Violates:** INV-2, INV-25, and `docs/grainfiles-spec.md §11.2`.

**Fix:**
1. Changed `VerifyGitHubSignature` to fail closed (`return false`) whenever `secret == ""` or `signatureHeader == ""`.
2. Changed `serveGitHubWebhook` to require a non-empty `WebhookSecret` and a valid `X-Hub-Signature-256` signature unless `AllowUnsignedWebhooks` is explicitly enabled in unit tests, returning `401 Unauthorized` otherwise.
3. Updated `hub/security/routes_test.go` so `POST /v1/webhooks/github` requires `everyone(401)` for unsigned callers.

---

### SA2-M2 (medium): `GitHubFetcher` SSRF, redirect following, and unscoped token minting

**Where:**
- `hub/grainfiles/http.go:70–295` (`GitHubFetcher.FetchFile`, `resolveToken`, `httpClient`)

**What:**
1. When `BaseURL == ""` (production default), `GitHubFetcher.FetchFile` did not verify that `repoURL` had host `github.com` before calling `f.resolveToken(ctx, repoURL)` (`TokenFunc`), violating `docs/grainfiles-spec.md §11.2` ("the Hub must never attach a host's token to a request directed at a different hostname").
2. When `BaseURL` was configured, `FetchFile` did not reject embedded URL credentials (`https://user:pass@...`) or link-local / cloud metadata IPs (`169.254.169.254`, `fe80::/10`).
3. `httpClient()` returned `http.DefaultClient`, which has no request timeout and follows up to 10 HTTP `301`/`302`/`307`/`308` redirects (allowing a compromised or spoofed upstream endpoint to redirect the Hub to internal pod or metadata addresses).
4. Base64-decoded content size was checked only after allocating and decoding the entire buffer.

**Reproduction / PoC Test:** `TestWebhookReceiverAndGitHubFetcherHardening` in `hub/security/grainfiles_test.go`.

**Violates:** INV-25 and `docs/grainfiles-spec.md §11.2`.

**Fix:**
1. Verified `repoURL` has scheme `https` and host `github.com` before calling `resolveToken` when `BaseURL == ""`.
2. Validated `BaseURL` to reject userinfo credentials and link-local/metadata IP literals (`ip.IsLinkLocalUnicast()`, `ip.IsLinkLocalMulticast()`, `ip.IsLoopback()` unless `127.0.0.1`/`::1` in local httptest).
3. Configured `httpClient()` with a 15s default timeout and `CheckRedirect` refusing all HTTP redirects.
4. Added pre-decode base64 size bounds against `MaxFileBytes` (`1 MiB`) and capped installation token responses with `io.LimitReader(..., 1<<20)`.

---

### SA2-M3 (medium): URL authority confusion, case-sensitive `IsSameRepo`, and encoded path traversal

**Where:**
- `hub/grainfiles/resolve.go:68–260` (`CanonicalRepoURL`, `ParseRef`, `ResolveRefWithSource`, `IsSameRepo`, `CheckReferenceAccess`)
- `hub/grainfiles/http.go:964–1000` (`hasRawQueryEncodingHazard`, `hasPathTraversalOrHazard`)
- `hub/grainfiles/store.go:700–710`, `810–820` (`GetIndex`, `loadLatestIndexQ`)

**What:**
1. `CanonicalRepoURL` and `parseHTTPSRef` did not validate `<org>` and `<repo>` segments against `@` or invalid characters, allowing authority-confusion strings such as `https://github.com/org/repo-a@evil.com`.
2. `IsSameRepo` compared canonical repository URLs with `==` (case-sensitive on `<org>/<repo>`) and `strings.TrimSuffix(p, ".git")` only stripped a single lowercase `.git` suffix.
3. `ParseRef` and `serveContent` did not reject percent-encoded path traversal sequences (`%2e%2e`, `%2f`, `%5c`, `%00`) or `..` segments in remote URL paths with `403 Forbidden`.

**Reproduction / PoC Test:** `TestGrainfileSameOriginContentProxy` in `hub/security/grainfiles_test.go`.

**Violates:** INV-24 and `docs/grainfiles-spec.md §8, §11.1`.

**Fix:**
1. Added `validRepoSegment`, `hasEncodedPathHazard`, `hasDotSegment`, and `trimDotGitSuffix` in `hub/grainfiles/resolve.go`.
2. Made `IsSameRepo` case-insensitive (`strings.EqualFold`) and updated `GetIndex` and `loadLatestIndexQ` in `hub/grainfiles/store.go` to match `LOWER(repo) = LOWER(?)`.
3. Added `hasRawQueryEncodingHazard` and `hasPathTraversalOrHazard` in `hub/grainfiles/http.go` so any traversal, encoded separator, null byte, double slash, or `@` authority character on `GET /v1/grainfiles/content` is rejected with `403 Forbidden`.

---

### SA2-L1 (low): unbounded YAML nesting depth and flow-collection tag bypass

**Where:**
- `hub/grainfiles/yaml.go:19` (`forbiddenYAMLRe`), `128–475` (`parseBlockMapping`, `parseBlockSequence`, `parseFlowSequence`, `parseFlowMapping`)

**What:**
`forbiddenYAMLRe` required whitespace or start-of-line before `!`, missing explicit tags inside flow collections (`[!custom x]`, `{k:!!str v}`), and the recursive YAML parser had no depth bound on nested block or flow structures (`[[[[...]]]]`).

**Reproduction / PoC Test:** `TestWebhookReceiverAndGitHubFetcherHardening` in `hub/security/grainfiles_test.go`.

**Violates:** INV-19, INV-25, and `docs/grainfiles-spec.md §2, §11.3`.

**Fix:**
Tightened `forbiddenYAMLRe` to reject `!` after `[\s\[{,:]` as well as start-of-line, and enforced `maxYAMLDepth = 32` across all block and flow parsing functions in `hub/grainfiles/yaml.go` (returning `GF006` `CodeLimitExceeded`).

---

### SA2-L2 (low): missing `nosniff` and `Content-Security-Policy: sandbox` headers on `GET /v1/grainfiles/content`

**Where:**
- `hub/grainfiles/http.go:1318–1328` (`streamRepoFile`)

**What:**
`streamRepoFile` set `Cache-Control: no-store` and `Content-Type: text/plain; charset=utf-8`, but omitted `X-Content-Type-Options: nosniff` and `Content-Security-Policy: sandbox` (which `hub/live` sets on artifact view-through responses under INV-4).

**Reproduction / PoC Test:** `TestGrainfileSameOriginContentProxy` and `TestNoCodeAtRest` in `hub/security/`.

**Violates:** INV-4, INV-24.

**Fix:**
Added `X-Content-Type-Options: nosniff` and `Content-Security-Policy: sandbox` to `streamRepoFile` in `hub/grainfiles/http.go`.

---

### SA2-L3 (low): protocol-relative URL (`//evil.com`) bypass in UI link helpers and missing `noopener`

**Where:**
- `ui/src/components/Markdown.tsx:68–84` (`safeUrl`)
- `ui/src/pages/Grainfiles.tsx:43–48`, `121` (`linkableHref`, `SafeExternalLink`)
- `ui/src/pages/Nodes.tsx:28–33`, `97`, `142`, `160` (`safeHref`)

**What:**
`safeUrl("//evil.com/phish")` returned `"//evil.com/phish"` because it contained no `:`, and `linkableHref` / `safeHref` matched `/^(https?:|mailto:|\/)/i`, which accepted any string starting with `/` (including `//evil.com` and `/\evil.com`), turning a protocol-relative string in `recipient`, `references`, `depends_on`, `trigger_with`, or `ui_url` into an external link to `evil.com`. External `<MuiLink target="_blank">` elements in `Grainfiles.tsx` and `Nodes.tsx` also used `rel="noreferrer"` without `noopener`.

**Reproduction / PoC Test:** `refuses javascript:, data:, vbscript:, and protocol-relative // hrefs on /grainfiles and /nodes` in `ui/src/trust.test.tsx`.

**Violates:** INV-15, INV-28.

**Fix:**
Rejected `//`, `/\`, and leading `\` in `safeUrl`, `linkableHref`, and `safeHref`, and set `rel="noopener noreferrer"` on all external links in `Grainfiles.tsx` and `Nodes.tsx`.

---

### SA2-L4 (low): TypeScript SDK path builders allowed `.` and `..` path segments

**Where:**
- `sdk/ts/src/client.ts:71–77` (`enc`, `taskPath`, `teamPath`, `repoPath`)

**What:**
`encodeURIComponent(".")` is `"."` and `encodeURIComponent("..")` is `".."`, so calling `getTask("..")`, `getTeam(".")`, or `assignRepo("infra", "acme/..")` constructed dot-segment traversal paths before sending the request.

**Reproduction / PoC Test:** `refuses . and .. path segments` in `sdk/ts/test/client.test.ts`.

**Violates:** INV-28.

**Fix:**
Updated `enc` in `sdk/ts/src/client.ts` to throw an error when an encoded path segment is `"."` or `".."`.

---

### SA2-L5 (low): unbounded `reason` in `RecordDependencyCompleted` and unquoted log fields

**Where:**
- `hub/grainfiles/store.go:1082–1095` (`RecordDependencyCompleted`)
- `hub/rendezvous/server.go:750–790` (`completeGrainfile`)

**What:**
`RecordDependencyCompleted` stored `reason` in `grainfile_events` without a length cap, and `completeGrainfile` logged node-supplied fields without `logsafe.Quote`.

**Reproduction / PoC Test:** `TestTenantRecipientIsolationAndControlStreamSpoofing` in `hub/security/grainfiles_test.go`.

**Violates:** INV-18, INV-19.

**Fix:**
Capped `reason` at 1024 bytes in `RecordDependencyCompleted` and quoted node-supplied strings with `logsafe.Quote` in `completeGrainfile`.
