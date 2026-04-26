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
  `not_paired` gate; the admin HTTP SSE endpoints (`/admin/pair/start`,
  `/admin/pair/phone`) remain available and operate on the same
  underlying flow (mutually exclusive via `wa.adminMu`).
- **Why**: REQUIREMENTS.md mandates programmatic pairing behind an
  external auth/proxy layer; exposing the flow as MCP tools lets that
  proxy mediate pairing alongside every other tool call without a
  second transport.

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

The reference raises Python exceptions or returns `None` for the same
cases; the Go shape is strictly more informative.
