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

var listMessagesInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "chat_jid":   {"type": ["string","null"]},
    "sender_jid": {"type": ["string","null"]},
    "query":      {"type": ["string","null"], "description": "Full-text search across message bodies (FTS5 when set; LIKE fallback handled server-side)."},
    "after":      {"type": ["string","null"], "description": "ISO-8601 timestamp; only messages strictly after this are returned."},
    "before":     {"type": ["string","null"], "description": "ISO-8601 timestamp; only messages strictly before this are returned."},
    "limit":      {"type": "integer", "minimum": 1, "maximum": 200, "default": 20},
    "page":       {"type": "integer", "minimum": 0, "default": 0}
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

func registerListMessages(reg *mcp.Registry, store *cache.Store) error {
	return reg.Register(mcp.Tool{
		Name: "list_messages",
		Description: "List cached WhatsApp messages, newest first. Supports filtering by chat, " +
			"sender, full-text body search (FTS5 when `query` is set), and timestamp range.",
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
LEFT JOIN chats c ON c.jid = m.chat_jid`
		// Wrap user input in a phrase so "foo bar" acts as a phrase match,
		// which is closer to the Python LIKE "%query%" semantics than FTS's
		// default whitespace-as-AND. Trim stray quotes before re-wrapping.
		q := strings.ReplaceAll(*in.Query, `"`, `""`)
		wheres = append(wheres, "f.messages_fts MATCH ?")
		params = append(params, `"`+q+`"`)
	} else {
		fromClause = `
FROM messages m
LEFT JOIN chats c ON c.jid = m.chat_jid`
	}

	whereSQL := ""
	if len(wheres) > 0 {
		whereSQL = "WHERE " + strings.Join(wheres, " AND ")
	}

	query := fmt.Sprintf(`
SELECT m.id, m.chat_jid, c.name, m.sender_jid, m.body, m.ts, m.is_from_me,
       m.kind, m.media_filename, m.media_length
%s
%s
ORDER BY m.ts DESC, m.id ASC
LIMIT ? OFFSET ?`, fromClause, whereSQL)
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
	return out, nil
}

// scanMessageRow decodes one row selected with the canonical message
// SELECT: (id, chat_jid, chat_name, sender_jid, body, ts, is_from_me,
// kind, media_filename, media_length). chat_name is COALESCE'd to empty
// when the chats row is missing (defensive — the ingestor always
// upserts, but tests sometimes poke fixtures directly).
func scanMessageRow(rows *sql.Rows) (MessageDTO, error) {
	var (
		id, chatJID, sender, body string
		chatName                  sql.NullString
		ts                        int64
		isFromMe                  int
		kind, mediaFilename       string
		mediaLength               int64
	)
	if err := rows.Scan(&id, &chatJID, &chatName, &sender, &body, &ts, &isFromMe, &kind, &mediaFilename, &mediaLength); err != nil {
		return MessageDTO{}, err
	}
	return buildMessageDTO(id, chatJID, chatName.String, sender, body, ts, isFromMe != 0, kind, mediaFilename, mediaLength), nil
}

// buildMessageDTO is shared across list_messages, get_message_context,
// and get_last_interaction so timestamp / media-mapping stays identical
// across every surface.
func buildMessageDTO(id, chatJID, chatName, sender, body string, ts int64, isFromMe bool, kind, mediaFilename string, mediaLength int64) MessageDTO {
	dto := MessageDTO{
		ID:        id,
		ChatJID:   chatJID,
		ChatName:  stringOrNil(chatName),
		Sender:    sender,
		Content:   body,
		Timestamp: tsISOOrNil(ts),
		IsFromMe:  isFromMe,
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
