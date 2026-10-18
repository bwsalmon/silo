#!/usr/bin/env bash
# End-to-end container packaging, production guardrail, and live 2-replica
# Kubernetes (kind) deployment test for Silo Hub.
#
# Usage:
#   deploy/kubernetes/hub/e2e-kind.sh
#
# Environment variables:
#   SILO_HUB_IMAGE        Container image tag to build/test (default: silo-hub:ci)
#   SILO_SKIP_IMAGE_BUILD Set to 1 to skip `docker build` if SILO_HUB_IMAGE is already built

set -euo pipefail

cd "$(dirname "$0")/../../.."
hub_dir="deploy/kubernetes/hub"

for tool in docker kind kubectl go curl openssl; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "missing required tool: $tool" >&2
		exit 1
	fi
done

image="${SILO_HUB_IMAGE:-silo-hub:ci}"
cluster="silo-hub-e2e-$$"
ctx="kind-${cluster}"
work="$(mktemp -d)"

cleanup() {
	docker rm -f "silo-hub-pkg-$$" >/dev/null 2>&1 || true
	kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Phase 1: Production Dockerfile packaging & guardrail verification
# ---------------------------------------------------------------------------
if [[ "${SILO_SKIP_IMAGE_BUILD:-0}" != "1" ]]; then
	echo "=== Building production container image ($image) ==="
	build_args=(--network host)
	if [[ -n "$(go env GOPROXY 2>/dev/null || true)" ]]; then
		build_args+=(--build-arg "GOPROXY=$(go env GOPROXY)")
	fi
	if command -v npm >/dev/null 2>&1 && [[ -n "$(npm config get registry 2>/dev/null || true)" ]]; then
		build_args+=(--build-arg "NPM_REGISTRY=$(npm config get registry)")
	fi
	docker build "${build_args[@]}" -t "$image" .
fi

echo "=== Verifying distroless image metadata & nonroot user ==="
img_user="$(docker image inspect "$image" --format '{{.Config.User}}')"
if [[ "$img_user" != "65532:65532" ]]; then
	echo "expected User 65532:65532 in image config, got: $img_user" >&2
	exit 1
fi
if docker run --rm --entrypoint /bin/sh "$image" -c "true" >/dev/null 2>&1; then
	echo "expected distroless image without /bin/sh" >&2
	exit 1
fi

echo "=== Verifying all CLI subcommands in distroless container ==="
for pair in \
	"hub serve:-iap-audience" \
	"hub migrate:-runtime-role" \
	"hub team:-id" \
	"hub token:-owner" \
	"conformance:-only"; do
	subcmd="${pair%%:*}"
	expected_flag="${pair#*:}"
	# Go's flag.FlagSet returns exit code 2 on -h; capture output and verify the
	# subcommand's help text is printed.
	# shellcheck disable=SC2086
	help_out="$(docker run --rm "$image" $subcmd -h 2>&1 || true)"
	if [[ "$help_out" != *"$expected_flag"* ]]; then
		echo "subcommand '$subcmd -h' did not contain '$expected_flag': $help_out" >&2
		exit 1
	fi
done

# Run functional CLI subcommands (hub migrate, hub team, hub token, conformance)
# directly inside the distroless container.
sqlite_dir="$work/sqlite"
mkdir -p "$sqlite_dir"
chmod 0777 "$sqlite_dir"
docker run --rm -v "$sqlite_dir:/tmp/db" "$image" \
	hub migrate -database "sqlite:/tmp/db/test.db"
team_out="$(docker run --rm -v "$sqlite_dir:/tmp/db" "$image" \
	hub team -database "sqlite:/tmp/db/test.db" -id pkg-team -name "Package Team" -admin user:alice@example.com -repo bwsalmon/silo)"
if [[ "$team_out" != *"team pkg-team: 1 admins, 0 members and 1 repositories added"* ]]; then
	echo "unexpected hub team output: $team_out" >&2
	exit 1
fi
token_out="$(docker run --rm -v "$sqlite_dir:/tmp/db" "$image" \
	hub token -database "sqlite:/tmp/db/test.db" -owner alice@example.com -node pkg-node | tr -d '\r\n')"
if [[ "$token_out" != silo-node_* ]]; then
	echo "unexpected hub token output: $token_out" >&2
	exit 1
fi
docker run --rm "$image" conformance -only register -- /silo conformance grain

echo "=== Verifying embedded web UI at / in distroless container ==="
docker run -d --name "silo-hub-pkg-$$" -p 127.0.0.1:18080:8080 -p 127.0.0.1:19091:9091 \
	"$image" hub serve \
	-addr :8080 \
	-metrics-addr :9091 \
	-database "sqlite::memory:" \
	-dev-user-header X-User >/dev/null

ui_ready=0
for _ in $(seq 1 30); do
	if curl -fsS http://127.0.0.1:18080/healthz >/dev/null 2>&1 &&
		curl -fsS http://127.0.0.1:18080/readyz >/dev/null 2>&1; then
		ui_ready=1
		break
	fi
	sleep 0.5
done
if [[ "$ui_ready" -ne 1 ]]; then
	docker logs "silo-hub-pkg-$$" >&2 || true
	echo "silo-hub container failed to become ready" >&2
	exit 1
fi
ui_html="$(curl -fsS http://127.0.0.1:18080/)"
if [[ "$ui_html" != *"<!doctype html>"* && "$ui_html" != *"<!DOCTYPE html>"* ]]; then
	echo "expected HTML document from /, got: $ui_html" >&2
	exit 1
fi
if [[ "$ui_html" != *'type="module"'* || "$ui_html" != *"/assets/"* ]]; then
	echo "expected embedded Vite asset bundle reference in / HTML, got: $ui_html" >&2
	exit 1
fi
asset_path="$(printf '%s\n' "$ui_html" | grep -oE '/assets/[^"'\'' >]+' | head -n1)"
if [[ -z "$asset_path" ]]; then
	echo "could not extract /assets/ path from index.html" >&2
	exit 1
fi
curl -fsS "http://127.0.0.1:18080${asset_path}" >/dev/null
docker rm -f "silo-hub-pkg-$$" >/dev/null

echo "=== Verifying Kubernetes production guardrails (KUBERNETES_SERVICE_HOST) ==="
# 1. CheckDatabase: refuses SQLite when KUBERNETES_SERVICE_HOST is set.
if out="$(docker run --rm -e KUBERNETES_SERVICE_HOST=10.96.0.1 "$image" \
	hub serve -database "sqlite::memory:" -webhook-secret "s" 2>&1)"; then
	echo "expected CheckDatabase to refuse sqlite under KUBERNETES_SERVICE_HOST" >&2
	exit 1
elif [[ "$out" != *"is SQLite, which is refused on Kubernetes"* ]]; then
	echo "unexpected CheckDatabase error output: $out" >&2
	exit 1
fi

# 2. CheckDevSignOn: refuses -dev-user-header when KUBERNETES_SERVICE_HOST is set.
if out="$(docker run --rm -e KUBERNETES_SERVICE_HOST=10.96.0.1 "$image" \
	hub serve -database "postgres://u:p@127.0.0.1:5432/silo" -dev-user-header X-User -webhook-secret "s" 2>&1)"; then
	echo "expected CheckDevSignOn to refuse -dev-user-header under KUBERNETES_SERVICE_HOST" >&2
	exit 1
elif [[ "$out" != *"the development user header is refused on Kubernetes"* ]]; then
	echo "unexpected CheckDevSignOn error output: $out" >&2
	exit 1
fi

# 3. CheckWebhookSecret: refuses empty webhook secret when KUBERNETES_SERVICE_HOST is set.
if out="$(docker run --rm -e KUBERNETES_SERVICE_HOST=10.96.0.1 "$image" \
	hub serve -database "postgres://u:p@127.0.0.1:5432/silo" 2>&1)"; then
	echo "expected CheckWebhookSecret to refuse empty webhook secret under KUBERNETES_SERVICE_HOST" >&2
	exit 1
elif [[ "$out" != *"a webhook secret (-webhook-secret or -webhook-secret-file) is required on Kubernetes"* ]]; then
	echo "unexpected CheckWebhookSecret error output: $out" >&2
	exit 1
fi

# ---------------------------------------------------------------------------
# Phase 2: Build helper images & boot throwaway kind cluster
# ---------------------------------------------------------------------------
echo "=== Building kind helper images (silo-postgres:kind, silo-github-mock:kind) ==="
if ! docker image inspect postgres:16-alpine >/dev/null 2>&1; then
	docker pull postgres:16-alpine
fi
# Build a single-platform tag from postgres:16-alpine so `kind load docker-image`
# does not attempt to export unpulled architectures from a multi-arch manifest list.
printf 'FROM postgres:16-alpine\n' | docker build -q -t silo-postgres:kind - >/dev/null

# Compile the in-cluster GitHub API mock from ./deploy/kubernetes/hub.
mock_dir="$work/github-mock"
mkdir -p "$mock_dir"
CGO_ENABLED=0 go test -c -o "$mock_dir/github-mock" ./deploy/kubernetes/hub
cat >"$mock_dir/Dockerfile" <<'EOF'
FROM gcr.io/distroless/static-debian12:nonroot
COPY github-mock /github-mock
USER 65532:65532
ENTRYPOINT ["/github-mock", "-addr=:8080"]
EOF
docker build -q -t silo-github-mock:kind "$mock_dir" >/dev/null

echo "=== Creating throwaway kind cluster ($cluster) ==="
kind create cluster --name "$cluster" --wait 90s

echo "=== Loading container images into kind cluster ==="
kind load docker-image --name "$cluster" "$image"
kind load docker-image --name "$cluster" silo-postgres:kind
kind load docker-image --name "$cluster" silo-github-mock:kind

# ---------------------------------------------------------------------------
# Phase 3: Install CRDs, Secrets, and deploy/kubernetes/hub/kind overlay
# ---------------------------------------------------------------------------
echo "=== Installing Gateway API, GKE, and Managed Service for Prometheus CRDs ==="
gateway_api=sigs.k8s.io/gateway-api@v1.3.0
gke_gateway_api=github.com/GoogleCloudPlatform/gke-gateway-api@v1.4.0
gmp=github.com/GoogleCloudPlatform/prometheus-engine@v0.17.2
crds="$work/crds"
mkdir -p "$crds"
moddir() { (cd "$work" && GOFLAGS=-mod=mod go mod download -json "$1") | sed -n 's/^\t"Dir": "\(.*\)",$/\1/p'; }
gw="$(moddir "$gateway_api")"
gke="$(moddir "$gke_gateway_api")"
prom="$(moddir "$gmp")"
cp "$gw"/config/crd/standard/gateway.networking.k8s.io_{gatewayclasses,gateways,httproutes,referencegrants}.yaml "$crds/"
cp "$gke"/config/crd/networking.gke.io_{gcpbackendpolicies,healthcheckpolicies}.yaml "$crds/"
cp "$prom"/charts/operator/crds/monitoring.googleapis.com_podmonitorings.yaml "$crds/"

kubectl --context "$ctx" apply --server-side -f "$crds" >/dev/null
kubectl --context "$ctx" wait --for=condition=Established crd --all --timeout=60s >/dev/null

echo "=== Creating silo-hub namespace and runtime Secrets ==="
kubectl --context "$ctx" create namespace silo-hub

claims_key_file="$work/claims.pem"
gh_key_file="$work/github-app.pem"
(umask 077; openssl ecparam -name prime256v1 -genkey -noout -out "$claims_key_file")
(umask 077; openssl genrsa -out "$gh_key_file" 2048 2>/dev/null)

webhook_secret="kind-e2e-webhook-secret"
internal_key="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

kubectl --context "$ctx" -n silo-hub create secret generic silo-hub-db \
	--from-literal=migrate-url='postgres://silo_migrate:silo-migrate-kind@postgres.silo-hub.svc.cluster.local:5432/silo?sslmode=disable&connect_timeout=3' \
	--from-literal=url='postgres://silo_runtime:silo-runtime-kind@postgres.silo-hub.svc.cluster.local:5432/silo?sslmode=disable&connect_timeout=3'

kubectl --context "$ctx" -n silo-hub create secret generic silo-hub-internal \
	--from-literal=key="$internal_key"

kubectl --context "$ctx" -n silo-hub create secret generic silo-hub-claims \
	--from-file=key="$claims_key_file"

kubectl --context "$ctx" -n silo-hub create secret generic silo-hub-github \
	--from-literal=webhook-secret="$webhook_secret" \
	--from-file=app-key="$gh_key_file"

echo "=== Starting in-cluster PostgreSQL and GitHub API mock ==="
kubectl --context "$ctx" -n silo-hub apply -f "$hub_dir/kind/postgres.yaml"
kubectl --context "$ctx" -n silo-hub rollout status deployment/postgres --timeout=120s
kubectl --context "$ctx" -n silo-hub rollout status deployment/github-mock --timeout=120s

echo "=== Applying deploy/kubernetes/hub/kind kustomize overlay ==="
kubectl --context "$ctx" apply -k "$hub_dir/kind"

if [[ "$image" != "silo-hub:ci" ]]; then
	kubectl --context "$ctx" -n silo-hub set image deployment/silo-hub \
		migrate="$image" hub="$image"
fi

echo "=== Waiting for 2-replica Silo Hub Deployment ==="
if ! kubectl --context "$ctx" -n silo-hub rollout status deployment/silo-hub --timeout=180s; then
	kubectl --context "$ctx" -n silo-hub get pods -o wide >&2 || true
	kubectl --context "$ctx" -n silo-hub describe pods -l app=silo-hub >&2 || true
	for p in $(kubectl --context "$ctx" -n silo-hub get pods -l app=silo-hub -o jsonpath='{.items[*].metadata.name}'); do
		echo "--- migrate logs ($p) ---" >&2
		kubectl --context "$ctx" -n silo-hub logs "$p" -c migrate >&2 || true
		echo "--- hub logs ($p) ---" >&2
		kubectl --context "$ctx" -n silo-hub logs "$p" -c hub >&2 || true
	done
	exit 1
fi

# ---------------------------------------------------------------------------
# Phase 4: Verify migrate init container, least-privileged DB role & bootstrap
# ---------------------------------------------------------------------------
echo "=== Verifying migrate init container, downward API, and pod securityContext ==="
pods="$(kubectl --context "$ctx" -n silo-hub get pods -l app=silo-hub -o jsonpath='{.items[*].metadata.name}')"
pod_count="$(wc -w <<<"$pods" | tr -d ' ')"
if [[ "$pod_count" -ne 2 ]]; then
	echo "expected 2 silo-hub pods, got $pod_count ($pods)" >&2
	exit 1
fi

for pod in $pods; do
	init_exit="$(kubectl --context "$ctx" -n silo-hub get pod "$pod" -o jsonpath='{.status.initContainerStatuses[?(@.name=="migrate")].state.terminated.exitCode}')"
	if [[ "$init_exit" != "0" ]]; then
		echo "expected migrate init container exitCode 0 on pod $pod, got: $init_exit" >&2
		kubectl --context "$ctx" -n silo-hub logs "$pod" -c migrate >&2 || true
		exit 1
	fi
	migrate_logs="$(kubectl --context "$ctx" -n silo-hub logs "$pod" -c migrate)"
	if [[ "$migrate_logs" != *"hub: tables migrated; silo_runtime may read and write them"* ]]; then
		echo "expected 'hub: tables migrated; silo_runtime may read and write them' in migrate logs on $pod, got: $migrate_logs" >&2
		exit 1
	fi
	sec_check="$(kubectl --context "$ctx" -n silo-hub get pod "$pod" -o jsonpath='{.spec.securityContext.runAsNonRoot}:{.spec.securityContext.runAsUser}:{.spec.containers[0].securityContext.readOnlyRootFilesystem}')"
	if [[ "$sec_check" != "true:65532:true" ]]; then
		echo "unexpected pod securityContext on $pod: $sec_check (want true:65532:true)" >&2
		exit 1
	fi
done

echo "=== Verifying least-privileged silo_runtime role in PostgreSQL ==="
kubectl --context "$ctx" -n silo-hub exec deploy/postgres -- \
	psql -U silo_runtime -d silo -c "SELECT count(*) FROM teams;" >/dev/null

if ddl_out="$(kubectl --context "$ctx" -n silo-hub exec deploy/postgres -- \
	psql -U silo_runtime -d silo -c "CREATE TABLE should_fail (id int);" 2>&1)"; then
	echo "expected silo_runtime CREATE TABLE to fail, but it succeeded" >&2
	exit 1
elif [[ "$ddl_out" != *"permission denied for schema public"* ]]; then
	echo "unexpected silo_runtime CREATE TABLE error: $ddl_out" >&2
	exit 1
fi

if drop_out="$(kubectl --context "$ctx" -n silo-hub exec deploy/postgres -- \
	psql -U silo_runtime -d silo -c "DROP TABLE teams;" 2>&1)"; then
	echo "expected silo_runtime DROP TABLE to fail, but it succeeded" >&2
	exit 1
elif [[ "$drop_out" != *"must be owner of table"* ]]; then
	echo "unexpected silo_runtime DROP TABLE error: $drop_out" >&2
	exit 1
fi

echo "=== Running documented bootstrap flow (kubectl exec deploy/silo-hub) ==="
team_boot="$(kubectl --context "$ctx" -n silo-hub exec deploy/silo-hub -c hub -- \
	/silo hub team -id e2e -name "E2E Team" -admin user:alice@example.com -repo bwsalmon/silo)"
if [[ "$team_boot" != *"team e2e: 1 admins, 0 members and 1 repositories added"* ]]; then
	echo "unexpected bootstrap team output: $team_boot" >&2
	exit 1
fi

node_token="$(kubectl --context "$ctx" -n silo-hub exec deploy/silo-hub -c hub -- \
	/silo hub token -owner alice@example.com -node kind-node-1 | tr -d '\r\n')"
if [[ "$node_token" != silo-node_* ]]; then
	echo "unexpected bootstrap token output: $node_token" >&2
	exit 1
fi

# ---------------------------------------------------------------------------
# Phase 5: Run live 2-replica kind E2E test suite
# ---------------------------------------------------------------------------
echo "=== Running live 2-replica kind E2E test suite ==="
SILO_E2E_KIND=1 \
	SILO_KIND_CONTEXT="$ctx" \
	SILO_KIND_NODE_TOKEN="$node_token" \
	SILO_KIND_WEBHOOK_SECRET="$webhook_secret" \
	go test -v -count=1 -timeout 10m ./deploy/kubernetes/hub -run '^TestKindClusterE2E$'

echo "=== Live 2-replica kind E2E test passed ==="
