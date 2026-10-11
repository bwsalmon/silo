# Silo conformance suite: the fence cases, second run against real GCP

**Date**: 2026-10-11 (task 2058, filed by task 2007)
**Commit tested**: `0fa34a0` (`main`) plus this branch: the new cases in
`pkg/silo/conformance/fence_cases_test.go`, the `http` and `sh` probe
checks, and silod's conformance-only test switches.
**Environment**: Debian sandbox with the `gcp-key` capability, as
`grain-sb-agent-5@bsalmon-fun-sandbox.iam.gserviceaccount.com`, against
project `bsalmon-fun-sandbox`, home region `us-central1`.
**Command**:
`go test -tags silo_conformance -count=1 -v ./pkg/silo/conformance/`
(what `.grain/workflows/silo-conformance.yml` runs).

## Result: install refused at prereqs again; every case skipped

Nothing changed in the project since the first run
(`2026-10-11-silo-conformance-first-run.md`): `silo install` stopped at
its `prereqs` step and created nothing, so every case that needs a tree
skipped with that reason. The harness then ran `silo uninstall --force
--delete-data --yes`. The project's instances and networks were the same
after the run as before it.

None of the new cases has yet run against a real install, so none of
them is validated against GCP: they compile and vet under the
`silo_conformance` tag, and their harness pieces have unit tests, but
the first install that gets past prereqs is their first real run.

| Case | Result | Why |
| --- | --- | --- |
| `TestInstall/Finished` | **FAIL** | prereqs: see below |
| `TestInstall/CloudSQLWithPSCAndIAMLogin` | SKIP | no install |
| `TestInstall/MigrateVMCameAndWent` | SKIP | no install |
| `TestAttachingOutsideTheSilo` | SKIP | no install |
| `TestSubnets` | SKIP | no install |
| `TestListAndGetLeaks` | SKIP | no install |
| `TestNameCollisions` | SKIP | no install |
| `TestRefusedFieldsAndMethods` | SKIP | no install |
| `TestQuota` | SKIP | no install |
| `TestInstanceTemplates` | SKIP | no install |
| `TestStillRefused` | SKIP | no install |
| `TestDatabase` | SKIP | no install |
| `TestOriginSpoofing` (new) | SKIP | no install |
| `TestCrossingSilosWithoutConsent` (new) | SKIP | no install |
| `TestEgressAndGateway` (new) | SKIP | no install |
| `TestDatabaseThroughTheGateway` (new) | SKIP | no install |
| `TestLeaderFailover` (new) | SKIP | no install |
| `TestSealedAndReadSilos` (new) | SKIP | no install |
| `TestControlInheritance` (new) | SKIP | no install |
| `TestManageGrants` (new) | SKIP | no install |
| `TestFenceMatchesEvaluator` (new) | SKIP | no install |
| `TestUninstallLeavesNothing` | **FAIL** | the leftover check can't list Cloud SQL instances (403) |
| harness unit tests (`TestPickAddressSpace*`, `TestProbeScriptAndReport`, `TestLeftovers`, `TestDriftFields`) | PASS | |

## Why install was refused

The same three blockers as the first run, none of them Silo's code:

1. **The key lacks install's roles.** prereqs reports missing
   `serviceusage.services.enable`, `dns.managedZones.create`,
   `dns.changes.create`, `iam.serviceAccounts.create`,
   `iam.roles.create`, `iam.roles.update`,
   `resourcemanager.projects.getIamPolicy`/`setIamPolicy`,
   `artifactregistry.repositories.create`/`setIamPolicy`,
   `iap.tunnelInstances.setIamPolicy`, `cloudsql.instances.create`,
   `cloudsql.databases.create`, `cloudsql.users.create`, and can't read
   Cloud SQL at all (403). The fix it prints: `roles/owner`, or
   `roles/serviceusage.serviceUsageAdmin`, `roles/dns.admin`,
   `roles/iam.serviceAccountAdmin`, `roles/iam.roleAdmin`,
   `roles/resourcemanager.projectIamAdmin`, `roles/artifactregistry.admin`,
   `roles/iap.admin`, `roles/cloudsql.admin`.
2. **A VPC named `silo` still exists** that install didn't create.
   Install owns that name, so it refuses. This run didn't create it and
   left it alone.
3. **No silod image.** The new sealed-silo cases also need it built
   with the conformance tag:
   `make silod-image SILOD_TAGS=silo_conformance SILOD_PUSH=1 SILOD_IMAGE=...`,
   passed by digest in `SILO_CONFORMANCE_IMAGE`.

## Bugs found in the code under test

No new ones: no case reached the code under test. The uninstall bug the
first run filed still reproduces: with no install record, `tenants`,
`orphans`, `credentials` and `admin` fail (`json: invalid use of
,string struct tag ... "projects/bsalmon-fun-sandbox/regions/" into
uint64`), and this run also saw a transient 503 listing enabled
services.
