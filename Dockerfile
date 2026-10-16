# The hub's container image: `silo hub serve` (README.md, docs/deploy.md).
# grain CI builds it (.grain/workflows/deploy.yml); publishing it is a
# deployment's own step.
#
#   docker build -t silo-hub .
#   docker run -p 8080:8080 silo-hub hub serve -database sqlite::memory: -dev-user-header X-User
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
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/silo ./cmd/silo

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/silo /silo
# Numeric, distroless's nonroot: the kubelet cannot check that a named
# user is not root, so runAsNonRoot refuses one (SA1-L9).
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/silo"]
CMD ["hub", "serve"]
