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
type MessageDTO struct {
	ID         string `json:"id"`
	ChatJID    string `json:"chat_jid"`
	ChatName   any    `json:"chat_name"` // string | null
	Sender     string `json:"sender"`
	Content    string `json:"content"`
	Timestamp  any    `json:"timestamp"` // ISO-8601 string | null
	IsFromMe   bool   `json:"is_from_me"`
	MediaType  any    `json:"media_type"` // string | null
	Filename   any    `json:"filename,omitempty"`
	FileLength any    `json:"file_length,omitempty"`
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
    "id":          {"type": "string"},
    "chat_jid":    {"type": "string"},
    "chat_name":   {"type": ["string","null"]},
    "sender":      {"type": "string"},
    "content":     {"type": "string"},
    "timestamp":   {"type": ["string","null"], "format": "date-time"},
    "is_from_me":  {"type": "boolean"},
    "media_type":  {"type": ["string","null"]},
    "filename":    {"type": ["string","null"]},
    "file_length": {"type": ["integer","null"]}
  },
  "required": ["id","chat_jid","sender","content","is_from_me"]
}`
