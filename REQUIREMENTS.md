# WhatsApp MCP (Docker) — Requirements

A single-container, Go-native [Model Context Protocol](https://modelcontextprotocol.io) server for WhatsApp, built on [`whatsmeow`](https://github.com/tulir/whatsmeow).

## Goals

1. **One Docker image.** Everything the server needs runs in a single container — no sidecars, no `docker-compose` bundle. Pair-brokerage, MCP transport, and session persistence are all in-process.
2. **One Go binary.** The library layer (`whatsmeow`) and the MCP layer (`mark3labs/mcp-go` or the official Go SDK) are linked into a single static-ish executable. No inter-process HTTP hop, no shared-secret between processes, no second language runtime.
3. **Full tool parity with `FelixIsaac/whatsapp-mcp-extended`.** All 41 tools that project exposes are available, with equivalent names and argument shapes to minimise migration friction.

Design-wise the container is also intended to be safely embeddable behind an external approval/permission/auth layer that proxies MCP tool calls — it should not assume it is the security boundary.

## Non-goals

- Web dashboard bundled inside the container (pair flow is programmatic; any UI lives upstream).
- Multi-account multiplexing inside one process (one instance = one WhatsApp account; run N containers for N accounts).
- Full historical message export (bounded by what WhatsApp's multidevice sync delivers).
- Broadcast lists, voice/video calls.
- Coupling to any specific deployment target (compose, Kubernetes, systemd, local dev all must work).

## Tool surface

41 tools, same names as the upstream reference, grouped for readability:

**Messages / chats (8)**
`list_messages`, `list_chats`, `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats`, `get_last_interaction`, `get_message_context`, `request_history`

**Sending (4)**
`send_message`, `send_file`, `send_audio_message`, `send_reaction`

**Message editing / state (3)**
`edit_message`, `delete_message`, `mark_read`

**Media (1)**
`download_media`

**Contacts (6)**
`search_contacts`, `get_contact_details`, `list_all_contacts`, `set_nickname`, `get_nickname`, `remove_nickname`, `list_nicknames` (local-only alias/notes store)

**Groups (8)**
`get_group_info`, `create_group`, `add_group_members`, `remove_group_members`, `promote_to_admin`, `demote_admin`, `leave_group`, `update_group`

**Polls (1)**
`create_poll`

**Presence (2)**
`set_presence`, `subscribe_presence`

**Profile / privacy (4)**
`get_profile_picture`, `get_blocklist`, `block_user`, `unblock_user`

**Newsletters / channels (3)**
`follow_newsletter`, `unfollow_newsletter`, `create_newsletter`

Each tool's input schema, output shape, and error semantics MUST match the upstream Python reference unless the Go binding surfaces a strictly better type (e.g. typed JID instead of loose strings). Breaking divergences require a note in `CHANGES.md`.

## Architecture

```
+--------------------------- container ---------------------------+
|                                                                  |
|  Go binary (static-ish, distroless / debian-slim runtime)       |
|                                                                  |
|   whatsmeow client <---- WS (TLS) ----> web.whatsapp.com         |
|       |                                                          |
|       | in-process calls                                         |
|       v                                                          |
|   MCP server (stdio or HTTP/SSE on :PORT)                       |
|       |                                                          |
|       +---- admin HTTP (pair, health, status) on :ADMIN_PORT    |
|                                                                  |
|   SQLite:                                                        |
|     /data/session.db  (whatsmeow sqlstore: device, ratchet, ...) |
|     /data/cache.db    (chats, messages, contacts, nicknames)     |
|                                                                  |
|   ffmpeg  (shelled out, optional, for audio transcode to Opus)  |
|                                                                  |
+------------------------------------------------------------------+
```

- The MCP tool layer calls `whatsmeow` functions directly — no internal RPC.
- Two SQLite files in a single `/data` volume: one owned by `whatsmeow` (crypto + protocol state), one owned by this project (mirror of chats / messages / contacts + local metadata like nicknames).
- Only `/data` is persistent. The rest of the filesystem MAY be mounted read-only by the operator.

## Transports

1. **HTTP/SSE MCP** on `BIND_ADDR:PORT` (primary). Suitable for out-of-container MCP clients and for proxying behind another service. Authentication is mandatory when HTTP is enabled (see Security).
2. **stdio MCP** (local-dev mode). When `TRANSPORT=stdio`, the binary speaks MCP on stdin/stdout and the admin HTTP surface is either disabled or exposed on `localhost` only. Intended for Claude Desktop / `mcp` CLI / test harnesses.

## Pairing

The container MUST expose pairing programmatically so that an external UI can broker it without shipping its own whatsmeow client.

- `POST /admin/pair/start` → opens a Server-Sent Events stream. Event types mirror `whatsmeow.QRChannelItem`:
  - `code`   — rotating pairing payload + `timeout_ms` for UI refresh
  - `success` — paired; session persisted; MCP tools become operational
  - `timeout`, `error`, `client-outdated`, `scanned-without-multidevice` — terminal
- `POST /admin/pair/phone` with `{ phone }` → returns `{ linking_code }` for phone-number pairing (no QR). Same underlying lifecycle; success/failure events also arrive on `/admin/pair/start` if that stream is open.
- `POST /admin/unpair` → logs the device out cleanly and deletes `/data/session.db`.

Until pairing succeeds, every MCP tool call MUST return a structured error with a stable code (`not_paired`) so callers can detect and surface a reconnect flow instead of displaying a transport error.

## Session persistence

- `/data` is the only persistent volume; nuke it to fully reset the device identity.
- `whatsmeow` is configured with its SQLite `sqlstore` pointed at `/data/session.db`. Ratchet state rotates on every message, so only one process may own the volume at a time — the binary acquires an exclusive `flock` on `/data/.lock` at startup and exits non-zero if it is already held.
- The cache DB at `/data/cache.db` stores the same entities Felix's reference ships: chats, messages, contacts, nicknames, FTS index over message text. Schema documented in `docs/schema.md`.

## Session lifecycle events

The container MUST surface these events to the outside world (for external orchestrators that drive reconnect UI):

- `logged_out` — WhatsApp unpaired the device (remote). Follow-on tool calls return `not_paired` until re-pair.
- `stream_replaced` — another process connected with the same keys; this instance is toast.
- `temporary_ban` — includes expire duration.
- `connection_failure` — includes the whatsmeow reason code.
- `connected`, `disconnected` — transport-level state.

Event delivery:
- Always visible via `GET /admin/events` (SSE stream).
- Mirrored as MCP notifications (`notifications/session`) when the MCP transport supports them.
- Summarised in the response of `GET /admin/status`.

When `logged_out` or `stream_replaced` fires, the process MUST NOT silently try to recover with stale credentials. It stays in `not_paired` state and waits for explicit re-pair.

## Configuration (environment variables)

| Var | Default | Purpose |
|---|---|---|
| `TRANSPORT` | `http` | `http` or `stdio` |
| `BIND_ADDR` | `0.0.0.0` | MCP + admin bind address |
| `PORT` | `8081` | MCP HTTP/SSE port |
| `ADMIN_PORT` | `8082` | Admin HTTP port (pair, health, events, status) |
| `DATA_DIR` | `/data` | Persistent state dir |
| `LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error` |
| `LOG_FORMAT` | `json` | `json` or `text` |
| `AUTH_TOKEN` | *(unset)* | Required bearer token for HTTP MCP + admin. REQUIRED in `http` mode. |
| `MTLS_CA_FILE`, `MTLS_CERT_FILE`, `MTLS_KEY_FILE` | *(unset)* | If all three are set, requires client mTLS and ignores `AUTH_TOKEN`. |
| `PAIR_DEVICE_NAME` | `whatsapp-mcp` | Shown on the user's phone after pairing. |
| `FFMPEG_PATH` | `/usr/bin/ffmpeg` | Used by `send_audio_message`; absent → audio conversion disabled, Opus input required. |
| `ENABLE_PPROF` | `false` | Exposes `/debug/pprof` on the admin port when true. |

No long-lived secrets in env vars in production: operators should deliver `AUTH_TOKEN`, `MTLS_*` via a secret store mount (tmpfs file + `file://` reference) rather than `-e`.

## Security posture

- **Non-root.** Runs as UID 1000 (`appuser`). Dockerfile builds this in; no `USER root` in the runtime stage.
- **Capabilities dropped.** No NET_ADMIN, no SYS_ADMIN.
- **Read-only root filesystem compatible.** `/data` and `/tmp` are the only writable paths; the binary must not write anywhere else.
- **Minimal base image.** Distroless static or `debian:bookworm-slim` — decision documented in `docs/image.md`. No shell in the distroless variant.
- **Signed images.** Releases pushed to `ghcr.io/angel-manuel/whatsapp-mcp-docker` and signed with [`cosign`](https://github.com/sigstore/cosign). Consumers pin by digest.
- **Reproducible-ish builds.** `go build -trimpath -buildvcs=false` + pinned base image digests; goal is byte-identical rebuilds from the same commit.
- **Mandatory auth on HTTP.** Starting in `http` mode without `AUTH_TOKEN` (or mTLS config) is a fatal error at startup.
- **Outbound egress.** The process only needs to reach WhatsApp endpoints; operators running under a strict egress policy should allow at minimum:
  - `*.whatsapp.net`
  - `web.whatsapp.com`
  - `mmg.whatsapp.net`, `media-*.whatsapp.net` (media CDN)
  Exact hostname list published in `docs/egress.md` and kept in sync with whatsmeow's client payload.
- **No telemetry.** The binary does not phone home.

## Observability

- Structured JSON logs to stdout; one event per line; includes `connection_id` (if passed via env) and a stable `event_type`.
- `GET /admin/health` — liveness (process up, SQLite open).
- `GET /admin/ready` — readiness (connected + logged in).
- `GET /admin/status` — snapshot: `{ connected, logged_in, jid, pushname, last_event, uptime_s }`.
- `GET /admin/metrics` — Prometheus exposition (counters for tool calls, message send/recv, reconnect attempts; gauges for connection state; histogram for whatsmeow IQ round-trip).

## Build & release

- `Dockerfile` — multi-stage: Go builder → distroless-static (preferred) runtime. Optional `Dockerfile.slim` variant with shell + ffmpeg for audio transcoding use cases.
- `Makefile` or `justfile` targets: `build`, `test`, `lint`, `image`, `run-local`.
- CI (GitHub Actions): unit tests, `go vet`, `staticcheck`, image build + push on tag, cosign sign.
- Versioning: SemVer; `v0.x` until tool surface is stable. Each release publishes both a `:vX.Y.Z` tag and an immutable digest.
- SBOM published alongside each image (`syft` → SPDX).

## Compatibility commitments

- **MCP protocol**: track the current MCP spec; bump major on breaking protocol changes.
- **whatsmeow**: pinned to a known-good commit in `go.mod`; upgrade cadence ~monthly or when WhatsApp forces it. Each whatsmeow bump gets a release note describing observed behavioural changes.
- **Tool surface**: argument/output shape stable within a minor version; additions allowed in minor, breaks require a major.

## Testing

- Unit tests for all pure helpers (JID parsing, pagination, schema migrations).
- Integration tests against whatsmeow's in-memory store for non-networked paths.
- A manual end-to-end harness (`tests/e2e/`) that runs against a real WhatsApp account via a dev phone; not part of CI, documented in `docs/e2e.md`.

## Open questions (tracked in issues, not blocking v1)

- Primary MCP transport default: `http` vs `stdio`. Current lean: `http` for hosted, `stdio` for local dev (`TRANSPORT=` env switches).
- Incoming-message delivery: polling via `list_messages(since=...)` is guaranteed; MCP notifications for new messages are desirable but some clients do not subscribe.
- Auto-ffmpeg vs BYO-Opus: ship both image variants and let operators pick.
- Keeping the cache DB small: eviction policy for old media metadata / messages older than N days.

## References

- `whatsmeow`: https://github.com/tulir/whatsmeow
- Reference Python MCP: https://github.com/lharries/whatsapp-mcp
- Extended reference (tool parity target): https://github.com/FelixIsaac/whatsapp-mcp-extended
- `mcp-go`: https://github.com/mark3labs/mcp-go
- MCP spec: https://modelcontextprotocol.io
