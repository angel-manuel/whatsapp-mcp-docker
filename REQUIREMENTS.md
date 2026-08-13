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

41 tools, same names as the upstream reference, grouped for readability.

> **This section is the target surface, not the shipped one.** Names below
> without a ✅ are requirements still to be built. For what the binary
> actually registers today, see [SUPPORTED.md](SUPPORTED.md) — the
> authoritative source is `internal/tools/register.go`, `internal/mcptools/`,
> and `internal/mcp/ping.go`.

**Messages / chats (8)**
`list_messages`, `list_chats`, `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats`, `get_last_interaction`, `get_message_context`, `request_history`

**Sending (4)**
✅ `send_message` (text), ✅ `send_file` (image / video / audio / document /
sticker envelopes), ✅ `send_audio_message` (voice note / PTT). Planned, not
yet implemented: `send_reaction`.

Both media tools take a `media_path` reference rather than bytes or a local
filesystem path — see "Media byte routes" below.

**Message editing / state (3)**
`edit_message`, `delete_message`, `mark_read`

**Media (1)**
`download_media`

**Contacts (6)**
`search_contacts`, `get_contact_details`, `list_all_contacts`, `set_nickname`, `get_nickname`, `remove_nickname`, `list_nicknames` (local-only alias/notes store)

**Groups (8)**
`get_group_info`, `create_group`, `add_group_members`, `remove_group_members`, `promote_to_admin`, `demote_admin`, `leave_group`, `update_group`

**Polls (1)**
✅ `send_poll` — named `send_poll`, not `create_poll`, to match the
`send_message` convention every other outbound tool follows. Noted in
[CHANGES.md](CHANGES.md).

**Presence (2)**
`set_presence`, `subscribe_presence`

**Profile / privacy (4)**
`get_profile_picture`, `get_blocklist`, `block_user`, `unblock_user`

**Newsletters / channels (3)**
`follow_newsletter`, `unfollow_newsletter`, `create_newsletter`

Each tool's input schema, output shape, and error semantics MUST match the upstream Python reference unless the Go binding surfaces a strictly better type (e.g. typed JID instead of loose strings). Breaking divergences require a note in `CHANGES.md`.

Beyond the parity surface, the Go build also exposes `get_conversation` — a native, additive read tool with no upstream equivalent. It is the canonical "what's the latest with this person?" front door: it merges every chat for a contact across their phone JID (…@s.whatsapp.net) and privacy LID (…@lid) into one newest-first, de-duplicated timeline of enriched messages. The lower-level per-JID read tools (e.g. `get_direct_chat_by_contact`, `get_contact_chats`) are unchanged and remain available for power use. Like the other cache reads, it is subject to the `not_paired` gate.

Polls also gain two native read/write tools with no upstream equivalent — `vote_poll` and `get_poll_results` — because a poll nobody can answer or count is not a feature. Both are documented in `CHANGES.md`; `get_poll_results` reads a tally the ingestor accumulates locally, since neither whatsmeow nor the WhatsApp protocol can be asked for a poll's standings.

In addition to the parity surface, the Go build exposes 2 native pairing tools — `pairing_start`, `pairing_complete` — for agents that drive pairing through the MCP transport. They are the only pairing path (the admin HTTP SSE endpoints were removed in `99b0ce7`); concurrent flows are serialised at the wa layer (`adminMu` + `ErrPairInProgress`). These tools, plus `ping`, are exempt from the `not_paired` gate so that a pre-pair agent can bootstrap itself.

`download_media`, `send_file` and `send_audio_message` diverge from the upstream reference in one deliberate way: bytes never pass through the tool call, in either direction. `download_media` returns a *descriptor*; the send tools take one. See §Media transfer.

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
|       +---- POST /media  +  GET /media/{sha256}                  |
|             on the SAME :PORT, same bearer                       |
|             (bytes only; MCP cannot carry them)                  |
|                                                                  |
|   SQLite + blobs:                                                |
|     /data/session.db  (whatsmeow sqlstore: device, ratchet, ...) |
|     /data/cache.db  (chats, messages, contacts, nicknames, polls)|
|     /data/media/    (attachment blobs in + out, content-addressed)|
|                                                                  |
|   ffmpeg  (shelled out, optional, for audio transcode to Opus)  |
|            used by send_audio_message; -slim image only          |
|                                                                  |
+------------------------------------------------------------------+
```

- The MCP tool layer calls `whatsmeow` functions directly — no internal RPC.
- Two SQLite files in a single `/data` volume: one owned by `whatsmeow` (crypto + protocol state), one owned by this project (mirror of chats / messages / contacts + local metadata like nicknames).
- Only `/data` is persistent. The rest of the filesystem MAY be mounted read-only by the operator.

## Transports

1. **HTTP/SSE MCP** on `BIND_ADDR:PORT` (primary). Suitable for out-of-container MCP clients and for proxying behind another service. Authentication is mandatory when HTTP is enabled (see Security).
2. **stdio MCP** (local-dev mode). When `TRANSPORT=stdio`, the binary speaks MCP on stdin/stdout. Intended for Claude Desktop / `mcp` CLI / test harnesses. The media byte routes are HTTP-only and therefore unavailable in stdio mode: `download_media` still stores the file under `$DATA_DIR/media`, but with no way to POST bytes in, the media send tools can only forward media the container already holds.

## Media transfer

MCP has no useful way to carry binary payloads, and putting attachment bytes into an agent's context is wasteful even when it is possible. Media is therefore split in two:

- **`download_media`** (MCP tool) — input `{ chat_jid, message_id }`. Fetches the attachment from WhatsApp's CDN, stores it content-addressed at `$DATA_DIR/media/<sha256>.<ext>`, and returns `{ media_path, mime, size, filename, sha256 }`. The call is idempotent: a digest already stored is a cache hit with no network I/O. Errors: `not_found` (message not cached), `no_media` (message carries no attachment), `media_unavailable` (locator expired or CDN failure), `internal`.
- **`GET /media/{sha256}`** (HTTP) — mounted on the same listener as `/mcp`, behind the same bearer auth. Sends `Content-Type`, `Content-Length`, `Content-Disposition`, `ETag`, `Last-Modified` and `Cache-Control`; supports `Range` (`206`). `401` without a valid bearer, `404` for an unknown or evicted digest. The path segment MUST be validated as a 64-character hex digest before any filesystem access.
- **`POST /media`** (HTTP) — the inbound mirror, same listener, same bearer auth. The request body is the raw file; `Content-Type` names the mimetype (sniffed from the leading bytes when absent or `application/octet-stream`) and `?filename=` names the file. Answers `201` with the same `{ media_path, mime, size, filename, sha256 }` descriptor, `413` above 100 MiB, `405` for a non-POST/PUT method. Storage is content-addressed and idempotent, so re-uploading identical bytes is a cache hit. The payload MUST be spooled to disk rather than buffered whole in memory.
- **`send_file` / `send_audio_message`** (MCP tools) — take `media_path` (a `/media/<sha256>` reference, a bare digest, or a gateway URL ending in one), never bytes, never base64, and never a local filesystem path. A message this server sends is mirrored into the cache with the plaintext digest of what went on the wire, so `download_media` resolves our own sends out of the store.

These are the only non-MCP routes the container serves, and they exist solely because MCP structurally cannot transfer bytes. They do not reopen the `:8082` admin API removed in `99b0ce7`.

### Outbound audio

Voice notes (`send_audio_message`, PTT) are Ogg/Opus or they are nothing: WhatsApp will accept another codec on the wire and then fail to play it, which is indistinguishable from a silent send failure. The contract is therefore two-sided and MUST fail closed:

- Input already Ogg/Opus (decided by the magic number, not the declared mimetype) → uploaded as-is.
- Otherwise, `FFMPEG_PATH` resolves to an executable → transcoded to mono 48 kHz Opus, and the converted blob is stored so the cached row and `download_media` agree with what the recipient received.
- Otherwise → `invalid_argument` naming the probed path and the `-slim` image, rather than an unplayable send.

`send_file` applies the same rule with a wider allow-list (`audio/ogg`, `audio/opus`, `audio/mpeg`, `audio/mp4`, `audio/aac`, `audio/amr` play as attachments), and sends without `PTT`.

`media_direct_path` (migration `004`) is what makes downloads durable: the pre-signed `media_url` expires, the direct path does not. It is only present on the live protobuf at ingest time and CANNOT be backfilled, so messages ingested before that migration may only be downloadable until their URL expires. `download_media` MUST say so explicitly and point at `cache_sync` rather than returning an opaque failure.

Retention: `$DATA_DIR/media` is a cache, not a system of record. It is bounded by `MEDIA_MAX_BYTES` (least-recently-requested evicted first) and optionally `MEDIA_TTL`, swept at startup and every `MEDIA_SWEEP_INTERVAL`. An evicted blob costs a re-download, not data.

## Pairing

The container MUST expose pairing programmatically so that an external UI (or an MCP-driven agent) can broker it without shipping its own whatsmeow client. Pairing is driven entirely through the MCP tools below, on top of `wa.Client.StartPairing`. Only one flow may be open at a time; a second caller receives `pair_in_progress` until the first flow ends.

> The admin HTTP surface that previously fronted pairing (`POST /admin/pair/start`, `POST /admin/pair/phone`, `POST /admin/unpair`, on `:ADMIN_PORT`) was removed in `99b0ce7`. Everything that can be a tool is a tool.

### Pairing — MCP tools

These two tools target agents that authenticate through the same bearer-token MCP transport (and any auth gateway proxying it). They are exempt from the `not_paired` gate.

- `pairing_start` — input `{ "phone"?: string }`. Without `phone`, opens a QR flow and returns `{ "status": "awaiting_scan", "code": "<raw QR>", "timeout_ms": <int> }`. With `phone`, also requests a phone linking code and returns `{ "status": "awaiting_phone_link", "linking_code": "ABCD-EFGH", "code": "<latest QR>", "timeout_ms": <int> }`. If the first QR has not been emitted within an internal timeout (rare; whatsmeow normally emits within ~1s), the QR-mode response degrades to `{ "status": "pending" }` and the caller should poll `pairing_complete` for the code. Errors: `already_paired`, `pair_in_progress`, `not_pairing` (only when `phone` is supplied — indicates the freshly-opened flow was already torn down by a concurrent unpair), `internal`.
- `pairing_complete` — input `{ "wait_seconds"?: integer (0..120, default 60) }`. Polls the in-progress flow. `wait_seconds=0` returns the latest cached event without blocking (status snapshot). Otherwise blocks up to `wait_seconds`, coalescing rotation events and returning either a terminal status (`success`, `timeout`, `error`, `client_outdated`, `scanned_without_multidevice`) or `pending` carrying the newest rotation code. `not_pairing` indicates no flow is active. On `success`, the response also carries `jid` and `pushname` from `wa.Status()`.

Until pairing succeeds, every MCP tool call MUST return a structured error with a stable code (`not_paired`), except for `ping`, `pairing_start`, and `pairing_complete` which remain callable pre-pair so agents can detect state and drive the link flow over MCP.

## Session persistence

- `/data` is the only persistent volume; nuke it to fully reset the device identity.
- `whatsmeow` is configured with its SQLite `sqlstore` pointed at `/data/session.db`. Ratchet state rotates on every message, so only one process may own the volume at a time — the binary acquires an exclusive `flock` on `/data/.lock` at startup and exits non-zero if it is already held.
- The cache DB at `/data/cache.db` stores the same entities Felix's reference ships: chats, messages, contacts, nicknames, FTS index over message text. It additionally stores poll ballots and votes (`polls`, `poll_options`, `poll_votes`, migration `006`), which have no reference equivalent because poll tallies exist nowhere else — see `get_poll_results` in CHANGES.md. Schema documented in `docs/schema.md`.

## Session lifecycle events

The container MUST surface these events to the outside world (for external orchestrators that drive reconnect UI):

- `logged_out` — WhatsApp unpaired the device (remote). Follow-on tool calls return `not_paired` until re-pair.
- `stream_replaced` — another process connected with the same keys; this instance is toast.
- `temporary_ban` — includes expire duration.
- `connection_failure` — includes the whatsmeow reason code.
- `connected`, `disconnected` — transport-level state.

Event delivery:
- Mirrored as MCP notifications (`notifications/session`) when the MCP transport supports them.
- Summarised by the `ping` and `cache_sync_status` tools.

When `logged_out` or `stream_replaced` fires, the process MUST NOT silently try to recover with stale credentials. It stays in `not_paired` state and waits for explicit re-pair.

## Configuration (environment variables)

| Var | Default | Purpose |
|---|---|---|
| `TRANSPORT` | `http` | `http` or `stdio` |
| `BIND_ADDR` | `0.0.0.0` | HTTP bind address |
| `PORT` | `8081` | Serves `/mcp`, `POST /media` and `GET /media/{sha256}` |
| `DATA_DIR` | `/data` | Persistent state dir (`session.db`, `cache.db`, `media/`) |
| `LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error` |
| `LOG_FORMAT` | `json` | `json` or `text` |
| `AUTH_TOKEN` | *(unset)* | Required bearer token for every HTTP route (`/mcp` and `/media/`). REQUIRED in `http` mode. |
| `MTLS_CA_FILE`, `MTLS_CERT_FILE`, `MTLS_KEY_FILE` | *(unset)* | **Not implemented.** Setting any of them in `http` mode is a fatal startup error. No TLS listener exists, so they never encrypted anything or verified a client certificate. If mTLS is ever built, it will be additive to `AUTH_TOKEN`, never a replacement. |
| `WHATSAPP_DEVICE_NAME` | `whatsapp-mcp` | Shown on the user's phone after pairing. |
| `FFMPEG_PATH` | `/usr/bin/ffmpeg` | ffmpeg binary used to transcode audio to Opus for `send_audio_message` (and for `send_file` audio WhatsApp cannot play). Probed per call, not at startup, so an ffmpeg bind-mounted into a running container is picked up without a restart. Present → arbitrary input is transcoded; absent → Opus input is required and anything else is refused with `invalid_argument`, never sent unplayable. |
| `ENABLE_PPROF` | `false` | Exposes `/debug/pprof` when true. |
| `MEDIA_MAX_BYTES` | `1073741824` | Cap on `$DATA_DIR/media`. Over the cap, least-recently-requested blobs are evicted. `0` = unlimited. |
| `MEDIA_TTL` | *(unset)* | Go duration; evicts media older than this. Unset/`0` disables age-based eviction. |
| `MEDIA_SWEEP_INTERVAL` | `1h` | Retention sweep period. A sweep also runs at startup. |

No long-lived secrets in env vars in production: operators should deliver `AUTH_TOKEN` via a secret store mount (tmpfs file + `file://` reference) rather than `-e`.

## Security posture

- **Non-root.** Runs as UID 1000 (`appuser`). Dockerfile builds this in; no `USER root` in the runtime stage.
- **Capabilities dropped.** No NET_ADMIN, no SYS_ADMIN.
- **Read-only root filesystem compatible.** `/data` and `/tmp` are the only writable paths; the binary must not write anywhere else.
- **Minimal base image.** Distroless static or `debian:bookworm-slim` — decision documented in `docs/image.md`. No shell in the distroless variant.
- **Signed images.** Releases pushed to `ghcr.io/angel-manuel/whatsapp-mcp-docker` and signed with [`cosign`](https://github.com/sigstore/cosign). Consumers pin by digest.
- **Reproducible-ish builds.** `go build -trimpath -buildvcs=false` + pinned base image digests; goal is byte-identical rebuilds from the same commit.
- **Mandatory auth on HTTP.** Starting in `http` mode without `AUTH_TOKEN` is a fatal error at startup. The bearer check rejects every request when the configured token is empty, rather than accepting an empty one.
- **TLS terminates upstream.** The listener is plaintext HTTP; the bearer token is the only auth this process enforces. Operators needing transport encryption or client-certificate auth must front it with a reverse proxy. Config that claims otherwise fails closed at startup rather than serving plaintext under an mTLS-shaped name.
- **Outbound egress.** The process only needs to reach WhatsApp endpoints; operators running under a strict egress policy should allow at minimum:
  - `*.whatsapp.net`
  - `web.whatsapp.com`
  - `mmg.whatsapp.net`, `media-*.whatsapp.net` (media CDN)
  Exact hostname list published in `docs/egress.md` and kept in sync with whatsmeow's client payload.
- **No telemetry.** The binary does not phone home.

## Observability

- Structured JSON logs to stdout; one event per line; includes `connection_id` (if passed via env) and a stable `event_type`.
- `GET /healthz` — unauthenticated liveness probe (HTTP server up and routing); drives the container HEALTHCHECK via `whatsapp-mcp --healthcheck`.
- `ping` (MCP tool) — readiness: liveness + pairing state. Exempt from the `not_paired` gate, so it answers before the device is linked.
- `cache_sync_status` (MCP tool) — cache counts, last ingested event, current/most-recent sync run.
- Prometheus exposition is not implemented yet.

## Build & release

- `Dockerfile` — multi-stage: Go builder → distroless-static (preferred) runtime. Optional `Dockerfile.slim` variant with shell + ffmpeg for audio transcoding use cases (`send_audio_message` with non-Opus input).
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
