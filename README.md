# whatsapp-mcp-docker

A single-container WhatsApp [Model Context Protocol](https://modelcontextprotocol.io)
server. Pull the image, run it, and your AI agent (Claude Code, or any
MCP-speaking client) can pair your phone and then read and send WhatsApp
messages on your behalf — all through MCP.

Built on [`whatsmeow`](https://github.com/tulir/whatsmeow). Everything —
MCP transport, pairing, session persistence — runs in **one Go process**
inside **one Docker image**. No sidecars, no compose bundle, no second
language runtime.

Today the server ships **24 MCP tools**: cache-backed read tools for
chats and messages, plus `send_message`, polls (`send_poll` / `vote_poll`
/ `get_poll_results`), `download_media`, contact / group lookups,
`resolve_jid` (any recipient → readable identity),
`cache_sync` / `cache_sync_status`, the `ping` health
check, and the native `pairing_start` / `pairing_complete` tools that let
an agent drive the link flow over MCP itself. The full coverage matrix —
including whatsmeow capabilities not yet exposed — lives in
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

`8081` carries everything: the MCP transport at `/mcp` and the media byte
route at `/media/<sha256>`, both behind the same `AUTH_TOKEN`. There is no
second port and no separate admin API.

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
| `angelmanuel/whatsapp-mcp:latest-slim` | `debian:bookworm-slim` + `ffmpeg` + `tini` | You want a shell for triage. `ffmpeg` is shipped ahead of the audio-send path (transcoding arbitrary input to Opus); no tool invokes it today. |

Both are multi-arch (`linux/amd64`, `linux/arm64`). Each release also
publishes immutable `:X.Y.Z` and `:X.Y.Z-slim` tags (no `v` prefix —
Docker tag convention) plus sha256 digests in the GitHub release
notes — **pin by digest in production.**

## Configuration

Most operators only touch these:

| Var | Default | Notes |
|---|---|---|
| `TRANSPORT` | `http` | `http` or `stdio`. HTTP **requires** `AUTH_TOKEN`. |
| `PORT` | `8081` | Serves `/mcp`, `/media/<sha256>` and `/healthz`. |
| `DATA_DIR` | `/data` | The only writable volume; holds `session.db` (whatsmeow identity), `cache.db` (chat/message cache), and `media/` (downloaded attachments). |
| `AUTH_TOKEN` | *(unset)* | Bearer token required on every HTTP request, `/mcp` and `/media/` alike. Only the `/healthz` liveness probe is exempt. |
| `MTLS_CA_FILE` / `MTLS_CERT_FILE` / `MTLS_KEY_FILE` | *(unset)* | **Not implemented.** Setting any of them is a fatal startup error — there is no TLS listener, so they only ever served plaintext. Terminate TLS in a reverse proxy. |
| `WHATSAPP_DEVICE_NAME` | `whatsapp-mcp` | Label shown on the user's phone. |
| `LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error`. |
| `LOG_FORMAT` | `json` | `json` or `text`. |
| `MEDIA_MAX_BYTES` | `1073741824` (1 GiB) | Cap on `$DATA_DIR/media`. Over the cap, least-recently-requested blobs are evicted. `0` disables the cap. |
| `MEDIA_TTL` | *(unset)* | Go duration (e.g. `168h`). Evicts media older than this. Unset/`0` disables age-based eviction. |
| `MEDIA_SWEEP_INTERVAL` | `1h` | How often retention runs. A sweep also runs at startup regardless. |

Full env-var contract: [REQUIREMENTS.md](REQUIREMENTS.md#configuration-environment-variables).

In production, deliver `AUTH_TOKEN` as a tmpfs-mounted file referenced by
path, not via `-e` — `-e` exposes the secret to anyone who can read
`/proc/<pid>/environ`.

The server speaks plaintext HTTP and authenticates with a bearer token only.
Transport encryption and client-certificate auth are the reverse proxy's job.

## Media downloads

MCP cannot carry bytes usefully, so attachments are a two-step flow:

1. The agent calls the **`download_media`** tool with `chat_jid` +
   `message_id`. The container fetches the attachment from WhatsApp's CDN,
   stores it content-addressed under `$DATA_DIR/media`, and returns a small
   JSON descriptor — never bytes, never base64:

   ```json
   {
     "media_path": "/media/<sha256>",
     "mime": "video/mp4",
     "size": 4210513,
     "filename": "video_20260804_150405.mp4",
     "sha256": "<sha256>"
   }
   ```

2. Anything that needs the actual file `GET`s `media_path` on the **same
   port, with the same bearer token**:

   ```bash
   curl -H "Authorization: Bearer $AUTH_TOKEN" \
     http://localhost:8081/media/<sha256> -o video.mp4
   ```

   The route sends `Content-Type`, `Content-Length`, `Content-Disposition`,
   `ETag`, `Last-Modified` and `Cache-Control`, supports `Range` requests
   (`206`), answers `401` without a valid bearer and `404` for an unknown or
   evicted digest. Repeat `download_media` calls for the same message are
   cache hits and re-download nothing.

Attachments cached before the `media_direct_path` column existed (migration
`004`) only have an expiring CDN URL, which cannot be backfilled. If
`download_media` returns `media_unavailable` for an old message, run
`cache_sync` to re-ingest it and retry.

## Tools

Tools shipping today (24):

- **Cache-backed reads** — `list_chats`, `list_conversations`, `get_chat`,
  `list_messages`, `get_message_context`, `get_last_interaction`,
  `get_contact_chats`, `get_direct_chat_by_contact`, `get_conversation`
- **Contacts** — `search_contacts`, `list_all_contacts`,
  `get_contact_details`, `resolve_jid`
- **Groups** — `get_group_info`
- **Sending** — `send_message` (text only today)
- **Polls** — `send_poll`, `vote_poll`, `get_poll_results`. Results are
  tallied from vote events as they arrive: WhatsApp offers no way to query
  a poll's standings, so votes cast before the device was linked (or while
  the container was down) are not counted.
- **Media** — `download_media` (returns a descriptor; bytes come from
  `GET /media/<sha256>`)
- **Native** — `ping`, `cache_sync`, `cache_sync_status`, `pairing_start`,
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

Pairing is driven exclusively through MCP. The former admin HTTP surface
(`/admin/pair/start`, `/admin/unpair`, `/admin/events`, `/admin/status`)
was removed; `make pair-qr` drives the MCP tools directly.

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
- **Healthcheck is built-in** — `whatsapp-mcp --healthcheck` probes the
  unauthenticated `http://127.0.0.1:$PORT/healthz` liveness endpoint. No shell
  or curl needed in the distroless image.
- **Media is a bounded cache.** `$DATA_DIR/media` holds attachments fetched
  by `download_media`, capped by `MEDIA_MAX_BYTES` / `MEDIA_TTL`. Evicted
  blobs are re-downloaded on the next `download_media` call, so losing them
  costs a round trip, not data.
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

Releases are cut by [release-please][rp]. Commit messages follow
[Conventional Commits][cc] (`feat:`, `fix:`, `docs:`, `feat!:` …); on
every push to `master`, release-please keeps a
`chore(master): release X.Y.Z` pull request up to date with the next
version and the generated `CHANGELOG.md`.

**Merging that PR is the whole release procedure.** release-please then
tags `vX.Y.Z` and opens the GitHub release, and the `release` workflow
builds both image variants for `linux/amd64` + `linux/arm64`, pushes
`X.Y.Z`, `X.Y.Z-slim`, `X.Y`, and (for non-prerelease tags)
`latest` / `latest-slim` to Docker Hub, then appends the immutable
digests and SPDX SBOMs (via `syft`) to that release.

Version bumps follow SemVer with `bump-minor-pre-major`: while the
project is pre-1.0, `feat:` and breaking changes bump the minor, `fix:`
bumps the patch. Pushing a `vX.Y.Z` tag by hand still works and runs the
same build — useful for a one-off or a backfill, but it bypasses the
changelog.

[rp]: https://github.com/googleapis/release-please
[cc]: https://www.conventionalcommits.org/

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
