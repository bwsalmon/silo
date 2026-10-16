---
kind: task
schema: grain/task@v1
id: task-m0jpyjbjz6p3
title: Prune superseded GHCR package versions with package-publish capability
team: hub
repo: https://github.com/bwsalmon/grain.git
base: candidate
state: running
submitter: "silo:schedule/sch-7tpkw2psymx2"
submitted_via: schedule
approved_by: "silo:schedule/sch-7tpkw2psymx2"
approved_at: "2026-10-11T04:00:01Z"
created_at: "2026-10-11T04:00:01Z"
auto_merge: true
template:
  ref: "shared:templates/prune-packages.md"
  id: tmpl-7txvezb4bfz9
  rev: 4b1d0c6a3e9f27d58c0e1a6b9f3d2c7e8a5b4f10
  inputs: {}
schedule:
  ref: "shared:schedules/prune-packages.md"
  id: sch-7tpkw2psymx2
  fired_at: "2026-10-11T04:00:01Z"
placement:
  grain: eu
  ref: /tasks/task-m0jpyjbjz6p3-prune-ghcr/task.md
  generation: 1
  at: "2026-10-11T04:00:03Z"
  intake: {ref: "intake:tasks/task-m0jpyjbjz6p3-prune-ghcr/task.md", id: task-m0jpyjbjz6p3}
attestation: eyJhbGciOiJFZERTQSIsImtpZCI6ImNsYWltcy0yMDI2LTEwIiwidHlwIjoiSldUIn0.eyJpc3MiOiJodHRwczovL3NpbG8uZXhhbXBsZS5jb20iLCJhdWQiOiJncmFpbjpldSIsImlhdCI6MTc5MjAzNjgwMCwiZXhwIjoxNzkyMDQwNDAwLCJyZXF1ZXN0ZXIiOiJic2FsbW9uQGdvb2dsZS5jb20iLCJ0ZWFtIjoiaHViIiwiYXBwcm92ZWRfYnkiOiJzaWxvOnNjaGVkdWxlL3NjaC03dHBrdzJwc3lteDIiLCJ0YXNrIjoidGFzay1tMGpweWpiano2cDMifQ.ZXhhbXBsZS1zaWduYXR1cmUtbm90LXZhbGlk
---

Run `cmd/ghcr-prune` using the `package-publish` capability to clean up old,
superseded container package versions in GitHub Container Registry (GHCR) that
deployments and CI workflows have stopped needing.

## 1. Verify credentials and capability environment

- Ensure the `package-publish` capability (or valid GitHub package management token)
  is available:
  - Check for credentials placed at `/home/debian/.package-publish-token`,
    `/home/debian/.github-sandbox-token`, or in `PACKAGE_PUBLISH_TOKEN` / `GITHUB_TOKEN`
    environment variables.
  - If required credentials are missing, note that `package-publish` capability
    must be granted to this repository/task in deployment settings.

## 2. Execute GHCR package pruning

- First run `go run ./cmd/ghcr-prune` in dry-run mode to inspect the deletion plan:
  - Review the count of package versions to be kept, deleted, and deferred.
  - Confirm that active branch tags, latest tags, recent rollback commits, and
    pinned guest caches will be preserved according to policy.
- Execute the pruning operation with `-apply`:
  ```bash
  go run ./cmd/ghcr-prune -apply=true
  ```
- Monitor the output for any deletion errors, rate limits, or warnings.

## 3. Review and report results

- Review the summary of deleted, refused, kept, and deferred package versions.
- If any unexpected refusals or high deferred counts remain across consecutive runs,
  document findings in the task report.
- Conclude the run with a clear summary of pruned packages and current registry status.
