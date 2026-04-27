# whatsapp-mcp

Single-container WhatsApp [Model Context Protocol](https://modelcontextprotocol.io)
server for AI agents (Claude Code, Cursor, any MCP HTTP client). Pull the
image, run it, pair your phone, point your agent at it.

Built on [`whatsmeow`](https://github.com/tulir/whatsmeow). Ships 16
MCP tools today (cache-backed chat / message reads, contact and group
lookup, `send_message`, `ping`, `cache_sync_status`, plus native
`pairing_start` / `pairing_complete`); coverage and gaps are tracked
in [SUPPORTED.md](https://github.com/angel-manuel/whatsapp-mcp-docker/blob/master/SUPPORTED.md).
Source, full docs, and changelog:
**[github.com/angel-manuel/whatsapp-mcp-docker](https://github.com/angel-manuel/whatsapp-mcp-docker)**.

> ⚠️ Unofficial. Uses an unofficial WhatsApp protocol implementation.
> Use at your own risk; WhatsApp may rate-limit or ban accounts that
> misuse automation.

## Tags

| Tag | Base | Use when |
|---|---|---|
| `latest` | distroless/static, non-root, no shell | **Default.** Smallest, hardest to misuse. |
| `latest-slim` | `debian:bookworm-slim` + `ffmpeg` + `tini` | You need `send_audio_message` to transcode arbitrary input to Opus, or want a shell for triage. |
| `vX.Y.Z`, `vX.Y.Z-slim` | (as above) | Immutable per-release pins. **Pin by digest in production.** |

Both variants are multi-arch: `linux/amd64`, `linux/arm64`.

## Quick start

```bash
mkdir -p ~/whatsapp-mcp/data
export WHATSAPP_MCP_AUTH_TOKEN=$(openssl rand -hex 32)

docker run -d \
  --name whatsapp-mcp \
  --restart unless-stopped \
  -p 8081:8081 -p 8082:8082 \
  -v ~/whatsapp-mcp/data:/data \
  -e AUTH_TOKEN="$WHATSAPP_MCP_AUTH_TOKEN" \
  docker.io/angelmanuel/whatsapp-mcp:latest
```

- `8081` — MCP HTTP transport (this is what your agent connects to).
- `8082` — admin HTTP (pairing, health, status, SSE events).
- `/data` — the only writable volume; holds `session.db` and
  `cache.db`. Preserve it across restarts to keep the paired session.

Save the token — your MCP client will need it later. Common pattern:

```bash
echo "$WHATSAPP_MCP_AUTH_TOKEN" > ~/whatsapp-mcp/data/.auth_token
chmod 600 ~/whatsapp-mcp/data/.auth_token
```

## Pair your phone

Render the rotating QR in your terminal (needs `qrencode`):

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

Phone-number linking instead of QR:

```bash
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -X POST http://localhost:8082/admin/pair/phone \
     -d '{"phone":"+15551234567"}'
# → { "linking_code": "ABCD-EFGH" } — enter in WhatsApp → Linked devices.
```

## Connect Claude Code

```bash
claude mcp add --transport http whatsapp http://localhost:8081/mcp \
  --header "Authorization: Bearer $WHATSAPP_MCP_AUTH_TOKEN" \
  --scope user
```

Restart Claude Code, run `/mcp`, and `whatsapp` should be listed as
`connected`. Done.

For project-scoped wiring (committed alongside a repo), instead create
`.mcp.json`:

```json
{
  "mcpServers": {
    "whatsapp": {
      "type": "http",
      "url": "http://localhost:8081/mcp",
      "headers": {
        "Authorization": "Bearer ${WHATSAPP_MCP_AUTH_TOKEN}"
      }
    }
  }
}
```

Export `WHATSAPP_MCP_AUTH_TOKEN` in the shell that launches Claude Code.

> **Claude Desktop** only speaks **stdio** MCP, not HTTP. Run the
> container with `-e TRANSPORT=stdio` and wrap it with a stdio bridge,
> or use Claude Code (which speaks HTTP natively).

## Configuration

| Var | Default | Notes |
|---|---|---|
| `TRANSPORT` | `http` | `http` or `stdio`. HTTP **requires** `AUTH_TOKEN` or the full `MTLS_*` trio. |
| `PORT` | `8081` | MCP transport port. |
| `ADMIN_PORT` | `8082` | Admin HTTP port. |
| `DATA_DIR` | `/data` | Persistent state directory. |
| `AUTH_TOKEN` | *(unset)* | Bearer token required by MCP HTTP + admin (except `/admin/health`). |
| `MTLS_CA_FILE` / `MTLS_CERT_FILE` / `MTLS_KEY_FILE` | *(unset)* | If all three set, client mTLS replaces `AUTH_TOKEN`. |
| `WHATSAPP_DEVICE_NAME` | `whatsapp-mcp` | Label shown on the user's phone. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |
| `LOG_FORMAT` | `json` | `json` or `text`. |

In production, deliver `AUTH_TOKEN` and `MTLS_*` as tmpfs-mounted files
referenced by path, not via `-e` — `-e` exposes the secret to anyone
who can read `/proc/<pid>/environ`.

## Tools

16 MCP tools today: cache-backed chat / message reads
(`list_chats`, `get_chat`, `list_messages`, `get_message_context`,
`get_last_interaction`, `get_contact_chats`,
`get_direct_chat_by_contact`), contacts (`search_contacts`,
`list_all_contacts`, `get_contact_details`), `get_group_info`,
`send_message` (text), and the native `ping`, `cache_sync_status`,
`pairing_start`, `pairing_complete`. Full coverage matrix and
not-yet-supported list:
[SUPPORTED.md](https://github.com/angel-manuel/whatsapp-mcp-docker/blob/master/SUPPORTED.md).

## Operational notes

- **Non-root** (UID 1000). No `NET_ADMIN` / `SYS_ADMIN` needed.
- **Read-only root filesystem compatible** — mount `/` as `ro`,
  `/data` (and `/tmp`) as `rw`.
- **Healthcheck built-in** — `whatsapp-mcp --healthcheck` probes
  `/admin/health`. No shell or curl needed in distroless.
- **One process per `/data`.** Ratchet state rotates on every message;
  startup acquires an exclusive `flock` on `/data/.lock` and exits
  non-zero if another process owns it.
- **No telemetry.** The binary does not phone home.

## Source & support

- **Repository / issues**: [github.com/angel-manuel/whatsapp-mcp-docker](https://github.com/angel-manuel/whatsapp-mcp-docker)
- **Design**: [REQUIREMENTS.md](https://github.com/angel-manuel/whatsapp-mcp-docker/blob/master/REQUIREMENTS.md)
- **Tool divergences**: [CHANGES.md](https://github.com/angel-manuel/whatsapp-mcp-docker/blob/master/CHANGES.md)
