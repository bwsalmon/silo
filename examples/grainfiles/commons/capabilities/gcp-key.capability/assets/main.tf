# Provisions the service accounts a deployment hands out under gcp-key.
# Never read by grainfile readers: it is this capability's attachment.
variable "project" {
  type = string
}

variable "pool_size" {
  type    = number
  default = 10
}

resource "google_service_account" "agent" {
  count        = var.pool_size
  project      = var.project
  account_id   = "grain-sb-agent-${count.index}"
  display_name = "grain sandbox agent ${count.index}"
}
