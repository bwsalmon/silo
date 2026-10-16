output "service_account_email" {
  description = "The Google service account the hub's Kubernetes service account is annotated with."
  value       = google_service_account.hub.email
}

output "sql_connection_name" {
  description = "The Cloud SQL instance, for the proxy: CLOUD_SQL_INSTANCE in the manifests' config."
  value       = google_sql_database_instance.hub.connection_name
}

output "database_url" {
  description = "SILO_HUB_DATABASE, through the Cloud SQL Auth Proxy beside the hub: the silo-hub-db Secret's url."
  value       = "postgres://${local.db_user}:${random_password.db.result}@127.0.0.1:5432/${local.db_name}?sslmode=disable"
  sensitive   = true
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
  description = "The records domain needs, made here when dns_zone is set and by hand otherwise."
  value = [
    { name = "${var.domain}.", type = "A", data = google_compute_global_address.hub.address },
    {
      name = google_certificate_manager_dns_authorization.hub.dns_resource_record[0].name
      type = google_certificate_manager_dns_authorization.hub.dns_resource_record[0].type
      data = google_certificate_manager_dns_authorization.hub.dns_resource_record[0].data
    },
  ]
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
