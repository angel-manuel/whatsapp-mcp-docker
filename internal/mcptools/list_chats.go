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

const listChatsDefaultLimit = 20

var listChatsInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query":                {"type": ["string","null"], "description": "Case-insensitive substring match on chat name or JID."},
    "limit":                {"type": "integer", "minimum": 1, "maximum": 200, "default": 20},
    "page":                 {"type": "integer", "minimum": 0, "default": 0},
    "include_last_message": {"type": "boolean", "default": true},
    "sort_by":              {"type": "string", "enum": ["last_active","name"], "default": "last_active"},
    "chat_type":            {"type": ["string","null"], "enum": ["direct","group","newsletter","community",null], "description": "Filter to a single chat type."}
  },
  "additionalProperties": false
}`)

var listChatsOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "chats": {
      "type": "array",
      "items": ` + chatSchemaFragment + `
    }
  },
  "required": ["chats"]
}`)

type listChatsInput struct {
	Query              *string `json:"query"`
	Limit              int     `json:"limit"`
	Page               int     `json:"page"`
	IncludeLastMessage *bool   `json:"include_last_message"`
	SortBy             string  `json:"sort_by"`
	ChatType           *string `json:"chat_type"`
}

type listChatsOutput struct {
	Chats []ChatDTO `json:"chats"`
}

func registerListChats(reg *mcp.Registry, store *cache.Store) error {
	return reg.Register(mcp.Tool{
		Name: "list_chats",
		Description: "List WhatsApp chats cached locally. Supports substring search, " +
			"pagination, and optional inclusion of each chat's last message preview.",
		InputSchema:  listChatsInputSchema,
		OutputSchema: listChatsOutputSchema,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in listChatsInput
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return mcp.InvalidArgumentError(fmt.Sprintf("decode arguments: %v", err)), nil
				}
			}
			return handleListChats(ctx, store, in)
		},
	})
}

func handleListChats(ctx context.Context, store *cache.Store, in listChatsInput) (any, error) {
	limit, offset, perr := validatePagination(in.Limit, in.Page, listChatsDefaultLimit)
	if perr != nil {
		return mcp.InvalidArgumentError(perr.Message), nil
	}

	include := true
	if in.IncludeLastMessage != nil {
		include = *in.IncludeLastMessage
	}
	sortBy := strings.TrimSpace(in.SortBy)
	if sortBy == "" {
		sortBy = "last_active"
	}
	if sortBy != "last_active" && sortBy != "name" {
		return mcp.InvalidArgumentError(fmt.Sprintf("sort_by must be 'last_active' or 'name', got %q", sortBy)), nil
	}

	var (
		query    string
		params   []any
		where    string
		whereKey = "WHERE"
	)
	addWhere := func(clause string, args ...any) {
		if whereKey == "WHERE" {
			where += whereKey + " " + clause
			whereKey = "AND"
		} else {
			where += " " + whereKey + " " + clause
		}
		params = append(params, args...)
	}
	if in.Query != nil && *in.Query != "" {
		like := "%" + *in.Query + "%"
		addWhere("(LOWER(c.name) LIKE LOWER(?) OR c.jid LIKE ?)", like, like)
	}
	if in.ChatType != nil && *in.ChatType != "" {
		switch *in.ChatType {
		case "direct", "group", "newsletter", "community":
			addWhere("c.chat_type = ?", *in.ChatType)
		default:
			return mcp.InvalidArgumentError(fmt.Sprintf("chat_type must be direct|group|newsletter|community, got %q", *in.ChatType)), nil
		}
	}

	orderBy := "c.last_message_ts DESC, c.jid ASC"
	if sortBy == "name" {
		orderBy = "CASE WHEN c.name = '' THEN 1 ELSE 0 END, c.name ASC, c.jid ASC"
	}

	if include {
		query = fmt.Sprintf(`
SELECT c.jid, c.name, c.is_group, c.chat_type, c.last_message_ts,
       COALESCE(m.body, ''), COALESCE(m.id, ''), COALESCE(m.sender_jid, ''),
       COALESCE(m.is_from_me, 0), CASE WHEN m.id IS NULL THEN 0 ELSE 1 END
FROM chats c
LEFT JOIN messages m
       ON m.chat_jid = c.jid
      AND m.ts = c.last_message_ts
%s
ORDER BY %s
LIMIT ? OFFSET ?`, where, orderBy)
	} else {
		query = fmt.Sprintf(`
SELECT c.jid, c.name, c.is_group, c.chat_type, c.last_message_ts,
       '', '', '', 0, 0
FROM chats c
%s
ORDER BY %s
LIMIT ? OFFSET ?`, where, orderBy)
	}
	params = append(params, limit, offset)

	rows, err := store.DB().QueryContext(ctx, query, params...)
	if err != nil {
		return mcp.InternalError(fmt.Sprintf("list_chats: %v", err)), nil
	}
	defer rows.Close()

	out := listChatsOutput{Chats: []ChatDTO{}}
	for rows.Next() {
		var (
			jid, name, chatType, body, id, sender string
			isGroup                               int
			ts                                    int64
			isFromMe, hasMessage                  int
		)
		if err := rows.Scan(&jid, &name, &isGroup, &chatType, &ts, &body, &id, &sender, &isFromMe, &hasMessage); err != nil {
			return mcp.InternalError(fmt.Sprintf("list_chats scan: %v", err)), nil
		}
		out.Chats = append(out.Chats, buildChatDTO(jid, name, isGroup != 0, chatType, ts, include && hasMessage == 1, body, id, sender, isFromMe != 0))
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return mcp.InternalError(fmt.Sprintf("list_chats rows: %v", err)), nil
	}
	return out, nil
}

// buildChatDTO converts DB columns into the JSON envelope used by every
// chat-returning tool. includeLast controls whether the `last_*` fields
// are populated or surfaced as nulls.
func buildChatDTO(jid, name string, isGroup bool, chatType string, ts int64, includeLast bool, body, id, sender string, isFromMe bool) ChatDTO {
	dto := ChatDTO{
		JID:             jid,
		Name:            stringOrNil(name),
		IsGroup:         isGroup,
		ChatType:        chatType,
		LastMessageTime: tsISOOrNil(ts),
	}
	if includeLast {
		dto.LastMessage = stringOrNil(body)
		dto.LastMessageID = stringOrNil(id)
		dto.LastSender = stringOrNil(sender)
		dto.LastIsFromMe = isFromMe
	} else {
		dto.LastMessage = nil
		dto.LastMessageID = nil
		dto.LastSender = nil
		dto.LastIsFromMe = nil
	}
	return dto
}
