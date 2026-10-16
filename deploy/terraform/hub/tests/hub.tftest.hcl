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
    condition     = length(google_iap_web_iam_member.access) == 2 && length(google_iap_web_backend_service_iam_member.access) == 0
    error_message = "before the backend service is known, access is granted project-wide"
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
    condition     = google_sql_database_instance.hub.database_version == "POSTGRES_16" && google_sql_database.hub.name == "silo"
    error_message = "the database is PostgreSQL"
  }
  assert {
    condition     = startswith(nonsensitive(output.database_url), "postgres://silo-hub:") && endswith(nonsensitive(output.database_url), "@127.0.0.1:5432/silo?sslmode=disable")
    error_message = "the database URL goes through the proxy"
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
