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

# The database: PostgreSQL on Cloud SQL, the hub's only production
# store (docs/deploy.md §1). Each setting below is shown at its default.
#
# A standby in another zone with automatic failover (REGIONAL,
# docs/hub.md §2.2), billed as a second instance and disk: about twice
# a ZONAL instance's cost. A development hub that can take a restore by
# hand after a zone's failure can have none:
# database_availability_type = "ZONAL"
database_availability_type = "REGIONAL"

# Protected from deletion, by terraform and by Cloud SQL. Unset only to
# throw a development hub away (its automated backups go with it).
deletion_protection = true

# The disk starts at 10 GB of SSD and grows by itself before it fills;
# a limit, in GB, bounds what it can grow to (0: none).
database_disk_size_gb             = 10
database_disk_autoresize          = true
database_disk_autoresize_limit_gb = 0

# Daily backups at 03:00 UTC, 14 kept, and point-in-time recovery over
# the last 7 days (docs/hub-operations.md).
backup_start_time           = "03:00"
backups_retained            = 14
point_in_time_recovery_days = 7

# Who may take the on-demand backup before each schema migration
# (docs/deploy.md §6), with a custom role of exactly that:
# backup_operators = ["group:silo-oncall@example.com"]

# Query Insights, and Cloud SQL's maintenance on Sundays at 05:00 UTC,
# on the stable track ("canary" takes updates early: a development hub).
database_query_insights  = true
maintenance_window_day   = 7
maintenance_window_hour  = 5
maintenance_update_track = "stable"

# TLS is always required (ssl_mode ENCRYPTED_ONLY) and the hub logs in
# as its service account's IAM database user, with no password; only
# `silo hub migrate` uses the built-in user's. The instance has a
# public IP with no authorized networks, which only the Cloud SQL Auth
# Proxy reaches, authorized by IAM. For a private IP on the cluster's
# VPC as well (docs/deploy.md §2), set database_private_ip; where the
# VPC already has private services access, also set
# database_private_ip_peering = false, since making it again replaces
# the ranges other services use:
# database_private_ip         = true
# database_private_ip_peering = false
