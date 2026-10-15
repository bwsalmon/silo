# A pool of agent service accounts in an existing project, laid out the
# way grain's gcp-key capability hands them out, with the roles Silo v2's
# live GCP tests need. See README.md.

locals {
  indexes = range(var.account_count)
  emails  = [for i in local.indexes : "${var.account_prefix}-${i}@${var.project_id}.iam.gserviceaccount.com"]

  project_roles = var.create_accounts ? distinct(concat(var.agent_roles, var.extra_roles)) : distinct(var.extra_roles)
  project_grants = {
    for pair in setproduct(local.indexes, local.project_roles) :
    "${pair[0]}/${pair[1]}" => { index = pair[0], role = pair[1] }
  }
}

resource "google_project_service" "apis" {
  for_each = toset([
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "cloudresourcemanager.googleapis.com",
  ])
  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

resource "google_service_account" "agent" {
  count        = var.create_accounts ? var.account_count : 0
  project      = var.project_id
  account_id   = "${var.account_prefix}-${count.index}"
  display_name = "Sandbox Agent Service Account ${count.index}"
  depends_on   = [google_project_service.apis]
}

# Existing accounts are named, not looked up, so that a missing one fails
# the plan with its name.
data "google_service_account" "existing" {
  count      = var.create_accounts ? 0 : var.account_count
  project    = var.project_id
  account_id = local.emails[count.index]
}

locals {
  members = var.create_accounts ? [for a in google_service_account.agent : a.member] : [for a in data.google_service_account.existing : a.member]
  names   = var.create_accounts ? [for a in google_service_account.agent : a.name] : [for a in data.google_service_account.existing : a.name]
}

# Non-authoritative: other members of these roles, and these accounts'
# other roles, are left alone, so this layers onto a pool grain's own
# terraform made without fighting it.
resource "google_project_iam_member" "pool" {
  for_each = local.project_grants
  project  = var.project_id
  role     = each.value.role
  member   = local.members[each.value.index]
}

# Each account may attach itself -- and no other account -- to the VMs
# and clusters it creates, as grain's sandbox_agents_sa_user grants.
resource "google_service_account_iam_member" "acts_as_self" {
  count              = var.create_accounts ? var.account_count : 0
  service_account_id = local.names[count.index]
  role               = "roles/iam.serviceAccountUser"
  member             = local.members[count.index]
}

# grain's minter may mint keys for the pool accounts and nothing else, as
# grain's sandbox_minter_key_admin grants.
resource "google_service_account_iam_member" "minter_key_admin" {
  count              = var.create_accounts && var.minter_email != "" ? var.account_count : 0
  service_account_id = local.names[count.index]
  role               = "roles/iam.serviceAccountKeyAdmin"
  member             = "serviceAccount:${var.minter_email}"
}
