#!/bin/sh
# The builder runs of docs/silo/local-registry.md §13, step 35, as they
# were made for §9: kaniko (built from the Chainguard fork) and rootless
# BuildKit, each in gVisor with silod's runsc flags, building Dockerfile
# here (a RUN that chowns a file to uid 1234 and removes one), pushing to
# the registry and pulling alpine through its cache. The registry is
# pkg/registry's handler and cache, served by TestBuildersLive on the
# docker bridge's address at port 80, as silo team-a/x.
#
# Needs: docker (root daemon) with a "runsc" runtime whose runtimeArgs
# are --platform=systrap --network=sandbox --overlay2=root:self
# --directfs=true (rootless.RunscFlags), runsc and its gvisor-bin/ from
# the release CI pins, Go, sudo, and Docker Hub reachable. The silo
# facade's own limits are what each run passes: default capabilities,
# no-new-privileges, no seccomp, AppArmor or systempaths options.
# Mounting this script read-only is a convenience of this rig; in a
# silo the script would come in a volume or the image.
#
#   pkg/registry/testdata/builders/run.sh            # every run
#   BRIDGE=10.240.0.1 KANIKO=v1.25.19 ...            # overrides
set -eu
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../.." && pwd)
BRIDGE=${BRIDGE:-$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')}
KANIKO=${KANIKO:-v1.25.19}
BUILDKIT=${BUILDKIT:-moby/buildkit:v0.34.0-rootless}
work=$(mktemp -d)
host="--add-host registry.silo.internal:$BRIDGE"
sandbox="--runtime=runsc --security-opt no-new-privileges"

# The registry.
(cd "$repo" && go test -c -o "$work/registry.test" ./pkg/registry/)
sudo env SILO_BUILDERS_ADDR="$BRIDGE:80" SILO_BUILDERS_STOP="$work/stop" HOME=/root \
	"$work/registry.test" -test.run TestBuildersLive -test.v -test.timeout 1h >"$work/registry.log" 2>&1 &
trap 'touch "$work/stop"; sleep 2; echo "registry log: $work/registry.log"' EXIT
sleep 2

# kaniko from the fork's source, packaged as its kaniko-slim stage is.
GOFLAGS=-mod=mod go mod download -json "github.com/chainguard-dev/kaniko@$KANIKO" >"$work/kaniko.json"
src=$(sed -n 's/.*"Dir": "\(.*\)".*/\1/p' "$work/kaniko.json")
mkdir "$work/kaniko"
(cd "$src" && CGO_ENABLED=0 go build -trimpath -o "$work/kaniko/executor" ./cmd/executor)
cp "$src/files/nsswitch.conf" /etc/ssl/certs/ca-certificates.crt "$work/kaniko/"
cat >"$work/kaniko/Dockerfile" <<'EOF'
FROM scratch
COPY --chmod=777 executor /kaniko/executor
COPY ca-certificates.crt /kaniko/ssl/certs/
COPY nsswitch.conf /etc/nsswitch.conf
ENV HOME=/root USER=root PATH=/usr/local/bin:/kaniko SSL_CERT_DIR=/kaniko/ssl/certs DOCKER_CONFIG=/kaniko/.docker/
WORKDIR /workspace
ENTRYPOINT ["/kaniko/executor"]
EOF
docker build -q -t "kaniko-fork:$KANIKO" "$work/kaniko"

ctx() { tar -C "$here" -cz Dockerfile; }
kaniko() { ctx | docker run -i --rm $sandbox $host "kaniko-fork:$KANIKO" --context tar://stdin \
	--insecure-registry registry.silo.internal "$@" 2>&1 | grep -E 'INFO|WARN|ERRO|error' || true; }

echo "== kaniko, base named in full: through the cache"
sed 's#^FROM alpine#FROM registry.silo.internal/docker.io/library/alpine#' "$here/Dockerfile" >"$work/Dockerfile.full"
tar -C "$work" -cz --transform 's/Dockerfile.full/Dockerfile/' Dockerfile.full |
	docker run -i --rm $sandbox $host "kaniko-fork:$KANIKO" --context tar://stdin \
		--insecure-registry registry.silo.internal --destination registry.silo.internal/team-a/x/app:kaniko 2>&1 | grep -E 'INFO|WARN|ERRO|error' || true
echo "== kaniko, --registry-map to .../docker.io: goes to Docker Hub, not the cache"
kaniko --no-push --registry-map index.docker.io=registry.silo.internal/docker.io --skip-default-registry-fallback
echo "== kaniko, --registry-map to a dotless path: honoured (and 404 here)"
kaniko --no-push --registry-map index.docker.io=registry.silo.internal/mirror --skip-default-registry-fallback

bk() { tag=$1; shift; ctx | docker run -i --rm --runtime=runsc $host "$@" -e TAG="$tag" \
	-e BKFLAGS="--oci-worker-no-process-sandbox --oci-worker-snapshotter=native" \
	-v "$here/buildkit.sh:/buildkit.sh:ro" --entrypoint sh "$BUILDKIT" /buildkit.sh 2>&1 | tail -6 || true; }
echo "== rootless BuildKit, no-new-privileges (the facade): newuidmap fails"
bk bk-nnp --security-opt no-new-privileges
echo "== rootless BuildKit, new privileges allowed: builds and pushes"
bk bk --security-opt no-new-privileges=false
echo "== BuildKit as root, no-new-privileges: no CAP_SYS_ADMIN for its mounts"
bk bk-root --security-opt no-new-privileges --user 0 -e HOME=/root

echo "== what was pushed"
for tag in kaniko bk; do
	m=$(curl -sf -H 'Accept: application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json' \
		"http://$BRIDGE/v2/team-a/x/app/manifests/$tag") || { echo "$tag: not pushed"; continue; }
	l=$(echo "$m" | python3 -c 'import json,sys; print(json.load(sys.stdin)["layers"][-1]["digest"])')
	echo "$tag:"; curl -sf "http://$BRIDGE/v2/team-a/x/app/blobs/$l" | tar tvz | grep -E 'owned|wh'
done
