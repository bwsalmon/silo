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

variable "dns_zone" {
  description = "A Cloud DNS managed zone in the project that holds domain. When set, the A record and the certificate's DNS authorization record are made in it; when empty, the dns_records output says what to create."
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
