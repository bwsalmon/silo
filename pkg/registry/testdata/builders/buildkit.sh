# Run inside moby/buildkit's rootless image by run.sh: the cache as
# Docker Hub's mirror and the registry over plain HTTP (local-registry.md
# §9), then a daemonless build of the Dockerfile on stdin, pushed.
set -e
mkdir -p ~/.config/buildkit /tmp/ctx
cat > ~/.config/buildkit/buildkitd.toml <<'T'
[registry."docker.io"]
  mirrors = ["registry.silo.internal"]
  http = true
[registry."registry.silo.internal"]
  http = true
T
tar -xz -C /tmp/ctx
id; grep -E "NoNewPrivs|CapEff" /proc/self/status
export BUILDKITD_FLAGS="${BKFLAGS}"
buildctl-daemonless.sh build --frontend dockerfile.v0 --local context=/tmp/ctx --local dockerfile=/tmp/ctx \
  --output type=image,name=registry.silo.internal/team-a/x/app:${TAG},push=true,registry.insecure=true
