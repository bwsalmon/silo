#!/usr/bin/env bash
# record.sh rewrites the vmcreate-*.args files in this directory: what
# `granule vm create` passes to `docker run` for the cases the granule
# driver's TestVMCreateMatchesGranule checks, recorded through granule's
# own fake docker (pkg/guestboot/testdata/fakedocker), one argument per
# line.
#
# Run it with GRANULE_SRC set to a gke-labs/granule checkout at the
# commit granule.Commit pins, after bumping the pin, and commit what it
# writes. The test then says what the driver has to change.
set -euo pipefail

: "${GRANULE_SRC:?set GRANULE_SRC to a gke-labs/granule checkout}"
here=$(cd "$(dirname "$0")" && pwd)
pin=$(sed -n 's/^const Commit = "\([0-9a-f]*\)".*/\1/p' "$here/../vm.go")
head=$(git -C "$GRANULE_SRC" rev-parse HEAD)
if [[ $head != "$pin"* ]]; then
	echo "record.sh: $GRANULE_SRC is at $head, and the driver pins $pin" >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
(cd "$GRANULE_SRC" && go build -o "$work/granule" ./cmd/granule && go build -o "$work/docker" ./pkg/guestboot/testdata/fakedocker)

# record NAME VM-ID FLAG... runs one create and writes NAME.args.
record() {
	local name=$1 id=$2
	shift 2
	rm -f "$work/log"
	FAKEDOCKER_LOG=$work/log GRANULE_DOCKER=$work/docker \
		"$work/granule" vm create "$id" -state-dir "$work/state-$name" "$@" >/dev/null
	# The log has one call per line, arguments split by \x1f; keep the
	# `docker run`.
	awk -F'\x1f' '$1 == "run" { for (i = 1; i <= NF; i++) print $i }' "$work/log" >"$here/$name.args"
}

# What the driver always asks for: its own image, resources, disk,
# the gateway as DNS, no control link and no IPv6. The two -v options
# are the driver's mounts: the wrapped image's guest half and the
# container's I/O directory.
common=(
	-kontur-image silo-granule
	-disk-mode overlay
	-control-cidr ''
	-ipv6 off
	-shutdown-timeout 10s
	-docker-run-opt -v -docker-run-opt /wraps/sha256-0123:/var/lib/granule/guest:ro
)

record vmcreate-basic c1 "${common[@]}" \
	-docker-run-opt -v -docker-run-opt /io/c1:/run/silo \
	-cpus 1 -memory-mb 512 -disk-size-mb 10240 -dns 10.88.0.1 \
	-e 'CHV_CONTAINER_OVERRIDE={"entrypoint":["/bin/sh","-c","echo hi"],"cmd":[],"env":["PATH=/usr/bin:/bin","A=b"],"user":"1000:1000","working_dir":"/srv"}' \
	-e GRANULE_WORKLOAD_STDOUT=/run/silo/stdout \
	-e GRANULE_WORKLOAD_STDERR=/run/silo/stderr \
	-e GRANULE_WORKLOAD_EXIT_FILE=/run/silo/exit.json

record vmcreate-tty-volumes c2 "${common[@]}" \
	-docker-run-opt -v -docker-run-opt /io/c2:/run/silo \
	-cpus 3 -memory-mb 2048 -disk-size-mb 20480 -dns 10.88.4.1 \
	-e 'CHV_CONTAINER_OVERRIDE={"entrypoint":["bash"],"cmd":[],"env":["TERM=xterm"]}' \
	-e GRANULE_WORKLOAD_STDOUT=/run/silo/stdout \
	-e GRANULE_WORKLOAD_EXIT_FILE=/run/silo/exit.json \
	-e GRANULE_WORKLOAD_STDIN=/run/silo/stdin \
	-e GRANULE_WORKLOAD_TTY=true \
	-e GRANULE_WORKLOAD_RESIZE=/run/silo/resize \
	-e 'CHV_VOLUMES=tag=v0,socket=/run/silo/fs/v0.sock,destination=/data;tag=v1,socket=/run/silo/fs/v1.sock,destination=/etc/app,readonly=on'
