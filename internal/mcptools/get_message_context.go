package mcptools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

const (
	messageContextDefaultBefore = 5
	messageContextDefaultAfter  = 5
	messageContextMaxWindow     = 100
)

var getMessageContextInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "message_id": {"type": "string", "minLength": 1},
    "before":     {"type": "integer", "minimum": 0, "maximum": 100, "default": 5},
    "after":      {"type": "integer", "minimum": 0, "maximum": 100, "default": 5}
  },
  "required": ["message_id"],
  "additionalProperties": false
}`)

var getMessageContextOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "message": ` + messageSchemaFragment + `,
    "before":  {"type": "array", "items": ` + messageSchemaFragment + `},
    "after":   {"type": "array", "items": ` + messageSchemaFragment + `}
  },
  "required": ["message","before","after"]
}`)

type getMessageContextInput struct {
	MessageID string `json:"message_id"`
	Before    *int   `json:"before"`
	After     *int   `json:"after"`
}

type getMessageContextOutput struct {
	Message MessageDTO   `json:"message"`
	Before  []MessageDTO `json:"before"`
	After   []MessageDTO `json:"after"`
}

func registerGetMessageContext(reg *mcp.Registry, store *cache.Store) error {
	return reg.Register(mcp.Tool{
		Name: "get_message_context",
		Description: "Fetch the N messages immediately before and after a target message, " +
			"scoped to the same chat_jid as the target.",
		InputSchema:  getMessageContextInputSchema,
		OutputSchema: getMessageContextOutputSchema,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in getMessageContextInput
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return mcp.InvalidArgumentError(fmt.Sprintf("decode arguments: %v", err)), nil
				}
			}
			return handleGetMessageContext(ctx, store, in)
		},
	})
}

func handleGetMessageContext(ctx context.Context, store *cache.Store, in getMessageContextInput) (any, error) {
	if in.MessageID == "" {
		return mcp.InvalidArgumentError("message_id must not be empty"), nil
	}
	before := messageContextDefaultBefore
	if in.Before != nil {
		before = *in.Before
	}
	after := messageContextDefaultAfter
	if in.After != nil {
		after = *in.After
	}
	if before < 0 || after < 0 {
		return mcp.InvalidArgumentError("before and after must be >= 0"), nil
	}
	if before > messageContextMaxWindow || after > messageContextMaxWindow {
		return mcp.InvalidArgumentError(fmt.Sprintf("before/after must be <= %d", messageContextMaxWindow)), nil
	}

	// Look up the target message. Our PK is (chat_jid, id) — the Python
	// reference assumes global id uniqueness, but WhatsApp stanza IDs can
	// collide across chats. Pick the first match (smallest chat_jid) so
	// the result is deterministic.
	var (
		chatJID, sender, body string
		ts                    int64
		isFromMe              int
		kind, mediaFilename   string
		mediaLength           int64
		chatName              sql.NullString
	)
	targetSQL := `
SELECT m.chat_jid, m.sender_jid, m.body, m.ts, m.is_from_me,
       m.kind, m.media_filename, m.media_length, c.name
FROM messages m
LEFT JOIN chats c ON c.jid = m.chat_jid
WHERE m.id = ?
ORDER BY m.chat_jid ASC
LIMIT 1`
	err := store.DB().QueryRowContext(ctx, targetSQL, in.MessageID).Scan(
		&chatJID, &sender, &body, &ts, &isFromMe, &kind, &mediaFilename, &mediaLength, &chatName,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return mcp.NotFoundError(fmt.Sprintf("message %q not found", in.MessageID)), nil
	}
	if err != nil {
		return mcp.InternalError(fmt.Sprintf("get_message_context: %v", err)), nil
	}

	out := getMessageContextOutput{
		Message: buildMessageDTO(in.MessageID, chatJID, chatName.String, sender, body, ts, isFromMe != 0, kind, mediaFilename, mediaLength),
		Before:  []MessageDTO{},
		After:   []MessageDTO{},
	}

	if before > 0 {
		rows, err := store.DB().QueryContext(ctx, `
SELECT m.id, m.chat_jid, c.name, m.sender_jid, m.body, m.ts, m.is_from_me,
       m.kind, m.media_filename, m.media_length
FROM messages m
LEFT JOIN chats c ON c.jid = m.chat_jid
WHERE m.chat_jid = ? AND (m.ts < ? OR (m.ts = ? AND m.id < ?))
ORDER BY m.ts DESC, m.id DESC
LIMIT ?`, chatJID, ts, ts, in.MessageID, before)
		if err != nil {
			return mcp.InternalError(fmt.Sprintf("get_message_context before: %v", err)), nil
		}
		dtos, err := scanMessagesAll(rows)
		if err != nil {
			return mcp.InternalError(fmt.Sprintf("get_message_context before scan: %v", err)), nil
		}
		// Return chronological order.
		out.Before = reverseMessages(dtos)
	}

	if after > 0 {
		rows, err := store.DB().QueryContext(ctx, `
SELECT m.id, m.chat_jid, c.name, m.sender_jid, m.body, m.ts, m.is_from_me,
       m.kind, m.media_filename, m.media_length
FROM messages m
LEFT JOIN chats c ON c.jid = m.chat_jid
WHERE m.chat_jid = ? AND (m.ts > ? OR (m.ts = ? AND m.id > ?))
ORDER BY m.ts ASC, m.id ASC
LIMIT ?`, chatJID, ts, ts, in.MessageID, after)
		if err != nil {
			return mcp.InternalError(fmt.Sprintf("get_message_context after: %v", err)), nil
		}
		dtos, err := scanMessagesAll(rows)
		if err != nil {
			return mcp.InternalError(fmt.Sprintf("get_message_context after scan: %v", err)), nil
		}
		out.After = dtos
	}

	return out, nil
}

func scanMessagesAll(rows *sql.Rows) ([]MessageDTO, error) {
	defer rows.Close()
	out := []MessageDTO{}
	for rows.Next() {
		dto, err := scanMessageRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dto)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return out, nil
}

func reverseMessages(in []MessageDTO) []MessageDTO {
	out := make([]MessageDTO, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}
