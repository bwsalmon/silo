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
	# One pod until docs/hub.md §2.2's stages R2 to R5 are built: a
	# node's connection lives on the one pod holding it.
	grep -q 'replicas: 1$' "$f" || { echo "$o: the hub must run one replica"; exit 1; }
	grep -q 'type: Recreate' "$f" || { echo "$o: the hub must roll with Recreate"; exit 1; }
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
	# Ingress is denied by default, and allowed to the hub's port only
	# from Google's load balancer and health-check ranges, and to its
	# metrics only from the Prometheus collectors (SA1-L5).
	np() { awk -v n="$1" 'BEGIN { RS = "\n---\n" } /kind: NetworkPolicy/ && $0 ~ "\n  name: " n "\n"' "$f"; }
	deny="$(np default-deny-ingress)"
	{ grep -q 'podSelector: {}' <<<"$deny" && ! grep -q 'ingress:' <<<"$deny"; } || { echo "$o: ingress must be denied by default"; exit 1; }
	lb="$(np silo-hub-from-load-balancer)"
	{ [ "$(grep -c 'cidr:' <<<"$lb")" = 2 ] && grep -q 'cidr: 35.191.0.0/16$' <<<"$lb" && grep -q 'cidr: 130.211.0.0/22$' <<<"$lb" &&
		[ "$(grep -c 'port:' <<<"$lb")" = 1 ] && grep -q 'port: 8080$' <<<"$lb"; } || { echo "$o: 8080 must be open to the load balancer's ranges alone"; exit 1; }
	prom="$(np silo-hub-metrics-from-prometheus)"
	{ grep -q 'kubernetes.io/metadata.name: gmp-system$' <<<"$prom" && [ "$(grep -c 'port:' <<<"$prom")" = 1 ] && grep -q 'port: 9091$' <<<"$prom"; } || { echo "$o: 9091 must be open to the Prometheus collectors alone"; exit 1; }
	[ "$(grep -c 'kind: NetworkPolicy' "$f")" = 3 ] || { echo "$o: a NetworkPolicy beyond the three checked here"; exit 1; }
	# The hub serves as its least-privileged role and only the migrate
	# init container has the schema owner's password (SA1-L8).
	grep -q 'auto-iam-authn=true$' "$f" || { echo "$o: the proxy must log the hub in as its IAM database user"; exit 1; }
	[ "$(grep -c 'key: migrate-url$' "$f")" = 1 ] || { echo "$o: only the migrate container may have the migrate URL"; exit 1; }
	grep -A1 'name: SILO_HUB_MIGRATE$' "$f" | grep -q "value: \"false\"$" || { echo "$o: the hub must serve with SILO_HUB_MIGRATE=false"; exit 1; }
	# The pod runs as a numeric non-root user, which runAsNonRoot can
	# check: a named USER makes the kubelet refuse to start it (SA1-L9).
	grep -q 'runAsUser: 65532$' "$f" || { echo "$o: the pod must run as user 65532"; exit 1; }
done
# The image's USER is numeric too.
grep -qE '^USER [0-9]+:[0-9]+$' "$here/../../../Dockerfile" || { echo "Dockerfile: USER must be numeric"; exit 1; }
echo "manifests valid"
