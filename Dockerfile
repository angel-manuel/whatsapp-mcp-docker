# syntax=docker/dockerfile:1.7

# ---- Builder -----------------------------------------------------------------
ARG GO_VERSION=1.24

FROM golang:${GO_VERSION}-bookworm AS builder

WORKDIR /src

# Cache module resolution separately from source so edits to .go files do not
# invalidate the module layer.
COPY go.mod ./
# go.sum is optional until we pull deps; copy it if present.
COPY go.su[m] ./
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
# Pin base image by digest in release tags for reproducibility.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/whatsapp-mcp /usr/local/bin/whatsapp-mcp

USER 1000:1000

VOLUME ["/data"]

EXPOSE 8081 8082

ENTRYPOINT ["/usr/local/bin/whatsapp-mcp"]
