variable "project" {
  description = "The GCP project the hub runs in."
  type        = string
}

variable "region" {
  description = "The region of the Cloud SQL instance and the image repository."
  type        = string
  default     = "us-central1"
}

variable "cluster_name" {
  description = "The existing GKE cluster the hub runs on. It is not created here: it needs Workload Identity and the Gateway API enabled (docs/deploy.md)."
  type        = string
}

variable "cluster_location" {
  description = "The cluster's region or zone."
  type        = string
}

variable "namespace" {
  description = "The Kubernetes namespace of the hub (deploy/kubernetes/hub)."
  type        = string
  default     = "silo-hub"
}

variable "service_account" {
  description = "The Kubernetes service account the hub runs as, and the account ID of the Google service account it is bound to."
  type        = string
  default     = "silo-hub"
}

variable "name" {
  description = "The name of the Cloud SQL instance, the static IP and the certificate map; the manifests name the last two silo-hub."
  type        = string
  default     = "silo-hub"
}

variable "domain" {
  description = "The hub's host name, hub.example.com: the certificate is for it and the HTTPRoute matches it."
  type        = string
  validation {
    condition     = can(regex("^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$", var.domain))
    error_message = "domain must be a lower-case host name."
  }
}

variable "node_ui_domain" {
  description = "The domain the nodes' UIs are served under, each at <node>.<it> (SILO_HUB_NODE_UI_DOMAIN, docs/hub.md §11.2): it gets a *.<it> certificate, certificate map entry and, with dns_zone, a wildcard A record. Unset, nodes.<domain>; \"\" makes none."
  type        = string
  default     = null
  validation {
    condition     = var.node_ui_domain == null || var.node_ui_domain == "" || can(regex("^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$", var.node_ui_domain))
    error_message = "node_ui_domain must be a lower-case host name, or empty."
  }
}

variable "dns_zone" {
  description = "A Cloud DNS managed zone in the project that holds domain. When set, the A records and the certificates' DNS authorization records are made in it; when empty, the dns_records output says what to create."
  type        = string
  default     = ""
}

variable "database_tier" {
  description = "The Cloud SQL machine tier. Shared-core tiers allow too few connections for the hub's three stores."
  type        = string
  default     = "db-custom-1-3840"
}

variable "database_version" {
  description = "The Cloud SQL PostgreSQL version."
  type        = string
  default     = "POSTGRES_16"
}

variable "backup_start_time" {
  description = "When, in UTC (HH:MM), the daily automated backup of the database starts."
  type        = string
  default     = "03:00"
  validation {
    condition     = can(regex("^([01][0-9]|2[0-3]):[0-5][0-9]$", var.backup_start_time))
    error_message = "backup_start_time must be HH:MM in UTC."
  }
}

variable "backups_retained" {
  description = "How many daily automated backups to keep."
  type        = number
  default     = 14
  validation {
    condition     = var.backups_retained >= 7 && var.backups_retained <= 365
    error_message = "backups_retained must be from 7 to 365."
  }
}

variable "point_in_time_recovery_days" {
  description = "How many days of write-ahead log to keep for point-in-time recovery; at most backups_retained, and at most 7 (Cloud SQL's limit for this edition)."
  type        = number
  default     = 7
  validation {
    condition     = var.point_in_time_recovery_days >= 1 && var.point_in_time_recovery_days <= 7
    error_message = "point_in_time_recovery_days must be from 1 to 7."
  }
}

variable "backup_location" {
  description = "Where backups are stored, a region or multi-region (\"us\"); null keeps them in the multi-region nearest the instance, Cloud SQL's default."
  type        = string
  default     = null
}

variable "alert_notification_channels" {
  description = "Cloud Monitoring notification channels (projects/PROJECT/notificationChannels/ID) the hub's alert policies notify. With none, the policies still open incidents in the console."
  type        = list(string)
  default     = []
}

variable "monitoring" {
  description = "Whether to create the hub's alert policies and dashboard (monitoring.tf). They need Managed Service for Prometheus on the cluster and the PodMonitoring in deploy/kubernetes/hub."
  type        = bool
  default     = true
}

variable "deletion_protection" {
  description = "Whether the Cloud SQL instance is protected from terraform destroy."
  type        = bool
  default     = true
}

variable "iap_members" {
  description = "Who may pass IAP to the hub's UI and API (roles/iap.httpsResourceAccessor): user:, group: or domain: members. The hub's team directory then decides what each may see."
  type        = list(string)
  default     = []
}

variable "iap_backend_service" {
  description = "The name of the IAP-enabled backend service the Gateway made for the silo-hub Service. When set, iap_members are granted on it alone; until then (it exists only after the first kubectl apply), on every IAP web resource in the project."
  type        = string
  default     = ""
}

variable "create_image_repository" {
  description = "Whether to create the Artifact Registry repository the image is pushed to."
  type        = bool
  default     = true
}

variable "enable_apis" {
  description = "Whether to enable the APIs the hub needs in the project."
  type        = bool
  default     = true
}
