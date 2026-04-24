BINARY      := whatsapp-mcp
BIN_DIR     := bin
MAIN_PKG    := ./cmd/whatsapp-mcp
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)

# Docker Hub publish target. The CI release workflow overrides IMAGE_TAG with
# the derived version (e.g. v1.2.3, v1.2.3-slim, latest). Local `make image`
# builds the distroless variant tagged `:dev`.
IMAGE       ?= docker.io/angelmanuel/whatsapp-mcp
IMAGE_TAG   ?= dev
DATA_DIR    ?= $(CURDIR)/data

.PHONY: build test lint vet tidy clean image image-slim run-local

build:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(MAIN_PKG)

test:
	go test ./... -race -count=1

lint:
	golangci-lint run

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR) dist coverage.out coverage.html

image:
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  -t $(IMAGE):$(IMAGE_TAG) \
	  -f Dockerfile \
	  .

image-slim:
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  -t $(IMAGE):$(IMAGE_TAG)-slim \
	  -f Dockerfile.slim \
	  .

run-local: image
	@mkdir -p $(DATA_DIR)
	docker run --rm -it \
	  --name whatsapp-mcp-local \
	  -p 8081:8081 -p 8082:8082 \
	  -v $(DATA_DIR):/data \
	  -e TRANSPORT=$${TRANSPORT:-http} \
	  -e AUTH_TOKEN=$${AUTH_TOKEN:-devtoken} \
	  $(IMAGE):$(IMAGE_TAG)
