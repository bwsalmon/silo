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
done
echo "manifests valid"
