package mcptools

// ChatDTO mirrors the shape of Python's `Chat.to_dict()` output and is
// the JSON envelope returned by every list/get chat tool.
//
// Fields that can be absent are emitted as JSON null via `any`.
type ChatDTO struct {
	JID             string `json:"jid"`
	Name            any    `json:"name"` // string | null
	IsGroup         bool   `json:"is_group"`
	ChatType        string `json:"chat_type"`         // "direct" | "group" | "newsletter" | "community"
	LastMessageTime any    `json:"last_message_time"` // ISO-8601 string | null
	LastMessage     any    `json:"last_message"`      // string | null
	LastMessageID   any    `json:"last_message_id"`   // string | null
	LastSender      any    `json:"last_sender"`       // string | null
	LastIsFromMe    any    `json:"last_is_from_me"`   // bool | null
}

// ConversationDTO is the list-level analog of get_conversation's per-contact
// merge: one logical conversation collapsing a contact's linked phone JID and
// privacy LID direct chats into a single row. It embeds ChatDTO (so every
// chat field flattens into the same JSON shape list_chats returns) and adds:
//   - JIDs: all linked identities behind this conversation, phone JID first —
//     the same merged identity set get_conversation returns as `jids`.
//   - UnreadCount: unread messages summed across the merged identities, so the
//     overview answers "what needs my attention" without double-counting a
//     split contact. Sourced from the cache's chats.unread_count (already
//     ingested); not surfaced by ChatDTO/list_chats.
//
// Non-direct chats (groups, newsletters) have a single JID and pass through as
// a one-identity conversation: JIDs is just [jid].
type ConversationDTO struct {
	ChatDTO
	JIDs        []string `json:"jids"`         // all linked identities, phone JID first
	UnreadCount int      `json:"unread_count"` // summed across merged identities
}

// MessageDTO mirrors the shape of Python's `Message.to_dict()` output.
// Media metadata is only populated for non-text kinds.
//
// Enriched delivery-state fields (SenderName, Direction, DeliveryStatus)
// exist so an agent reports message state *from data* instead of inferring
// it. Before these, a caller had to interpret the is_from_me bool to know
// direction, had only the raw sender JID (no resolved name), and had to
// guess "delivered, two ticks" from context. Now:
//   - SenderName: the sender JID resolved to its contact display name (null
//     when no real name is known — the JID is not echoed as a pseudo-name).
//   - Direction: "incoming" / "outgoing", derived from is_from_me so callers
//     never re-interpret the bool. is_from_me is preserved alongside it.
//   - DeliveryStatus: "sent" / "delivered" / "read" / "unknown". The cache
//     does not yet ingest WhatsApp receipt/ack stanzas (the messages table
//     has no delivery columns), so this is currently always "unknown" — a
//     truthful stub, never an invented checkmark. When receipt ingestion
//     lands, this field carries the real ack level.
//   - Kind: the cached message kind verbatim. MediaType covers only the
//     downloadable envelopes, so without this a non-media, non-text message
//     is indistinguishable from plain text: a poll would arrive as an
//     ordinary message whose content happens to be the question, and the
//     id vote_poll / get_poll_results need would be unfindable.
//   - Reactions: the emoji reactions currently on the message, so "they
//     thumbs-upped it" is data rather than something the caller infers. Omitted
//     from the JSON entirely when there are none, which is the common case.
type MessageDTO struct {
	ID             string `json:"id"`
	ChatJID        string `json:"chat_jid"`
	ChatName       any    `json:"chat_name"` // string | null
	Sender         string `json:"sender"`
	SenderName     any    `json:"sender_name"` // string | null — sender JID resolved to contact display name
	Content        string `json:"content"`
	Timestamp      any    `json:"timestamp"` // ISO-8601 string | null
	IsFromMe       bool   `json:"is_from_me"`
	Direction      string `json:"direction"`       // "incoming" | "outgoing", derived from is_from_me
	DeliveryStatus string `json:"delivery_status"` // "sent" | "delivered" | "read" | "unknown"
	Kind           string `json:"kind"`            // "text" | "image" | "video" | "audio" | "document" | "sticker" | "poll" | "other"
	MediaType      any    `json:"media_type"`      // string | null
	Filename       any    `json:"filename,omitempty"`
	FileLength     any    `json:"file_length,omitempty"`
	// Reactions is omitted when empty so messages without reactions keep
	// exactly the payload they had before reactions were supported.
	Reactions []ReactionDTO `json:"reactions,omitempty"`
}

// ReactionDTO is one emoji reaction on a message. WhatsApp allows a single
// reaction per person per message, so each entry is a distinct reactor.
//
// Our own reaction is reported with an empty Sender and IsFromMe true: the
// cache stores it under a canonical empty sender key so that the live,
// history-sync, and send_reaction paths cannot produce duplicate rows (see
// cache.ReactionSenderKey).
type ReactionDTO struct {
	Emoji      string `json:"emoji"`
	Sender     string `json:"sender"`
	SenderName any    `json:"sender_name"` // string | null
	IsFromMe   bool   `json:"is_from_me"`
}

// JSON schema fragments shared across tools. Kept as raw strings so the
// MCP registry can pass them straight through without a marshal round
// trip.

const chatSchemaFragment = `{
  "type": "object",
  "properties": {
    "jid":               {"type": "string"},
    "name":              {"type": ["string","null"]},
    "is_group":          {"type": "boolean"},
    "chat_type":         {"type": "string", "enum": ["direct","group","newsletter","community"]},
    "last_message_time": {"type": ["string","null"], "format": "date-time"},
    "last_message":      {"type": ["string","null"]},
    "last_message_id":   {"type": ["string","null"]},
    "last_sender":       {"type": ["string","null"]},
    "last_is_from_me":   {"type": ["boolean","null"]}
  },
  "required": ["jid","is_group","chat_type"]
}`

const conversationSchemaFragment = `{
  "type": "object",
  "properties": {
    "jid":               {"type": "string", "description": "Representative identity for the conversation (the phone JID when the contact is split across phone JID + @lid)."},
    "name":              {"type": ["string","null"]},
    "is_group":          {"type": "boolean"},
    "chat_type":         {"type": "string", "enum": ["direct","group","newsletter","community"]},
    "last_message_time": {"type": ["string","null"], "format": "date-time"},
    "last_message":      {"type": ["string","null"]},
    "last_message_id":   {"type": ["string","null"]},
    "last_sender":       {"type": ["string","null"]},
    "last_is_from_me":   {"type": ["boolean","null"]},
    "jids":              {"type": "array", "items": {"type": "string"}, "description": "All linked identities merged into this conversation (phone JID first)."},
    "unread_count":      {"type": "integer", "description": "Unread messages summed across the merged identities."}
  },
  "required": ["jid","is_group","chat_type","jids","unread_count"]
}`

const messageSchemaFragment = `{
  "type": "object",
  "properties": {
    "id":              {"type": "string"},
    "chat_jid":        {"type": "string"},
    "chat_name":       {"type": ["string","null"]},
    "sender":          {"type": "string"},
    "sender_name":     {"type": ["string","null"], "description": "Sender JID resolved to contact display name; null when no name is known."},
    "content":         {"type": "string"},
    "timestamp":       {"type": ["string","null"], "format": "date-time"},
    "is_from_me":      {"type": "boolean"},
    "direction":       {"type": "string", "enum": ["incoming","outgoing"], "description": "Derived from is_from_me; callers need not interpret the bool."},
    "delivery_status": {"type": "string", "enum": ["sent","delivered","read","unknown"], "description": "WhatsApp ack level. Currently always 'unknown' until the cache ingests receipt/ack stanzas; never an inferred value."},
    "kind":            {"type": "string", "enum": ["text","image","video","audio","document","sticker","poll","other"], "description": "Cached message kind. 'poll' marks a poll creation message whose content is the question; its id is what vote_poll and get_poll_results take."},
    "media_type":      {"type": ["string","null"]},
    "filename":        {"type": ["string","null"]},
    "file_length":     {"type": ["integer","null"]},
    "reactions":       {"type": "array", "description": "Emoji reactions currently on this message; absent when there are none. Your own reaction has is_from_me true and an empty sender.",
                        "items": {"type": "object",
                                  "properties": {"emoji":       {"type": "string"},
                                                 "sender":      {"type": "string"},
                                                 "sender_name": {"type": ["string","null"]},
                                                 "is_from_me":  {"type": "boolean"}},
                                  "required": ["emoji","sender","is_from_me"]}}
  },
  "required": ["id","chat_jid","sender","content","is_from_me","direction","delivery_status","kind"]
}`
