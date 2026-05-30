# whatsapp-mcp-docker

A single-container WhatsApp [Model Context Protocol](https://modelcontextprotocol.io)
server. Pull the image, run it, and your AI agent (Claude Code, or any
MCP-speaking client) can pair your phone and then read and send WhatsApp
messages on your behalf — all through MCP.

Built on [`whatsmeow`](https://github.com/tulir/whatsmeow). Everything —
MCP transport, pairing, session persistence — runs in **one Go process**
inside **one Docker image**. No sidecars, no compose bundle, no second
language runtime.

Today the server ships **18 MCP tools**: cache-backed read tools for
chats and messages, plus `send_message`, contact / group lookups, a
diagnostic `cache_sync_status`, the `ping` health check, and the native
`pairing_start` / `pairing_complete` tools that let an agent drive the
link flow over MCP itself. The full coverage matrix — including
whatsmeow capabilities not yet exposed — lives in
[SUPPORTED.md](SUPPORTED.md).

> ⚠️ **Unofficial.** This project uses `whatsmeow`, an unofficial
> reimplementation of the WhatsApp protocol. Use at your own risk;
> WhatsApp may rate-limit or ban accounts that misuse automation.

## Demo

<!-- Demo video -->
https://github.com/user-attachments/assets/5a947d64-8d06-4a6c-a760-3a785398a84e

---

## Quick start

You need: Docker, a phone with WhatsApp, and Claude Code.

### 1. Run the container

```bash
mkdir -p ~/whatsapp-mcp
( umask 077 && openssl rand -hex 32 > ~/whatsapp-mcp/.auth_token )

docker run -d \
  --name whatsapp-mcp \
  --restart unless-stopped \
  -p 8081:8081 \
  -v whatsapp-mcp-data:/data \
  -e AUTH_TOKEN="$(cat ~/whatsapp-mcp/.auth_token)" \
  docker.io/angelmanuel/whatsapp-mcp:latest
```

`8081` is the MCP transport. The admin port (`8082`) stays inside the
container and is used internally for pairing and health checks.

### 2. Configure the MCP in Claude Code

```bash
claude mcp add --transport http whatsapp http://localhost:8081/mcp \
  --header "Authorization: Bearer $(cat ~/whatsapp-mcp/.auth_token)" \
  --scope user
```

Restart Claude Code, run `/mcp`, and `whatsapp` should be listed.

Then **ask Claude to pair the device.** It calls `pairing_start`
through MCP, gets back the QR code, and renders it for you. Scan it in
WhatsApp → Linked devices → **Link a device**. Claude polls
`pairing_complete` until the link succeeds; the session then survives
container restarts (everything lives under `/data`).

For a project-scoped config (committed alongside a repo), use
`--scope project` instead — `claude mcp add` writes to `./.mcp.json`.
Don't commit the token; the env-var form `Bearer ${WHATSAPP_MCP_AUTH_TOKEN}`
in `.mcp.json` works once you export the variable in the shell that
launches Claude Code.

> 💡 **Claude Desktop?** Claude Desktop only speaks **stdio** MCP, not
> HTTP. Run the container with `-e TRANSPORT=stdio` and wrap it with a
> stdio-bridging launcher (or just use Claude Code, which speaks HTTP
> natively).

---

## Image variants

Published to Docker Hub on every release tag:

| Tag | Base | Use when |
|---|---|---|
| `angelmanuel/whatsapp-mcp:latest` | distroless/static, non-root, no shell | **Default.** Smallest, hardest to misuse. |
| `angelmanuel/whatsapp-mcp:latest-slim` | `debian:bookworm-slim` + `ffmpeg` + `tini` | You need `send_audio_message` to transcode arbitrary input to Opus, or you want a shell for triage. |

Both are multi-arch (`linux/amd64`, `linux/arm64`). Each release also
publishes immutable `:X.Y.Z` and `:X.Y.Z-slim` tags (no `v` prefix —
Docker tag convention) plus sha256 digests in the GitHub release
notes — **pin by digest in production.**

## Configuration

Most operators only touch these:

| Var | Default | Notes |
|---|---|---|
| `TRANSPORT` | `http` | `http` or `stdio`. HTTP **requires** `AUTH_TOKEN` or the full `MTLS_*` trio. |
| `PORT` | `8081` | MCP transport port. |
| `ADMIN_PORT` | `8082` | Admin HTTP port (pair, health, status, SSE events). |
| `DATA_DIR` | `/data` | The only writable volume; holds `session.db` (whatsmeow identity) and `cache.db` (chat/message cache). |
| `AUTH_TOKEN` | *(unset)* | Bearer token for MCP HTTP + every admin route except `/admin/health`. |
| `MTLS_CA_FILE` / `MTLS_CERT_FILE` / `MTLS_KEY_FILE` | *(unset)* | If all three are set, client mTLS replaces `AUTH_TOKEN`. |
| `WHATSAPP_DEVICE_NAME` | `whatsapp-mcp` | Label shown on the user's phone. |
| `LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error`. |
| `LOG_FORMAT` | `json` | `json` or `text`. |

Full env-var contract: [REQUIREMENTS.md](REQUIREMENTS.md#configuration-environment-variables).

In production, deliver `AUTH_TOKEN` and `MTLS_*` as tmpfs-mounted files
referenced by path, not via `-e` — `-e` exposes the secret to anyone
who can read `/proc/<pid>/environ`.

## Tools

Tools shipping today (18):

- **Cache-backed reads** — `list_chats`, `list_conversations`, `get_chat`,
  `list_messages`, `get_message_context`, `get_last_interaction`,
  `get_contact_chats`, `get_direct_chat_by_contact`, `get_conversation`
- **Contacts** — `search_contacts`, `list_all_contacts`,
  `get_contact_details`
- **Groups** — `get_group_info`
- **Sending** — `send_message` (text only today)
- **Native** — `ping`, `cache_sync_status`, `pairing_start`,
  `pairing_complete`

For the full picture — including the long list of `whatsmeow`
capabilities not yet exposed (media send, reactions, edits, group
admin, newsletters, presence, privacy/blocklist, …) — see
[SUPPORTED.md](SUPPORTED.md). Intentional divergences from the prior
Python reference's argument shapes are tracked in
[CHANGES.md](CHANGES.md).

## Pairing reference

Pairing is driven by the **MCP tools** (`pairing_start`,
`pairing_complete`) — the agent calls `pairing_start`, receives the QR
code, renders it, and polls `pairing_complete` until the link succeeds.

An **Admin HTTP / SSE** surface also exists (`POST /admin/pair/start`,
`POST /admin/unpair`, `GET /admin/events`, `GET /admin/status`) for
external UI brokers; it shares the same underlying flow and is mutually
exclusive with the MCP path (whoever opens the flow holds it; the other
receives `pair_in_progress`).

`ping`, `pairing_start`, and `pairing_complete` are exempt from the
`not_paired` gate; every other tool returns a structured `not_paired`
error until pairing succeeds.

Full pairing contract — events, error codes — is in
[REQUIREMENTS.md §Pairing](REQUIREMENTS.md#pairing).

## Operational notes

- **One process per `/data` volume.** Ratchet state rotates on every
  message; the binary acquires an exclusive `flock` on `/data/.lock` at
  startup and exits non-zero if another process owns it.
- **`/data` is the only persistent volume.** Run `docker volume rm whatsapp-mcp-data`
  to fully reset the device identity; preserve it across container restarts to avoid
  re-pairing.
- **Read-only root filesystem compatible** — mount `/` as `ro`,
  `/data` and `/tmp` as `rw`.
- **Healthcheck is built-in** — `whatsapp-mcp --healthcheck` probes
  `http://127.0.0.1:$ADMIN_PORT/admin/health`. No shell or curl needed
  in the distroless image.
- **Rootless Podman**: the image runs as UID 1000 (non-root). Named volumes
  are initialised with the correct ownership automatically. If you switch to a
  bind mount instead, add `--userns=keep-id` so the host directory is writable
  by the container user.
- **No telemetry.** The binary does not phone home.

## Building locally

```bash
make build         # bin/whatsapp-mcp
make test          # unit tests with -race
make image         # docker.io/angelmanuel/whatsapp-mcp:dev (distroless)
make image-slim    # …:dev-slim  (debian:bookworm-slim + ffmpeg)
make run-local     # build + run with a local ./data volume
make run-master    # pull :master, run detached, mint a token at ./.auth_token
make pair-qr       # render QR for the running container in the terminal
```

## Releases

```bash
git tag v0.1.0
git push origin v0.1.0
```

The `release` GitHub Actions workflow builds both image variants for
`linux/amd64` + `linux/arm64`, pushes `X.Y.Z`, `X.Y.Z-slim`, and (for
non-prerelease tags) `latest` / `latest-slim` to Docker Hub, and
attaches SPDX SBOMs (via `syft`) plus the immutable digests to the
GitHub release.

## See also

- [SUPPORTED.md](SUPPORTED.md) — what the server actually exposes
  today, mapped against the underlying `whatsmeow.Client` capabilities.
- [REQUIREMENTS.md](REQUIREMENTS.md) — full design & env-var contract.
- [CHANGES.md](CHANGES.md) — every divergence from the Python
  reference, with rationale.
- [DOCKERHUB.md](DOCKERHUB.md) — the trimmed-down README synced to the
  Docker Hub repo overview.

## License

See repository.
