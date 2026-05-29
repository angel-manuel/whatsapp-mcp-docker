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
type MessageDTO struct {
	ID             string `json:"id"`
	ChatJID        string `json:"chat_jid"`
	ChatName       any    `json:"chat_name"` // string | null
	Sender         string `json:"sender"`
	SenderName     any    `json:"sender_name"`     // string | null — sender JID resolved to contact display name
	Content        string `json:"content"`
	Timestamp      any    `json:"timestamp"` // ISO-8601 string | null
	IsFromMe       bool   `json:"is_from_me"`
	Direction      string `json:"direction"`       // "incoming" | "outgoing", derived from is_from_me
	DeliveryStatus string `json:"delivery_status"` // "sent" | "delivered" | "read" | "unknown"
	MediaType      any    `json:"media_type"`      // string | null
	Filename       any    `json:"filename,omitempty"`
	FileLength     any    `json:"file_length,omitempty"`
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
    "media_type":      {"type": ["string","null"]},
    "filename":        {"type": ["string","null"]},
    "file_length":     {"type": ["integer","null"]}
  },
  "required": ["id","chat_jid","sender","content","is_from_me","direction","delivery_status"]
}`
