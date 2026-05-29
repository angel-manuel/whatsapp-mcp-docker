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

const getContactChatsDefaultLimit = 20

var getContactChatsInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "contact_jid": {"type": "string", "minLength": 1},
    "limit":       {"type": "integer", "minimum": 1, "maximum": 200, "default": 20},
    "page":        {"type": "integer", "minimum": 0, "default": 0}
  },
  "required": ["contact_jid"],
  "additionalProperties": false
}`)

var getContactChatsOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "chats": {"type": "array", "items": ` + chatSchemaFragment + `}
  },
  "required": ["chats"]
}`)

type getContactChatsInput struct {
	ContactJID string `json:"contact_jid"`
	Limit      int    `json:"limit"`
	Page       int    `json:"page"`
}

// Intended tool description:
//
// Find the full conversation with a contact. Use this to locate the conversation
// when a contact may exist under multiple JIDs (phone JID + @lid privacy LID): it
// matches both the direct chat keyed on the JID you pass AND every other chat where
// that JID is a recorded sender, so you surface the contact's threads instead of
// having to guess the exact chat key. This is the canonical entry point and
// recommended FIRST call when answering "what's the latest with this person?", "did
// they respond?", or "show me my conversation / chat history with them". Returns
// every cached chat (direct + groups) involving the given contact JID, ordered
// most-recent-first, so the latest thread surfaces at the top. If you only have one
// of a contact's identities, call this with it; pass the other JID in a follow-up
// call to cover the remaining identity. Keywords: conversation, thread, full
// history, all chats, latest message, did they reply.
func registerGetContactChats(reg *mcp.Registry, store *cache.Store) error {
	return reg.Register(mcp.Tool{
		Name: "get_contact_chats",
		Description: "Find the full conversation with a contact. Use this to locate the conversation " +
			"when a contact may exist under multiple JIDs (phone JID + @lid privacy LID): it matches " +
			"both the direct chat keyed on the JID you pass AND every other chat where that JID is a " +
			"recorded sender, so you surface the contact's threads instead of having to guess the exact " +
			"chat key. This is the canonical entry point and recommended FIRST call when answering " +
			"\"what's the latest with this person?\", \"did they respond?\", or \"show me my conversation " +
			"/ chat history with them\". Returns every cached chat (direct + groups) involving the given " +
			"contact JID, ordered most-recent-first, so the latest thread surfaces at the top. If you " +
			"only have one of a contact's identities, call this with it; pass the other JID in a " +
			"follow-up call to cover the remaining identity. " +
			"Keywords: conversation, thread, full history, all chats, latest message, did they reply.",
		InputSchema:  getContactChatsInputSchema,
		OutputSchema: getContactChatsOutputSchema,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in getContactChatsInput
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return mcp.InvalidArgumentError(fmt.Sprintf("decode arguments: %v", err)), nil
				}
			}
			return handleGetContactChats(ctx, store, in)
		},
	})
}

func handleGetContactChats(ctx context.Context, store *cache.Store, in getContactChatsInput) (any, error) {
	jid := strings.TrimSpace(in.ContactJID)
	if jid == "" {
		return mcp.InvalidArgumentError("contact_jid must not be empty"), nil
	}
	limit, offset, perr := validatePagination(in.Limit, in.Page, getContactChatsDefaultLimit)
	if perr != nil {
		return mcp.InvalidArgumentError(perr.Message), nil
	}

	// Match either:
	//  - the direct chat keyed on the contact jid, OR
	//  - any chat (direct or group) where the contact is a recorded sender.
	query := `
SELECT c.jid, c.name, c.is_group, c.chat_type, c.last_message_ts,
       COALESCE(m.body, ''), COALESCE(m.id, ''), COALESCE(m.sender_jid, ''),
       COALESCE(m.is_from_me, 0), CASE WHEN m.id IS NULL THEN 0 ELSE 1 END
FROM chats c
LEFT JOIN messages m ON m.chat_jid = c.jid AND m.ts = c.last_message_ts
WHERE c.jid = ?
   OR EXISTS (SELECT 1 FROM messages mm WHERE mm.chat_jid = c.jid AND mm.sender_jid = ?)
ORDER BY c.last_message_ts DESC, c.jid ASC
LIMIT ? OFFSET ?`
	rows, err := store.DB().QueryContext(ctx, query, jid, jid, limit, offset)
	if err != nil {
		return mcp.InternalError(fmt.Sprintf("get_contact_chats: %v", err)), nil
	}
	defer rows.Close()

	out := struct {
		Chats []ChatDTO `json:"chats"`
	}{Chats: []ChatDTO{}}
	for rows.Next() {
		var (
			cj, name, chatType, body, id, sender string
			isGroup, isFromMe                    int
			ts                                   int64
			hasMessage                           int
		)
		if err := rows.Scan(&cj, &name, &isGroup, &chatType, &ts, &body, &id, &sender, &isFromMe, &hasMessage); err != nil {
			return mcp.InternalError(fmt.Sprintf("get_contact_chats scan: %v", err)), nil
		}
		out.Chats = append(out.Chats, buildChatDTO(cj, name, isGroup != 0, chatType, ts, hasMessage == 1, body, id, sender, isFromMe != 0))
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return mcp.InternalError(fmt.Sprintf("get_contact_chats rows: %v", err)), nil
	}
	return out, nil
}
