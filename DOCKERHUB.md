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

- `8081` — MCP HTTP transport (this is what your agent connects to).
- `/data` — the only writable volume; holds `session.db` and
  `cache.db`. Preserve it across restarts to keep the paired session.

The admin port (`8082`) stays inside the container by default. Bind
it explicitly only if you want to drive pairing from the host (see
[Pair from the host](#pair-from-the-host-optional) below).

### 2. Configure the MCP in Claude Code

```bash
claude mcp add --transport http whatsapp http://localhost:8081/mcp \
  --header "Authorization: Bearer $(cat ~/whatsapp-mcp/data/.auth_token)" \
  --scope user
```

Restart Claude Code, run `/mcp`, and `whatsapp` should be listed as
`connected`.

Then **ask Claude to pair the device** — it calls `pairing_start`
through MCP and walks you through it. Phone-number linking is the
smoothest path in chat:

```
> Pair my WhatsApp using phone number +15551234567
```

Claude calls `pairing_start({phone: "+15551234567"})`, hands you back
the 8-character linking code, and you enter it in
WhatsApp → Linked devices → **Link with phone number**. Claude polls
`pairing_complete` until success.

For project-scoped wiring (committed alongside a repo), use
`--scope project`; that writes to `./.mcp.json`. Don't commit the
token — the `Bearer ${WHATSAPP_MCP_AUTH_TOKEN}` form works once you
export the variable in the shell that launches Claude Code.

> **Claude Desktop** only speaks **stdio** MCP, not HTTP. Run the

> **Claude Desktop** only speaks **stdio** MCP, not HTTP. Run the
> container with `-e TRANSPORT=stdio` and wrap it with a stdio bridge,
> or use Claude Code (which speaks HTTP natively).

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
`qrencode`):

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

Phone-number linking from the host instead of QR:

```bash
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -X POST http://localhost:8082/admin/pair/phone \
     -d '{"phone":"+15551234567"}'
# → { "linking_code": "ABCD-EFGH" } — enter in WhatsApp → Linked devices.
```

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
