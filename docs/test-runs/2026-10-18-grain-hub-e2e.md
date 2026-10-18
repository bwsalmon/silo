# Grain Hub End-to-End Integration & Search Test Suite — 2026-10-18

End-to-end integration and search test run for **Grain Hub** (`hub/server`), `hub/grainfiles`, and the TypeScript SDK (`sdk/ts`) against real bare git repositories and a local Git + GitHub simulator (`internal/githubsim` and `hub/grainfiles/simtest`).

Every Go integration test runs against both **SQLite** and **PostgreSQL 15** (`internal/pgtest`) with `-race`, and the TypeScript SDK integration suite runs against a live `silo hub serve` child process and `internal/fakegrain` node over real WebSocket `rendezvous` connections.

## 1. Local Git + GitHub Simulator Harness (`internal/githubsim`)

`internal/githubsim` provides an in-process HTTP Git + GitHub simulator modeled on `bwsalmon/grain`'s `pkg/github/githubsim` and `scripts/fake-github.py`:

- **Real bare git repositories (`git init --bare` + `git http-backend`)**:
  - `CreateRepo(owner, repo, defaultBranch)` initializes a real bare git repository on disk and exposes Smart HTTP git endpoints (`/<owner>/<repo>.git/info/refs`, `/<owner>/<repo>.git/git-upload-pack`, `/<owner>/<repo>.git/git-receive-pack`) via `git http-backend`.
  - Working-tree helpers (`CommitFiles`, `OpenPullRequest`, `SynchronizePullRequest`, `MergePullRequest`) clone/checkout branches against the bare repo, create real git commits with added/modified/removed files, push refs back to the bare repository, and record commit parent/head SHAs and changed file lists.
- **GitHub REST Contents API & App authentication**:
  - `GET /repos/{owner}/{repo}/contents/{path}?ref={ref}` reads committed blobs directly from the bare git repository via `git show <ref>:<path>`.
  - Supports `Accept: application/vnd.github.raw+json` (raw file bytes) and JSON metadata responses (`type`, `encoding: "base64"`, `content`, `sha`, `size`, `path`).
  - Authenticates both static bearer tokens (`Authorization: Bearer <token>`) and GitHub App JWT installation token exchange (`GET /repos/{owner}/{repo}/installation` verifying RS256 JWT + `POST /app/installations/{id}/access_tokens` issuing `ghs_sim_*` installation tokens).
- **HMAC-SHA256 signed webhook delivery**:
  - Automatically emits signed `X-GitHub-Event: push` and `X-GitHub-Event: pull_request` (`opened`, `synchronize`, `closed` with `merged: true`) webhooks with `X-Hub-Signature-256: sha256=<hmac>` to the Hub's `POST /v1/webhooks/github` endpoint whenever commits are pushed or pull requests are opened, updated, or merged.

## 2. Scenarios Tested & Results

| Suite / Package | Test Scenario | Backend(s) | Result | Duration (`-race`) |
|---|---|---|---|---|
| `internal/githubsim` | `TestSimulatorGitContentsAppAuthAndWebhooks` — bare git Smart HTTP clone/push, `GET /repos/{owner}/{repo}/contents/{path}?ref={ref}` (raw + JSON), RS256 GitHub App JWT -> installation token exchange, and HMAC-SHA256 signed `push` & `pull_request` webhooks | Git + HTTP | **PASS** | 1.36s |
| `hub/grainfiles/simtest` | `TestEndToEndSearchAndFiltering` — multi-repo `.task.md` & `.process.md` indexing across 3 repositories (`acme/alpha-service`, `acme/beta-platform`, `acme/gamma-docs`), 3 `recipient` deployments, `Name <email>` & `https://` `requestor` identities, `draft: true/false`, `not_before`, `on` triggers, and `gate: true/false`; exercises every single and combined filter (`kind`, `recipient`, `repo`, `draft`, `gate`, `requestor`, `path`, `url`, `node`) and verifies deterministic ordering and detail metadata (`references`, `authorized_references`, `depends_on`, `trigger_with`, `subscribed_nodes`, `recent_events`) | SQLite, PostgreSQL | **PASS** | 9.81s |
| `hub/grainfiles/simtest` | `TestEndToEndLiveSubscriptionsDraftNotBeforeAndCatchup` — 2 live `rendezvous.Client` nodes connected over WebSocket to `hub/server`; verifies live `grainfile_event` routing (`task_ready`, `process_updated`, `process_triggered`) only to matching `recipient` subscribers, `draft: true` suppression until a commit flips `draft: false`, `not_before` deferral until `PromoteDeferred` reaches the timestamp, and offline node disconnect + cursor catch-up (`Cursor: lastSeq`) without duplicates or gaps | SQLite, PostgreSQL | **PASS** | 11.42s |
| `hub/grainfiles/simtest` | `TestEndToEndTriggersTriggerWithAndCrossDeploymentDependsOn` — verifies `on: push` (branch + path + ignore globs), `on: pull_request` (`opened` / `synchronize` against base branch + paths), `on: merge` (merged PR into base branch), topological dependency ordering of `trigger_with` companion suites (`step-a` -> `step-b` -> `suite`), and cross-deployment `depends_on` (`Node A` calls `CompleteGrainfile` -> `Node B` receives live `dependency_completed` event) | SQLite, PostgreSQL | **PASS** | 9.94s |
| `hub/grainfiles/simtest` | `TestEndToEndCrossRepoContentProxyAndGitHubAppAuth` — verifies `GET /v1/grainfiles/content` proxies the Grainfile body and same-repo `references` (`/docs/spec.md`, `./schema.json`) from the real git repository via GitHub App installation token auth, while rejecting cross-repo references (`https://github.com/acme/repo-c/blob/main/secret.md`), unreferenced same-repo files (`/secret.env`), and unauthorized nodes with `403 Forbidden` | SQLite, PostgreSQL | **PASS** | 8.76s |
| `sdk/ts` | `test/integration/hub.integration.ts` — spawns `silo hub serve` and `fakegrain` with `-webhook-secret`, `-github-token`, `-github-api-url`, and `-recipients`; commits `.task.md` & `.process.md` files to real git repos, delivers signed webhooks, and verifies `listGrainfiles`, `getGrainfilesOverview`, and `inspectGrainfile` across `kind`, `recipient`, `repo`, `draft`, `gate`, `requestor`, and combined filters, plus live `grainfile_event` delivery to `fakegrain` | SQLite + Node.js | **PASS** | 3.47s |

## 3. Hub & SDK Improvements Made During Verification

1. **Deterministic multi-file commit ordering (`hub/grainfiles/store.go`)**:
   - Added `indexGrainfileAt(ctx, g, at)` so all Grainfiles indexed within a single webhook delivery share the same commit indexing timestamp (`webhookNow`), and extended `loadLatestIndexQ` with `ORDER BY updated_at DESC, repo ASC, path ASC, commit_sha DESC` so listing results are 100% deterministic across SQLite and PostgreSQL.
2. **Extended search filters & per-record inspection metadata (`hub/grainfiles/http.go`, `hub/grainfiles/store.go`, `sdk/ts`)**:
   - Added `gate` (`true`/`false`) and `requestor` (case-insensitive, trailing-slash normalized) query filters to `GET /v1/grainfiles` and `sdk/ts`'s `GrainfileQuery`.
   - Enriched each returned `IndexRecord` (`Grainfile` in `sdk/ts`) with `subscribed_nodes` (nodes whose active subscriptions match the Grainfile) and `recent_events` (recent trigger/execution events for that Grainfile).
3. **Topological ordering of `trigger_with` companion suites (`hub/grainfiles/store.go`)**:
   - Updated `EvaluateTriggers` so `trigger_with` companion Grainfiles are emitted in topological order respecting `depends_on` and chained `trigger_with` edges before the parent suite Grainfile.
4. **Automatic promotion of past-due `not_before` Grainfiles on webhook ingestion (`hub/grainfiles/http.go`)**:
   - Called `PromoteDeferred` during webhook processing so Grainfiles committed with an already-elapsed `not_before` timestamp are immediately promoted alongside newly ready tasks.
