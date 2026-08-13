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

var getLastInteractionInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "contact_jid": {"type": "string", "minLength": 1}
  },
  "required": ["contact_jid"],
  "additionalProperties": false
}`)

var getLastInteractionOutputSchema = json.RawMessage(messageSchemaFragment)

type getLastInteractionInput struct {
	ContactJID string `json:"contact_jid"`
}

// Argument contract:
//   - `contact_jid` (required) is a person's identity (phone JID or @lid).
//   - No pagination and no sort parameter: returns the single newest message
//     (ts DESC) across the contact's 1:1 chat and any group they sent to.
//
// Intended tool description (verbatim):
//
//	Return the most recent cached message exchanged with the contact —
//	either in the 1:1 chat keyed on their JID or any group where they were
//	the sender. is_from_me reflects the sending account/device, not the
//	speaker's role. With multi-device or business accounts the same person
//	can send from multiple JIDs, and an operator controlling both ends sees
//	their own JID on both sides. Don't infer who said what from message
//	content — trust sender JID + is_from_me. Returns only the single most
//	recent message.
//
// Intended tool description addendum (freshness contract, verbatim):
// "Served from local cache, which may lag live WhatsApp. An empty result can mean 'not yet synced,' not 'no message.' For a guaranteed-fresh read: cache_sync → poll cache_sync_status until finished → read."
func registerGetLastInteraction(reg *mcp.Registry, store *cache.Store) error {
	return reg.Register(mcp.Tool{
		Name: "get_last_interaction",
		Description: "Return the most recent cached message exchanged with the contact — " +
			"either in the 1:1 chat keyed on their JID or any group where they were the sender. " +
			"is_from_me reflects the sending account/device, not the speaker's role. " +
			"With multi-device or business accounts the same person can send from multiple JIDs, " +
			"and an operator controlling both ends sees their own JID on both sides. " +
			"Don't infer who said what from message content — trust sender JID + is_from_me. " +
			"Returns only the single most recent message. " +
			"Served from local cache, which may lag live WhatsApp. An empty result can mean " +
			"'not yet synced,' not 'no message.' For a guaranteed-fresh read: cache_sync → " +
			"poll cache_sync_status until finished → read.",
		InputSchema:  getLastInteractionInputSchema,
		OutputSchema: getLastInteractionOutputSchema,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in getLastInteractionInput
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return mcp.InvalidArgumentError(fmt.Sprintf("decode arguments: %v", err)), nil
				}
			}
			return handleGetLastInteraction(ctx, store, in)
		},
	})
}

func handleGetLastInteraction(ctx context.Context, store *cache.Store, in getLastInteractionInput) (any, error) {
	jid := strings.TrimSpace(in.ContactJID)
	if jid == "" {
		return mcp.InvalidArgumentError("contact_jid must not be empty"), nil
	}

	query := `
SELECT m.id, m.chat_jid, c.name, m.sender_jid, m.body, m.ts, m.is_from_me,
       m.kind, m.media_filename, m.media_length,
       ` + messageSenderNameColumns + `
FROM messages m
LEFT JOIN chats c ON c.jid = m.chat_jid` + messageContactJoins + `
WHERE m.sender_jid = ? OR m.chat_jid = ?
ORDER BY m.ts DESC, m.id DESC
LIMIT 1`
	rows, err := store.DB().QueryContext(ctx, query, jid, jid)
	if err != nil {
		return mcp.InternalError(fmt.Sprintf("get_last_interaction: %v", err)), nil
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return mcp.InternalError(fmt.Sprintf("get_last_interaction rows: %v", err)), nil
		}
		return mcp.NotFoundError(fmt.Sprintf("no interaction found for %q", in.ContactJID)), nil
	}
	dto, err := scanMessageRow(rows)
	if err != nil {
		return mcp.InternalError(fmt.Sprintf("get_last_interaction scan: %v", err)), nil
	}
	// attachReactions works on a slice; the one-element window is the whole
	// result here.
	one := []MessageDTO{dto}
	if err := attachReactions(ctx, store, one); err != nil {
		return mcp.InternalError(fmt.Sprintf("get_last_interaction reactions: %v", err)), nil
	}
	return one[0], nil
}
