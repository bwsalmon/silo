#!/bin/bash
# The step-15 spike (docs/silo/local-backend.md §15, calls L27-L29),
# run by grain CI's `nsd` job as the unprivileged runner user. It uses
# only stock tools, so that it says whether the kernel and the runtimes
# allow the design, independently of silo-nsd's own code:
#
#  1. a holder: a user namespace mapped with newuidmap/newgidmap, a
#     private mount namespace and silo-net, with gw0 in it;
#  2. a container's user namespace nested in the holder's, its netns
#     inside that, both pinned by bind mounts in the holder's mount
#     namespace, and a veth from gw0 into it;
#  3. "silod" (this script's own uid, no capabilities, the host's
#     netns) making a bridge and an nftables table in silo-net through
#     NETLINK_ROUTE and NETLINK_NETFILTER sockets the holder opened and
#     passed with SCM_RIGHTS;
#  4. runc joining the pinned user namespace and netns by path;
#  5. runsc started inside the pinned user namespace with the netns
#     named in the spec (gVisor's caller-configured mode), and netstack
#     taking the veth's address.
#
# Usage: spike.sh <runsc binary>. Exits non-zero on the first failure.
set -euo pipefail
RUNSC=${1:?usage: spike.sh <runsc>}
HERE=$(cd "$(dirname "$0")" && pwd)
W=$(mktemp -d)
NFT=$(command -v nft || echo /usr/sbin/nft)
holder=
cleanup() { [ -n "$holder" ] && kill "$holder" 2>/dev/null; rm -rf "$W" 2>/dev/null || true; }
trap cleanup EXIT
step() { echo; echo "=== $*"; }

user=$(id -un); uid=$(id -u); gid=$(id -g)
IFS=: read -r _ subuid subcount < <(grep "^$user:" /etc/subuid)
IFS=: read -r _ subgid subgcount < <(grep "^$user:" /etc/subgid)

step "1. holder namespaces through newuidmap"
unshare -U -m -n --propagation private sleep infinity </dev/null >/dev/null 2>&1 &
holder=$!
for _ in $(seq 50); do [ "$(readlink /proc/$holder/ns/user)" != "$(readlink /proc/self/ns/user)" ] && break; sleep 0.1; done
newuidmap "$holder" 0 "$uid" 1 1 "$subuid" "$subcount"
newgidmap "$holder" 0 "$gid" 1 1 "$subgid" "$subgcount"
cat /proc/$holder/uid_map
hin() { nsenter -t "$holder" -U -m -n "$@"; }
hin mount -t tmpfs -o mode=0755 tmpfs /run
hin mkdir -p /run/silo/netns
hin ip link add gw0 type bridge
hin ip addr add 10.99.0.1/16 dev gw0
hin ip link set gw0 up
hin ip -br addr show gw0

step "2. a container's userns nested in the holder's, its netns inside, pinned"
hin bash -euc '
  unshare -U -n sleep infinity </dev/null >/dev/null 2>&1 & cp=$!
  for _ in $(seq 50); do [ "$(readlink /proc/$cp/ns/user)" != "$(readlink /proc/self/ns/user)" ] && break; sleep 0.1; done
  echo "0 1 65536" > /proc/$cp/uid_map
  echo "0 1 65536" > /proc/$cp/gid_map
  touch /run/silo/netns/c1.user /run/silo/netns/c1.net
  mount --bind /proc/$cp/ns/user /run/silo/netns/c1.user
  mount --bind /proc/$cp/ns/net /run/silo/netns/c1.net
  kill $cp
  ip link add veth-c1 type veth peer name eth0 netns /run/silo/netns/c1.net
  ip link set veth-c1 master gw0 up
  nsenter --net=/run/silo/netns/c1.net ip addr add 10.99.0.2/16 dev eth0
  nsenter --net=/run/silo/netns/c1.net ip link set eth0 up
  nsenter --net=/run/silo/netns/c1.net ip link set lo up
  nsenter --net=/run/silo/netns/c1.net ip -br addr show eth0
'

step "3. netlink and nftables from silod through passed sockets"
sock=$W/nl.sock
hin python3 "$HERE/spike_netlink.py" holder "$sock" &
for _ in $(seq 50); do [ -S "$sock" ] && break; sleep 0.1; done
grep -q CapEff:.0000000000000000 /proc/self/status && echo "silod has no capabilities"
python3 "$HERE/spike_netlink.py" silod "$sock"
wait %%
hin ip -br link show br-spike
hin "$NFT" list tables | grep -qx 'table inet spike'
if ip link show br-spike >/dev/null 2>&1; then echo "br-spike leaked into the host netns"; exit 1; fi
echo "bridge and table are in silo-net, not the host's"

step "4. runc joins the pinned userns and netns by path"
B=/run/silo/bundle
hin bash -euc "
  mkdir -p $B/rootfs/bin $B/rootfs/proc $B/rootfs/dev $B/rootfs/sys $B/rootfs/tmp
  cp \"\$(command -v busybox)\" $B/rootfs/bin/busybox
  for t in sh ip cat id; do ln -sf busybox $B/rootfs/bin/\$t; done
  cd $B && runc spec
  python3 '$HERE/spike_spec.py' $B
  chown -R 1:1 $B/rootfs
  runc --root /run/silo/runc run -b $B/runc spike-runc
" | tee "$W/runc.out"
grep -q 'inet 10.99.0.2/16' "$W/runc.out"
grep -qE '^ +0 +1 +65536$' "$W/runc.out"

step "5. runsc started inside the pinned userns, netns from the spec"
hin bash -euc "
  mkdir -p /run/silo/runsc && chown 1:1 /run/silo/runsc
  nsenter --user=/run/silo/netns/c1.user -- \
    '$RUNSC' --root /run/silo/runsc --ignore-cgroups --network=sandbox --platform=systrap run -bundle $B/runsc spike-runsc
" | tee "$W/runsc.out"
grep -q 'inet 10.99.0.2/16' "$W/runsc.out"

echo; echo "SPIKE OK"
