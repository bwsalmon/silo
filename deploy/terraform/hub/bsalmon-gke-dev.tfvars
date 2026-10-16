# The hub in bsalmon-gke-dev (docs/deploy.md):
#   terraform apply -var-file=bsalmon-gke-dev.tfvars
# The values marked SET are not known here and have to be filled in.
project          = "bsalmon-gke-dev"
region           = "us-central1"
cluster_name     = "SET-cluster-name"
cluster_location = "us-central1"

domain   = "silo-hub.example.com" # SET: a name whose DNS you control
dns_zone = ""                     # or a Cloud DNS zone in the project that holds domain

# The nodes' UI hosts are *.nodes.<domain> unless node_ui_domain says
# otherwise; "" makes none (docs/deploy.md §8).

# Who may pass IAP; the hub's team directory then decides what each
# sees. IAP's OAuth client is made in the console (docs/deploy.md).
iap_members = ["user:bsalmon@google.com"]

# The database has a standby in another zone by default (REGIONAL,
# docs/hub.md §2.2). A development hub that can take a restore by hand
# after a zone's failure can have none, at about half the cost:
# database_availability_type = "ZONAL"
