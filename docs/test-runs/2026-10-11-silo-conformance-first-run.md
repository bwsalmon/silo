# Silo conformance suite: first run against real GCP

**Date**: 2026-10-11 (task 2050, filed by task 2007)
**Commit tested**: `4880e3d` (`main`) plus this branch: the conformance
harness in `pkg/silo/conformance` and the prereqs fix below.
**Environment**: Debian sandbox with the `gcp-key` capability, as
`grain-sb-agent-4@bsalmon-fun-sandbox.iam.gserviceaccount.com`, against
project `bsalmon-fun-sandbox`, home region `us-central1`.
**Command**:
`go test -tags silo_conformance -count=1 -v ./pkg/silo/conformance/`
(what `.grain/workflows/silo-conformance.yml` runs).

## Result: install refused at prereqs; every case skipped

`silo install` (fresh `/16` from `PickAddressSpace`, `--db-tier
db-f1-micro --control-replicas 2`) stopped at its `prereqs` step and
created nothing. The harness then skipped every case with that reason,
ran `silo uninstall --force --delete-data --yes`, and checked for
leftovers. The project's instance list was the same after the run as
before it (`grain`, `storage-bench-vm`, a GKE node; none of them Silo's).

| Case | Result | Why |
| --- | --- | --- |
| `TestInstall/Finished` | **FAIL** | prereqs: see below |
| `TestInstall/CloudSQLWithPSCAndIAMLogin` | SKIP | no install |
| `TestInstall/MigrateVMCameAndWent` | SKIP | no install |
| `TestAttachingOutsideTheSilo` | SKIP | no install |
| `TestSubnets` | SKIP | no install |
| `TestListAndGetLeaks` | SKIP | no install |
| `TestNameCollisions` | SKIP | no install |
| `TestRefusedFieldsAndMethods` (incl. `DiscoveryDrift`) | SKIP | no install |
| `TestQuota` (incl. `GPU`) | SKIP | no install |
| `TestInstanceTemplates` | SKIP | no install |
| `TestStillRefused` | SKIP | no install |
| `TestDatabase` | SKIP | no install |
| `TestUninstallLeavesNothing` | **FAIL** | the leftover check can't list Cloud SQL instances (403) |
| unit tests of the harness (`TestPickAddressSpace*`, `TestProbeScriptAndReport`, `TestLeftovers`, `TestDriftFields`) | PASS | |

## Why install was refused

All of these are the project or the key, not Silo's code:

1. **The key lacks most of install's roles.** prereqs, after the fix
   below, reports missing: `serviceusage.services.enable`,
   `dns.managedZones.create`, `dns.changes.create`,
   `iam.serviceAccounts.create`, `iam.roles.create`, `iam.roles.update`,
   `resourcemanager.projects.getIamPolicy`/`setIamPolicy`,
   `artifactregistry.repositories.create`/`setIamPolicy`,
   `iap.tunnelInstances.setIamPolicy`, `cloudsql.instances.create`,
   `cloudsql.databases.create`, `cloudsql.users.create`. It also can't
   read org policies (`orgpolicy.policy.get`) or the Cloud Quotas API.
   install.md "Input" lists the roles (or `roles/owner`).
2. **A VPC named `silo` already exists** (created 2026-10-05, routing
   mode `REGIONAL`, no subnets, not labelled by install). Install owns
   that name, so it refuses. It wasn't made by this run and was left
   alone; whoever made it should delete it.
3. **No silod image** is published (`ghcr.io/bwsalmon/silod` is empty);
   the workflow takes one by digest through `SILO_CONFORMANCE_IMAGE`,
   which also needs a registry the key can push to.

Warnings that don't stop install: the project isn't dedicated to Silo
(3 other instances), and its common metadata carries `ssh-keys` and
GKE keys that every silo's VMs could read.

## Bugs found in the code under test

- **Fixed here**: prereqs' credentials check sent
  `billing.resourceAssociations.list` to the project's
  `testIamPermissions`, which answers 400 "not valid for this resource"
  for the whole request, so no permission was checked at all and the
  check failed with a misleading message. Dropped from the tested set
  (the billing check reads billing directly); the install fake now
  refuses `billing.*` as GCE does, with
  `TestCredentialsCheckIsValidOnAProject`.
- **Filed as a follow-up**: `silo uninstall` with no install record
  (nothing installed, or install stopped at prereqs) fails nearly every
  step: the regional steps observe with an empty region (`Invalid value
  for field 'region': ''`), and the tenants/orphans/credentials/admin
  steps fail decoding a regions URL (`json: invalid use of ,string
  struct tag ... into uint64`). It should report every step as nothing
  to do.
