output "service_account_email" {
  description = "The Google service account the hub's Kubernetes service account is annotated with."
  value       = google_service_account.hub.email
}

output "sql_connection_name" {
  description = "The Cloud SQL instance, for the proxy: CLOUD_SQL_INSTANCE in the manifests' config."
  value       = google_sql_database_instance.hub.connection_name
}

output "database_url" {
  description = "SILO_HUB_DATABASE: the hub serves as its IAM database user, through the Cloud SQL Auth Proxy's IAM-authenticated port beside it. No password; the silo-hub-db Secret's url."
  value       = "postgres://${urlencode(local.db_runtime_user)}@127.0.0.1:5432/${local.db_name}?sslmode=disable"
}

output "migrate_database_url" {
  description = "SILO_HUB_MIGRATE_DATABASE: `silo hub migrate` runs as the schema's owner, the built-in user, through the proxy's password port. The silo-hub-db Secret's migrate-url."
  value       = "postgres://${local.db_user}:${random_password.db.result}@127.0.0.1:5433/${local.db_name}?sslmode=disable"
  sensitive   = true
}

output "runtime_role" {
  description = "SILO_HUB_RUNTIME_ROLE: the PostgreSQL role the hub serves as, which `silo hub migrate` grants the hub's rows."
  value       = local.db_runtime_user
}

output "ip_address" {
  description = "The hub's static IP; the Gateway names it silo-hub."
  value       = google_compute_global_address.hub.address
}

output "certificate_map" {
  description = "The Certificate Manager map the Gateway's networking.gke.io/certmap annotation names."
  value       = google_certificate_manager_certificate_map.hub.name
}

output "dns_records" {
  description = "The records domain and node_ui_domain need, made here when dns_zone is set and by hand otherwise."
  value = concat([
    { name = "${var.domain}.", type = "A", data = google_compute_global_address.hub.address },
    {
      name = google_certificate_manager_dns_authorization.hub.dns_resource_record[0].name
      type = google_certificate_manager_dns_authorization.hub.dns_resource_record[0].type
      data = google_certificate_manager_dns_authorization.hub.dns_resource_record[0].data
    },
    ], flatten([for d, a in google_certificate_manager_dns_authorization.node_ui : [
      { name = "*.${d}.", type = "A", data = google_compute_global_address.hub.address },
      { name = a.dns_resource_record[0].name, type = a.dns_resource_record[0].type, data = a.dns_resource_record[0].data },
  ]]))
}

output "node_ui_domain" {
  description = "SILO_HUB_NODE_UI_DOMAIN in the manifests' config, and the HTTPRoute silo-hub-node-ui's host name without its \"*.\"; empty when there is none."
  value       = local.node_ui_domain
}

output "iap_audience_prefix" {
  description = "SILO_HUB_IAP_AUDIENCE without the backend service's ID, which exists once the Gateway has made it (docs/deploy.md)."
  value       = "/projects/${data.google_project.this.number}/global/backendServices/"
}

output "image_repository" {
  description = "Where the hub's image is pushed."
  value       = "${var.region}-docker.pkg.dev/${var.project}/silo/silo-hub"
}

output "get_credentials" {
  description = "The command that points kubectl at the cluster."
  value       = "gcloud container clusters get-credentials ${var.cluster_name} --location ${var.cluster_location} --project ${var.project}"
}
