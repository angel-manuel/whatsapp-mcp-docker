package mcptools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

const listMessagesDefaultLimit = 20

const (
	// directionIncoming / directionOutgoing are the explicit message
	// directions derived from is_from_me, so callers report direction from
	// data instead of re-interpreting the bool.
	directionIncoming = "incoming"
	directionOutgoing = "outgoing"
	// deliveryStatusUnknown is the stub returned for every message until the
	// cache ingests WhatsApp receipt/ack stanzas — the messages table has no
	// delivery columns yet. It is deliberately truthful: we never invent
	// "delivered"/"read" the agent would otherwise have to infer.
	deliveryStatusUnknown = "unknown"
)

// messageContactJoins joins the contacts + nicknames tables on the message
// sender so the canonical SELECT can resolve the sender JID to a display
// name in a single query (no N+1 lookups). Sender columns are COALESCE'd to
// empty when the sender has no contact/nickname row.
const messageContactJoins = `
LEFT JOIN contacts ct  ON ct.jid = m.sender_jid
LEFT JOIN nicknames nk ON nk.jid = m.sender_jid`

// messageSenderNameColumns is the trailing column list every canonical
// message SELECT appends so scanMessageRow can resolve the sender name.
const messageSenderNameColumns = `COALESCE(ct.push_name,''), COALESCE(ct.business_name,''),
       COALESCE(ct.first_name,''), COALESCE(ct.full_name,''), COALESCE(nk.nickname,'')`

var listMessagesInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "chat_jid":   {"type": ["string","null"], "description": "Restrict to one chat key (a chat's JID). Distinct from contact_jid/sender_jid."},
    "sender_jid": {"type": ["string","null"], "description": "Restrict to messages authored by this JID."},
    "query":      {"type": ["string","null"], "description": "The search argument (named 'query', NOT 'search'). Full-text search across message bodies (FTS5 when set; LIKE fallback handled server-side)."},
    "after":      {"type": ["string","null"], "description": "ISO-8601 timestamp (a date/time string, NOT a count); only messages strictly after this are returned."},
    "before":     {"type": ["string","null"], "description": "ISO-8601 timestamp (a date/time string, NOT a count); only messages strictly before this are returned."},
    "limit":      {"type": "integer", "minimum": 1, "maximum": 200, "default": 20},
    "page":       {"type": "integer", "minimum": 0, "default": 0, "description": "0-based page index. Results are always ordered newest-first; there is NO sort parameter — page through time with before/after."}
  },
  "additionalProperties": false
}`)

var listMessagesOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "messages": {
      "type": "array",
      "items": ` + messageSchemaFragment + `
    }
  },
  "required": ["messages"]
}`)

type listMessagesInput struct {
	ChatJID   *string `json:"chat_jid"`
	SenderJID *string `json:"sender_jid"`
	Query     *string `json:"query"`
	After     *string `json:"after"`
	Before    *string `json:"before"`
	Limit     int     `json:"limit"`
	Page      int     `json:"page"`
}

type listMessagesOutput struct {
	Messages []MessageDTO `json:"messages"`
}

// Argument contract (normalized across the list_* family):
//   - Search arg is `query` (FTS5 over message bodies). There is NO `search` arg.
//   - Pagination is `limit` (1–200, default 20) + `page` (0-based).
//   - Time range is `before`/`after` as ISO-8601 timestamps (NOT counts —
//     contrast get_message_context, where before/after are integer windows).
//   - Filters: `chat_jid` (a chat key), `sender_jid` (author).
//   - Ordering is fixed newest-first (ts DESC, id ASC). There is NO sort
//     parameter; page backwards through time with before/after.
//
// Intended tool description (verbatim):
//
//	List cached WhatsApp messages. Supports filtering by chat, sender,
//	full-text body search (FTS5 when `query` is set), and timestamp range.
//	is_from_me reflects the sending account/device, not the speaker's role.
//	With multi-device or business accounts the same person can send from
//	multiple JIDs, and an operator controlling both ends sees their own JID
//	on both sides. Don't infer who said what from message content — trust
//	sender JID + is_from_me. Always ordered newest-first; there is no sort
//	parameter — use before/after for ranges. The search arg is `query`.
//
// Intended tool description addendum (freshness contract, verbatim):
// "Served from local cache, which may lag live WhatsApp. An empty result can mean 'not yet synced,' not 'no message.' For a guaranteed-fresh read: cache_sync → poll cache_sync_status until finished → read."
func registerListMessages(reg *mcp.Registry, store *cache.Store) error {
	return reg.Register(mcp.Tool{
		Name: "list_messages",
		Description: "List cached WhatsApp messages. Supports filtering by chat, sender, " +
			"full-text body search (FTS5 when `query` is set), and timestamp range. " +
			"is_from_me reflects the sending account/device, not the speaker's role. " +
			"With multi-device or business accounts the same person can send from multiple JIDs, " +
			"and an operator controlling both ends sees their own JID on both sides. " +
			"Don't infer who said what from message content — trust sender JID + is_from_me. " +
			"Always ordered newest-first; there is no sort parameter — use before/after for ranges. " +
			"The search arg is `query` (not `search`). " +
			"Served from local cache, which may lag live WhatsApp. An empty result can mean " +
			"'not yet synced,' not 'no message.' For a guaranteed-fresh read: cache_sync → " +
			"poll cache_sync_status until finished → read.",
		InputSchema:  listMessagesInputSchema,
		OutputSchema: listMessagesOutputSchema,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in listMessagesInput
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return mcp.InvalidArgumentError(fmt.Sprintf("decode arguments: %v", err)), nil
				}
			}
			return handleListMessages(ctx, store, in)
		},
	})
}

func handleListMessages(ctx context.Context, store *cache.Store, in listMessagesInput) (any, error) {
	limit, offset, perr := validatePagination(in.Limit, in.Page, listMessagesDefaultLimit)
	if perr != nil {
		return mcp.InvalidArgumentError(perr.Message), nil
	}

	var (
		wheres []string
		params []any
	)
	if in.ChatJID != nil && *in.ChatJID != "" {
		wheres = append(wheres, "m.chat_jid = ?")
		params = append(params, *in.ChatJID)
	}
	if in.SenderJID != nil && *in.SenderJID != "" {
		wheres = append(wheres, "m.sender_jid = ?")
		params = append(params, *in.SenderJID)
	}
	if in.After != nil && *in.After != "" {
		t, err := parseISOTime(*in.After)
		if err != nil {
			return mcp.InvalidArgumentError(fmt.Sprintf("after: invalid ISO-8601 timestamp %q", *in.After)), nil
		}
		wheres = append(wheres, "m.ts > ?")
		params = append(params, t.Unix())
	}
	if in.Before != nil && *in.Before != "" {
		t, err := parseISOTime(*in.Before)
		if err != nil {
			return mcp.InvalidArgumentError(fmt.Sprintf("before: invalid ISO-8601 timestamp %q", *in.Before)), nil
		}
		wheres = append(wheres, "m.ts < ?")
		params = append(params, t.Unix())
	}

	var fromClause string
	if in.Query != nil && *in.Query != "" {
		// FTS5 path: join messages_fts on rowid.
		fromClause = `
FROM messages m
JOIN messages_fts f ON f.rowid = m.rowid
LEFT JOIN chats c ON c.jid = m.chat_jid` + messageContactJoins
		// Wrap user input in a phrase so "foo bar" acts as a phrase match,
		// which is closer to the Python LIKE "%query%" semantics than FTS's
		// default whitespace-as-AND. Trim stray quotes before re-wrapping.
		q := strings.ReplaceAll(*in.Query, `"`, `""`)
		wheres = append(wheres, "f.messages_fts MATCH ?")
		params = append(params, `"`+q+`"`)
	} else {
		fromClause = `
FROM messages m
LEFT JOIN chats c ON c.jid = m.chat_jid` + messageContactJoins
	}

	whereSQL := ""
	if len(wheres) > 0 {
		whereSQL = "WHERE " + strings.Join(wheres, " AND ")
	}

	query := fmt.Sprintf(`
SELECT m.id, m.chat_jid, c.name, m.sender_jid, m.body, m.ts, m.is_from_me,
       m.kind, m.media_filename, m.media_length,
       %s
%s
%s
ORDER BY m.ts DESC, m.id ASC
LIMIT ? OFFSET ?`, messageSenderNameColumns, fromClause, whereSQL)
	params = append(params, limit, offset)

	rows, err := store.DB().QueryContext(ctx, query, params...)
	if err != nil {
		return mcp.InternalError(fmt.Sprintf("list_messages: %v", err)), nil
	}
	defer rows.Close()

	out := listMessagesOutput{Messages: []MessageDTO{}}
	for rows.Next() {
		dto, err := scanMessageRow(rows)
		if err != nil {
			return mcp.InternalError(fmt.Sprintf("list_messages scan: %v", err)), nil
		}
		out.Messages = append(out.Messages, dto)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return mcp.InternalError(fmt.Sprintf("list_messages rows: %v", err)), nil
	}
	if err := attachReactions(ctx, store, out.Messages); err != nil {
		return mcp.InternalError(fmt.Sprintf("list_messages reactions: %v", err)), nil
	}
	return out, nil
}

// scanMessageRow decodes one row selected with the canonical message
// SELECT: (id, chat_jid, chat_name, sender_jid, body, ts, is_from_me,
// kind, media_filename, media_length) followed by the five sender-name
// columns from messageSenderNameColumns (push_name, business_name,
// first_name, full_name, nickname). chat_name is COALESCE'd to empty when
// the chats row is missing (defensive — the ingestor always upserts, but
// tests sometimes poke fixtures directly); the sender-name columns are
// COALESCE'd in SQL so a sender with no contact row scans to empty strings.
func scanMessageRow(rows *sql.Rows) (MessageDTO, error) {
	var (
		id, chatJID, sender, body string
		chatName                  sql.NullString
		ts                        int64
		isFromMe                  int
		kind, mediaFilename       string
		mediaLength               int64
		pushName, businessName    string
		firstName, fullName       string
		nickname                  string
	)
	if err := rows.Scan(&id, &chatJID, &chatName, &sender, &body, &ts, &isFromMe, &kind, &mediaFilename, &mediaLength,
		&pushName, &businessName, &firstName, &fullName, &nickname); err != nil {
		return MessageDTO{}, err
	}
	senderName := resolveSenderName(cache.ContactRow{
		JID: sender, PushName: pushName, BusinessName: businessName,
		FirstName: firstName, FullName: fullName, Nickname: nickname,
	})
	return buildMessageDTO(id, chatJID, chatName.String, sender, senderName, body, ts, isFromMe != 0, kind, mediaFilename, mediaLength), nil
}

// resolveSenderName resolves a sender contact row to a display name, or ""
// when no real name is known. It returns "" rather than the phone/JID
// fallback so MessageDTO.SenderName is null (not a pseudo-name echoing the
// JID) for senders with no contact or nickname row.
func resolveSenderName(row cache.ContactRow) string {
	if row.Nickname == "" && row.FullName == "" && row.PushName == "" &&
		row.FirstName == "" && row.BusinessName == "" {
		return ""
	}
	return row.DisplayName()
}

// buildMessageDTO is shared across list_messages, get_message_context,
// and get_last_interaction so timestamp / media-mapping stays identical
// across every surface. It also stamps the enriched delivery-state fields
// (see MessageDTO) so callers report direction and delivery state from data
// rather than interpreting is_from_me or inferring checkmarks: Direction is
// derived from isFromMe, SenderName carries the resolved sender display name
// ("" → null), and DeliveryStatus is the honest "unknown" stub until the
// cache ingests WhatsApp receipt/ack stanzas.
func buildMessageDTO(id, chatJID, chatName, sender, senderName, body string, ts int64, isFromMe bool, kind, mediaFilename string, mediaLength int64) MessageDTO {
	direction := directionIncoming
	if isFromMe {
		direction = directionOutgoing
	}
	dto := MessageDTO{
		ID:             id,
		ChatJID:        chatJID,
		ChatName:       stringOrNil(chatName),
		Sender:         sender,
		SenderName:     stringOrNil(senderName),
		Content:        body,
		Timestamp:      tsISOOrNil(ts),
		IsFromMe:       isFromMe,
		Direction:      direction,
		DeliveryStatus: deliveryStatusUnknown,
		Kind:           normaliseKind(kind),
	}
	media := mapKindToMediaType(kind)
	if media == "" {
		dto.MediaType = nil
	} else {
		dto.MediaType = media
		dto.Filename = stringOrNil(mediaFilename)
		if mediaLength > 0 {
			dto.FileLength = mediaLength
		} else {
			dto.FileLength = nil
		}
	}
	return dto
}

// normaliseKind maps the stored `messages.kind` value onto the closed set the
// message schema declares. An empty column (rows written before a kind was
// recorded) reads as text; anything outside the vocabulary collapses to
// "other" rather than escaping as a value clients validating against the
// declared enum would reject.
func normaliseKind(kind string) string {
	switch cache.MessageKind(kind) {
	case cache.KindText, cache.KindImage, cache.KindVideo, cache.KindAudio,
		cache.KindDocument, cache.KindSticker, cache.KindPoll, cache.KindOther:
		return kind
	case "":
		return string(cache.KindText)
	default:
		return string(cache.KindOther)
	}
}

// mapKindToMediaType converts the local `messages.kind` column
// vocabulary to the Python reference's `media_type`. Text messages have
// no media_type (returned as null).
func mapKindToMediaType(kind string) string {
	switch cache.MessageKind(kind) {
	case cache.KindImage:
		return "image"
	case cache.KindVideo:
		return "video"
	case cache.KindAudio:
		return "audio"
	case cache.KindDocument:
		return "document"
	case cache.KindSticker:
		return "sticker"
	default:
		return ""
	}
}
