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

var getChatInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "chat_jid":             {"type": "string", "minLength": 1},
    "include_last_message": {"type": "boolean", "default": true}
  },
  "required": ["chat_jid"],
  "additionalProperties": false
}`)

var getChatOutputSchema = json.RawMessage(chatSchemaFragment)

type getChatInput struct {
	ChatJID            string `json:"chat_jid"`
	IncludeLastMessage *bool  `json:"include_last_message"`
}

func registerGetChat(reg *mcp.Registry, store *cache.Store) error {
	return reg.Register(mcp.Tool{
		Name:         "get_chat",
		Description:  "Fetch cached metadata for a single WhatsApp chat by JID, optionally including the last message preview.",
		InputSchema:  getChatInputSchema,
		OutputSchema: getChatOutputSchema,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in getChatInput
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return mcp.InvalidArgumentError(fmt.Sprintf("decode arguments: %v", err)), nil
				}
			}
			return handleGetChat(ctx, store, in)
		},
	})
}

func handleGetChat(ctx context.Context, store *cache.Store, in getChatInput) (any, error) {
	if strings.TrimSpace(in.ChatJID) == "" {
		return mcp.InvalidArgumentError("chat_jid must not be empty"), nil
	}
	include := true
	if in.IncludeLastMessage != nil {
		include = *in.IncludeLastMessage
	}

	var (
		name, chatType, body, id, sender string
		isGroup, isFromMe                int
		ts                               int64
		hasMessage                       int
	)
	query := `
SELECT c.name, c.is_group, c.chat_type, c.last_message_ts,
       COALESCE(m.body, ''), COALESCE(m.id, ''), COALESCE(m.sender_jid, ''),
       COALESCE(m.is_from_me, 0), CASE WHEN m.id IS NULL THEN 0 ELSE 1 END
FROM chats c
LEFT JOIN messages m
       ON m.chat_jid = c.jid
      AND m.ts = c.last_message_ts
WHERE c.jid = ?`
	err := store.DB().QueryRowContext(ctx, query, in.ChatJID).Scan(
		&name, &isGroup, &chatType, &ts, &body, &id, &sender, &isFromMe, &hasMessage,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return mcp.NotFoundError(fmt.Sprintf("chat %q not found", in.ChatJID)), nil
	}
	if err != nil {
		return mcp.InternalError(fmt.Sprintf("get_chat: %v", err)), nil
	}

	return buildChatDTO(in.ChatJID, name, isGroup != 0, chatType, ts, include && hasMessage == 1, body, id, sender, isFromMe != 0), nil
}
