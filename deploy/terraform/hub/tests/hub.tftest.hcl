# terraform test, against mock providers: nothing is planned against or
# applied to a real project.

mock_provider "google" {
  mock_data "google_project" {
    defaults = {
      number = "123456789"
    }
  }
  mock_data "google_container_cluster" {
    defaults = {
      workload_identity_config = [{ workload_pool = "bsalmon-gke-dev.svc.id.goog" }]
      gateway_api_config       = [{ channel = "CHANNEL_STANDARD" }]
      monitoring_config        = [{ managed_prometheus = [{ enabled = true }] }]
    }
  }
  mock_resource "google_certificate_manager_dns_authorization" {
    defaults = {
      dns_resource_record = [{
        name = "_acme-challenge.silo-hub.example.com."
        type = "CNAME"
        data = "abc.authorize.certificatemanager.goog."
      }]
    }
  }
  mock_resource "google_sql_database_instance" {
    defaults = {
      connection_name = "bsalmon-gke-dev:us-central1:silo-hub"
    }
  }
  mock_resource "google_service_account" {
    defaults = {
      email  = "silo-hub@bsalmon-gke-dev.iam.gserviceaccount.com"
      member = "serviceAccount:silo-hub@bsalmon-gke-dev.iam.gserviceaccount.com"
      name   = "projects/bsalmon-gke-dev/serviceAccounts/silo-hub@bsalmon-gke-dev.iam.gserviceaccount.com"
    }
  }
}

mock_provider "random" {}

variables {
  project          = "bsalmon-gke-dev"
  cluster_name     = "dev"
  cluster_location = "us-central1"
  domain           = "silo-hub.example.com"
  iap_members      = ["user:bsalmon@google.com", "group:silo@example.com"]
}

run "defaults" {
  command = apply

  assert {
    condition     = google_sql_database_instance.hub.settings[0].availability_type == "REGIONAL"
    error_message = "the database has a standby by default"
  }
  assert {
    condition     = google_sql_database_instance.hub.deletion_protection && google_sql_database_instance.hub.settings[0].deletion_protection_enabled
    error_message = "the database is protected from deletion by default"
  }
  assert {
    condition     = google_sql_database_instance.hub.settings[0].disk_autoresize && google_sql_database_instance.hub.settings[0].disk_autoresize_limit == 0
    error_message = "the database's disk grows by itself, without a limit, by default"
  }
  assert {
    condition     = google_sql_database_instance.hub.settings[0].insights_config[0].query_insights_enabled && !google_sql_database_instance.hub.settings[0].insights_config[0].record_client_address
    error_message = "Query Insights is on, without client addresses"
  }
  assert {
    condition     = google_sql_database_instance.hub.settings[0].maintenance_window[0].day == 7 && google_sql_database_instance.hub.settings[0].maintenance_window[0].hour == 5
    error_message = "maintenance is on Sundays at 05:00 UTC by default"
  }
  assert {
    condition     = google_sql_database_instance.hub.settings[0].ip_configuration[0].ssl_mode == "ENCRYPTED_ONLY" && length(google_sql_database_instance.hub.settings[0].ip_configuration[0].authorized_networks) == 0
    error_message = "the database takes only encrypted connections, from no authorized network"
  }
  assert {
    condition     = google_sql_database_instance.hub.settings[0].ip_configuration[0].ipv4_enabled && google_sql_database_instance.hub.settings[0].ip_configuration[0].private_network == null && length(google_service_networking_connection.private_ip) == 0
    error_message = "the database has a public IP alone by default, and no peering is made"
  }
  assert {
    condition     = one([for f in google_sql_database_instance.hub.settings[0].database_flags : f.value if f.name == "cloudsql.iam_authentication"]) == "on"
    error_message = "the hub logs in with IAM database authentication"
  }
  assert {
    condition     = google_sql_database_instance.hub.settings[0].edition == "ENTERPRISE"
    error_message = "the instance is Enterprise, which the db-custom tier needs"
  }
  assert {
    condition     = length(google_project_iam_custom_role.backup) == 0 && length(google_project_iam_member.backup) == 0
    error_message = "no backup operators, no backup role"
  }

  assert {
    condition     = length(google_iap_web_iam_member.access) == 0 && length(google_iap_web_backend_service_iam_member.access) == 0
    error_message = "before the backend service is known, access is granted nowhere"
  }
  assert {
    condition     = google_service_account_iam_member.workload_identity.member == "serviceAccount:bsalmon-gke-dev.svc.id.goog[silo-hub/silo-hub]"
    error_message = "the KSA silo-hub/silo-hub is bound to the GSA"
  }
  assert {
    condition     = google_project_iam_member.cloudsql_client.role == "roles/cloudsql.client"
    error_message = "the GSA can connect to Cloud SQL"
  }
  assert {
    condition     = strcontains(google_project_iam_member.cloudsql_client.condition[0].expression, "resource.name == \"projects/bsalmon-gke-dev/instances/silo-hub\"")
    error_message = "the GSA can connect to the hub's instance alone"
  }
  assert {
    condition     = google_sql_database_instance.hub.database_version == "POSTGRES_16" && google_sql_database.hub.name == "silo"
    error_message = "the database is PostgreSQL"
  }
  assert {
    condition     = output.database_url == "postgres://silo-hub%40bsalmon-gke-dev.iam@127.0.0.1:5432/silo?sslmode=disable"
    error_message = "the hub serves as its IAM database user, with no password, through the proxy's IAM port"
  }
  assert {
    condition     = startswith(nonsensitive(output.migrate_database_url), "postgres://silo-hub:") && endswith(nonsensitive(output.migrate_database_url), "@127.0.0.1:5433/silo?sslmode=disable")
    error_message = "migrations run as the built-in user, through the proxy's password port"
  }
  assert {
    condition     = google_sql_user.runtime.type == "CLOUD_IAM_SERVICE_ACCOUNT" && google_sql_user.runtime.name == "silo-hub@bsalmon-gke-dev.iam" && output.runtime_role == google_sql_user.runtime.name
    error_message = "the runtime role is the GSA's IAM database user"
  }
  assert {
    condition     = one([for f in google_sql_database_instance.hub.settings[0].database_flags : f.value if f.name == "cloudsql.iam_authentication"]) == "on"
    error_message = "the instance takes IAM database logins"
  }
  assert {
    condition     = google_project_iam_member.cloudsql_instance_user.role == "roles/cloudsql.instanceUser" && strcontains(google_project_iam_member.cloudsql_instance_user.condition[0].expression, "resource.name == \"projects/bsalmon-gke-dev/instances/silo-hub\"")
    error_message = "the GSA can log in to the hub's instance alone"
  }
  assert {
    condition     = output.iap_audience_prefix == "/projects/123456789/global/backendServices/"
    error_message = "the audience is the project number's"
  }
  assert {
    condition     = length(google_dns_record_set.hub) == 0 && length(google_dns_record_set.node_ui) == 0 && length(output.dns_records) == 4
    error_message = "with no zone, the records (the hub's and the node UI hosts') are output rather than made"
  }
  assert {
    condition     = contains([for r in output.dns_records : r.name], "*.nodes.silo-hub.example.com.")
    error_message = "the records include the node UI hosts' wildcard A record"
  }
  assert {
    condition     = google_certificate_manager_certificate_map_entry.hub.hostname == "silo-hub.example.com"
    error_message = "the certificate map serves the domain"
  }
  assert {
    condition     = output.node_ui_domain == "nodes.silo-hub.example.com"
    error_message = "the node UI domain defaults to nodes.<domain>"
  }
  assert {
    condition     = google_certificate_manager_dns_authorization.node_ui["nodes.silo-hub.example.com"].domain == "nodes.silo-hub.example.com"
    error_message = "the node UI domain has its own DNS authorization, which covers its wildcard"
  }
  assert {
    condition     = one(google_certificate_manager_certificate.node_ui["nodes.silo-hub.example.com"].managed[0].domains) == "*.nodes.silo-hub.example.com"
    error_message = "the node UI hosts' certificate is the wildcard"
  }
  assert {
    condition     = google_certificate_manager_certificate_map_entry.node_ui["nodes.silo-hub.example.com"].hostname == "*.nodes.silo-hub.example.com" && google_certificate_manager_certificate_map_entry.node_ui["nodes.silo-hub.example.com"].map == google_certificate_manager_certificate_map.hub.name
    error_message = "the hub's certificate map serves the node UI hosts"
  }
  assert {
    condition     = google_certificate_manager_certificate.hub.managed[0].domains == tolist(["silo-hub.example.com"])
    error_message = "the hub's own certificate is unchanged"
  }
  assert {
    condition = (
      google_sql_database_instance.hub.settings[0].backup_configuration[0].enabled &&
      google_sql_database_instance.hub.settings[0].backup_configuration[0].point_in_time_recovery_enabled &&
      google_sql_database_instance.hub.settings[0].backup_configuration[0].transaction_log_retention_days == 7 &&
      google_sql_database_instance.hub.settings[0].backup_configuration[0].start_time == "03:00" &&
      google_sql_database_instance.hub.settings[0].backup_configuration[0].backup_retention_settings[0].retained_backups == 14
    )
    error_message = "daily backups, 14 kept, and 7 days of point-in-time recovery"
  }
  assert {
    condition     = google_sql_database_instance.hub.settings[0].deletion_protection_enabled
    error_message = "Cloud SQL's own deletion protection is on"
  }
  assert {
    condition     = toset(keys(google_monitoring_alert_policy.hub)) == toset(["hub_down", "hub_pod_down", "no_online_nodes", "dispatch_stuck", "error_rate", "no_dispatcher", "database_errors"])
    error_message = "the seven alert policies"
  }
  assert {
    condition = (
      strcontains(google_monitoring_alert_policy.hub["hub_down"].conditions[0].condition_prometheus_query_language[0].query, "sum(up{namespace=\"silo-hub\",job=\"silo-hub\"}) == 0") &&
      strcontains(google_monitoring_alert_policy.hub["hub_down"].conditions[0].condition_prometheus_query_language[0].query, "max(silo_hub_ready{namespace=\"silo-hub\",job=\"silo-hub\"}) == 0") &&
      google_monitoring_alert_policy.hub["hub_down"].severity == "CRITICAL"
    )
    error_message = "the hub is down when no pod is up or ready"
  }
  assert {
    condition = (
      google_monitoring_alert_policy.hub["hub_pod_down"].conditions[0].condition_prometheus_query_language[0].query == "count(silo_hub_ready{namespace=\"silo-hub\",job=\"silo-hub\"} == 1) < 2" &&
      google_monitoring_alert_policy.hub["hub_pod_down"].severity == "WARNING"
    )
    error_message = "a pod down while others serve is a warning, against the two replicas"
  }
  assert {
    condition = (
      strcontains(google_monitoring_alert_policy.hub["no_dispatcher"].conditions[0].condition_prometheus_query_language[0].query, "sum(silo_hub_dispatcher_leader{namespace=\"silo-hub\",job=\"silo-hub\"}) == 0") &&
      google_monitoring_alert_policy.hub["no_dispatcher"].severity == "CRITICAL"
    )
    error_message = "no pod dispatching is an alert of its own"
  }
  assert {
    condition     = alltrue([for p in google_monitoring_alert_policy.hub : strcontains(p.conditions[0].condition_prometheus_query_language[0].query, "namespace=\"silo-hub\",job=\"silo-hub\"")])
    error_message = "every alert selects the hub's scrape target"
  }
  assert {
    condition     = length(jsondecode(google_monitoring_dashboard.hub[0].dashboard_json).mosaicLayout.tiles) == 18
    error_message = "the dashboard is JSON, with its tiles"
  }
  assert {
    condition     = jsondecode(google_monitoring_dashboard.hub[0].dashboard_json).mosaicLayout.tiles[0].widget.scorecard.timeSeriesQuery.prometheusQuery == "sum(silo_hub_ready{namespace=\"silo-hub\",job=\"silo-hub\"})"
    error_message = "the dashboard's queries select the hub's scrape target, and it counts the ready pods"
  }
  assert {
    condition     = jsondecode(google_monitoring_dashboard.hub[0].dashboard_json).mosaicLayout.tiles[0].widget.scorecard.thresholds[1].value == 1.5
    error_message = "the ready pods' scorecard warns below the replicas"
  }
}

run "monitoring_off" {
  command = plan

  variables {
    monitoring                  = false
    alert_notification_channels = ["projects/bsalmon-gke-dev/notificationChannels/1"]
  }

  assert {
    condition     = length(google_monitoring_alert_policy.hub) == 0 && length(google_monitoring_dashboard.hub) == 0
    error_message = "monitoring = false makes no policies or dashboard"
  }
}

run "notification_channels" {
  command = plan

  variables {
    alert_notification_channels = ["projects/bsalmon-gke-dev/notificationChannels/1"]
  }

  assert {
    condition     = alltrue([for p in google_monitoring_alert_policy.hub : toset(p.notification_channels) == toset(["projects/bsalmon-gke-dev/notificationChannels/1"])])
    error_message = "every policy notifies the channels"
  }
}

run "bad_backup_settings" {
  command = plan

  variables {
    backup_start_time           = "3am"
    point_in_time_recovery_days = 30
  }

  expect_failures = [var.backup_start_time, var.point_in_time_recovery_days]
}

run "iap_project_wide" {
  command = apply

  variables {
    iap_project_wide = true
  }

  assert {
    condition     = length(google_iap_web_iam_member.access) == 2 && length(google_iap_web_backend_service_iam_member.access) == 0
    error_message = "asked for, access is granted project-wide until the backend service is known"
  }
}

run "dns_and_backend_service" {
  command = apply

  variables {
    dns_zone            = "example-com"
    iap_backend_service = "gkegw1-abcd-silo-hub-silo-hub-80-xyz"
  }

  assert {
    condition     = google_dns_record_set.hub[0].name == "silo-hub.example.com." && google_dns_record_set.hub[0].type == "A"
    error_message = "the A record is made in the zone"
  }
  assert {
    condition     = google_dns_record_set.cert_authorization[0].type == "CNAME"
    error_message = "the certificate's authorization record is made in the zone"
  }
  assert {
    condition     = google_dns_record_set.node_ui["nodes.silo-hub.example.com"].name == "*.nodes.silo-hub.example.com." && google_dns_record_set.node_ui["nodes.silo-hub.example.com"].type == "A"
    error_message = "the node UI hosts' wildcard A record is made in the zone"
  }
  assert {
    condition     = length(google_dns_record_set.node_ui_cert_authorization) == 1
    error_message = "the node UI certificate's authorization record is made in the zone"
  }
  assert {
    condition     = length(google_iap_web_iam_member.access) == 0 && length(google_iap_web_backend_service_iam_member.access) == 2
    error_message = "once the backend service is known, access is granted on it alone"
  }
}

run "node_ui_domain" {
  command = apply

  variables {
    dns_zone       = "example-com"
    node_ui_domain = "silo-nodes.example.com"
  }

  assert {
    condition     = output.node_ui_domain == "silo-nodes.example.com" && google_certificate_manager_certificate_map_entry.node_ui["silo-nodes.example.com"].hostname == "*.silo-nodes.example.com"
    error_message = "node_ui_domain names the node UI hosts' domain"
  }
  assert {
    condition     = google_dns_record_set.node_ui["silo-nodes.example.com"].name == "*.silo-nodes.example.com."
    error_message = "the wildcard A record follows node_ui_domain"
  }
}

run "no_node_ui" {
  command = apply

  variables {
    dns_zone       = "example-com"
    node_ui_domain = ""
  }

  assert {
    condition     = output.node_ui_domain == "" && length(google_certificate_manager_certificate.node_ui) == 0 && length(google_certificate_manager_certificate_map_entry.node_ui) == 0 && length(google_dns_record_set.node_ui) == 0 && length(output.dns_records) == 2
    error_message = "an empty node_ui_domain makes no node UI hosts"
  }
}

run "bad_node_ui_domain" {
  command = plan

  variables {
    node_ui_domain = "*.nodes.example.com"
  }

  expect_failures = [var.node_ui_domain]
}

run "bad_domain" {
  command = plan

  variables {
    domain = "Not A Domain"
  }

  expect_failures = [var.domain]
}

run "zonal_database" {
  command = plan

  variables {
    database_availability_type = "ZONAL"
  }

  assert {
    condition     = google_sql_database_instance.hub.settings[0].availability_type == "ZONAL"
    error_message = "database_availability_type = ZONAL leaves the instance without a standby"
  }
}

run "three_replicas" {
  command = plan

  variables {
    hub_replicas = 3
  }

  assert {
    condition     = endswith(google_monitoring_alert_policy.hub["hub_pod_down"].conditions[0].condition_prometheus_query_language[0].query, "< 3")
    error_message = "the pod-down warning follows hub_replicas"
  }
}

run "bad_replicas" {
  command = plan

  variables {
    hub_replicas = 0
  }

  expect_failures = [var.hub_replicas]
}

run "bad_availability_type" {
  command = plan

  variables {
    database_availability_type = "MULTI_REGION"
  }

  expect_failures = [var.database_availability_type]
}

run "disk_limit_and_maintenance" {
  command = apply

  variables {
    database_disk_size_gb             = 20
    database_disk_autoresize_limit_gb = 200
    maintenance_window_day            = 2
    maintenance_window_hour           = 9
    maintenance_update_track          = "canary"
    database_query_insights           = false
  }

  assert {
    condition     = google_sql_database_instance.hub.settings[0].disk_autoresize_limit == 200
    error_message = "the disk grows to database_disk_autoresize_limit_gb at most"
  }
  # The instance was made by an earlier run, at 10 GB: terraform does
  # not change the size of a disk autoresize may have grown.
  assert {
    condition     = google_sql_database_instance.hub.settings[0].disk_size == 10
    error_message = "terraform leaves the size of an existing instance's disk alone"
  }
  assert {
    condition     = google_sql_database_instance.hub.settings[0].maintenance_window[0].day == 2 && google_sql_database_instance.hub.settings[0].maintenance_window[0].hour == 9 && google_sql_database_instance.hub.settings[0].maintenance_window[0].update_track == "canary"
    error_message = "the maintenance window follows its variables"
  }
  assert {
    condition     = !google_sql_database_instance.hub.settings[0].insights_config[0].query_insights_enabled
    error_message = "database_query_insights = false turns Query Insights off"
  }
}

run "disk_limit_below_size" {
  command = plan

  variables {
    database_disk_size_gb             = 50
    database_disk_autoresize_limit_gb = 20
  }

  expect_failures = [google_sql_database_instance.hub]
}

run "bad_maintenance_window" {
  command = plan

  variables {
    maintenance_window_day   = 0
    maintenance_window_hour  = 24
    maintenance_update_track = "preview"
  }

  expect_failures = [var.maintenance_window_day, var.maintenance_window_hour, var.maintenance_update_track]
}

run "private_ip" {
  command = apply

  variables {
    database_private_ip      = true
    database_public_ip       = false
    database_private_network = "projects/bsalmon-gke-dev/global/networks/default"
  }

  assert {
    condition     = google_sql_database_instance.hub.settings[0].ip_configuration[0].private_network == "projects/bsalmon-gke-dev/global/networks/default" && !google_sql_database_instance.hub.settings[0].ip_configuration[0].ipv4_enabled
    error_message = "database_private_ip puts the instance on the VPC, and database_public_ip = false takes its public IP away"
  }
  assert {
    condition     = google_service_networking_connection.private_ip[0].network == "projects/bsalmon-gke-dev/global/networks/default" && google_compute_global_address.private_ip[0].purpose == "VPC_PEERING"
    error_message = "the VPC's private services access is made"
  }
}

run "private_ip_existing_peering" {
  command = plan

  variables {
    database_private_ip         = true
    database_private_ip_peering = false
    database_private_network    = "projects/bsalmon-gke-dev/global/networks/default"
  }

  assert {
    condition     = length(google_service_networking_connection.private_ip) == 0 && length(google_compute_global_address.private_ip) == 0
    error_message = "database_private_ip_peering = false uses the VPC's peering as it is"
  }
}

run "no_address" {
  command = plan

  variables {
    database_public_ip = false
  }

  expect_failures = [google_sql_database_instance.hub]
}

run "backup_operators" {
  command = apply

  variables {
    backup_operators = ["group:silo-oncall@example.com"]
  }

  assert {
    condition     = toset(google_project_iam_custom_role.backup[0].permissions) == toset(["cloudsql.backupRuns.create", "cloudsql.backupRuns.get", "cloudsql.backupRuns.list", "cloudsql.instances.get"])
    error_message = "the backup role may take, list and read backups, and nothing else"
  }
  assert {
    condition     = google_project_iam_member.backup["group:silo-oncall@example.com"].role == google_project_iam_custom_role.backup[0].name
    error_message = "backup_operators are granted the backup role"
  }
  assert {
    condition     = strcontains(google_project_iam_member.backup["group:silo-oncall@example.com"].condition[0].expression, "projects/bsalmon-gke-dev/instances/silo-hub")
    error_message = "the backup role is granted on the hub's instance alone"
  }
}
