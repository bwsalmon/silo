# The Silo hub's GCP resources in a project that already has a GKE
# cluster (docs/deploy.md): its database, the identity it reaches the
# database as, IAP, a static IP and managed certificates, for the hub's
# host and for its nodes' UI hosts. The Kubernetes side is
# deploy/kubernetes/hub.

data "google_project" "this" {
  project_id = var.project
}

data "google_container_cluster" "hub" {
  name     = var.cluster_name
  location = var.cluster_location
  project  = var.project
}

# The cluster is not made here; these say when it lacks what the hub
# needs, without failing the plan.
check "cluster" {
  assert {
    condition     = try(data.google_container_cluster.hub.workload_identity_config[0].workload_pool, "") == "${var.project}.svc.id.goog"
    error_message = "Cluster ${var.cluster_name} does not have Workload Identity enabled (workload pool ${var.project}.svc.id.goog)."
  }
  assert {
    condition     = contains(["CHANNEL_STANDARD", "CHANNEL_EXPERIMENTAL"], try(data.google_container_cluster.hub.gateway_api_config[0].channel, ""))
    error_message = "Cluster ${var.cluster_name} does not have the Gateway API enabled (gateway_api_config.channel CHANNEL_STANDARD)."
  }
}

locals {
  apis = toset([
    "sqladmin.googleapis.com",
    "iap.googleapis.com",
    "certificatemanager.googleapis.com",
    "artifactregistry.googleapis.com",
    "dns.googleapis.com",
  ])
  workload_identity = "serviceAccount:${var.project}.svc.id.goog[${var.namespace}/${var.service_account}]"
  db_user           = "silo-hub"
  db_name           = "silo"
  # SILO_HUB_NODE_UI_DOMAIN: node ws-1's UI is at ws-1.<it>
  # (docs/hub.md §11.2). Empty when node_ui_domain is "".
  node_ui_domain = var.node_ui_domain == null ? "nodes.${var.domain}" : var.node_ui_domain
  node_ui        = local.node_ui_domain == "" ? toset([]) : toset([local.node_ui_domain])
}

resource "google_project_service" "apis" {
  for_each           = var.enable_apis ? local.apis : toset([])
  project            = var.project
  service            = each.value
  disable_on_destroy = false
}

# --- The database ---------------------------------------------------

resource "google_sql_database_instance" "hub" {
  name                = var.name
  project             = var.project
  region              = var.region
  database_version    = var.database_version
  deletion_protection = var.deletion_protection

  settings {
    tier              = var.database_tier
    availability_type = "ZONAL"
    ip_configuration {
      # Reached only through the Cloud SQL Auth Proxy, which
      # authorizes with IAM: no authorized networks.
      ipv4_enabled = true
      ssl_mode     = "ENCRYPTED_ONLY"
    }
    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
    }
  }

  depends_on = [google_project_service.apis]
}

resource "google_sql_database" "hub" {
  name     = local.db_name
  project  = var.project
  instance = google_sql_database_instance.hub.name
}

# A built-in user: Cloud SQL makes it a member of cloudsqlsuperuser, so
# it can create the hub's tables, which an IAM database user could not
# without a grant run by hand.
resource "random_password" "db" {
  length  = 32
  special = false
}

resource "google_sql_user" "hub" {
  name     = local.db_user
  project  = var.project
  instance = google_sql_database_instance.hub.name
  password = random_password.db.result
}

# --- The hub's identity ----------------------------------------------

resource "google_service_account" "hub" {
  project      = var.project
  account_id   = var.service_account
  display_name = "Silo hub"
  description  = "The Silo hub's pods (Kubernetes ${var.namespace}/${var.service_account}), through Workload Identity: the Cloud SQL Auth Proxy connects as it."
}

resource "google_project_iam_member" "cloudsql_client" {
  project = var.project
  role    = "roles/cloudsql.client"
  member  = google_service_account.hub.member
}

resource "google_service_account_iam_member" "workload_identity" {
  service_account_id = google_service_account.hub.name
  role               = "roles/iam.workloadIdentityUser"
  member             = local.workload_identity
}

# --- IAP access -----------------------------------------------------------

# IAP's OAuth client is not made here: the IAP OAuth Admin API that
# google_iap_brand and google_iap_client used was shut down in March
# 2026. It is made once in the console (docs/deploy.md), and its ID and
# secret go into the manifests, never into terraform state.

resource "google_iap_web_iam_member" "access" {
  for_each = var.iap_backend_service == "" ? toset(var.iap_members) : toset([])
  project  = var.project
  role     = "roles/iap.httpsResourceAccessor"
  member   = each.value
}

resource "google_iap_web_backend_service_iam_member" "access" {
  for_each            = var.iap_backend_service == "" ? toset([]) : toset(var.iap_members)
  project             = var.project
  web_backend_service = var.iap_backend_service
  role                = "roles/iap.httpsResourceAccessor"
  member              = each.value
}

# --- The address and certificate ---------------------------------------

resource "google_compute_global_address" "hub" {
  name    = var.name
  project = var.project
}

resource "google_certificate_manager_dns_authorization" "hub" {
  name    = var.name
  project = var.project
  domain  = var.domain

  depends_on = [google_project_service.apis]
}

resource "google_certificate_manager_certificate" "hub" {
  name    = var.name
  project = var.project
  managed {
    domains            = [var.domain]
    dns_authorizations = [google_certificate_manager_dns_authorization.hub.id]
  }
}

resource "google_certificate_manager_certificate_map" "hub" {
  name    = var.name
  project = var.project
}

resource "google_certificate_manager_certificate_map_entry" "hub" {
  name         = var.name
  project      = var.project
  map          = google_certificate_manager_certificate_map.hub.name
  hostname     = var.domain
  certificates = [google_certificate_manager_certificate.hub.id]
}

# The nodes' UI hosts, *.<node_ui_domain>. A DNS authorization covers
# its domain and the wildcard one label below it, so the hub's own
# does not cover these and they have their own. Their own certificate,
# too, so turning them on or off leaves the hub's certificate alone.
resource "google_certificate_manager_dns_authorization" "node_ui" {
  for_each = local.node_ui
  name     = "${var.name}-nodes"
  project  = var.project
  domain   = each.value

  depends_on = [google_project_service.apis]
}

resource "google_certificate_manager_certificate" "node_ui" {
  for_each = local.node_ui
  name     = "${var.name}-nodes"
  project  = var.project
  managed {
    domains            = ["*.${each.value}"]
    dns_authorizations = [google_certificate_manager_dns_authorization.node_ui[each.key].id]
  }
}

resource "google_certificate_manager_certificate_map_entry" "node_ui" {
  for_each     = local.node_ui
  name         = "${var.name}-nodes"
  project      = var.project
  map          = google_certificate_manager_certificate_map.hub.name
  hostname     = "*.${each.value}"
  certificates = [google_certificate_manager_certificate.node_ui[each.key].id]
}

# --- DNS, when the zone is in the project --------------------------------

resource "google_dns_record_set" "hub" {
  count        = var.dns_zone == "" ? 0 : 1
  project      = var.project
  managed_zone = var.dns_zone
  name         = "${var.domain}."
  type         = "A"
  ttl          = 300
  rrdatas      = [google_compute_global_address.hub.address]
}

resource "google_dns_record_set" "cert_authorization" {
  count        = var.dns_zone == "" ? 0 : 1
  project      = var.project
  managed_zone = var.dns_zone
  name         = google_certificate_manager_dns_authorization.hub.dns_resource_record[0].name
  type         = google_certificate_manager_dns_authorization.hub.dns_resource_record[0].type
  ttl          = 300
  rrdatas      = [google_certificate_manager_dns_authorization.hub.dns_resource_record[0].data]
}

resource "google_dns_record_set" "node_ui" {
  for_each     = var.dns_zone == "" ? toset([]) : local.node_ui
  project      = var.project
  managed_zone = var.dns_zone
  name         = "*.${each.value}."
  type         = "A"
  ttl          = 300
  rrdatas      = [google_compute_global_address.hub.address]
}

resource "google_dns_record_set" "node_ui_cert_authorization" {
  for_each     = var.dns_zone == "" ? toset([]) : local.node_ui
  project      = var.project
  managed_zone = var.dns_zone
  name         = google_certificate_manager_dns_authorization.node_ui[each.key].dns_resource_record[0].name
  type         = google_certificate_manager_dns_authorization.node_ui[each.key].dns_resource_record[0].type
  ttl          = 300
  rrdatas      = [google_certificate_manager_dns_authorization.node_ui[each.key].dns_resource_record[0].data]
}

# --- The image repository ------------------------------------------------

resource "google_artifact_registry_repository" "silo" {
  count         = var.create_image_repository ? 1 : 0
  project       = var.project
  location      = var.region
  repository_id = "silo"
  format        = "DOCKER"
  description   = "Silo hub images"

  depends_on = [google_project_service.apis]
}
