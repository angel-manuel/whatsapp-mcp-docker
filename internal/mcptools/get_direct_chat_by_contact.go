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

// groupJIDSuffix is the well-known whatsmeow server for group JIDs; we
// reject it here because a "direct" chat never lives on @g.us.
const groupJIDSuffix = "@g.us"

var getDirectChatByContactInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "contact_jid": {"type": "string", "minLength": 1, "description": "JID or bare phone number of the contact."}
  },
  "required": ["contact_jid"],
  "additionalProperties": false
}`)

var getDirectChatByContactOutputSchema = json.RawMessage(chatSchemaFragment)

type getDirectChatByContactInput struct {
	ContactJID string `json:"contact_jid"`
}

func registerGetDirectChatByContact(reg *mcp.Registry, store *cache.Store) error {
	return reg.Register(mcp.Tool{
		Name: "get_direct_chat_by_contact",
		Description: "Resolve a contact identifier (full JID or bare phone number) to the " +
			"1:1 chat metadata. Returns not_found when no direct chat exists.",
		InputSchema:  getDirectChatByContactInputSchema,
		OutputSchema: getDirectChatByContactOutputSchema,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in getDirectChatByContactInput
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return mcp.InvalidArgumentError(fmt.Sprintf("decode arguments: %v", err)), nil
				}
			}
			return handleGetDirectChatByContact(ctx, store, in)
		},
	})
}

func handleGetDirectChatByContact(ctx context.Context, store *cache.Store, in getDirectChatByContactInput) (any, error) {
	input := strings.TrimSpace(in.ContactJID)
	if input == "" {
		return mcp.InvalidArgumentError("contact_jid must not be empty"), nil
	}
	if strings.HasSuffix(input, groupJIDSuffix) {
		return mcp.InvalidArgumentError("contact_jid must identify a direct (non-group) chat"), nil
	}

	// The Python reference accepts either a full JID or a phone fragment
	// and LIKE-matches. We preserve that flexibility: prefer an exact
	// match, fall back to LIKE for bare-number input.
	var (
		jid, name, body, id, sender string
		isGroup, isFromMe           int
		ts                          int64
		hasMessage                  int
	)
	query := `
SELECT c.jid, c.name, c.is_group, c.last_message_ts,
       COALESCE(m.body, ''), COALESCE(m.id, ''), COALESCE(m.sender_jid, ''),
       COALESCE(m.is_from_me, 0), CASE WHEN m.id IS NULL THEN 0 ELSE 1 END
FROM chats c
LEFT JOIN messages m ON m.chat_jid = c.jid AND m.ts = c.last_message_ts
WHERE c.is_group = 0 AND c.jid = ?`
	err := store.DB().QueryRowContext(ctx, query, input).Scan(
		&jid, &name, &isGroup, &ts, &body, &id, &sender, &isFromMe, &hasMessage,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// Try LIKE fallback — caller gave a bare number or partial JID.
		likeQuery := strings.ReplaceAll(strings.ReplaceAll(input, "%", `\%`), "_", `\_`)
		err = store.DB().QueryRowContext(ctx, `
SELECT c.jid, c.name, c.is_group, c.last_message_ts,
       COALESCE(m.body, ''), COALESCE(m.id, ''), COALESCE(m.sender_jid, ''),
       COALESCE(m.is_from_me, 0), CASE WHEN m.id IS NULL THEN 0 ELSE 1 END
FROM chats c
LEFT JOIN messages m ON m.chat_jid = c.jid AND m.ts = c.last_message_ts
WHERE c.is_group = 0 AND c.jid LIKE ? ESCAPE '\'
ORDER BY c.jid ASC
LIMIT 1`, "%"+likeQuery+"%").Scan(
			&jid, &name, &isGroup, &ts, &body, &id, &sender, &isFromMe, &hasMessage,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return mcp.NotFoundError(fmt.Sprintf("no direct chat matching %q", in.ContactJID)), nil
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return mcp.InternalError(fmt.Sprintf("get_direct_chat_by_contact: %v", err)), nil
	}
	return buildChatDTO(jid, name, isGroup != 0, ts, hasMessage == 1, body, id, sender, isFromMe != 0), nil
}
