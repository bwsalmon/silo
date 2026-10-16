# The hub in bsalmon-gke-dev (docs/deploy.md):
#   terraform apply -var-file=bsalmon-gke-dev.tfvars
# The values marked SET are not known here and have to be filled in.
project          = "bsalmon-gke-dev"
region           = "us-central1"
cluster_name     = "SET-cluster-name"
cluster_location = "us-central1"

domain   = "silo-hub.example.com" # SET: a name whose DNS you control
dns_zone = ""                     # or a Cloud DNS zone in the project that holds domain

# Who may pass IAP; the hub's team directory then decides what each
# sees. IAP's OAuth client is made in the console (docs/deploy.md).
iap_members = ["user:bsalmon@google.com"]
