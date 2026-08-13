# CHANGES — divergences from the Python reference

This file tracks each point where the Go implementation's tool surface
diverges from the upstream Python reference
(`reference/whatsapp-mcp-extended/whatsapp-mcp-server/`). Every entry
must be strictly non-breaking for MCP clients OR justified as a
"strictly better Go-native type" per REQUIREMENTS.md §"Tool surface".

## Native tools (no upstream equivalent)

The Go build adds tools that the Python reference does not expose. These
are strictly additive — clients that don't call them see no change.

### `pairing_start` / `pairing_complete`

- **Reference**: pairing is brokered out-of-band; the Python server
  assumes the device is already linked when MCP clients connect.
- **Go**: agents that authenticate through the MCP transport can drive
  the pair flow themselves via two tools — `pairing_start` (opens the
  flow, returns the first QR payload or, with `phone`, a linking code)
  and `pairing_complete` (polls/waits for terminal). Both bypass the
  `not_paired` gate. They are the only pairing path; concurrent flows are
  serialised at the wa layer (`wa.adminMu` + `ErrPairInProgress`).
- **Why**: REQUIREMENTS.md mandates programmatic pairing behind an
  external auth/proxy layer; exposing the flow as MCP tools lets that
  proxy mediate pairing alongside every other tool call without a
  second transport.

### `get_conversation`

- **Reference**: no equivalent. The Python server exposes only per-JID
  lookups (`get_direct_chat_by_contact`, `get_contact_chats`,
  `get_last_interaction`), so reconstructing a contact's full thread when
  WhatsApp has split it across their phone JID (…@s.whatsapp.net) and
  privacy LID (…@lid) takes several calls plus manual merging.
- **Go**: a single front-door tool — `get_conversation(contact, limit?,
  page?, before?, after?)` — resolves the contact to all of its
  identities (via the `jid_aliases` mapping the ingestor learns from each
  message's alternate address) and merges every chat into one
  newest-first, de-duplicated timeline of enriched messages (resolved
  sender name, explicit direction, delivery status). Subject to the
  `not_paired` gate like every other cache read.
- **Why**: strictly additive — the lower-level per-JID tools are
  unchanged for power use. It only collapses the common
  "what's the latest with this person?" workflow into one call instead of
  six, which the multi-identity split otherwise forces.

## Media tools

### `download_media` — descriptor + HTTP byte route, not a local file path

- **Reference**: returns `{ success, message, file_path }`, where
  `file_path` is a path on the machine running the MCP server. That only
  works when the server and the client share a filesystem — which is
  precisely what a containerised deployment does not do.
- **Go**: returns `{ media_path, mime, size, filename, sha256 }`.
  `media_path` is `/media/<sha256>`, an HTTP route served on the same
  port and behind the same bearer token as `/mcp`. The bytes are fetched
  out-of-band with a plain `GET`; the tool result never contains them,
  and never base64.
- **Why**: MCP cannot carry binary payloads usefully, and pushing an
  attachment through an agent's context window is wasteful even when it
  is technically possible. Splitting the flow into "tool returns a
  pointer, HTTP returns the bytes" lets a gateway stream a file straight
  to the caller without it ever entering the model's context. Storage is
  content-addressed, so `sha256` doubles as the cache key and the
  integrity check, and repeat calls cost nothing.
- **Note**: an attachment cached before the `media_direct_path` column
  existed (migration `004`) has only an expiring CDN URL, which cannot be
  backfilled. Those calls fail with `media_unavailable` and a message
  telling the caller to run `cache_sync` and retry.

## Sending tools

### `send_reaction` — structured result, plus an optional `sender_jid`

- **Reference**: `send_reaction(chat_jid, message_id, emoji)` returning
  `{ success, chat_jid, message_id, emoji, action, error }`. The bridge
  builds the reaction with **its own JID** as the target's sender
  (`whatsapp-bridge/internal/whatsapp/messages.go:255`), so
  `MessageKey.FromMe` is always true — reacting to someone else's message
  in a group produces a misattributed key.
- **Go**: same three arguments, plus an optional `sender_jid`. The
  target message's author is resolved from the local cache (an explicit
  `sender_jid` wins; a message we sent resolves to the empty JID, which
  is whatsmeow's "this is mine" signal), so group reactions carry the
  correct `MessageKey.FromMe`/`Participant`. A target that is neither
  cached nor supplied returns `not_found` rather than being guessed at.
  The result is `{ message_id, chat_jid, target_id, emoji, action,
  sent_ts }` — `message_id` is the reaction stanza's own id and
  `target_id` the message reacted to, which the reference conflated
  into one field. `action` (`"add"` / `"remove"`) is preserved, as is
  the empty-emoji removal convention. Failures use the structured error
  codes (see §"Error surface") instead of `{ success: false, error }`.
  Newsletter/channel chats are rejected with `invalid_argument`: they
  need `NewsletterSendReaction` and a `MessageServerID` the cache does
  not capture, so a normal reaction stanza would be silently dropped.
- **Why**: the reference's key-building bug is invisible to the caller
  and produces a wrong reaction on the wire; resolving the author is the
  only correct way to build the key, and the cache already has it. The
  split `message_id` / `target_id` is required to report the send at all
  — a reaction has its own stanza id.

## Read-side tools (cache-backed)

### `list_chats` — list wrapped in object

- **Reference**: returns a bare JSON array of Chat dicts.
- **Go**: returns `{ "chats": [Chat, ...] }`.
- **Why**: MCP's `structuredContent` is specified as an object, and
  having a declared `outputSchema` of type `object` is how the registry
  advertises shape to clients. Wrapping lists in a one-field envelope is
  the minimum change that keeps schemas valid without changing element
  shapes.

### `list_messages` — list wrapped in object, `sender_phone_number` → `sender_jid`

- **List wrapping**: same rationale as `list_chats`; returns
  `{ "messages": [Message, ...] }`.
- **Sender filter**: reference accepts `sender_phone_number: str` and
  matches against the raw `messages.sender` column (which in the Python
  schema is a plain phone-number string). The Go cache stores a typed
  JID on the sender column, so the tool input is `sender_jid` instead
  of `sender_phone_number`. Callers holding only a phone number should
  resolve it through `get_direct_chat_by_contact` first.
- **`query` semantics**: the reference performs a SQL `LIKE %q%` scan.
  The Go tool uses the FTS5 index declared in `001_init.up.sql`, with
  the user's query wrapped in a phrase match to approximate substring
  behaviour. Stop-words and tokenizer differences may cause a Python
  LIKE match to differ from a Go FTS match at the margins; if that
  becomes a problem the FTS path can be made a LIKE fallback.

### Every message-returning tool — additive `reactions` field

- **Reference**: message dicts carry no reaction data; the Python
  bridge does not ingest incoming reactions at all.
- **Go**: `MessageDTO` gains `reactions`, a list of
  `{ emoji, sender, sender_name, is_from_me }`, populated by
  `list_messages`, `get_message_context`, `get_last_interaction`, and
  `get_conversation` in a single batched query per call. The key is
  omitted entirely when a message has no reactions, so existing clients
  see a byte-identical payload for the common case. Our own reaction
  reports an empty `sender` with `is_from_me: true` — the cache stores it
  under a canonical empty sender key so the live, history-sync, and
  `send_reaction` paths cannot produce duplicate rows.
- **Why**: strictly additive. A reaction is often the only response a
  message gets; without this an agent has to infer it or report nothing.

### `get_message_context`

- **Reference**: looks up the target by `messages.id` only (assumes
  stanza IDs are globally unique).
- **Go**: our messages table PK is `(chat_jid, id)` so the same id can
  recur across chats. The tool resolves the target with
  `ORDER BY chat_jid ASC LIMIT 1` for determinism, then pages context
  scoped to that `chat_jid`. Input schema is unchanged.

### `get_direct_chat_by_contact` — `sender_phone_number` → `contact_jid`

- **Reference**: `sender_phone_number: str`, LIKE-matches against chat
  JIDs.
- **Go**: `contact_jid: str`. Attempts an exact-match first (so a full
  JID like `14155552671@s.whatsapp.net` hits the primary key path);
  falls back to a LIKE substring match for legacy phone-number inputs,
  preserving the reference's flexibility. Group JIDs (`@g.us`) are
  rejected with `invalid_argument` — the tool only resolves 1:1 chats.

### `get_contact_chats` — input renamed `jid` → `contact_jid`

- Cosmetic: the reference used a bare `jid` parameter, overloaded with
  "could be a contact, could be a chat". The Go version names it
  `contact_jid` to match the task-level shape and reduce ambiguity.
  Output shape (list of Chat) is wrapped in `{ "chats": [...] }` for
  the same reason as `list_chats`.

### `get_last_interaction` — input renamed `jid` → `contact_jid`

- Same rationale as above. The reference declared a return type of
  `str` in `main.py` but actually returned a Message dict from the
  underlying module; the Go tool returns a Message-shaped object
  directly with a `message` JSON schema, closing the gap.

## Error surface

All read-side tools use the structured error contract introduced in
`internal/mcp/mcp.go`:

- `not_paired` — gated by the existing pairing middleware; unchanged.
- `not_found` — missing chat / message / contact.
- `invalid_argument` — malformed inputs (bad pagination, unparseable
  ISO-8601 timestamps, group JIDs on direct-only tools).
- `internal` — unexpected SQLite errors the tool couldn't attribute to
  user input.

`download_media` adds two codes of its own:

- `no_media` — the message exists but carries no attachment. Retrying
  will never help; the caller picked the wrong message.
- `media_unavailable` — the attachment exists but its bytes could not be
  fetched (expired locator, CDN failure). Recoverable, usually by
  re-ingesting the message with `cache_sync`.

The reference raises Python exceptions or returns `None` for the same
cases; the Go shape is strictly more informative.
