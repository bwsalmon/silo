# Silo conformance suite: the agent cases, against real GCP

**Date**: 2026-10-11 (task 2060, filed by task 2007)
**Commit tested**: `941804a` (`main`) plus this branch: the agent cases
in `pkg/silo/conformance/agent_cases_test.go`, the install's agent
settings (`--agent-sessions 4 --control-machine-type e2-standard-2
--root-model-budget 5`), `ToolOutcomes` in `conformance.go` with its unit
test, and the uninstall fix below.
**Environment**: Debian sandbox with the `gcp-key` capability, as
`grain-sb-agent-7@bsalmon-fun-sandbox.iam.gserviceaccount.com`, against
project `bsalmon-fun-sandbox`, home region `us-central1`. No other
conformance run was in progress: grain CI had none, and the project had
no `silo`-labelled instance.
**Command**:
`go test -tags silo_conformance -count=1 -v -run 'TestInstall|TestAgent|TestToolOutcomes' ./pkg/silo/conformance/`,
with the workflow's `silo uninstall --force --delete-data --yes` trap
around it. Run twice, before and after the uninstall fix.

## Model credential: none

The sandbox has a Gemini key, but v1's adapters are `claude-code`
(Anthropic) and `codex` (OpenAI), and the model proxy serves only those
two providers. So this run had no model credential. Each case that needs
a model call is skipped, not passed. The cases that don't need one
(create, the tool server attaching, the manage holders, and the lease
running out) run with a placeholder credential once an install comes up.
A run with `SILO_CONFORMANCE_MODEL_KEY` naming a file with an Anthropic
API key runs them all. No key is in the repo.

## Result: install refused at prereqs a fourth time; every case skipped

`silo install` stopped at `prereqs` and created nothing, for the same
reasons as the three earlier runs (`2026-10-11-silo-conformance-*.md`):

1. The key lacks install's roles (`serviceusage.services.enable`,
   `dns.*`, `iam.serviceAccounts.create`, `iam.roles.*`,
   project IAM, Artifact Registry, IAP, Cloud SQL, org policy reads).
2. A VPC named `silo` that install didn't create still exists.
3. There is no conformance silod image. With `--agent-sessions` there
   also has to be a harness image beside it
   (`-silo.harness-image` / `SILO_CONFORMANCE_HARNESS_IMAGE`).

None of the agent cases has run against a real install yet.

| Case | Result | Why |
| --- | --- | --- |
| `TestInstall/Finished` | **FAIL** | prereqs, as above |
| `TestInstall/CloudSQLWithPSCAndIAMLogin` | SKIP | no install |
| `TestInstall/MigrateVMCameAndWent` | SKIP | no install |
| `TestAgentsInASealedSilo/CreateWorks` | SKIP | no install |
| `TestAgentsInASealedSilo/ToolServerAttaches` | SKIP | no install |
| `TestAgentsInASealedSilo/ExecRunsOnToolVM` | SKIP | no install; needs a model credential too |
| `TestAgentsInASealedSilo/ModelProxyAnswers` | SKIP | no install; needs a model credential too |
| `TestAgentsInASealedSilo/AskWaitsForAPerson` | SKIP | no install; needs a model credential too |
| `TestAgentsInASealedSilo/MessageStartsATurn` | SKIP | no install; needs a model credential too |
| `TestAgentsInASealedSilo/HarnessFenceAndComputeRefused` | SKIP | no install; needs a model credential too |
| `TestAgentsInASealedSilo/UnderRead` | SKIP | no install; needs a model credential too |
| `TestAgentsInASealedSilo/ExecToAPIPortTimesOut` | SKIP | no install; needs a model credential too |
| `TestAgentsInASealedSilo/ManageAgentsScope` (with and without the `agents` scope) | SKIP | no install |
| `TestAgentUnsealed/CreateWorks` | SKIP | no install |
| `TestAgentUnsealed/TurnUsesATool` | SKIP | no install; needs a model credential too |
| `TestAgentUnsealed/BudgetChargedPerModelCall` | SKIP | no install; needs a model credential too |
| `TestAgentUnsealed/LeaseRunsOutDeletesToolVM` | SKIP | no install |
| `TestUninstallLeavesNothing` | **FAIL** | the leftover check can't list Cloud SQL (403); uninstall's own steps, below |
| `TestToolOutcomes` (unit) | PASS | |

(Go reports the parent tests as skipped at `installed(t)`, before any
subtest starts. The subtests are listed here so that the next run's table
lines up.)

Nothing was created. The project's instances were the same after the
run as before it.

## Bugs found and fixed

- **Uninstall with no install record failed in four steps.** `tenants`,
  `orphans`, `credentials` and `admin` failed with `json: invalid use of
  ,string struct tag, trying to unmarshal "projects/bsalmon-fun-sandbox/regions/"
  into uint64`, as every earlier run reported. With no record there's no
  home region, and `InstanceZone` asked GCE for `regions/""`. GCE
  answers that with the region list, whose string `id` doesn't decode as
  a `Region`. `InstanceZone` now finds nothing when there's no region.
  `TestInstanceZoneWithNoRegion` fails without the fix. After it, all
  four steps are `ok` on the real project.
- **`TestManageGrants` (an earlier task's case) called the wrong URLs.**
  It passed `/silo/v1/silos/...` to `client.Do`, whose paths are relative
  to the `/silo/v1/` endpoint, so its offer and revoke would have gone to
  `/silo/v1//silo/v1/...`. That was never seen because no install has
  come up. It now passes `silos/...`.

## Still open

- Uninstall's other regional steps (`image`, `dns`, `address`, `nat`,
  `control-subnet`, `gateway-subnet`) still fail with no install record:
  GCE refuses `region: ''` with 400. This is the same missing home
  region, and it's filed as a follow-up. The 403s on `control`,
  `migrate`, `db-endpoint`, `role` and `db-instance` are the key's
  missing roles.
- An install needs the human fixes listed above. The model cases need
  an Anthropic key, and grain CI's conformance workflow needs it as a
  capability (grain/config) to run them nightly.
