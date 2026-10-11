# Silo conformance suite: the export and link cases, against real GCP

**Date**: 2026-10-11 (task 2059, filed by task 2007)
**Commit tested**: `a510f50` (`main`) plus this branch: the new cases in
`pkg/silo/conformance/grant_cases_test.go`, and the exporter probe's
servers and `TCPHello` check in `conformance.go` with their unit test.
**Environment**: Debian sandbox with the `gcp-key` capability, as
`grain-sb-agent-6@bsalmon-fun-sandbox.iam.gserviceaccount.com`, against
project `bsalmon-fun-sandbox`, home region `us-central1`. No other
conformance run was in progress.
**Command**:
`go test -tags silo_conformance -count=1 -v ./pkg/silo/conformance/`
(what `.grain/workflows/silo-conformance.yml` runs), with the
workflow's `silo uninstall --force --delete-data --yes` trap around it.

## Result: install refused at prereqs a third time; every case skipped

Nothing in the project has changed since the fence-cases run
(`2026-10-11-silo-conformance-fence-cases.md`). `silo install` stopped
at its `prereqs` step and created nothing. Every case that needs a tree
skipped for that reason, and the harness uninstalled. The project's
instances and networks were the same after the run as before it.

None of the new cases has run against a real install yet. They compile
and vet under the `silo_conformance` tag. The probe scripts they read
results from have a unit test that runs them locally. The first install
that gets past prereqs will be their first real run.

| Case | Result | Why |
| --- | --- | --- |
| `TestInstall/Finished` | **FAIL** | prereqs: see below |
| `TestInstall/CloudSQLWithPSCAndIAMLogin`, `MigrateVMCameAndWent` | SKIP | no install |
| `TestExportsWithoutConsent` (new): offered not accepted, accepted not offered, no direct path, no port for an unaccepted TCP offer | SKIP | no install |
| `TestExportsEndToEnd` (new): HTTP `Silo-Caller`/`Silo-Caller-Path` (forged ones dropped), TCP with the gateway as source, PROXY v2 naming the importer, TCP port refused outside the importer, withdraw closes an open connection within 1s | SKIP | no install |
| `TestLinksWithoutConsent` (new): one side only, both sides on the named port only, deleted | SKIP | no install |
| `TestLinksPolicy` (new): `links: deny` on `/a/c` suspends the active link, refuses a new one, and allowing again resumes | SKIP | no install |
| `TestSealedSiloKeepsGrants` (new): from a sealed silo, HTTP and TCP exports and an L3 link work, the API port doesn't | SKIP | no install |
| `TestDatabaseWithGrants` (new): with an active all-protocol link and accepted exports, `/a` and `/a/c` reach no port of the PSC endpoint, nor ICMP | SKIP | no install |
| Every earlier tree case (`TestAttachingOutsideTheSilo` … `TestFenceMatchesEvaluator`) | SKIP | no install |
| `TestUninstallLeavesNothing` | **FAIL** | the leftover check can't list Cloud SQL instances (403) |
| Unit tests: `TestExporterServersAndTCPHello` (new), `TestPickAddressSpace*`, `TestProbeScriptAndReport`, `TestLeftovers`, `TestDriftFields` | PASS | |

## Why install was refused

The same three blockers as the two earlier runs. None of them is in
Silo's code:

1. **The key lacks install's roles.** prereqs reports the same missing
   permissions, and Cloud SQL and org policy can't be read (403). The
   fix it prints: `roles/owner`, or
   `roles/serviceusage.serviceUsageAdmin`, `roles/dns.admin`,
   `roles/iam.serviceAccountAdmin`, `roles/iam.roleAdmin`,
   `roles/resourcemanager.projectIamAdmin`, `roles/artifactregistry.admin`,
   `roles/iap.admin`, `roles/cloudsql.admin`.
2. **A VPC named `silo` still exists**, and install didn't create it.
   Install owns that name, so it refuses. This run didn't create it and
   left it alone.
3. **No silod image** built with the conformance tag, which the sealed
   cases (`TestSealedSiloKeepsGrants` among them) need.

## Bugs found in the code under test

No new ones: no case reached the code under test. The uninstall bug the
first run reported still reproduces. With no install record, `tenants`,
`orphans`, `credentials` and `admin` fail with `json: invalid use of
,string struct tag`. The workflow's trap uninstall also hit a transient
503 listing enabled services.

## Judgement call

testing.md says a link both sides want under `links: deny` is "refused
with `LOOSER_THAN_PARENT` or suspended". silod refuses the proposal with
`PERMISSION_DENIED` and names the policy. fence-api.md's error table
keeps `LOOSER_THAN_PARENT` for a change that gives a child more than its
parent has, and a proposal isn't one. So `TestLinksPolicy` takes either
code, and also checks that a link already active goes `SUSPENDED`.
