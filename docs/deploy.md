# Deploying the hub on GKE behind IAP

This is the runbook for running `silo hub serve` (hub.md §2.1) in GCP
project **`bsalmon-gke-dev`**, on a GKE cluster that already exists,
behind Google Cloud IAP. Nothing here has been applied to that project:
this repository's CI only builds the image, validates the manifests and
runs the terraform's tests against mock providers
(`.grain/workflows/deploy.yml`).

The pieces:

| Path | What it is |
| --- | --- |
| `Dockerfile` | The hub's image: a static `silo` binary on distroless. |
| `deploy/terraform/hub` | The project's side: Cloud SQL (PostgreSQL) with its two database users (below), the hub's Google service account and its Workload Identity binding, IAP access, a static IP, Certificate Manager certificates (the hub's host and the nodes' UI hosts, `*.nodes.<domain>`) and their map, DNS records (optional), an Artifact Registry repository, and Cloud Monitoring alert policies and a dashboard over the hub's metrics (`monitoring.tf`). `bsalmon-gke-dev.tfvars` is the example for this project. |
| `deploy/kubernetes/hub` | The cluster's side, with kustomize: `base/`, the `bsalmon-gke-dev/` production overlay, and the `kind/` local/CI overlay. Two hub replicas with the Cloud SQL Auth Proxy beside each pod (or in-cluster PostgreSQL in `kind/`) and a `migrate` init container, two Services, the Gateway and its HTTPRoutes (the hub's host, and the nodes' UI hosts), the GKE policies (IAP, timeouts, health checks), and a `PodMonitoring` that scrapes the hub's metrics. `validate.sh` validates the manifests and policy invariants on a kind cluster; `e2e-kind.sh` boots the live 2-replica `kind/` overlay and runs the end-to-end deployment test (§9). |

Traffic, once deployed:

```
humans, SDK ──HTTPS──> Gateway ─ PathPrefix / ───────────> Service silo-hub        (IAP on, 1h timeout)  ─┐
grain nodes ──WSS───>  (static IP,  Exact /v1/nodes/connect > Service silo-hub-nodes (IAP off, 24h timeout) ─┴─> the hub pod :8080
                        managed cert)                                                         └─ cloud-sql-proxy ─> Cloud SQL
```

The node path is outside IAP and the hub checks each node's token
itself, as are `/v1/claims/keys` (the public keys nodes check the hub's
task claims with, hub.md §6.4), `/v1/webhooks/github` (verified by
`X-Hub-Signature-256` HMAC), and `/v1/grainfiles/content` (authenticated
by a node's `Bearer silo-node_...` token), which all go to the
`silo-hub-nodes` Service as exact paths; every other path is behind IAP,
and the hub checks IAP's signed assertion too (hub.md §5.2.1). The
nodes' UI hosts (`<node>.nodes.<domain>`, §8) have a route of their
own, every path on them to the `silo-hub` Service and so behind IAP.

The store is **PostgreSQL on Cloud SQL**, in every deployment, one
replica or several: Cloud SQL's managed backups, point-in-time
recovery, on-demand backups before each schema migration and storage
that grows by itself are how the hub's data is kept
([hub-operations.md](hub-operations.md) §4). SQLite is for local
development and tests only: `silo hub serve` refuses a `sqlite:`
database unless `-dev` (or the development user header) says it is
local development, and on Kubernetes refuses it whatever is set.

## 1. Prerequisites

- **The cluster.** A GKE cluster in `bsalmon-gke-dev` with:
  - **Workload Identity** (workload pool `bsalmon-gke-dev.svc.id.goog`;
    Autopilot has it always), and on a Standard cluster a node pool
    with the GKE metadata server.
  - **The Gateway API** (`--gateway-api=standard`), which brings the
    `gke-l7-global-external-managed` GatewayClass and the
    `GCPBackendPolicy` and `HealthCheckPolicy` CRDs.
  - GKE 1.29 or later, for the proxy as a native sidecar
    (`restartPolicy: Always` in `initContainers`).
  - **Managed Service for Prometheus**'s managed collection (on by
    default since GKE 1.27; `--enable-managed-prometheus` otherwise),
    for the `PodMonitoring` and the alerts (hub-operations.md §5).

  The terraform reads the cluster and warns (it does not fail) when
  the first two are missing.
- **Tools:** `gcloud`, `terraform` 1.7 or later, `kubectl` (its
  built-in kustomize), and `docker`.
- **Roles** for whoever applies: enough to create Cloud SQL, service
  accounts and their IAM, IAP IAM, compute addresses, Certificate
  Manager and Artifact Registry resources (Owner of the dev project
  will do), and to apply to the cluster.
- **A host name** whose DNS you control, for example
  `silo-hub.<your domain>`. If its zone is a Cloud DNS zone in the
  project, terraform makes the records; otherwise it prints them.
- **IAP's OAuth client.** Google shut down the API that terraform's
  `google_iap_brand` and `google_iap_client` used (March 2026), so it is
  made by hand, once:
  1. Console → *Google Auth Platform* in `bsalmon-gke-dev`: configure
     the consent screen (branding) if the project has none, audience
     *Internal*.
  2. *Clients* → *Create client* → *Web application*, named
     `silo-hub`. Create it, then add the authorized redirect URI
     `https://iap.googleapis.com/v1/oauth/clientIds/<CLIENT_ID>:handleRedirect`.
  3. Keep the client ID and secret for step 3.

### The database's two roles

The hub reaches PostgreSQL as two roles (security audit 1, SA1-L8):

| Role | Who it is | What it may do | Used by |
| --- | --- | --- | --- |
| `silo-hub` | a Cloud SQL built-in user, with a password (terraform's `random_password.db`) | own the schema: create and change the hub's tables. Cloud SQL makes it a member of `cloudsqlsuperuser`. | the `migrate` init container alone: `silo hub migrate`, through the proxy's port 5433 |
| `silo-hub@bsalmon-gke-dev.iam` | the hub's Google service account's IAM database user; no password, the proxy logs it in with the pod's Workload Identity | `SELECT`, `INSERT`, `UPDATE` and `DELETE` on the hub's tables, and nothing else: no DDL, no other schema's tables | the hub (`silo hub serve -migrate=false`), and `silo hub team` and `token` run in its container, through the proxy's port 5432 |

Each time the pod starts, `silo hub migrate` runs as `silo-hub`: it
creates any table this version adds, then grants the runtime role
(`-runtime-role`, the config's `SILO_HUB_RUNTIME_ROLE`) its privileges
on every table, and by default on every table `silo-hub` creates
later. It stops the pod from starting if the runtime role could change
the schema (a `cloudsqlsuperuser` member, one that may create roles,
databases or objects in the schema, or one that owns a table). The
hub then opens its tables without creating anything, and says to run
`silo hub migrate` if one is missing.

Locally, none of this is needed: `silo hub serve -database
postgres://...` (or, for development and tests only, `-dev -database
sqlite:hub.db`) creates its own tables as before, since `-migrate`
defaults to true.

## 2. Terraform

```sh
cd deploy/terraform/hub
# Edit bsalmon-gke-dev.tfvars: cluster_name, cluster_location, domain,
# dns_zone (or leave it empty), iap_members.
terraform init
terraform apply -var-file=bsalmon-gke-dev.tfvars
```

State: the module declares no backend, so state is local. For anything
longer-lived than a trial, add a `backend "gcs"` block for a bucket in
the project first.

The Cloud SQL instance it makes, each setting a variable shown at its
default in `bsalmon-gke-dev.tfvars` ([hub-operations.md](hub-operations.md)
§4.1 has the full table):

- **Availability** `REGIONAL`: a standby in another zone and automatic
  failover in about a minute, billed as a second instance and disk, so
  about twice a `ZONAL` instance's cost. A development hub may set
  `database_availability_type = "ZONAL"`.
- **Storage** 10 GB of SSD that grows by itself before it fills
  (`database_disk_autoresize`), without a limit unless
  `database_disk_autoresize_limit_gb` sets one; terraform leaves the
  size it has grown to alone.
- **Backups** daily, 14 kept, and point-in-time recovery over 7 days;
  **deletion protection**, terraform's and Cloud SQL's, on.
- **Query Insights** on (no client addresses or application tags), and
  a **maintenance window** of Sunday 05:00 UTC on the `stable` track.
- **Connections** TLS only (`ssl_mode = ENCRYPTED_ONLY`), and no
  authorized networks: only the Cloud SQL Auth Proxy, authorized by
  IAM, reaches the instance. The hub logs in with **IAM database
  authentication** as its service account (no password); only the
  `migrate` container uses the schema owner's password (§1, "The
  database's two roles"). Between the hub and the proxy, on the pod's
  loopback, the URL's `sslmode=disable` is right: the proxy carries
  the connection on to Cloud SQL over TLS.
- **Private IP**, off by default. With `database_private_ip = true`
  the instance also gets an address on the cluster's VPC, through
  private services access, and terraform makes that access (a reserved
  /20 peered to `servicenetworking.googleapis.com`). It is off because
  a VPC has one such peering, shared by every Google service on it,
  and making it replaces the ranges it already has; where the VPC has
  it already, also set `database_private_ip_peering = false`. To use
  the private address alone, set `database_public_ip = false` and add
  `--private-ip` to the proxy's arguments in the overlay. The public
  address is not open: it has no authorized networks, and the proxy
  authorizes every connection with IAM.
- **Backup operators** (`backup_operators`): who may take the
  on-demand backup before an upgrade (§7) without broader rights: a
  custom role of `cloudsql.backupRuns.create`, `.get`, `.list` and
  `cloudsql.instances.get`, on the hub's instance alone. Restoring
  needs `cloudsql.instances.restoreBackup` (`roles/cloudsql.admin`),
  which it does not grant.

Then, unless `dns_zone` was set, create the records
`terraform output dns_records` lists: for the hub's host and for the
nodes' UI hosts (§8) each, an A record and the CNAME that lets
Certificate Manager issue its certificate. Each certificate turns
`ACTIVE` some minutes after its CNAME resolves:

```sh
gcloud certificate-manager certificates describe silo-hub --project bsalmon-gke-dev
gcloud certificate-manager certificates describe silo-hub-nodes --project bsalmon-gke-dev
```

## 3. The image

```sh
REPO=$(terraform -chdir=deploy/terraform/hub output -raw image_repository)
TAG=$(git rev-parse --short HEAD)
gcloud auth configure-docker us-central1-docker.pkg.dev
docker build -t $REPO:$TAG . && docker push $REPO:$TAG
```

## 4. The manifests

Fill in the overlay, `deploy/kubernetes/hub/bsalmon-gke-dev/kustomization.yaml`
(the values marked `SET`):

- `newTag`: the tag pushed in step 3.
- `CLOUD_SQL_INSTANCE`: `terraform output -raw sql_connection_name`.
- `SILO_HUB_RUNTIME_ROLE`: `terraform output -raw runtime_role`.
- The HTTPRoute's host name: the `domain` given to terraform.
- The HTTPRoute `silo-hub-node-ui`'s host name: `*.` and
  `terraform output -raw node_ui_domain`. `SILO_HUB_NODE_UI_DOMAIN`
  stays empty until the nodes' UIs are turned on (§8).
- The `GCPBackendPolicy` `silo-hub`'s `clientID`: the OAuth client's ID.
- `SILO_HUB_ADMINS`: the hub admins, comma-separated, each
  `user:<email>` or `group:<name>` (already `user:bsalmon@google.com`).
  An entry with no prefix is taken as `user:` and logged as a warning
  at start-up; a group must be written `group:`, even one named like an
  email (hub.md §5.3).
- `SILO_HUB_GITHUB_APP_ID`: the GitHub App ID whose private key is in
  `silo-hub-github`, used by the hub to fetch grainfiles and referenced
  files from source repositories.
- `SILO_HUB_IAP_AUDIENCE` is filled in after the first apply, below.

The terraform's names (the `silo-hub` static IP, certificate map and
service account) are the manifests' defaults; if `name` or
`service_account` was changed, change the Gateway's address and
`networking.gke.io/certmap` annotation and the service account's
annotation to match.

Then create the Secrets and apply. `silo-hub-db` holds both
database URLs: `url`, the hub's (its IAM user, no password), and
`migrate-url`, the schema owner's (with its password), which only the
`migrate` init container is given. `silo-hub-internal` holds the key the
hub's pods share for their internal listeners (hub.md §2.2.3), random
and at least 32 bytes. `silo-hub-claims` holds the private key the hub
signs task claims with (hub.md §6.4), an EC P-256 key in PEM; set
`SILO_HUB_CLAIMS_ISSUER` in the overlay to `https://<domain>`, which
nodes name as `silo_issuer`. `silo-hub-github` holds `webhook-secret`
(`SILO_HUB_WEBHOOK_SECRET_FILE`), the HMAC secret verifying
`X-Hub-Signature-256` on `/v1/webhooks/github` (`silo hub serve`
refuses to start on Kubernetes without one, `server.CheckWebhookSecret`),
and `app-key` (`SILO_HUB_GITHUB_APP_KEY_FILE`), the GitHub App's PEM
private key:

```sh
eval "$(terraform -chdir=deploy/terraform/hub output -raw get_credentials)"
kubectl create namespace silo-hub
kubectl -n silo-hub create secret generic silo-hub-db \
  --from-literal=url="$(terraform -chdir=deploy/terraform/hub output -raw database_url)" \
  --from-literal=migrate-url="$(terraform -chdir=deploy/terraform/hub output -raw migrate_database_url)"
kubectl -n silo-hub create secret generic silo-hub-iap --from-literal=key=<the OAuth client's secret>
kubectl -n silo-hub create secret generic silo-hub-internal --from-literal=key="$(openssl rand -hex 32)"
(umask 077; openssl ecparam -name prime256v1 -genkey -noout -out claims.pem)
kubectl -n silo-hub create secret generic silo-hub-claims --from-file=key=claims.pem && rm claims.pem
kubectl -n silo-hub create secret generic silo-hub-github \
  --from-literal=webhook-secret="$(openssl rand -hex 32)" \
  --from-file=app-key=github-app.pem
kubectl apply -k deploy/kubernetes/hub/bsalmon-gke-dev
kubectl -n silo-hub rollout status deploy/silo-hub
kubectl -n silo-hub get gateway silo-hub   # PROGRAMMED True, with the static IP, after a few minutes
```

**The IAP audience.** The hub checks IAP's assertion against the
IAP-enabled backend service, whose ID exists only now that the Gateway
has made it. Until it is set, the hub runs and nodes connect, but
every human request is refused (401).

```sh
gcloud compute backend-services list --global --project bsalmon-gke-dev \
  --filter='name~silo-hub-silo-hub-80' --format='value(name,id)'
```

The backend for the `silo-hub` Service (not `silo-hub-nodes`) is the
one; set `SILO_HUB_IAP_AUDIENCE` in the overlay to
`$(terraform output -raw iap_audience_prefix)<its id>` and apply
again. The pods are rolled with the new config. Set terraform's
`iap_backend_service` to that backend's name and apply too: that is
what grants `iap_members` access through IAP, on this backend alone.
(`iap_project_wide = true` grants it on every IAP resource in the
project before the backend exists, which reaches every other
IAP-protected app there too; it is off by default.)

**Checking it.**

```sh
curl -sI https://<domain>/v1/me                  # 302 to Google sign-in: IAP is on
curl -s  https://<domain>/v1/nodes/connect       # 401 unauthenticated: the node path reaches the hub, past IAP
curl -s  https://<domain>/v1/claims/keys         # {"keys":[...]}: the claims keys, past IAP, for nodes' silo_keys_url
curl -s  https://<domain>/v1/grainfiles/content  # 401 unauthenticated: node content fetches reach the hub, past IAP
```

In a browser, `https://<domain>/v1/me` shows the signed-in principal.

## 5. The first team and node token

A fresh hub has no teams, and nobody can mint node tokens for a
repository no team owns. Hub admins (`SILO_HUB_ADMINS`) can do all of
this through the API (hub.md §5.3), but a browser makes POSTs awkward
behind IAP, so the image carries commands that act directly on the
database, run inside the pod (which reaches it through the proxy).
Members are `user:<email>` or `group:<name>`; one with neither prefix
is refused:

```sh
kubectl -n silo-hub exec deploy/silo-hub -c hub -- /silo hub team \
  -id infra -name "Infra" -admin user:bsalmon@google.com -repo bwsalmon/silo
kubectl -n silo-hub exec deploy/silo-hub -c hub -- /silo hub token \
  -owner bsalmon@google.com -node my-workstation
```

They take the database from the pod's `SILO_HUB_DATABASE`, as the
hub's own least-privileged role (the container's `SILO_HUB_MIGRATE` is
`false`, so they create no tables; the `migrate` init container made
them). The token
(`silo-node_...`) is printed once; the hub keeps only its hash. Minting
again for the same node revokes the earlier token. Later tokens can
be minted by their owners through `POST /v1/node-tokens` (the planned
`grain hub login`), and revoked with `DELETE /v1/node-tokens/{id}`.

## 6. Pointing a Grain node at the hub

A node connects to `https://<domain>` with its token as a bearer
credential, registering under the node ID the token was minted for
and the repositories its owner's teams own:

```go
cl := &rendezvous.Client{
	Hub:   "https://<domain>",
	Token: func(context.Context) (string, error) { return os.Getenv("SILO_HUB_TOKEN"), nil },
	Node:  registry.Node{ID: "my-workstation", Repos: []string{"bwsalmon/silo"}, Slots: 2},
	Logf:  log.Printf,
}
err := cl.Run(ctx, handleStream) // offers, resumes, cancels and views (hub-interface.md)
```

Grain embedding this client is grain's own change (hub.md §9), not
built yet; until it is, a node is whatever program runs the client.
The node needs only outbound HTTPS to the hub. It reconnects by
itself when the hub's pod rolls (it is told why with a `goodbye`), and
when the load balancer ends its WebSocket after the 24h backend
timeout.

## 7. Operating it

[hub-operations.md](hub-operations.md) is the full runbook: upgrades
and how node connections drain, rotating the IAP, node-token and
database secrets, backups and restoring them, and the dashboard and
alerts. In short:

- **Upgrades:** take an on-demand backup of the database first
  (`gcloud sql backups create --instance=silo-hub
  --project=bsalmon-gke-dev --description="before silo-hub TAG"`), since
  the new pods' `migrate` container may change the schema and there is
  no down-migration; rolling back past it is restoring that backup
  ([hub-operations.md](hub-operations.md) §2). Then push a new tag, set
  `newTag`, `kubectl apply -k`. The
  Deployment rolls its two pods one at a time (`RollingUpdate`,
  `maxSurge: 1`, `maxUnavailable: 0`): a new pod is ready before an old
  one says goodbye to its nodes and stops, so nodes reconnect to a pod
  that is serving within seconds, the dispatcher's lease moves to
  another pod, and running tasks resume on their nodes (hub.md §2.2.5,
  §6.2).
- **Logs:** `kubectl -n silo-hub logs deploy/silo-hub -c hub` (and
  `-c migrate`, `-c cloud-sql-proxy`).
- **Metrics:** Managed Service for Prometheus scrapes the pod's port
  9091 (the `PodMonitoring` `silo-hub`); the **Silo hub** dashboard and
  seven alert policies are terraform's (`monitoring.tf`), notifying
  `alert_notification_channels`.
- **Backups:** daily, 14 kept, with 7 days of point-in-time recovery
  (terraform's `backup_*` and `point_in_time_recovery_days`), and an
  on-demand one before each upgrade.
- **Two replicas.** Any pod serves any request, with no session
  affinity: the registry is shared through the database, streams and
  live events reach a node's pod over the internal listener, and the
  dispatcher runs on one pod at a time, under a database lease (hub.md
  §2.2). A `PodDisruptionBudget` keeps one pod through GKE node
  upgrades and drains, and the pods spread over zones and GKE nodes,
  so the cluster needs at least two nodes. To run more, change
  `replicas` and terraform's `hub_replicas` together
  ([hub-operations.md](hub-operations.md) §2).
- **Database availability:** `REGIONAL` by default, a standby in
  another zone at about twice the instance's cost; a development hub
  can set `database_availability_type = "ZONAL"` (hub.md §2.2).
- **Network policy:** `networkpolicy.yaml` denies ingress to the
  namespace by default and allows the hub's port 8080 only from
  Google's load balancer and health-check ranges (`35.191.0.0/16`,
  `130.211.0.0/22`), and its metrics only from the Prometheus
  collectors. The cluster needs network policy enforcement (GKE
  Dataplane V2, or `--enable-network-policy`) for it to apply.
- **Rate limiting the nodes' backend (recommended):** the
  `silo-hub-nodes` backend is reachable from the internet without
  IAP. The hub refuses a bearer that is not shaped like a node token
  before touching the database, but every well-shaped one costs a
  query. Attach a Cloud Armor security policy with a per-IP
  `rate_based_ban` (for example 60 requests a minute) to that backend
  through its `GCPBackendPolicy` (`securityPolicy`); nodes connect
  once and hold the connection, so a real node never comes near it.
- **Removing it:** `kubectl delete -k deploy/kubernetes/hub/bsalmon-gke-dev`,
  then `terraform destroy` (after setting `deletion_protection = false`
  for the database).

### A hub deployed before the two roles

A hub deployed when it served as `silo-hub` moves over once, in this
order, with the image built from this change or later:

1. `terraform apply`: it turns on `cloudsql.iam_authentication` (if
   Cloud SQL restarts the instance for it, the hub is without its
   database for a minute or so), creates the IAM database user and grants `roles/cloudsql.instanceUser` on the
   instance. `silo-hub`'s password is unchanged.
2. Add `SILO_HUB_RUNTIME_ROLE` to the overlay, and replace the Secret
   with both keys (the `kubectl create secret` above, with
   `--dry-run=client -o yaml | kubectl apply -f -`).
3. `kubectl apply -k` the overlay. The new pod's `migrate` container
   finds the tables there and already `silo-hub`'s, grants the runtime
   role, and the hub starts as it.

### A hub deployed before the internal listener

Create `silo-hub-internal` (the `kubectl create secret` above) before
applying a change that mounts it, or the new pod waits for the Secret.

### A hub deployed before signed claims

Create `silo-hub-claims` (the `openssl ecparam` above) and set
`SILO_HUB_CLAIMS_ISSUER` in the overlay before applying a change that
mounts it, or the new pod waits for the Secret. A node then verifies
the hub's claims with, in its `grain_policy.yml`, `silo_issuer:
https://<domain>`, `silo_keys_url: https://<domain>/v1/claims/keys`
and, to check that each claim is for it, `silo_audience` (grain's
`docs/local-policy.md`). To rotate the key, put the new one in the
Secret as `key` and the old as `previous`, set
`SILO_HUB_CLAIMS_KEY_FILE` to
`/var/run/secrets/silo-hub-claims/key,/var/run/secrets/silo-hub-claims/previous`,
and drop `previous` an hour after the roll.

## 8. The nodes' UIs

The hub can serve each connected node's own UI to that node's owner,
at `https://<node>.<node_ui_domain>` (hub.md §11.2), through the node's
existing connection. It is off until the hub is told the domain.

- **Terraform.** `node_ui_domain` is `nodes.<domain>` unless set (set
  it to `""` for none). For it terraform makes a DNS authorization of
  its own (the hub's covers only `domain` and `*.<domain>`), a
  certificate `silo-hub-nodes` for `*.<node_ui_domain>`, its entry in
  the certificate map the Gateway already uses and, with `dns_zone`, the
  wildcard A record `*.<node_ui_domain>` to the hub's address and the
  authorization's CNAME. Without `dns_zone`, both are in
  `terraform output dns_records`; the zone that holds `domain` takes
  them as they are.
- **The manifests.** The HTTPRoute `silo-hub-node-ui` matches
  `*.<node_ui_domain>` and sends every path to the `silo-hub` Service,
  so the hub's IAP signs viewers in there as on the hub's host. To turn
  it on, set in the overlay `SILO_HUB_NODE_UI_DOMAIN` to
  `terraform output -raw node_ui_domain` and that route's host name to
  `*.` and the same, and apply. Once the `silo-hub-nodes` certificate
  is `ACTIVE`, a node's owner opens `https://<node>.<node_ui_domain>/`.
- **They need IAP.** Behind an OIDC hub (hub.md §5.1) the node hosts
  sign nobody in: its session cookie belongs to the hub's host, and no
  node host can set one. Turn them on only for a hub behind IAP, as
  this one is. Who may pass IAP is the same `iap_members` (or a dedicated
  IAP-enabled backend service when `SILO_HUB_NODE_UI_IAP_AUDIENCE` /
  `-node-ui-iap-audience` is set); the hub then admits only each node's
  owner and team admins of the node's validated `ui.team` (others get
  404), strips `X-Goog-IAP-*` headers and `GCP_IAP_*` cookies before
  proxying to the node, and passes the verified viewer and role in the
  stream handshake ([grain-ui-proxy.md](grain-ui-proxy.md)).
- **Prefer a domain of its own.** `nodes.<domain>` is the same *site*
  as the hub (one registrable domain), so a node's UI page, which is
  the Grain's own content, gets the hub's `SameSite` cookies on its
  requests to the hub. The hub refuses every write that a browser
  sends from anywhere but its own origin (`identity.SameOrigin`,
  threat-model.md INV-16), but a node UI host on another registrable
  domain (`node_ui_domain = "nodes.example-grain-ui.com"`, not a
  subdomain of `domain`) keeps those cookies off its requests
  altogether. Its DNS records then go in that domain's zone, from
  `terraform output dns_records`, as without `dns_zone`.
- **Which nodes.** Only a node whose ID is a DNS label (lower-case
  letters, digits and `-`) has a host. A node's UI is served at its
  host's root, so grain's UI works there unchanged.
- **Checking it.** `curl -sI https://<node>.<node_ui_domain>/` is a
  302 to Google sign-in; signed in as the node's owner, the node's UI.

## 9. Local and CI 2-replica `kind` deployment test

`deploy/kubernetes/hub/kind/` is a kustomize overlay that builds
directly on `deploy/kubernetes/hub/base/` to run the full 2-replica
Deployment on a throwaway `kind` cluster without GCP credentials. It
replaces only the GCP-specific `cloud-sql-proxy` sidecar with an
in-cluster PostgreSQL service (`deploy/kubernetes/hub/kind/postgres.yaml`)
configured with the same two-role split (`silo_migrate` schema owner and
`silo_runtime` least-privileged runtime role) and an in-cluster GitHub
REST API mock (`github-mock`), while preserving:

- the `migrate` init container (`silo hub migrate` with
  `SILO_HUB_RUNTIME_ROLE=silo_runtime`);
- the distroless `hub` container (`SILO_HUB_MIGRATE=false`,
  `runAsNonRoot: true`, `runAsUser: 65532`,
  `readOnlyRootFilesystem: true`);
- downward-API pod identity (`SILO_HUB_POD`, `SILO_HUB_POD_IP`,
  `SILO_HUB_POD_ADDR`) and the `:9092` internal listener authenticated
  with `silo-hub-internal`;
- the `silo-hub-claims` and `silo-hub-github` Secret mounts; and
- `default-deny-ingress` network policies (`base/networkpolicy.yaml`).

Run the end-to-end packaging and `kind` deployment test locally or in CI
(`.grain/workflows/deploy.yml`):

```sh
deploy/kubernetes/hub/e2e-kind.sh
```

`e2e-kind.sh` and `deploy/kubernetes/hub/kind_e2e_test.go` verify:

1. **Distroless image packaging and Kubernetes guardrails:** builds
   `silo-hub:ci` (`gcr.io/distroless/static-debian12:nonroot`,
   `65532:65532`), checks that all CLI subcommands (`hub serve`,
   `hub migrate`, `hub team`, `hub token`, `conformance`) and the
   embedded web UI at `/` work inside the distroless image, and verifies
   that when `KUBERNETES_SERVICE_HOST` is set `silo hub serve` refuses
   SQLite (`CheckDatabase`), refuses `-dev-user-header`
   (`CheckDevSignOn`), and refuses an empty webhook secret
   (`CheckWebhookSecret`).
2. **2-replica startup, `migrate` init container, and least-privileged
   DB role:** boots a throwaway `kind` cluster, applies
   `deploy/kubernetes/hub/kind/`, waits for both `silo-hub` pods to pass
   `/healthz` and `/readyz`, verifies that `silo_runtime` is denied DDL
   (`CREATE TABLE`, `DROP TABLE`), and runs the documented bootstrap
   commands (`kubectl exec deploy/silo-hub -c hub -- /silo hub team ...`
   and `/silo hub token ...`).
3. **Multi-pod rendezvous, `:9092` internal streams, and
   `LISTEN/NOTIFY`:** connects a simulated Grain node over WebSocket
   (`/v1/nodes/connect`) to pod 0, verifies the node is visible on both
   pods (`GET /v1/nodes`), proxies the node's UI (`<node>.nodes...`)
   and dispatches a task + live SSE stream through pod 1 across the
   `:9092` internal listener to pod 0.
4. **Signed webhooks, Grainfile indexing, and content proxy:** verifies
   unsigned/invalid `POST /v1/webhooks/github` deliveries are rejected
   with `401`, delivers a valid `X-Hub-Signature-256` `push` webhook to
   pod 1, verifies `grainfile_event` (`task_ready`) delivery to the
   node connected on pod 0, and checks `GET /v1/grainfiles/content`
   for the Grainfile (`200`), an authorized same-repo reference (`200`),
   and unauthorized paths (`403` / `401`).
5. **Prometheus metrics and zero-downtime rolling update:** scrapes
   `:9091/metrics` on both pods (`silo_hub_node_connects_total`,
   `silo_hub_webhooks_total`, `silo_hub_grainfiles_indexed`,
   `silo_hub_db_open_connections{store="grainfiles"}`), then runs
   `kubectl rollout restart deployment/silo-hub`, verifying
   `maxUnavailable: 0`, `goodbye` delivery when the old pod terminates,
   and clean node reconnect + `catchup` on the replacement pod.

## Cloud Run, the alternative not taken

The same image serves on Cloud Run with `hub serve` and the same
environment, and Cloud SQL attached as a Cloud SQL connection. It was
not chosen because Cloud Run ends every request, and so every node
WebSocket, at 60 minutes, and it leaves less room for several replicas
(hub.md §2.1). Behind IAP it would need the same split: the node path
on a backend without IAP, through a serverless NEG.
