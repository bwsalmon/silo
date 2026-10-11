# silod's container image: silod serve, silod gateway, silod migrate and
# silod pause, one static binary (docs/silo/install.md, step 11). The
# control VM runs two containers of it, silod and the gateway, and the
# migrate VM one. Build it from the repository root:
#
#   make silod-image                       # this machine's platform
#   make silod-image SILOD_PLATFORMS=linux/amd64,linux/arm64 SILOD_PUSH=1 \
#       SILOD_IMAGE=<registry>/silod:<tag>
#
# and install it with silo install --image <registry>/silod@sha256:<digest>.
#
# The builder runs on the build machine's own platform and cross-compiles
# (silod is pure Go), so a multi-arch build needs no emulation.
#
# Both base images are pinned by digest as well as by tag, with the same
# pins as grain's Dockerfile.relay, for the reason its main Dockerfile gives.
ARG GO_VERSION=1.26.6
ARG GO_IMAGE=golang
ARG GO_IMAGE_DIGEST=sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36

FROM --platform=$BUILDPLATFORM ${GO_IMAGE}:${GO_VERSION}-bookworm@${GO_IMAGE_DIGEST} AS builder

WORKDIR /src

# Unset everywhere but a build that passes one (see grain's Dockerfile.relay).
ARG GOPROXY
ARG TARGETOS
ARG TARGETARCH
ARG RELEASE=dev

COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -buildvcs=false -trimpath \
    -ldflags="-s -w -X main.release=${RELEASE}" -o /out/silod ./cmd/silod

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=builder /out/silod /silod

# The control VM runs each container as a user of its own (silod and the
# gateway apart); nonroot is the default for anything else.
USER nonroot:nonroot

ENTRYPOINT ["/silod"]
