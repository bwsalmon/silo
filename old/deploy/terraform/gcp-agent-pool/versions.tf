terraform {
  required_version = ">= 1.5.0"

  required_providers {
    google = {
      source = "hashicorp/google"
      # The same major version as grain's deploy/terraform/gcp.
      version = ">= 6.0, < 8.0"
    }
  }

  # Local state unless the operator configures a backend with
  #   terraform init -backend-config=...
  # after adding a backend block here.
}

provider "google" {
  project = var.project_id
}
