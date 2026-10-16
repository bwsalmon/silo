# Operating the hub

This is the runbook for a hub that is already deployed
([deploy.md](deploy.md) is how it gets there): upgrading it, rotating
its secrets, restoring its database, and reading its dashboard and
alerts. The hub's design is [hub.md](hub.md); §2.2 there is why it runs
one replica, how long each kind of failure keeps it down, and the
staged design for several replicas.

The commands assume the cluster's credentials
(`eval "$(terraform -chdir=deploy/terraform/hub output -raw get_credentials)"`),
project `bsalmon-gke-dev` and the names the manifests use: namespace
`silo-hub`, Deployment `silo-hub`, Cloud SQL instance `silo-hub`,
database `silo`.

## 1. What the hub keeps, and where

| What | Where | Lost with the pod? |
| --- | --- | --- |
| Teams, members, repositories, team settings, node tokens (hashes) | PostgreSQL: `teams`, `team_members`, `team_repos`, `team_settings`, `node_tokens` | no |
| The backlog | PostgreSQL: `tasks` | no |
| Stored events (lifecycle and conversation) | PostgreSQL: `task_events` | no |
| The node registry: who is connected, their slots and heartbeats | memory | yes: rebuilt as nodes reconnect, within their backoff (at most a minute) |
| Token-stream rings, live subscribers | memory | yes: subscribers reconnect from their cursor; ring events older than the restart are not replayed |
| Open task streams | memory | yes: on start, offered tasks are requeued and running ones are `lost` until their nodes resume them (hub.md §6.2) |
| The dispatcher's lease | PostgreSQL: an advisory lock held by the dispatching pod's session; `hub_leases` numbers its terms and names the holder | released: the next pod to start takes it (hub.md §2.2.1) |

Backing up the database is therefore backing up the hub.

## 2. Upgrades

The Deployment is one replica rolled with `Recreate` (hub.md §2.2):
the old pod stops before the new one starts, and nothing is served in
between.

1. Build and push the new image (deploy.md §3), set `newTag` in the
   overlay, and check what changes:
   `kubectl diff -k deploy/kubernetes/hub/bsalmon-gke-dev`.
2. Look at the dashboard (§5) first: an upgrade during an incident
   hides it. Note the online node count.
3. `kubectl apply -k deploy/kubernetes/hub/bsalmon-gke-dev`, then
   `kubectl -n silo-hub rollout status deploy/silo-hub`.

What happens, and how node connections are drained:

- Kubernetes sends the old pod SIGTERM. The hub turns `/readyz` to 503
  at once, so the load balancer stops sending it new requests, and
  answers new node connections 503, which nodes retry.
- It writes `{"type":"goodbye","error":"the hub is shutting down"}` on
  every node's control stream and waits for the nodes to close their
  connections (hub-interface.md); a node logs the reason and
  reconnects with its backoff (1s, doubling to a minute). It ends the
  open event subscriptions, which reconnect from their cursor, stops
  the dispatcher and closes the database, all within 20 seconds
  (`-shutdown-grace`). The pod's `terminationGracePeriodSeconds` (40)
  leaves the Cloud SQL proxy, a native sidecar, running until the hub
  has gone.
- The new pod starts, its proxy first. It takes the dispatcher's
  lease, which the old pod released as it stopped (if the old pod is
  gone without releasing it, PostgreSQL frees it when the old session
  ends, about ten seconds later). Its dispatcher marks every
  running task `lost` and requeues every offered one; as each node
  reconnects, its lost tasks are resumed on it. No team's lost-task
  grace (hub.md §6.3) can expire for a task that was running at the
  upgrade: the grace is at least five minutes, and for those tasks it
  is measured from the restart.

Check afterwards: `silo_hub_ready` is 1, the online node count is back
to where it was (it takes up to a minute, the nodes' longest backoff),
and the backlog's `lost` count falls back to what it was before.

**Schema changes.** The pod's `migrate` init container runs `silo hub
migrate` as the schema's owner, `silo-hub`, before the hub starts: each
store creates its tables if they are missing (`CREATE TABLE IF NOT
EXISTS`), so a new table (such as `team_settings`) appears on the first
start of the version that adds it, and the hub's runtime role is
granted its rows (deploy.md, "The database's two roles"). The hub
itself cannot change the schema. If the `migrate` container fails, the
pod does not start and the old one is already gone (`Recreate`): read
`kubectl -n silo-hub logs deploy/silo-hub -c migrate`. There is no down-migration: to roll back past a version that
changed a table, restore the database from before the upgrade (§4) as
well as the image. Take an on-demand backup before such an upgrade:
`gcloud sql backups create --instance=silo-hub --project=bsalmon-gke-dev`.

**Rolling back** is the same apply with the previous tag.

**Never scale the Deployment.** The dispatcher's lease (hub.md
§2.2.1) keeps two pods from both dispatching, but a node's connection,
its events and the streams to it still live on the one pod holding it:
a second pod would not see half the nodes (hub.md §2.2, stages R2 to
R5). `validate.sh` refuses manifests that do.

## 3. Rotating secrets

### 3.1 The IAP OAuth client secret

The Gateway's `GCPBackendPolicy` `silo-hub` reads the OAuth client's
secret from the Secret `silo-hub-iap` (key `key`). The client is made in
the console (deploy.md §2), so it is rotated there:

1. In the console, **Google Auth Platform → Clients →** the hub's
   client: **Add secret**. The old secret keeps working until it is
   disabled.
2. Replace the Secret:
   ```sh
   kubectl -n silo-hub create secret generic silo-hub-iap \
     --from-literal=key='<the new secret>' --dry-run=client -o yaml | kubectl apply -f -
   ```
3. GKE's Gateway controller pushes the new secret to the backend
   service on its next reconcile; nudge it by re-applying the overlay
   (`kubectl apply -k ...`), and check that
   `kubectl -n silo-hub describe gcpbackendpolicy silo-hub` shows the
   policy attached with no error.
4. Sign in afresh in a private window (`https://<domain>/v1/me`). Once
   it works, **disable** and then **delete** the old secret in the
   console.

The hub itself holds no IAP secret: it checks IAP's signed assertion
against Google's public keys and the backend service's audience
(`SILO_HUB_IAP_AUDIENCE`). The audience changes only if the backend
service is made again (the Gateway or the `silo-hub` Service deleted
and re-created); then set it again as deploy.md §4 says.

### 3.2 Node tokens

A node token proves one node ID for one owner (hub.md §5.2); the hub
keeps only its hash. Minting a token for a node revokes that owner's
earlier tokens for it, so rotating one is minting a new one:

1. The owner mints it: `POST /v1/node-tokens {"node":"<id>"}` (or a
   hub admin, in the pod:
   `kubectl -n silo-hub exec deploy/silo-hub -c hub -- /silo hub token -owner <owner> -node <id>`).
   The old token is revoked at once, but a connection it already made
   stays up until it drops.
2. Put the new token where the node reads it, and restart the node's
   client, so that it reconnects with the new token.

**A leaked token** is revoked rather than rotated:
`DELETE /v1/node-tokens/{id}` (its owner or a hub admin), which also
ends the node's connection at once. `GET /v1/node-tokens` lists them,
with which are revoked. To retire every token of a node ID for good,
revoke its live one and mint none.

### 3.3 The database password

Only the schema owner, `silo-hub`, has a password: terraform's
`random_password.db`, which reaches the pod as the Secret
`silo-hub-db`'s `migrate-url`, given to the `migrate` init container
alone. The hub serves as its IAM database user, which has no password
to rotate: the proxy logs it in with short-lived tokens from the pod's
Workload Identity. To rotate the owner's password:

```sh
terraform -chdir=deploy/terraform/hub apply -var-file=bsalmon-gke-dev.tfvars -replace=random_password.db
kubectl -n silo-hub create secret generic silo-hub-db \
  --from-literal=url="$(terraform -chdir=deploy/terraform/hub output -raw database_url)" \
  --from-literal=migrate-url="$(terraform -chdir=deploy/terraform/hub output -raw migrate_database_url)" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n silo-hub rollout restart deploy/silo-hub
```

The running hub is untouched by the apply: it does not use the
password. Only the next pod start needs the new one, so if the Secret
still holds the old password then, the `migrate` container fails and
the pod does not start; the restart above checks it while you are
watching.

To cut the hub off from the database (a leaked pod, say), remove
`roles/cloudsql.instanceUser` from its service account, or delete its
IAM database user; terraform puts both back.

### 3.4 OIDC (a hub not behind IAP)

`SILO_HUB_OIDC_CLIENT_SECRET` rotates like 3.1, at the provider.
Changing `SILO_HUB_OIDC_SESSION_KEY` signs everyone out, since it signs
their session cookies.

## 4. Backups and restoring

### 4.1 What is backed up

Terraform (`deploy/terraform/hub/main.tf`) sets the Cloud SQL instance
up with:

| Setting | Value (variable) |
| --- | --- |
| Daily automated backup | at `backup_start_time`, 03:00 UTC |
| Backups kept | `backups_retained`, 14 |
| Point-in-time recovery | on, with `point_in_time_recovery_days` (7) of write-ahead log |
| Backup location | `backup_location`; Cloud SQL's default multi-region when unset |
| Deletion protection | terraform's and Cloud SQL's own (`deletion_protection`) |
| Availability | `database_availability_type`: `ZONAL` (the default, no standby) or `REGIONAL` (a standby in another zone, failover in about a minute, about twice the cost; hub.md §2.2) |

So the database can be put back to any second in the last seven days,
or to any of the last fourteen daily backups. Check the backups exist:
`gcloud sql backups list --instance=silo-hub --project=bsalmon-gke-dev`.

### 4.2 Choosing a restore

- **The instance is gone or broken, the data is fine** (zone outage,
  instance deleted with its backups kept): restore the latest backup
  into a new instance (4.3). A `REGIONAL` instance fails over to its
  standby by itself for a zone's outage; restoring is for losing the
  instance.
- **The data is wrong** (a bad migration, rows deleted by hand): clone
  the instance to just before the change (4.4), check the clone, and
  point the hub at it.
- **Moving the hub's data elsewhere, or keeping an off-Cloud SQL
  copy**: a logical export (4.5). This is the path CI tests (4.6).

Whichever it is, a restored hub is only as current as the backup:
tasks submitted, placed or finished after it are not in it. Nodes
still hold their own tasks, so after a restore, a running task the
backup does not know is unknown to the hub (its node's events for it
are refused), and a task the backup has as running that finished since
is resumed and its node reports it finished.

### 4.3 From a backup, into a new instance

Restore into a new instance rather than over the old one, so the old
one stays to look at:

```sh
gcloud sql backups list --instance=silo-hub --project=bsalmon-gke-dev   # pick BACKUP_ID
gcloud sql instances create silo-hub-restored --project=bsalmon-gke-dev \
  --region=us-central1 --database-version=POSTGRES_16 --tier=db-custom-1-3840 \
  --edition=enterprise
gcloud sql backups restore BACKUP_ID --restore-instance=silo-hub-restored \
  --backup-instance=silo-hub --project=bsalmon-gke-dev
```

The restored instance has the `silo-hub` user with its old password,
so the `silo-hub-db` Secret is unchanged. Check it has the hub's IAM
user too (`gcloud sql users list --instance=silo-hub-restored`; if not,
`gcloud sql users create silo-hub@bsalmon-gke-dev.iam
--type=cloud_iam_service_account --instance=silo-hub-restored`) and
takes IAM logins (`gcloud sql instances describe
silo-hub-restored --format='value(settings.databaseFlags)'` lists
`cloudsql.iam_authentication`; if not, `gcloud sql instances patch
silo-hub-restored --database-flags=cloudsql.iam_authentication=on`).
Then point the hub at it (4.7).

### 4.4 To a point in time (a clone)

```sh
gcloud sql instances clone silo-hub silo-hub-pitr --project=bsalmon-gke-dev \
  --point-in-time='2026-10-15T09:41:00Z'
```

The time is UTC, and must be within the last
`point_in_time_recovery_days`. Read the clone before switching to it,
for instance with the Cloud SQL Auth Proxy on a workstation and `psql`:
`SELECT state, COUNT(*) FROM tasks GROUP BY state;` and
`SELECT id FROM teams;`. Then point the hub at it (4.7).

### 4.5 A logical export, and importing one

```sh
gcloud sql export sql silo-hub gs://BUCKET/silo-hub-$(date +%F).sql.gz \
  --database=silo --project=bsalmon-gke-dev
# into an instance (a new one, or the old one emptied: DROP DATABASE silo; CREATE DATABASE silo OWNER "silo-hub";)
gcloud sql import sql silo-hub-restored gs://BUCKET/silo-hub-2026-10-15.sql.gz \
  --database=silo --user=silo-hub --project=bsalmon-gke-dev
```

Import as `silo-hub`, as above, so the tables are its own and the
`migrate` container can grant them; into a new instance, first create
that user and the IAM user (`gcloud sql users create
silo-hub@bsalmon-gke-dev.iam --type=cloud_iam_service_account
--instance=...`) and turn on `cloudsql.iam_authentication` as in 4.3.
The instance's service account needs `roles/storage.objectAdmin` on the
bucket. With plain PostgreSQL tools the same is
`pg_dump --format=custom --no-owner --no-privileges` and
`pg_restore --no-owner --no-privileges --exit-on-error`, which is what
CI runs.

### 4.6 What CI tests

`hub/server`'s `TestBackupRestore` (`go test ./hub/server -run
BackupRestore`, on every push in the `hub` job) runs 4.5 on throwaway
PostgreSQL servers: it fills a hub's database with a team, its members,
repositories and settings, a node token, a queued and a running task
and a stored event; dumps it with `pg_dump`; restores it into an empty
server with `pg_restore`; and starts a hub on the copy. It checks that
the directory and the settings are there; that the registry starts
empty; that the running task is `lost` and the queued one still
queued; and that the node reconnects with its old token, is asked to
resume its task, and is offered the queued one.

Cloud SQL's own backup and point-in-time restore (4.3, 4.4) are
Cloud SQL's and are not exercised in CI; rehearse one in a scratch
instance when the instance's settings change.

### 4.7 Pointing the hub at the restored instance

1. The hub's `roles/cloudsql.client` and `roles/cloudsql.instanceUser`
   are conditioned on the instance's name, so they do not reach the
   restored one. Grant them on it for now (or restore into an instance
   named `silo-hub`, step 5):
   `gcloud projects add-iam-policy-binding bsalmon-gke-dev
   --member=serviceAccount:silo-hub@bsalmon-gke-dev.iam.gserviceaccount.com
   --role=ROLE --condition='expression=resource.name == "projects/bsalmon-gke-dev/instances/<the restored instance>",title=silo-hub-restored'`
   for each ROLE.
2. Set the overlay's `CLOUD_SQL_INSTANCE` to
   `bsalmon-gke-dev:us-central1:<the restored instance>` and apply.
   With `Recreate`, the old pod stops first, so the two never write at
   once. The `migrate` container grants the runtime role again on the
   restored tables.
3. Watch the hub start: `kubectl -n silo-hub logs deploy/silo-hub -c migrate`
   and `-c hub`, then `silo_hub_ready` and `silo_hub_database_up` on
   the dashboard.
4. Nodes reconnect within a minute, and the registry fills up again:
   it is memory, never in the backup.
5. Once it is right, bring terraform into line: either import the new
   instance in place of the old one
   (`terraform state rm google_sql_database_instance.hub` and
   `terraform import ... projects/bsalmon-gke-dev/instances/<name>`, with
   `name` set to match), or restore back into an instance named
   `silo-hub` the same way, and switch to it. Until then a terraform
   apply would act on the old instance only.

## 5. The dashboard and the alerts

### 5.1 Where they are

Terraform (`deploy/terraform/hub/monitoring.tf`, `monitoring = true` by
default) makes:

- a Cloud Monitoring dashboard, **Silo hub** (Monitoring → Dashboards),
  from `dashboards/hub.json`;
- six alert policies (Monitoring → Alerting), each notifying
  `alert_notification_channels` and linking to this section.

They read what GKE's Managed Service for Prometheus scrapes from the
hub every 30 seconds: the `PodMonitoring` `silo-hub` scrapes port 9091,
`/metrics`, which no Service exposes, so it is reachable only inside
the cluster. Queries select `namespace="silo-hub",job="silo-hub"`.
Managed collection must be on in the cluster (it is by default on GKE
1.27 and later; terraform's `check "cluster"` warns if not). To look
at the raw metrics:
`kubectl -n silo-hub port-forward deploy/silo-hub 9091 && curl -s localhost:9091/metrics | grep ^silo_hub`.

Cloud Monitoring and not Grafana: the scraped metrics are already in
Cloud Monitoring, which alerts and charts them with PromQL and with
nothing more to run; Grafana would add a deployment, its own sign-in,
data-source credentials and an Alertmanager.

### 5.2 The metrics

| Metric | Type | What it says |
| --- | --- | --- |
| `silo_hub_ready` | gauge | 1 while `/readyz` would answer 200 |
| `silo_hub_database_up` | gauge | 1 if the database answered a ping at this scrape |
| `silo_hub_nodes{status}` | gauge | nodes in the registry: `online`, `stale`, `offline` |
| `silo_hub_team_nodes{team,status}` | gauge | nodes serving one of the team's repositories |
| `silo_hub_team_free_slots{team}` | gauge | unclaimed slots on those nodes that are online |
| `silo_hub_node_connects_total`, `_disconnects_total`, `_refusals_total` | counters | node connections welcomed, ended, refused (bad token, not admitted, another owner's ID) |
| `silo_hub_backlog_tasks{team,state}` | gauge | unfinished tasks: `queued`, `offered`, `running`, `lost` |
| `silo_hub_backlog_oldest_queued_seconds{team}` | gauge | how long the team's longest-waiting queued task has waited |
| `silo_hub_dispatch_latency_seconds` | histogram | submission to a node accepting the task |
| `silo_hub_dispatch_events_total{event}` | counter | `accepted`, `resumed`, `refused`, `timeout` (no answer in a minute), `unreachable`, `lost` (a running task's node went away), `lost_requeued` (hub.md §6.3) |
| `silo_hub_events_ingested_total{class,result}` | counter | events nodes streamed up: `kept`, `rejected` by schema, or `error` (not stored) |
| `silo_hub_event_store_events`, `_bytes` | gauges | the event store's size |
| `silo_hub_subscribers` | gauge | live event subscriptions open |
| `silo_hub_view_requests_total{code}`, `silo_hub_view_latency_seconds` | counter, histogram | artifact view-throughs by answer, and the time to the node's answer |
| `silo_hub_http_requests_total{route,code}` | counter | every request, by its route pattern (`POST /v1/tasks`, `unmatched`; `node-ui` for every request to a node's own UI, hub.md §11) |
| `silo_hub_dispatcher_leader` | gauge | 1 on the pod holding the dispatcher's lease and running the dispatcher (hub.md §2.2.1) |
| `silo_hub_dispatcher_terms_total` | counter | terms of the lease this pod has taken; each recovers the backlog |
| `silo_hub_store_errors_total{store}` | counter | database errors: `backlog` (the dispatcher), `events` (keeping events), `database` (pings and the scrape's reads) |
| `silo_hub_db_open_connections{store}`, `silo_hub_db_wait_count_total{store}` | gauge, counter | each store's connection pool |

Plus the Go runtime's and the process's (`go_*`, `process_*`), and the
scrape's own `up`.

### 5.3 Reading the dashboard

Top row: **Hub ready** and **Database up** should both read 1.

- **Nodes by status** and **Online nodes and free slots by team**: how
  much capacity there is, and for whom. Stale nodes are connected but
  silent; the dispatcher passes them over.
- **Node connects, disconnects and refusals**: a spike of both connects
  and disconnects is nodes flapping (the network, or the 24h WebSocket
  timeout all at once after a roll); a rise in refusals is revoked or
  wrong tokens, or nodes listing repositories their owner's teams do
  not own.
- **Backlog by team and state** and **Oldest queued task by team**: the
  queue. `queued` growing with free slots available is the dispatch
  stuck alert's case; `lost` that stays up is nodes that are gone.
- **Dispatch latency** and **Dispatch events**: how long tasks wait to
  be placed, and what happens to offers. Many `refused` is a node's
  local policy turning tasks away; `timeout` or `unreachable` is nodes
  that are connected but not answering.
- **Events ingested** and **Event store size**: the event stream, and
  how fast the database grows. `rejected` means a node is sending
  events outside the schema (hub.md §7.1): a Grain version mismatch.
- **Live subscribers**: people (or SDK clients) watching tasks.
- **View-through latency and errors**: 503 is an offline node (by
  design); 502 is a node that did not answer.
- **HTTP requests by route and code** and **HTTP 5xx ratio**: the API.
- **Store errors** and **Database connections by store**: the database
  from the hub's side. Waits that climb mean the pool is starved.

### 5.4 The alerts, and what to do

**Hub down or not ready** (critical). No scrape, a failed scrape, or
`silo_hub_ready` 0 for 5 minutes.
`kubectl -n silo-hub get pods`, then `describe` the pod and read
`logs -c hub` and `logs -c cloud-sql-proxy`. Not ready with a running
pod is almost always the database (next alert); a pod that never starts
is the image, the config map or the proxy. A pod stuck terminating
is a node that will not close; deleting the pod is safe.

**Team has queued work and no online node** (warning). For 15 minutes
a team has had queued tasks and no online node that serves any of its
repositories. Its nodes are down, their tokens were revoked (refusals
on the dashboard), or the team's repositories changed so that no node
lists them. Tell the team; nothing on the hub is broken.

**Dispatch stuck** (warning). A team's oldest queued task has waited
30 minutes while its nodes have free slots. Either every node with
room refuses the task (check the task: `GET /v1/tasks/{id}` shows
`excluded` and the last refusal as `reason`; `POST
/v1/tasks/{id}/requeue` clears the exclusions), the task's constraints
match no node, or the dispatcher is wedged (no `accepted` dispatch
events at all while tasks are queued: restart the pod).

**Hub error rate** (warning). Over 5% of requests answered 5xx (not
counting `/readyz` while draining, the view-through's 503 for an
offline node, or requests to nodes' own UIs, route `node-ui`) for 5
minutes. The dashboard's route breakdown says
which API; the hub's logs say why. Usually it is the database.

**Not dispatching** (critical). The hub has been up for 5 minutes and
no pod reports `silo_hub_dispatcher_leader` 1: no task is placed,
resumed or cancelled. Read the pod's log for `lease dispatcher:`
lines. A term that fails at once (`recovering the backlog`) is the
database; see the next alert. No term at all means another session
holds the lease: `SELECT holder, epoch, taken_at FROM hub_leases` names
the last pod to take it, and `SELECT pid, backend_start FROM pg_locks
JOIN pg_stat_activity USING (pid) WHERE locktype = 'advisory' AND
classid = 1936288879` the session holding it (1936288879 is "silo").
If that session belongs to no running pod, `SELECT
pg_terminate_backend(pid)` frees the lease, and the hub takes it
within five seconds.

**Database errors** (critical). The database did not answer, or the
stores failed, for 5 minutes. Check the proxy's logs, the instance in
the console (Cloud SQL → `silo-hub`: running? maintenance? out of
disk or connections?), and that the hub's IAM database user can still
log in: the proxy's logs say if its token was refused, which is
`roles/cloudsql.instanceUser`, the IAM user or
`cloudsql.iam_authentication` gone (terraform puts each back). A pod
that will not start at all is usually the `migrate` container: its
logs, and whether `migrate-url`'s password still matches (3.3). If the
instance is gone or corrupt, restore (4).

## 6. Lost tasks

A running task whose node went away is `lost`, and stays with its node
until the node resumes it (hub.md §6.2). A team admin can make the hub
requeue the team's lost tasks after a grace (hub.md §6.3):

```sh
# Through the API (behind IAP), as a team admin:
#   PUT /v1/teams/infra/settings {"lost_requeue_after_seconds": 7200}
# Or on the database, from the pod:
kubectl -n silo-hub exec deploy/silo-hub -c hub -- /silo hub team -id infra -lost-requeue-after 2h
```

At least five minutes; 0 turns it off. A human can always requeue one
lost task by hand: `POST /v1/tasks/{id}/requeue`. Either way, the node
that had it may still be running it, and is not told; check with the
node's owner first if the duplicate would matter.
