# The same steps .grain/workflows/tests.yml's `go` job runs, so `make`
# reproduces CI locally.

GO_IMAGE ?= golang
# Read out of go.mod rather than written down a second time here.
GO_VERSION := $(shell sed -n 's/^go[[:space:]]\{1,\}//p' go.mod)

.PHONY: all build test vet fmt silod-image silod-harness-image clean

all: fmt build vet test

build:
	go build -o bin/ ./cmd/...

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; \
		exit 1; \
	fi

# silod's container image (cmd/silod/Dockerfile, docs/silo/install.md
# step 11). One platform by default, loaded into the local engine; give
# SILOD_PLATFORMS a list (linux/amd64,linux/arm64) and SILOD_PUSH=1 for a
# multi-arch index pushed to SILOD_IMAGE, whose digest is what
# silo install --image takes. Publishing release images is not wired up
# yet.
SILOD_IMAGE ?= silod:dev
SILOD_PLATFORMS ?=
SILOD_PUSH ?=
silod-image:
	docker buildx build \
		--build-arg GO_IMAGE=$(GO_IMAGE) \
		--build-arg GO_VERSION=$(GO_VERSION) \
		$(if $(RELEASE),--build-arg RELEASE=$(RELEASE),) \
		$(if $(SILOD_PLATFORMS),--platform $(SILOD_PLATFORMS),) \
		$(if $(SILOD_PUSH),--push,--load) \
		-t $(SILOD_IMAGE) -f cmd/silod/Dockerfile .

# The harness image (cmd/silod/Dockerfile.harness): agents' session
# containers on the control VM, with both adapters' CLIs pinned. silo
# install --agent-sessions copies it by digest from silod's repository
# with -harness appended (or --harness-image).
SILOD_HARNESS_IMAGE ?= silod-harness:dev
silod-harness-image:
	docker buildx build \
		--build-arg GO_IMAGE=$(GO_IMAGE) \
		--build-arg GO_VERSION=$(GO_VERSION) \
		$(if $(RELEASE),--build-arg RELEASE=$(RELEASE),) \
		$(if $(SILOD_PLATFORMS),--platform $(SILOD_PLATFORMS),) \
		$(if $(SILOD_PUSH),--push,--load) \
		-t $(SILOD_HARNESS_IMAGE) -f cmd/silod/Dockerfile.harness .

clean:
	rm -rf bin
