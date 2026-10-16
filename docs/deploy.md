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
| `deploy/terraform/hub` | The project's side: Cloud SQL (PostgreSQL), the hub's Google service account and its Workload Identity binding, IAP access, a static IP, Certificate Manager certificates (the hub's host and the nodes' UI hosts, `*.nodes.<domain>`) and their map, DNS records (optional) and an Artifact Registry repository. `bsalmon-gke-dev.tfvars` is the example for this project. |
| `deploy/kubernetes/hub` | The cluster's side, with kustomize: `base/` and the `bsalmon-gke-dev/` overlay. One hub pod with the Cloud SQL Auth Proxy beside it, two Services, the Gateway and its HTTPRoutes (the hub's host, and the nodes' UI hosts), and the GKE policies (IAP, timeouts, health checks). `validate.sh` checks them on a kind cluster. |

Traffic, once deployed:

```
humans, SDK ──HTTPS──> Gateway ─ PathPrefix / ───────────> Service silo-hub        (IAP on, 1h timeout)  ─┐
grain nodes ──WSS───>  (static IP,  Exact /v1/nodes/connect > Service silo-hub-nodes (IAP off, 24h timeout) ─┴─> the hub pod :8080
                        managed cert)                                                         └─ cloud-sql-proxy ─> Cloud SQL
```

The node path is outside IAP and the hub checks each node's token
itself; every other path is behind IAP, and the hub checks IAP's
signed assertion too (hub.md §5.2.1). The nodes' UI hosts
(`<node>.nodes.<domain>`, §8) have a route of their own, every path on
them to the `silo-hub` Service and so behind the same IAP.

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
- The HTTPRoute's host name: the `domain` given to terraform.
- The HTTPRoute `silo-hub-node-ui`'s host name: `*.` and
  `terraform output -raw node_ui_domain`. `SILO_HUB_NODE_UI_DOMAIN`
  stays empty until the nodes' UIs are turned on (§8).
- The `GCPBackendPolicy` `silo-hub`'s `clientID`: the OAuth client's ID.
- `SILO_HUB_ADMINS`: the hub admins (already `bsalmon@google.com`).
- `SILO_HUB_IAP_AUDIENCE` is filled in after the first apply, below.

The terraform's names (the `silo-hub` static IP, certificate map and
service account) are the manifests' defaults; if `name` or
`service_account` was changed, change the Gateway's address and
`networking.gke.io/certmap` annotation and the service account's
annotation to match.

Then create the two Secrets and apply:

```sh
eval "$(terraform -chdir=deploy/terraform/hub output -raw get_credentials)"
kubectl create namespace silo-hub
kubectl -n silo-hub create secret generic silo-hub-db \
  --from-literal=url="$(terraform -chdir=deploy/terraform/hub output -raw database_url)"
kubectl -n silo-hub create secret generic silo-hub-iap --from-literal=key=<the OAuth client's secret>
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
again. The pod is restarted with the new config. Optionally, set
terraform's `iap_backend_service` to that backend's name and apply, to
narrow IAP access from the whole project to this backend.

**Checking it.**

```sh
curl -sI https://<domain>/v1/me              # 302 to Google sign-in: IAP is on
curl -s  https://<domain>/v1/nodes/connect   # 401 unauthenticated: the node path reaches the hub, past IAP
```

In a browser, `https://<domain>/v1/me` shows the signed-in principal.

## 5. The first team and node token

A fresh hub has no teams, and nobody can mint node tokens for a
repository no team owns. Hub admins (`SILO_HUB_ADMINS`) can do all of
this through the API (hub.md §5.3), but a browser makes POSTs awkward
behind IAP, so the image carries commands that act directly on the
database, run inside the pod (which reaches it through the proxy):

```sh
kubectl -n silo-hub exec deploy/silo-hub -c hub -- /silo hub team \
  -id infra -name "Infra" -admin bsalmon@google.com -repo bwsalmon/silo
kubectl -n silo-hub exec deploy/silo-hub -c hub -- /silo hub token \
  -owner bsalmon@google.com -node my-workstation
```

They take the database from the pod's `SILO_HUB_DATABASE`. The token
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

- **Upgrades:** push a new tag, set `newTag`, `kubectl apply -k`. The
  Deployment is `Recreate`: the old pod says goodbye to its nodes and
  stops, then the new one starts, so nodes are away for a few seconds
  and running tasks resume when their node is back (hub.md §6.2).
- **Logs:** `kubectl -n silo-hub logs deploy/silo-hub -c hub` (and
  `-c cloud-sql-proxy`).
- **One replica only.** Do not scale the Deployment: the dispatcher
  assumes it is alone (hub.md §10).
- **Removing it:** `kubectl delete -k deploy/kubernetes/hub/bsalmon-gke-dev`,
  then `terraform destroy` (after setting `deletion_protection = false`
  for the database).

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
  this one is. Who may pass IAP is the same `iap_members`; the hub then
  admits only each node's owner (others get 404), and a node checking
  the assertion itself uses the hub's `SILO_HUB_IAP_AUDIENCE`.
- **Which nodes.** Only a node whose ID is a DNS label (lower-case
  letters, digits and `-`) has a host. A node's UI is served at its
  host's root, so grain's UI works there unchanged.
- **Checking it.** `curl -sI https://<node>.<node_ui_domain>/` is a
  302 to Google sign-in; signed in as the node's owner, the node's UI.

## Cloud Run, the alternative not taken

The same image serves on Cloud Run with `hub serve` and the same
environment, and Cloud SQL attached as a Cloud SQL connection. It was
not chosen because Cloud Run ends every request, and so every node
WebSocket, at 60 minutes, and it leaves less room for several replicas
(hub.md §2.1). Behind IAP it would need the same split: the node path
on a backend without IAP, through a serverless NEG.
