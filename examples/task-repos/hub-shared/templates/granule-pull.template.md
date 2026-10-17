---
kind: template
schema: grain/template@v1
aka:
- template-1
name: Nightly granule pull
title: Pull new changes from granule into vendored copy
inputs:
  - name: repo
    type: ref
    ref_kind: repo
    default: "https://github.com/bwsalmon/grain.git@candidate"
    label: The repo the change lands in, with its base branch
team: hub
auto_merge: true
context:
  - {input: repo, mode: write}
  - {ref: "https://github.com/gke-labs/granule.git@main", mode: read}
created_at: '2026-10-02T18:30:00Z'
---

Pull all new upstream changes from `gke-labs/granule` (or the upstream granule
repository) into Grain's vendored copy under `third_party/granule/`, verify tests
and build inputs, and auto-merge the updated vendored tree.

## 1. Fetch and pull latest changes from upstream granule

- Clone or fetch the latest default branch (`main`) from `gke-labs/granule`.
- Sync all contents into `third_party/granule/` (excluding `.git`), preserving
  any documented in-flight local patches (e.g. as noted in `docs/granule-local-patches.md`).
- Update vendored dependencies or modules in `third_party/granule/vendor/` and `go.mod` / `go.sum` if applicable.

## 2. Verify build inputs and local integrity

- Verify that build and guest inputs in `scripts/granule/guest-inputs.sh` and
  `scripts/granule/build-granule-image.sh` are consistent with the synced files.
- Run unit and integration tests across the repository:
  - Run granule's own tests: `go test ./...` in `third_party/granule/`
  - Run grain's related tests: `go test ./pkg/granule/...` and `./tests/deploy/...`
  - Run `go run ./cmd/grain declarations check`
- If upstream has introduced any breaking changes or bug fixes requiring changes in `gke-labs/granule`,
  file follow-up tasks against `gke-labs/granule` using `propose_task` with `repo: "gke-labs/granule"`.

## 3. Commit, push, and gate on CI

- Commit the vendored changes with a clear commit message detailing the upstream changes synced.
- Push the branch and verify CI checks pass with `wait_for_checks`.
- Let the pull request auto-merge upon green CI verdict.
