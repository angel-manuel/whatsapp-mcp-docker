# whatsapp-mcp

Single-container WhatsApp [Model Context Protocol](https://modelcontextprotocol.io) server, built on
[`whatsmeow`](https://github.com/tulir/whatsmeow). MCP transport, pair brokerage,
and session persistence all run in one Go process — no sidecars, no compose
bundle. Design goals and tool surface are documented in
[`REQUIREMENTS.md`](REQUIREMENTS.md).

## Images

Published to Docker Hub on every tag push:

- `docker.io/angelmanuel/whatsapp-mcp:latest` — distroless/static, non-root,
  no shell. **Default choice.**
- `docker.io/angelmanuel/whatsapp-mcp:latest-slim` — `debian:bookworm-slim`
  with `ffmpeg` and `tini` pre-installed. Use this variant if you need
  `send_audio_message` transcoding or a shell for triage.

Both variants are multi-arch (`linux/amd64`, `linux/arm64`). Each release also
publishes `:vX.Y.Z` and `:vX.Y.Z-slim` immutable tags plus the corresponding
sha256 digests in the release notes; production deployments should pin by
digest.

## Quick start

```bash
docker run --rm -it \
  --name whatsapp-mcp \
  -p 8081:8081 -p 8082:8082 \
  -v "$PWD/data:/data" \
  -e TRANSPORT=http \
  -e AUTH_TOKEN="$(openssl rand -hex 32)" \
  docker.io/angelmanuel/whatsapp-mcp:latest
```

- `8081` — MCP HTTP/SSE transport.
- `8082` — admin HTTP surface (pairing, health, status, SSE events).
- `/data` — the only writable volume; holds `session.db` (whatsmeow identity)
  and `cache.db` (chat/message cache). Preserve this to keep the paired
  session; delete it to fully reset the device identity.

`--help`, `--version`, and `--healthcheck` are the only CLI flags. The
container's `HEALTHCHECK` invokes `whatsapp-mcp --healthcheck`, which probes
`http://127.0.0.1:$ADMIN_PORT/admin/health` — no shell or curl needed.

## Configuration

Full env-var contract lives in [REQUIREMENTS.md](REQUIREMENTS.md#configuration-environment-variables).
Operators typically only care about these:

| Var | Default | Notes |
|---|---|---|
| `TRANSPORT` | `http` | `http` or `stdio`. HTTP mode **requires** `AUTH_TOKEN` or the full `MTLS_*` trio. |
| `PORT` | `8081` | MCP transport port. |
| `ADMIN_PORT` | `8082` | Admin HTTP port. |
| `DATA_DIR` | `/data` | Persistent state directory. |
| `AUTH_TOKEN` | *(unset)* | Bearer token required by MCP HTTP + every admin route except `/admin/health`. |
| `MTLS_CA_FILE` / `MTLS_CERT_FILE` / `MTLS_KEY_FILE` | *(unset)* | If all three are set, client mTLS replaces `AUTH_TOKEN`. |
| `PAIR_DEVICE_NAME` | `whatsapp-mcp` | Label shown on the user's phone. |
| `LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error`. |
| `LOG_FORMAT` | `json` | `json` or `text`. |

Do not pass secrets via `-e` in production — mount them as tmpfs files and
reference by path.

## Pairing

The pair flow is driven entirely through the admin API. There is no QR
display inside the container; an external orchestrator is expected to stream
the pair events and present them to the user.

```bash
ADMIN="http://localhost:8082"
AUTH="Authorization: Bearer $AUTH_TOKEN"

# 1. Start a pair session and stream events (SSE).
curl -N -H "$AUTH" -X POST "$ADMIN/admin/pair/start"
# → emits `qr` events with the raw pairing code; operator renders them as QR
#   on any device the user can scan with WhatsApp → Linked devices.

# 2. Alternative: phone-number linking (no QR).
curl -H "$AUTH" -H 'Content-Type: application/json' \
  -X POST "$ADMIN/admin/pair/phone" \
  -d '{"phone":"+15551234567"}'
# → returns { "linking_code": "ABCD-EFGH" }. User enters the code in
#   WhatsApp → Linked devices → Link with phone number.

# 3. Watch session lifecycle (connected, logged_out, stream_replaced, …).
curl -N -H "$AUTH" "$ADMIN/admin/events"

# 4. Current snapshot at any time.
curl -H "$AUTH" "$ADMIN/admin/status"

# 5. Reset and re-pair.
curl -H "$AUTH" -X POST "$ADMIN/admin/unpair"
```

Until pairing succeeds every MCP tool call returns a structured `not_paired`
error so callers can drive the reconnect UI themselves.

## Building locally

```bash
make build         # bin/whatsapp-mcp
make test          # unit tests with -race
make image         # docker.io/angelmanuel/whatsapp-mcp:dev (distroless)
make image-slim    # …:dev-slim  (debian:bookworm-slim + ffmpeg)
make run-local     # build + run with a local ./data volume
```

## Releases

Releases are cut by pushing a SemVer tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The `release` GitHub Actions workflow then:

1. Builds the distroless and slim variants for `linux/amd64` + `linux/arm64`.
2. Pushes `vX.Y.Z`, `vX.Y.Z-slim`, and (for non-prerelease tags) `latest`
   and `latest-slim` to Docker Hub.
3. Generates SPDX JSON SBOMs with `syft` and attaches them to the GitHub
   release alongside the immutable digests.

The same workflow supports `workflow_dispatch` for dry runs against `master`
(pushes `:master` / `:master-slim` without cutting a release).

### Required repository secrets

| Secret | Purpose |
|---|---|
| `DOCKERHUB_USERNAME` | Docker Hub account that owns the `angelmanuel` namespace. |
| `DOCKERHUB_TOKEN`    | Docker Hub access token with write scope on that namespace. |

Cosign keyless signing is planned but explicitly out of scope for the
basic-support milestone; there is a `TODO(hardening)` marker in
`.github/workflows/release.yml` tracking it.
