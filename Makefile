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
VOLUME_NAME ?= whatsapp-mcp-data

# Smoke-test the published :master image as a Claude Code MCP server.
TEST_IMAGE_TAG ?= master
TEST_CONTAINER ?= whatsapp-mcp-test
# TOKEN_FILE holds the dev *session* token (what .mcp.json sends);
# ADMIN_TOKEN_FILE holds the token for the /sessions admin API.
TOKEN_FILE       := $(CURDIR)/.auth_token
ADMIN_TOKEN_FILE := $(CURDIR)/.admin_token
SESSION_ID       ?= dev
BASE_URL         ?= http://localhost:8081
# Everything the container exposes lives on this one port.
MCP_URL        ?= $(BASE_URL)/mcp

.PHONY: build test lint vet tidy clean image image-slim run-local run-master stop-master volume-rm pair-qr

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
	docker run --rm -it \
	  --name whatsapp-mcp-local \
	  -p 8081:8081 \
	  -v $(VOLUME_NAME):/data \
	  -e TRANSPORT=$${TRANSPORT:-http} \
	  -e ADMIN_TOKEN=$${ADMIN_TOKEN:-devtoken} \
	  $(IMAGE):$(IMAGE_TAG)

# Pull and run the published :master image detached, with a stable
# ADMIN_TOKEN persisted at $(ADMIN_TOKEN_FILE), and create the
# $(SESSION_ID) session once, saving its token at $(TOKEN_FILE). Then:
#   1) export WHATSAPP_MCP_AUTH_TOKEN=$$(cat $(TOKEN_FILE))
#   2) restart Claude Code so .mcp.json picks it up
#   3) make pair-qr   (in another terminal)
run-master:
	@if [ ! -s $(ADMIN_TOKEN_FILE) ]; then \
	  umask 077 && openssl rand -hex 32 > $(ADMIN_TOKEN_FILE) && \
	  echo "generated $(ADMIN_TOKEN_FILE)"; \
	fi
	docker pull $(IMAGE):$(TEST_IMAGE_TAG)
	-docker rm -f $(TEST_CONTAINER) >/dev/null 2>&1
	# Podman (rootless): add --userns=keep-id to the run command below
	# (required when using bind mounts; named volumes handle ownership automatically)
	docker run -d \
	  --name $(TEST_CONTAINER) \
	  --restart unless-stopped \
	  -p 8081:8081 \
	  -v $(VOLUME_NAME):/data \
	  -e ADMIN_TOKEN=$$(cat $(ADMIN_TOKEN_FILE)) \
	  $(IMAGE):$(TEST_IMAGE_TAG)
	@for i in $$(seq 1 30); do curl -sf $(BASE_URL)/healthz >/dev/null && break; sleep 1; done
	@if [ ! -s $(TOKEN_FILE) ]; then \
	  command -v jq >/dev/null || { echo "jq not found (apt install jq)"; exit 1; }; \
	  resp=$$(curl -s -X POST $(BASE_URL)/sessions \
	    -H "Authorization: Bearer $$(cat $(ADMIN_TOKEN_FILE))" \
	    -d '{"id":"$(SESSION_ID)"}'); \
	  token=$$(printf '%s' "$$resp" | jq -r '.token // empty'); \
	  [ -n "$$token" ] || { echo "POST /sessions failed: $$resp"; \
	    echo "if the session exists but $(TOKEN_FILE) is lost, DELETE $(BASE_URL)/sessions/$(SESSION_ID) with the admin token"; exit 1; }; \
	  (umask 077 && printf '%s\n' "$$token" > $(TOKEN_FILE)); \
	  echo "created session $(SESSION_ID); token saved at $(TOKEN_FILE)"; \
	fi
	@echo
	@echo "container: $(TEST_CONTAINER)  image: $(IMAGE):$(TEST_IMAGE_TAG)"
	@echo "MCP HTTP endpoint: $(MCP_URL)"
	@echo "admin API:         $(BASE_URL)/sessions"
	@echo
	@echo "next:"
	@echo "  export WHATSAPP_MCP_AUTH_TOKEN=\$$(cat $(TOKEN_FILE))"
	@echo "  make pair-qr     # scan QR with WhatsApp -> Linked devices"

stop-master:
	-docker rm -f $(TEST_CONTAINER)

volume-rm:
	docker volume rm $(VOLUME_NAME)

# Drive the pairing_start / pairing_complete MCP tools and render each
# rotating pair payload as a QR in the terminal, for the session whose token
# is in $(TOKEN_FILE). Pairing itself always goes through /mcp.
# Requires qrencode (apt: qrencode, brew: qrencode) and jq.
pair-qr:
	@command -v qrencode >/dev/null || { echo "qrencode not found (apt install qrencode)"; exit 1; }
	@command -v jq >/dev/null || { echo "jq not found (apt install jq)"; exit 1; }
	@test -s $(TOKEN_FILE) || { echo "$(TOKEN_FILE) missing; run 'make run-master' first"; exit 1; }
	@token=$$(cat $(TOKEN_FILE)); \
	call() { \
	  curl -s -X POST $(MCP_URL) \
	    -H "Authorization: Bearer $$token" \
	    -H "Content-Type: application/json" \
	    -H "Accept: application/json, text/event-stream" \
	    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$$1\",\"arguments\":$$2}}" \
	  | sed -n 's/^data: //p;/^{/p' | tail -n1 \
	  | jq -r '.result.structuredContent // .error // empty'; \
	}; \
	render() { \
	  code=$$(printf '%s' "$$1" | jq -r '.code // empty'); \
	  [ -n "$$code" ] || return 0; \
	  clear; printf '\nscan with WhatsApp -> Linked devices -> Link a device\n\n'; \
	  printf '%s' "$$code" | qrencode -t ANSIUTF8; \
	}; \
	result=$$(call pairing_start '{}'); \
	[ -n "$$result" ] || { echo "pairing_start returned nothing; is the container up?"; exit 1; }; \
	render "$$result"; \
	while :; do \
	  status=$$(printf '%s' "$$result" | jq -r '.status // "error"'); \
	  case "$$status" in \
	    success) echo "paired: $$(printf '%s' "$$result" | jq -r '.jid // ""')"; exit 0 ;; \
	    timeout|error|not_pairing|client_outdated|scanned_without_multidevice) \
	      echo "pair $$status: $$result"; exit 1 ;; \
	  esac; \
	  result=$$(call pairing_complete '{"wait_seconds":60}'); \
	  [ -n "$$result" ] || { echo "pairing_complete returned nothing"; exit 1; }; \
	  render "$$result"; \
	done
