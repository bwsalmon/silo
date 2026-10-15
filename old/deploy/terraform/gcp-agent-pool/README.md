# gcp-agent-pool

Terraform for a pool of agent service accounts in an **existing** GCP
project, laid out the way grain hands GCP identities to sandboxes, and
holding the roles Silo v2's live GCP test needs.

## How it matches grain

grain's `gcp-key` capability (`pkg/capability/gcpkey`) is configured
with one service-account email and a count. With a count above 1 it uses
a 0-indexed pool, `<prefix>-0@<project>.iam.gserviceaccount.com` …
`<prefix>-<count-1>@…`, and mints a short-lived key for one account per
task with a separate minter account. That minter needs
`roles/iam.serviceAccountKeyAdmin` on each pool account, and on nothing
else. grain's own `deploy/terraform/gcp/sandbox.tf` makes such a pool
(`<name_prefix>-sb-agent-0..9`). Each account gets `roles/compute.admin`,
`roles/container.admin` and `roles/iap.tunnelResourceAccessor`, and
`roles/iam.serviceAccountUser` on itself only.

This module makes the same pool and the same grants, with these defaults:

| Variable | Default | |
| --- | --- | --- |
| `project_id` | required | The existing project. |
| `account_prefix` | `grain-sb-agent` | Accounts are `<prefix>-0` … `<prefix>-<count-1>`. |
| `account_count` | `10` | grain's `gcp_service_account_count`. |
| `create_accounts` | `true` | `false` adds `extra_roles` to accounts that already exist, such as ones grain's terraform made. |
| `agent_roles` | grain's three agent roles | Used only when creating the accounts. |
| `extra_roles` | `roles/iam.serviceAccountAdmin`, `roles/resourcemanager.projectIamAdmin`, `roles/logging.viewer` | Every account gets these. |
| `minter_email` | empty | grain's minter. When set, it gets key admin on each created account. |

`extra_roles` is what `old/gcp/live_test.go` needs of the account it runs
as (design §11.2):
- step 1's setup permissions, which come from `serviceAccountAdmin` and
  `projectIamAdmin`;
- `roles/logging.viewer`, the role the test grants. Under call 19, the
  invoking principal must hold every permission of a role it grants.

Every grant is `google_project_iam_member` or
`google_service_account_iam_member`, which are non-authoritative. Other
members and other roles are left alone, so applying this beside grain's
own terraform does not fight it.

## Use

The account that applies this needs `roles/resourcemanager.projectIamAdmin`
on the project. With `create_accounts = true` it also needs
`roles/iam.serviceAccountAdmin`, and enabling the APIs needs
`roles/serviceusage.serviceUsageAdmin`. Project owners have all of these.

Give `bsalmon-fun-sandbox`'s existing grain pool what the silo live test
needs:

```sh
cd old/deploy/terraform/gcp-agent-pool
terraform init
terraform apply -var project_id=bsalmon-fun-sandbox -var create_accounts=false
```

Make a new pool and point a grain `gcp-key` capability at it:

```sh
terraform apply \
  -var project_id=my-project \
  -var account_prefix=silo-agent \
  -var minter_email=grain-sb-minter@my-project.iam.gserviceaccount.com
terraform output gcp_key_service_account_email   # with count = account_count
```

## Caution

`roles/resourcemanager.projectIamAdmin` lets every pool account change
the project's IAM policy, so a task holding one of their keys can grant
itself any role in the project, `roles/owner` included. That is
acceptable in a disposable sandbox project such as `bsalmon-fun-sandbox`,
which is why the role is in `extra_roles`. Do not apply these defaults to
a project that holds anything else. In such a project, narrow
`extra_roles`, or bind `projectIamAdmin` under an IAM Condition on
`iam.googleapis.com/modifiedGrantsByRole` that limits which roles it may
grant. silo's setup checks have not been tried against such a condition.
