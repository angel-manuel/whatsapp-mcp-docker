package cache

import (
	"strings"
	"time"
)

// ChatType discriminates direct chats, regular groups, newsletters, and
// community parents. Stored as TEXT in the chats.chat_type column.
type ChatType string

const (
	ChatTypeDirect     ChatType = "direct"
	ChatTypeGroup      ChatType = "group"
	ChatTypeNewsletter ChatType = "newsletter"
	ChatTypeCommunity  ChatType = "community"
)

// Chat mirrors the chats table row used by upserts.
type Chat struct {
	JID           string
	Name          string
	IsGroup       bool
	// Type, when non-empty, sets chat_type. Empty falls back to deriveChatType
	// from JID + IsGroup, so callers that don't care about the discriminator
	// (e.g. handleMessage) can leave it zero.
	Type          ChatType
	LastMessageTS time.Time
	UnreadCount   int
	Archived      bool
	Pinned        bool
	MutedUntil    time.Time
}

// deriveChatType infers chat_type from JID suffix + is_group when the caller
// did not set it explicitly. JIDs ending in @newsletter map to newsletter,
// any other group-flagged row to group, everything else to direct. Community
// parents must be set explicitly via Chat.Type by handlers that have the
// metadata (handleJoinedGroup uses GroupParent.IsParent).
func deriveChatType(jid string, isGroup bool) ChatType {
	switch {
	case strings.HasSuffix(jid, "@newsletter"):
		return ChatTypeNewsletter
	case isGroup:
		return ChatTypeGroup
	default:
		return ChatTypeDirect
	}
}

// Contact mirrors the contacts table row used by upserts.
type Contact struct {
	JID          string
	PushName     string
	BusinessName string
	FirstName    string
	FullName     string
}

// Nickname mirrors the nicknames table row used by upserts.
type Nickname struct {
	JID      string
	Nickname string
	Note     string
}

// MessageKind classifies a message row. The vocabulary is intentionally small
// — downloads and typed media payloads land later; this package only needs to
// know enough to hide edits/deletes without dropping context.
type MessageKind string

const (
	// KindText is a plain text or extended-text message.
	KindText MessageKind = "text"
	// KindImage is an image envelope. Body is caption (if any).
	KindImage MessageKind = "image"
	// KindVideo is a video envelope. Body is caption (if any).
	KindVideo MessageKind = "video"
	// KindAudio is an audio or voice-note envelope.
	KindAudio MessageKind = "audio"
	// KindDocument is a document envelope. Body is caption (if any).
	KindDocument MessageKind = "document"
	// KindSticker is a sticker envelope.
	KindSticker MessageKind = "sticker"
	// KindOther covers message kinds we do not surface yet.
	KindOther MessageKind = "other"
)

// Media captures the metadata portion of a media message. The actual bytes are
// downloaded lazily by the download_media tool; we keep only enough to
// re-request the CDN object.
type Media struct {
	Mime      string
	Filename  string
	URL       string
	Key       []byte
	SHA256    []byte
	EncSHA256 []byte
	Length    uint64
}

// Message mirrors the messages table row used by upserts.
type Message struct {
	ID             string
	ChatJID        string
	SenderJID      string
	SenderPushName string
	Timestamp      time.Time
	Kind           MessageKind
	Body           string
	ReplyToID      string
	IsFromMe       bool
	Media          *Media
}
