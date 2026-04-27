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

# Smoke-test the published :master image as a Claude Code MCP server.
TEST_IMAGE_TAG ?= master
TEST_CONTAINER ?= whatsapp-mcp-test
TOKEN_FILE     := $(DATA_DIR)/.auth_token

.PHONY: build test lint vet tidy clean image image-slim run-local run-master stop-master pair-qr

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

# Pull and run the published :master image detached, with a stable AUTH_TOKEN
# persisted under $(DATA_DIR)/.auth_token. Pair the resulting container by:
#   1) export WHATSAPP_MCP_AUTH_TOKEN=$$(cat $(DATA_DIR)/.auth_token)
#   2) restart Claude Code so .mcp.json picks it up
#   3) make pair-qr   (in another terminal)
run-master:
	@mkdir -p $(DATA_DIR)
	@if [ ! -s $(TOKEN_FILE) ]; then \
	  umask 077 && openssl rand -hex 32 > $(TOKEN_FILE) && \
	  echo "generated $(TOKEN_FILE)"; \
	fi
	docker pull $(IMAGE):$(TEST_IMAGE_TAG)
	-docker rm -f $(TEST_CONTAINER) >/dev/null 2>&1
	docker run -d \
	  --name $(TEST_CONTAINER) \
	  --restart unless-stopped \
	  --userns=keep-id \
	  -p 8081:8081 -p 8082:8082 \
	  -v $(DATA_DIR):/data \
	  -e AUTH_TOKEN=$$(cat $(TOKEN_FILE)) \
	  $(IMAGE):$(TEST_IMAGE_TAG)
	@echo
	@echo "container: $(TEST_CONTAINER)  image: $(IMAGE):$(TEST_IMAGE_TAG)"
	@echo "MCP HTTP endpoint: http://localhost:8081/mcp"
	@echo "admin endpoint:    http://localhost:8082"
	@echo
	@echo "next:"
	@echo "  export WHATSAPP_MCP_AUTH_TOKEN=\$$(cat $(TOKEN_FILE))"
	@echo "  make pair-qr     # scan QR with WhatsApp -> Linked devices"

stop-master:
	-docker rm -f $(TEST_CONTAINER)

# Stream /admin/pair/start and render each rotating pair payload as a QR in
# the terminal. Requires qrencode (apt: qrencode, brew: qrencode).
pair-qr:
	@command -v qrencode >/dev/null || { echo "qrencode not found (apt install qrencode)"; exit 1; }
	@test -s $(TOKEN_FILE) || { echo "$(TOKEN_FILE) missing; run 'make run-master' first"; exit 1; }
	@token=$$(cat $(TOKEN_FILE)); \
	curl -sN -H "Authorization: Bearer $$token" -X POST http://localhost:8082/admin/pair/start \
	| awk -v RS='' -F'\n' '{ \
	    evt=""; data=""; \
	    for (i=1;i<=NF;i++) { \
	      if ($$i ~ /^event: /)      { evt = substr($$i,8) } \
	      else if ($$i ~ /^data: /)  { data = substr($$i,7) } \
	    } \
	    print evt "\t" data; fflush(); \
	  }' \
	| while IFS=$$'\t' read -r evt data; do \
	    case "$$evt" in \
	      code) \
	        code=$$(printf '%s' "$$data" | sed -n 's/.*"code":"\([^"]*\)".*/\1/p'); \
	        clear; printf '\nscan with WhatsApp -> Linked devices -> Link a device\n\n'; \
	        printf '%s' "$$code" | qrencode -t ANSIUTF8 ;; \
	      success) echo "paired."; exit 0 ;; \
	      timeout|error|client-outdated|scanned-without-multidevice) \
	        echo "pair $$evt: $$data"; exit 1 ;; \
	    esac; \
	  done
