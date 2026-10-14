#!/bin/sh
# The vm half of docs/silo/local-registry.md §13, step 35 (35b): the
# builders of run.sh, each wrapped with `granule wrap` and booted as a
# granule VM container with the flags `granule vm create` gives one
# (--device /dev/kvm and /dev/net/tun, NET_ADMIN only, no-new-privileges
# on the VM container). The workload runs in granule-container's chroot
# in the guest, which is what silod's vm class runs it in. The registry
# is TestBuildersLive again, reached by the docker bridge's address:
# a wrapped workload sees the image's own /etc/hosts, so there is no
# --add-host here, and the address stands in for registry.silo.internal.
#
# Needs: what run.sh needs (kaniko-fork:$KANIKO is built by it), /dev/kvm
# and the kvm group, and granule at the driver's pin
# (pkg/backend/local/runtime/granule.Commit): its image as $GRANULE_IMAGE
# (docker build --target final in a checkout of that commit) and its
# granule-shim as $GRANULE_SHIM (go build ./cmd/granule-shim). The
# wrapped workload's process, user and environment are set per run with
# CHV_CONTAINER_OVERRIDE, as the driver sets them.
#
#   GRANULE_IMAGE=granule:3ca10a3 GRANULE_SHIM=~/bin/granule-shim \
#     pkg/registry/testdata/builders/vm.sh
set -eu
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../.." && pwd)
BRIDGE=${BRIDGE:-$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')}
KANIKO=${KANIKO:-v1.25.19}
BUILDKIT=${BUILDKIT:-moby/buildkit:v0.34.0-rootless}
GRANULE_IMAGE=${GRANULE_IMAGE:?granule image built at granule.Commit}
GRANULE_SHIM=${GRANULE_SHIM:?granule-shim built at granule.Commit}
work=$(mktemp -d)
vm="--rm --device /dev/kvm --device /dev/net/tun --cap-drop=ALL --cap-add=NET_ADMIN --security-opt no-new-privileges"

# The registry.
(cd "$repo" && go test -c -o "$work/registry.test" ./pkg/registry/)
sudo env SILO_BUILDERS_ADDR="$BRIDGE:80" SILO_BUILDERS_STOP="$work/stop" HOME=/root \
	"$work/registry.test" -test.run TestBuildersLive -test.v -test.timeout 1h >"$work/registry.log" 2>&1 &
trap 'touch "$work/stop"; sleep 2; echo "registry log: $work/registry.log"' EXIT
sleep 2

# The workload's own lines from the guest console.
out() { grep -a 'granule-container\[' | sed 's/^\[ *[0-9.]*\] granule-container\[[0-9]*\]: //' |
	grep -vE '^	|^(main|github\.com|runtime|google\.golang)' | tail -"${1:-8}" || true; }
wrap() { "$GRANULE_SHIM" wrap -kontur-image "$GRANULE_IMAGE" -t "$@" 2>&1 | tail -1; }

# Images with the context in them: granule's stdin path needs a FIFO the
# host writes, which this rig doesn't set up.
mkdir "$work/k" "$work/b" "$work/p"
sed "s#^FROM alpine#FROM $BRIDGE/docker.io/library/alpine#" "$here/Dockerfile" >"$work/k/Dockerfile.app"
printf 'FROM kaniko-fork:%s\nCOPY Dockerfile.app /workspace/Dockerfile\n' "$KANIKO" >"$work/k/Dockerfile"
docker build -q -t kaniko-ctx "$work/k"
wrap kaniko-vm kaniko-ctx
tar -C "$here" -cz Dockerfile >"$work/b/ctx.tgz"
sed "s/registry\.silo\.internal/$BRIDGE/g" "$here/buildkit.sh" >"$work/b/buildkit.sh"
cat >"$work/b/Dockerfile" <<EOF
FROM $BUILDKIT
COPY buildkit.sh ctx.tgz /
ENTRYPOINT ["sh", "-c", "export BKFLAGS=\"\$BUILDKITD_FLAGS\"; sh /buildkit.sh < /ctx.tgz"]
EOF
docker build -q -t buildkit-ctx "$work/b"
wrap buildkit-vm buildkit-ctx
cat >"$work/p/probe.sh" <<'EOF'
grep -E "NoNewPrivs|CapEff" /proc/self/status
echo "max_user_namespaces=$(cat /proc/sys/user/max_user_namespaces)"
unshare -U id 2>&1
sleep 30
EOF
printf 'FROM debian:bookworm-slim\nCOPY probe.sh /\nENTRYPOINT ["sh", "/probe.sh"]\n' >"$work/p/Dockerfile"
docker build -q -t probe-ctx "$work/p"
wrap probe-vm probe-ctx

kargs='"--context","dir:///workspace","--insecure-registry","'$BRIDGE'","--destination","'$BRIDGE'/team-a/x/app:kaniko-vm"'
echo "== kaniko without --force: refuses, no container markers in the guest"
docker run $vm -e CHV_CONTAINER_OVERRIDE='{"cmd":['"$kargs"']}' kaniko-vm 2>&1 | out 3
echo "== kaniko --force: builds, chowns, pulls through the cache, pushes"
docker run $vm -e CHV_CONTAINER_OVERRIDE='{"cmd":["--force",'"$kargs"']}' kaniko-vm 2>&1 | out 4

echo "== the chroot: guest root has every capability and new privileges, but no user namespace"
docker run -d --name probe-vm $vm probe-vm >/dev/null
sleep 12
docker logs probe-vm 2>&1 | out 4
echo "-- the same guest, outside the chroot (docker exec reaches the guest through granule's sh shim):"
docker exec probe-vm sh -c 'unshare -U id' 2>&1 || true
docker rm -f probe-vm >/dev/null

bkflags="--oci-worker-no-process-sandbox --oci-worker-snapshotter=native"
echo "== rootless BuildKit: rootlesskit can't make its user namespace"
docker run $vm -e CHV_CONTAINER_OVERRIDE='{"env":["TAG=bk-vm","BUILDKITD_FLAGS='"$bkflags"'"]}' buildkit-vm 2>&1 | out 8
echo "== BuildKit as root, cgroup2 mounted: runc can't make / a slave mount in the chroot"
docker run $vm -e CHV_CONTAINER_OVERRIDE='{"user":"0","entrypoint":["sh","-c","mount -t cgroup2 none /sys/fs/cgroup && export BKFLAGS=\"$BUILDKITD_FLAGS\" && sh /buildkit.sh < /ctx.tgz"],"env":["HOME=/root","TAG=bk-root-vm","BUILDKITD_FLAGS=--config /root/.config/buildkit/buildkitd.toml"]}' \
	buildkit-vm 2>&1 | out 40 | grep -E 'runc run failed|exited' | sort -u

echo "== what was pushed"
for tag in kaniko-vm bk-vm bk-root-vm; do
	m=$(curl -sf -H 'Accept: application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json' \
		"http://$BRIDGE/v2/team-a/x/app/manifests/$tag") || { echo "$tag: not pushed"; continue; }
	l=$(echo "$m" | python3 -c 'import json,sys; print(json.load(sys.stdin)["layers"][-1]["digest"])')
	echo "$tag:"; curl -sf "http://$BRIDGE/v2/team-a/x/app/blobs/$l" | tar tvz | grep -E 'owned|wh'
done
