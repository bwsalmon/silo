output "pool_emails" {
  value       = local.emails
  description = "The pool's accounts, in grain's gcp-key order."
}

output "gcp_key_service_account_email" {
  value       = local.emails[0]
  description = "What grain's gcp-key capability takes as its service account email, with a count of account_count."
}

output "project_roles" {
  value       = local.project_roles
  description = "The project roles every pool account holds through this module."
}
