# syntax=docker/dockerfile:1.7

# ---- Builder -----------------------------------------------------------------
# Pinned by digest for reproducible builds. To bump: resolve the current digest
# of the target tag (docker manifest inspect / registry HEAD) and update both
# the tag hint (after the digest, for humans) and the digest itself.
ARG GO_VERSION=1.25
FROM golang:${GO_VERSION}-bookworm@sha256:1a1408bf8d2d3077f9508880caf0e8bb0fde195fe3c890e7ea480dfb66dc7827 AS builder

WORKDIR /src

# Cache module resolution separately from source so edits to .go files do not
# invalidate the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/whatsapp-mcp \
      ./cmd/whatsapp-mcp

# ---- Runtime -----------------------------------------------------------------
# distroless/static: no shell, no package manager, minimal CA bundle + tzdata.
# Digest pin matches distroless/static-debian12:nonroot at build time; refresh
# alongside the builder digest whenever either moves.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1

COPY --from=builder /out/whatsapp-mcp /usr/local/bin/whatsapp-mcp

# UID 1000 matches the "nonroot" user shipped by the distroless base, and keeps
# /data writable when operators bind-mount a host directory owned by the
# default non-root user.
USER 1000:1000

VOLUME ["/data"]

EXPOSE 8081 8082

# Distroless has no shell/curl, so the binary self-probes /admin/health.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/whatsapp-mcp", "--healthcheck"]

ENTRYPOINT ["/usr/local/bin/whatsapp-mcp"]
