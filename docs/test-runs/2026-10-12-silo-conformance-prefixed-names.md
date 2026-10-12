# Silo conformance suite: prefixed tenant names, against real GCP

**Date**: 2026-10-12 (task 3179, filed by task 3124)
**Commit tested**: `9466ba8` (`main`, with task 3124's prefixed names
and the `pkg/silo` → `pkg` flatten).
**Environment**: Debian sandbox with the `gcp-key` capability, as
`grain-sb-agent-6@bsalmon-fun-sandbox.iam.gserviceaccount.com`, against
project `bsalmon-fun-sandbox`, home region `us-central1`.
**Command**: the workflow's steps (`.grain/workflows/silo-conformance.yml`):
`go test -tags silo_conformance -count=1 -timeout 160m -v ./pkg/conformance/`,
with the `silo uninstall --force --delete-data --yes` trap around it.

## Result: install refused at prereqs a fifth time; every case skipped

`silo install` stopped at `prereqs` and created nothing, for the same
reasons as the four earlier runs (`2026-10-11-silo-conformance-*.md`):

1. The key lacks install's roles (`serviceusage.services.enable`,
   `dns.*`, `iam.serviceAccounts.create`, `iam.roles.*`, project IAM,
   Artifact Registry, IAP, Cloud SQL, org policy reads).
2. A VPC named `silo` that install didn't create still exists.

So none of the cases task 3124 rewrote for prefixed names has run
against GCP: `TestInstall/Finished` FAIL (prereqs), every GCP case SKIP
at `installed(t)`, `TestUninstallLeavesNothing` FAIL (the leftover
check can't list Cloud SQL: 403), and the unit tests in the package
PASS. Nothing was created.

## Read instead of run

With no install, the cases were reviewed against the facade as it now
is. The ones task 3124 named check out:

- `Name63Allowed64Refused`: ids are six characters, so
  `<id>-` plus 57 is 64 (refused 400) and plus 56 is 63 (allowed).
- `NameWithoutThePrefixRefused`: a bare name and another silo's prefix
  are both 400 with `must start with "<id>-"`.
- `refused(...)` existence checks all GET a prefixed name.
- `TestManageGrants` inserts `<target id>-held` in the target's facade,
  on `<target id>-net`, as the target's namer requires.
- The agent cases make `<id>-net` and expect the tool VM
  `<id>-agent-op` / `<id>-agent-worker`; the tool VM's subnet goes into
  the facade as a link, which has to carry the prefix and does.

Three things the cases had wrong that aren't about names, and that
would have failed the first run to get past install, are fixed:

- **`SiloLabelRefused` and `SiloTagRefused` wanted 400.** The facade
  refuses a label or tag starting with `silo` with 403 (`checkReserved`,
  pinned by its unit test), before it looks at names. They now want 403.
- **`mkSilo` gave every silo a /24 net subnet,** but its callers ask
  for /26 or /27 blocks, so the subnet wasn't inside the block and the
  ledger would refuse it. The net subnet is now `netOf(block)`: the
  first /24, or the first half of a /24 or smaller block.
- **Three cases made a /28 at offset 128 of a /26 block**, outside it
  (all in `TestSealedAndReadSilos`: the sealed probe's facade write,
  `/r`'s `subnetworks.insert`, and the root's
  `AncestorWritesAndAllocations`). They now use offset 32, the first
  address after the /27 net subnet.

## Still open

The human fixes above, before any of this runs: the key needs
install's roles (or `roles/owner`) on `bsalmon-fun-sandbox`, and the
stray `silo` VPC has to go. As the earlier runs found, it also needs a
conformance silod image (this run's 403 on the image step hid whether
one exists), and an Anthropic key for the model cases.
