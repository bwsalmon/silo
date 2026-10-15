variable "project_id" {
  type        = string
  description = "The existing project the pool lives in, such as bsalmon-fun-sandbox."
}

variable "account_prefix" {
  type        = string
  default     = "grain-sb-agent"
  description = <<-EOT
    Pool accounts are <account_prefix>-0 ... <account_prefix>-<count-1>,
    0-indexed, which is how grain's gcp-key capability numbers a pool
    (pkg/capability/gcpkey accountEmails, gcp_service_account_count) and
    how grain's deploy/terraform/gcp/sandbox.tf names
    <name_prefix>-sb-agent-N. Point grain's gcp-key capability at
    <account_prefix>-0@<project_id>.iam.gserviceaccount.com with a
    service-account count of account_count.
  EOT
}

variable "account_count" {
  type        = number
  default     = 10
  description = "How many accounts the pool has (grain's gcp_service_account_count)."
}

variable "create_accounts" {
  type        = bool
  default     = true
  description = <<-EOT
    true: this module creates the accounts, with grain's agent grants
    (agent_roles, each account acting as itself, the minter's key admin).
    false: the accounts already exist (for example made by grain's own
    deploy/terraform/gcp/sandbox.tf, which already makes those grants),
    and this module only adds extra_roles to them.
  EOT
}

variable "agent_roles" {
  type = list(string)
  default = [
    "roles/compute.admin",
    "roles/container.admin",
    "roles/iap.tunnelResourceAccessor",
  ]
  description = "Project roles every account gets when create_accounts is true: grain's sandbox.tf agent roles."
}

variable "extra_roles" {
  type = list(string)
  default = [
    "roles/iam.serviceAccountAdmin",
    "roles/resourcemanager.projectIamAdmin",
    "roles/logging.viewer",
  ]
  description = <<-EOT
    Project roles every account gets, created here or not. The default is
    what Silo v2's journey 2 live test (v2/gcp/live_test.go) needs of its
    invoking principal: the two setup roles of design §11.2 step 1, and
    roles/logging.viewer, the role the test grants its account (under
    call 19 the invoker must hold every permission of a role it grants).
  EOT
}

variable "minter_email" {
  type        = string
  default     = ""
  description = <<-EOT
    The account grain's gcp-key capability mints keys with (for example
    grain-sb-minter@<project>.iam.gserviceaccount.com). When set and
    create_accounts is true, it gets roles/iam.serviceAccountKeyAdmin on
    each pool account -- and only those, as grain's
    sandbox_minter_key_admin grants it.
  EOT
}
