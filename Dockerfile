# The hub's container image: `silo hub serve` (README.md, docs/deploy.md).
# grain CI builds it (.grain/workflows/deploy.yml); publishing it is a
# deployment's own step.
#
#   docker build -t silo-hub .
#   docker run -p 8080:8080 silo-hub hub serve -database sqlite::memory: -dev-user-header X-User
# The web UI's bundle (ui/, docs/ui.md), embedded by hub/ui. A builder
# that reaches npm through a registry of its own (grain CI's sandboxes
# do) passes it in: --build-arg NPM_REGISTRY=$(npm config get registry).
FROM node:20-bookworm-slim AS ui
ARG NPM_REGISTRY=https://registry.npmjs.org/
ENV npm_config_registry=$NPM_REGISTRY
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY ui ./
RUN npm run build

FROM golang:1.26-bookworm AS build
# A builder that reaches modules through a proxy of its own (grain CI's
# sandboxes do) passes it in: --build-arg GOPROXY=$(go env GOPROXY).
ARG GOPROXY=https://proxy.golang.org,direct
# Not GOTOOLCHAIN=local: go.mod's toolchain line is the Go the hub is
# built with, whatever 1.26 the builder image happens to be (SA1-L4).
ENV GOPROXY=$GOPROXY GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go version
COPY cmd cmd
COPY hub hub
COPY internal internal
COPY events events
COPY --from=ui /src/hub/ui/static/app hub/ui/static/app
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/silo ./cmd/silo

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/silo /silo
# Numeric, distroless's nonroot: the kubelet cannot check that a named
# user is not root, so runAsNonRoot refuses one (SA1-L9).
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/silo"]
CMD ["hub", "serve"]
