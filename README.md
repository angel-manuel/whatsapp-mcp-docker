# whatsapp-mcp-docker

A single-container WhatsApp [Model Context Protocol](https://modelcontextprotocol.io)
server. Pull the image, run it, pair your phone, and your AI agent (Claude
Code, or any MCP-speaking client) can read and send WhatsApp messages on
your behalf.

Built on [`whatsmeow`](https://github.com/tulir/whatsmeow). Everything —
MCP transport, pairing, session persistence — runs in **one Go process**
inside **one Docker image**. No sidecars, no compose bundle, no second
language runtime.

Today the server ships **16 MCP tools**: cache-backed read tools for
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

<!--
  Drop a screen recording of the MCP being used inside Claude Code here.
  Recommended formats:
    - GitHub README:  upload an .mp4/.mov via "Add files" or drag-and-drop
                      in the GitHub web editor; GitHub rewrites it to a
                      user-attachment URL automatically. Or commit a .gif
                      under docs/demo.gif and reference it below.
    - Docker Hub:     paste a YouTube/Loom link in DOCKERHUB.md (Docker
                      Hub does not host video; the embed below will not
                      render there).
-->

<!-- Replace this block with the uploaded video URL or `docs/demo.gif`. -->
_Demo video coming soon — Claude Code calling `list_chats`, `list_messages`, and `send_message` against a paired account._

---

## Quick start

You need: Docker, a phone with WhatsApp, and Claude Code.

### 1. Run the container

```bash
mkdir -p ~/whatsapp-mcp/data
( umask 077 && openssl rand -hex 32 > ~/whatsapp-mcp/data/.auth_token )

docker run -d \
  --name whatsapp-mcp \
  --restart unless-stopped \
  -p 8081:8081 \
  -v ~/whatsapp-mcp/data:/data \
  -e AUTH_TOKEN="$(cat ~/whatsapp-mcp/data/.auth_token)" \
  docker.io/angelmanuel/whatsapp-mcp:latest
```

`8081` is the MCP transport. The admin port (`8082`) stays inside the
container by default — bind it explicitly only if you want to drive
pairing from the host (see [Pair from the host](#pair-from-the-host-optional)
below).

### 2. Configure the MCP in Claude Code

```bash
claude mcp add --transport http whatsapp http://localhost:8081/mcp \
  --header "Authorization: Bearer $(cat ~/whatsapp-mcp/data/.auth_token)" \
  --scope user
```

Restart Claude Code, run `/mcp`, and `whatsapp` should be listed.

Then just **ask Claude to pair the device.** It calls `pairing_start`
through MCP and walks you through it. Phone-number linking is the
smoothest path in chat:

```
> Pair my WhatsApp using phone number +15551234567
```

Claude calls `pairing_start({phone: "+15551234567"})`, hands you back
the 8-character linking code, and you enter it in
WhatsApp → Linked devices → **Link with phone number**. Claude polls
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

### Pair from the host (optional)

If you'd rather render the QR in your terminal — useful when Claude
Code isn't running yet, or when you want a visual scan instead of a
linking code — bind the admin port to loopback when you start the
container:

```bash
# Add this to the docker run above:
  -p 127.0.0.1:8082:8082 \
```

Then stream the rotating pair payload and render it as a QR (needs
`qrencode`: `brew install qrencode` / `apt install qrencode`):

```bash
TOKEN=$(cat ~/whatsapp-mcp/data/.auth_token)
curl -sN -H "Authorization: Bearer $TOKEN" \
     -X POST http://localhost:8082/admin/pair/start \
| while IFS= read -r line; do
    case "$line" in
      'data: '*'"code":"'*)
        code=$(printf '%s' "$line" | sed -n 's/.*"code":"\([^"]*\)".*/\1/p')
        clear; echo "Scan with WhatsApp → Linked devices → Link a device"
        printf '%s' "$code" | qrencode -t ANSIUTF8 ;;
      'event: success') echo "paired."; break ;;
    esac
  done
```

The `Makefile` ships a ready-made `make pair-qr` target that does
exactly this against the running container.

---

## Image variants

Published to Docker Hub on every release tag:

| Tag | Base | Use when |
|---|---|---|
| `angelmanuel/whatsapp-mcp:latest` | distroless/static, non-root, no shell | **Default.** Smallest, hardest to misuse. |
| `angelmanuel/whatsapp-mcp:latest-slim` | `debian:bookworm-slim` + `ffmpeg` + `tini` | You need `send_audio_message` to transcode arbitrary input to Opus, or you want a shell for triage. |

Both are multi-arch (`linux/amd64`, `linux/arm64`). Each release also
publishes immutable `:vX.Y.Z` and `:vX.Y.Z-slim` tags plus sha256
digests in the GitHub release notes — **pin by digest in production.**

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

Tools shipping today (16):

- **Cache-backed reads** — `list_chats`, `get_chat`, `list_messages`,
  `get_message_context`, `get_last_interaction`, `get_contact_chats`,
  `get_direct_chat_by_contact`
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

There are two surfaces for the link flow, both backed by the same
`wa.Client.StartPairing` and mutually exclusive (whoever opens the flow
holds it; the other receives `pair_in_progress`):

- **Admin HTTP / SSE** (`POST /admin/pair/start`,
  `POST /admin/pair/phone`, `POST /admin/unpair`,
  `GET /admin/events`, `GET /admin/status`) — for an external UI broker
  rendering QR / linking codes.
- **MCP tools** (`pairing_start`, `pairing_complete`) — for an agent
  driving the flow itself through the same MCP transport.

`ping`, `pairing_start`, and `pairing_complete` are exempt from the
`not_paired` gate; every other tool returns a structured `not_paired`
error until pairing succeeds.

Full pairing contract — events, error codes, phone-link semantics — is
in [REQUIREMENTS.md §Pairing](REQUIREMENTS.md#pairing).

## Operational notes

- **One process per `/data` volume.** Ratchet state rotates on every
  message; the binary acquires an exclusive `flock` on `/data/.lock` at
  startup and exits non-zero if another process owns it.
- **`/data` is the only persistent volume.** Delete it to fully reset
  the device identity; preserve it across container restarts to avoid
  re-pairing.
- **Read-only root filesystem compatible** — mount `/` as `ro`,
  `/data` and `/tmp` as `rw`.
- **Healthcheck is built-in** — `whatsapp-mcp --healthcheck` probes
  `http://127.0.0.1:$ADMIN_PORT/admin/health`. No shell or curl needed
  in the distroless image.
- **No telemetry.** The binary does not phone home.

## Building locally

```bash
make build         # bin/whatsapp-mcp
make test          # unit tests with -race
make image         # docker.io/angelmanuel/whatsapp-mcp:dev (distroless)
make image-slim    # …:dev-slim  (debian:bookworm-slim + ffmpeg)
make run-local     # build + run with a local ./data volume
make run-master    # pull :master, run detached, mint a token under ./data
make pair-qr       # render QR for the running container in the terminal
```

## Releases

```bash
git tag v0.1.0
git push origin v0.1.0
```

The `release` GitHub Actions workflow builds both image variants for
`linux/amd64` + `linux/arm64`, pushes `vX.Y.Z`, `vX.Y.Z-slim`, and (for
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
