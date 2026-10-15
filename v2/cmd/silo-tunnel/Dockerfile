# The k8s location's gateway sidecar (design §5.4). Build it from v2/:
#
#   docker build -f cmd/silo-tunnel/Dockerfile -t ghcr.io/bwsalmon/silo-tunnel:v2 .
#
# The v2 README says how it is published, and how to use your own.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /silo-tunnel ./cmd/silo-tunnel

FROM scratch
COPY --from=build /silo-tunnel /silo-tunnel
USER 65532:65532
ENTRYPOINT ["/silo-tunnel"]
