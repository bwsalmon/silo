#!/usr/bin/env bash
# Validates the hub's manifests (docs/deploy.md) without a GKE cluster:
# each kustomization builds, and what it builds is accepted, with
# strict field validation, by a throwaway kind cluster's API server
# that has the Gateway API's CRDs and GKE's policy CRDs
# (GCPBackendPolicy, HealthCheckPolicy) and Managed Service for
# Prometheus's PodMonitoring installed. Nothing is created
# but the namespace: the rest is a server-side dry run.
#
# It needs kind, kubectl, docker and go (the CRDs are read from their
# Go modules, through the module proxy). grain CI runs it
# (.grain/workflows/deploy.yml).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
gateway_api=sigs.k8s.io/gateway-api@v1.3.0
gke_gateway_api=github.com/GoogleCloudPlatform/gke-gateway-api@v1.4.0
gmp=github.com/GoogleCloudPlatform/prometheus-engine@v0.17.2
cluster="${SILO_VALIDATE_CLUSTER:-silo-hub-validate}"

work="$(mktemp -d)"
cleanup() {
	kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

overlays=(base bsalmon-gke-dev)
for o in "${overlays[@]}"; do
	echo "building $o"
	kubectl kustomize "$here/$o" >"$work/$o.yaml"
done

# The CRDs, from their modules.
crds="$work/crds"
mkdir -p "$crds"
moddir() { (cd "$work" && GOFLAGS=-mod=mod go mod download -json "$1") | sed -n 's/^\t"Dir": "\(.*\)",$/\1/p'; }
gw="$(moddir "$gateway_api")"
gke="$(moddir "$gke_gateway_api")"
prom="$(moddir "$gmp")"
cp "$gw"/config/crd/standard/gateway.networking.k8s.io_{gatewayclasses,gateways,httproutes,referencegrants}.yaml "$crds/"
cp "$gke"/config/crd/networking.gke.io_{gcpbackendpolicies,healthcheckpolicies}.yaml "$crds/"
cp "$prom"/charts/operator/crds/monitoring.googleapis.com_podmonitorings.yaml "$crds/"

kind create cluster --name "$cluster" --wait 120s
ctx="kind-$cluster"
kubectl --context "$ctx" apply --server-side -f "$crds" >/dev/null
kubectl --context "$ctx" wait --for condition=established --timeout=60s crd --all >/dev/null
kubectl --context "$ctx" create namespace silo-hub --save-config >/dev/null

for o in "${overlays[@]}"; do
	echo "validating $o"
	kubectl --context "$ctx" apply --dry-run=server --validate=strict -f "$work/$o.yaml"
done

# What the API server's schemas cannot say.
for o in "${overlays[@]}"; do
	f="$work/$o.yaml"
	# Several pods, rolled without ever having fewer ready than the
	# replicas, kept serving through drains, and spread over zones and
	# GKE nodes (docs/hub.md §2.2.5).
	dep="$(awk 'BEGIN { RS = "\n---\n" } /\nkind: Deployment\n/' "$f")"
	grep -q '^  replicas: 2$' <<<"$dep" || { echo "$o: the hub must run two replicas"; exit 1; }
	{ grep -q 'type: RollingUpdate$' <<<"$dep" && grep -q 'maxSurge: 1$' <<<"$dep" && grep -q 'maxUnavailable: 0$' <<<"$dep"; } ||
		{ echo "$o: the hub must roll with RollingUpdate, maxSurge 1 and maxUnavailable 0"; exit 1; }
	pdb="$(awk 'BEGIN { RS = "\n---\n" } /\nkind: PodDisruptionBudget\n/' "$f")"
	{ grep -q 'minAvailable: 1$' <<<"$pdb" && grep -q 'app: silo-hub$' <<<"$pdb"; } || { echo "$o: a PodDisruptionBudget must keep one hub pod available"; exit 1; }
	{ grep -q 'topologyKey: topology.kubernetes.io/zone$' <<<"$dep" && grep -q 'topologyKey: kubernetes.io/hostname$' <<<"$dep"; } ||
		{ echo "$o: the hub's pods must spread over zones and nodes"; exit 1; }
	# Each pod names itself, and its internal address, in the shared
	# registry from the downward API.
	{ grep -A3 'name: SILO_HUB_POD$' <<<"$dep" | grep -q 'fieldPath: metadata.name$' &&
		grep -A3 'name: SILO_HUB_POD_IP$' <<<"$dep" | grep -q 'fieldPath: status.podIP$' &&
		grep -A1 'name: SILO_HUB_POD_ADDR$' <<<"$dep" | grep -q 'value: $(SILO_HUB_POD_IP):9092$'; } ||
		{ echo "$o: SILO_HUB_POD and SILO_HUB_POD_ADDR must come from the downward API"; exit 1; }
	grep -q 'containerPort: 9092$' <<<"$dep" || { echo "$o: the hub container must declare its internal port"; exit 1; }
	# Any pod serves any request: no session affinity, on the Services
	# or the load balancer.
	if grep -qiE 'sessionAffinity|consistentHash' "$f"; then
		echo "$o: the hub's backends must not have session affinity"; exit 1
	fi
	[ "$(grep -c 'requestPath: /readyz' "$f")" = 2 ] || { echo "$o: both backends' health checks must be /readyz"; exit 1; }
	# The node UI hosts are IAP's alone: their route goes to silo-hub
	# and never to the nodes' backend.
	route="$(awk 'BEGIN { RS = "\n---\n" } /kind: HTTPRoute/ && /\n  name: silo-hub-node-ui\n/' "$f")"
	{ grep -q -- '- name: silo-hub$' <<<"$route" && ! grep -q 'silo-hub-nodes' <<<"$route"; } || { echo "$o: the node UI hosts' route must go to the IAP backend alone"; exit 1; }
	grep -q 'kind: PodMonitoring' "$f" || { echo "$o: the hub's metrics must be scraped"; exit 1; }
	grep -q -- '-metrics-addr=:9091' "$f" || { echo "$o: the hub must serve metrics on 9091"; exit 1; }
	# The metrics stay off the load balancer: no Service may expose them.
	if awk '/^kind: Service$/,/^---$/' "$f" | grep -qE 'metrics|9091'; then
		echo "$o: a Service exposes the metrics port"; exit 1
	fi
	# Nor the internal listener (docs/hub.md §2.2.3), which only the
	# hub's own pods reach, with the key from its Secret.
	grep -q -- '-internal-addr=:9092' "$f" || { echo "$o: the hub must serve its internal listener on 9092"; exit 1; }
	if awk '/^kind: Service$/,/^---$/' "$f" | grep -qE 'internal|9092'; then
		echo "$o: a Service exposes the internal port"; exit 1
	fi
	grep -q 'secretName: silo-hub-internal$' "$f" && grep -q 'value: /var/run/secrets/silo-hub-internal/key$' "$f" ||
		{ echo "$o: the hub must read its internal key from the silo-hub-internal Secret"; exit 1; }
	# Task claims are signed with the key from its Secret, as an
	# issuer, and their keys are published past IAP (docs/hub.md §6.4).
	grep -q 'secretName: silo-hub-claims$' "$f" && grep -A1 'name: SILO_HUB_CLAIMS_KEY_FILE$' <<<"$dep" | grep -q 'value: /var/run/secrets/silo-hub-claims/key$' ||
		{ echo "$o: the hub must read its claims key from the silo-hub-claims Secret"; exit 1; }
	grep -q 'SILO_HUB_CLAIMS_ISSUER: https://' "$f" || { echo "$o: the hub must sign claims as an https:// issuer"; exit 1; }
	route="$(awk 'BEGIN { RS = "\n---\n" } /kind: HTTPRoute/ && /\n  name: silo-hub\n/' "$f")"
	# kustomize writes a rule's backendRefs before its matches.
	grep -B1 'value: /v1/claims/keys$' <<<"$route" | grep -q 'type: Exact$' && grep -B6 'value: /v1/claims/keys$' <<<"$route" | grep -q 'name: silo-hub-nodes$' ||
		{ echo "$o: the claims keys must reach the nodes' backend, as an exact path"; exit 1; }
	# Ingress is denied by default, and allowed to the hub's port only
	# from Google's load balancer and health-check ranges, and to its
	# metrics only from the Prometheus collectors (SA1-L5), and to its
	# internal listener only from the hub's own pods.
	np() { awk -v n="$1" 'BEGIN { RS = "\n---\n" } /kind: NetworkPolicy/ && $0 ~ "\n  name: " n "\n"' "$f"; }
	deny="$(np default-deny-ingress)"
	{ grep -q 'podSelector: {}' <<<"$deny" && ! grep -q 'ingress:' <<<"$deny"; } || { echo "$o: ingress must be denied by default"; exit 1; }
	lb="$(np silo-hub-from-load-balancer)"
	{ [ "$(grep -c 'cidr:' <<<"$lb")" = 2 ] && grep -q 'cidr: 35.191.0.0/16$' <<<"$lb" && grep -q 'cidr: 130.211.0.0/22$' <<<"$lb" &&
		[ "$(grep -c 'port:' <<<"$lb")" = 1 ] && grep -q 'port: 8080$' <<<"$lb"; } || { echo "$o: 8080 must be open to the load balancer's ranges alone"; exit 1; }
	prom="$(np silo-hub-metrics-from-prometheus)"
	{ grep -q 'kubernetes.io/metadata.name: gmp-system$' <<<"$prom" && [ "$(grep -c 'port:' <<<"$prom")" = 1 ] && grep -q 'port: 9091$' <<<"$prom"; } || { echo "$o: 9091 must be open to the Prometheus collectors alone"; exit 1; }
	internal="$(np silo-hub-internal-from-hub)"
	{ ! grep -q 'namespaceSelector\|ipBlock' <<<"$internal" && grep -q 'app: silo-hub$' <<<"$(sed -n '/from:/,$p' <<<"$internal")" &&
		[ "$(grep -c 'port:' <<<"$internal")" = 1 ] && grep -q 'port: 9092$' <<<"$internal"; } || { echo "$o: 9092 must be open to the hub's pods alone"; exit 1; }
	[ "$(grep -c 'kind: NetworkPolicy' "$f")" = 4 ] || { echo "$o: a NetworkPolicy beyond the four checked here"; exit 1; }
	# The hub serves as its least-privileged role and only the migrate
	# init container has the schema owner's password (SA1-L8).
	grep -q 'auto-iam-authn=true$' "$f" || { echo "$o: the proxy must log the hub in as its IAM database user"; exit 1; }
	[ "$(grep -c 'key: migrate-url$' "$f")" = 1 ] || { echo "$o: only the migrate container may have the migrate URL"; exit 1; }
	grep -A1 'name: SILO_HUB_MIGRATE$' "$f" | grep -q "value: \"false\"$" || { echo "$o: the hub must serve with SILO_HUB_MIGRATE=false"; exit 1; }
	# The store is PostgreSQL on Cloud SQL, through the proxy: never
	# SQLite, which `silo hub serve` refuses on Kubernetes anyway, and
	# never the development mode that would allow it elsewhere.
	if grep -qiE 'sqlite|SILO_HUB_DEV|-dev([= ]|$)|-dev-user-header' "$f"; then
		echo "$o: the hub must not run on SQLite or in development mode"; exit 1
	fi
	{ grep -A4 'name: SILO_HUB_DATABASE$' <<<"$dep" | grep -q 'key: url$' && grep -A4 'name: SILO_HUB_DATABASE$' <<<"$dep" | grep -q 'name: silo-hub-db$'; } ||
		{ echo "$o: the hub's database must be the silo-hub-db Secret's postgres:// url"; exit 1; }
	# The pod runs as a numeric non-root user, which runAsNonRoot can
	# check: a named USER makes the kubelet refuse to start it (SA1-L9).
	grep -q 'runAsUser: 65532$' "$f" || { echo "$o: the pod must run as user 65532"; exit 1; }
done
# The image's USER is numeric too.
grep -qE '^USER [0-9]+:[0-9]+$' "$here/../../../Dockerfile" || { echo "Dockerfile: USER must be numeric"; exit 1; }
echo "manifests valid"
