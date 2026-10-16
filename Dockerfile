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
ENV GOPROXY=$GOPROXY GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY hub hub
COPY internal internal
COPY events events
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/silo ./cmd/silo

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/silo /silo
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/silo"]
CMD ["hub", "serve"]
