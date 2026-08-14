package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

// markReadMaxIDs bounds one receipt. whatsmeow packs every id after the first
// into a <list> child of a single stanza, so a huge batch is one very large
// frame; WhatsApp clients batch far more conservatively than this.
const markReadMaxIDs = 200

var markReadSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "chat_jid": {
      "type": "string",
      "minLength": 1,
      "description": "Chat the messages live in: ` + chatJIDForms + `. A newsletter JID is also accepted."
    },
    "message_ids": {
      "type": "array",
      "minItems": 1,
      "maxItems": 200,
      "items": {"type": "string", "minLength": 1},
      "description": "Stanza ids to acknowledge. All of them must have been sent by the same author — WhatsApp encodes one author per receipt."
    },
    "sender_jid": {
      "type": "string",
      "description": "Author of the messages. Required in group chats; ignored in direct chats, where the author is the chat itself."
    }
  },
  "required": ["chat_jid", "message_ids"],
  "additionalProperties": false
}`)

// MarkReadResult confirms the receipt that was sent. ReadTS is the UNIX-
// seconds read-at timestamp reported to the sender.
type MarkReadResult struct {
	ChatJID    string   `json:"chat_jid"`
	MessageIDs []string `json:"message_ids"`
	Count      int      `json:"count"`
	ReadTS     int64    `json:"read_ts"`
	// CacheWarning is set when the receipt went out but the local unread
	// flag could not be cleared. The result is still a success: the
	// account-visible half is done and cannot be undone, and the flag is
	// derived state that cache_sync's app_state stage re-reads from the
	// server. Reporting an error here would throw away ReadTS and
	// MessageIDs, leaving the caller unable to tell what it acknowledged.
	CacheWarning string `json:"cache_warning,omitempty"`
}

// markRead is the handler for mark_read. It sends the receipt and then
// clears the chat's cached unread flag, which is otherwise only written when
// a MarkChatAsRead app-state event arrives from another device — reading from
// here would leave the cache claiming the chat is still unread.
func markRead(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			ChatJID    string   `json:"chat_jid"`
			MessageIDs []string `json:"message_ids"`
			SenderJID  string   `json:"sender_jid"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		if strings.TrimSpace(in.ChatJID) == "" {
			return mcp.InvalidArgumentError("chat_jid must not be empty"), nil
		}
		chat, kind, err := chatTarget(in.ChatJID)
		if err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		if kind == jidKindUnknown {
			return mcp.InvalidArgumentError(
				fmt.Sprintf("chat_jid %q is a @%s chat, which does not take read receipts", in.ChatJID, chat.Server)), nil
		}

		ids, err := normaliseMessageIDs(in.MessageIDs)
		if err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}

		// whatsmeow only puts a `participant` attribute on the receipt for
		// non-direct chats, and the server needs it there: without it a group
		// receipt cannot be attributed to an author.
		var sender types.JID
		senderIn := strings.TrimSpace(in.SenderJID)
		if chat.Server == types.GroupServer && senderIn == "" {
			return mcp.InvalidArgumentError("sender_jid is required in group chats"), nil
		}
		if senderIn != "" {
			parsed, _, err := chatTarget(senderIn)
			if err != nil {
				return mcp.InvalidArgumentError(fmt.Sprintf("sender_jid: %v", err)), nil
			}
			sender = parsed.ToNonAD()
		}

		readAt := time.Now().UTC()
		if err := deps.WA.MarkRead(ctx, ids, readAt, chat, sender); err != nil {
			return accountOpError("mark read", err), nil
		}

		chatJID := chat.String()
		out := MarkReadResult{
			ChatJID:    chatJID,
			MessageIDs: ids,
			Count:      len(ids),
			ReadTS:     readAt.Unix(),
		}
		if deps.Cache != nil {
			if err := deps.Cache.SetChatUnread(ctx, chatJID, kind == jidKindGroup, false); err != nil {
				out.CacheWarning = fmt.Sprintf("read receipt sent, but clearing the cached unread flag failed: %v; run cache_sync to reconcile", err)
			}
		}
		return out, nil
	}
}

// normaliseMessageIDs trims, drops duplicates (a repeated id in one stanza is
// wasted bytes) and enforces the batch bound. Order is preserved because
// whatsmeow sends ids[0] as the stanza's own id and the rest as children.
func normaliseMessageIDs(in []string) ([]types.MessageID, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("message_ids must contain at least one id")
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]types.MessageID, 0, len(in))
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, fmt.Errorf("message_ids must not contain empty ids")
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) > markReadMaxIDs {
		return nil, fmt.Errorf("message_ids has %d entries; at most %d are accepted per call", len(out), markReadMaxIDs)
	}
	return out, nil
}
