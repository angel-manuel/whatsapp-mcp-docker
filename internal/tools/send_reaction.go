package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// maxReactionEmojiBytes bounds the reaction glyph. A single emoji — even a
// long ZWJ sequence with skin-tone and variation selectors — fits comfortably;
// anything larger is a caller mistake (a message body in the wrong field),
// which WhatsApp would render as an unusable reaction.
const maxReactionEmojiBytes = 64

// sendReactionSchema is the JSONSchema exposed to MCP clients for the
// send_reaction tool. Argument names match the upstream Python reference
// (chat_jid / message_id / emoji); `sender_jid` is the one addition, needed
// only when the target message is not in the local cache.
var sendReactionSchema = json.RawMessage(`{
  "type": "object",
  "required": ["chat_jid", "message_id", "emoji"],
  "properties": {
    "chat_jid": {
      "type": "string",
      "description": "Chat containing the message: a JID ('user@s.whatsapp.net' or 'group@g.us') or a raw phone number with country code (digits only, no + or spaces)."
    },
    "message_id": {
      "type": "string",
      "description": "Stanza id of the message to react to.",
      "minLength": 1
    },
    "emoji": {
      "type": "string",
      "description": "Emoji to react with. An empty string removes your existing reaction. A new emoji replaces it — WhatsApp allows one reaction per person per message."
    },
    "sender_jid": {
      "type": "string",
      "description": "Optional JID of the message's author. Only needed when the target message is not in the local cache; otherwise it is looked up. Pass your own JID for a message you sent."
    }
  },
  "additionalProperties": false
}`)

// SendReactionResult is the structured output of the send_reaction tool.
// MessageID is the id of the reaction stanza itself; TargetID is the message
// that was reacted to.
type SendReactionResult struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
	TargetID  string `json:"target_id"`
	Emoji     string `json:"emoji"`
	Action    string `json:"action"` // "add" | "remove"
	SentTS    int64  `json:"sent_ts"`
}

// sendReaction is the handler for the send_reaction MCP tool. It resolves the
// target message's author (which is what whatsmeow keys a reaction off),
// delegates to BuildReaction + SendMessage, and mirrors the result into the
// cache so the read tools show the reaction immediately.
func sendReaction(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			ChatJID   string `json:"chat_jid"`
			MessageID string `json:"message_id"`
			Emoji     string `json:"emoji"`
			SenderJID string `json:"sender_jid,omitempty"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}
		chatArg := strings.TrimSpace(in.ChatJID)
		if chatArg == "" {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, "chat_jid must not be empty"), nil
		}
		targetID := strings.TrimSpace(in.MessageID)
		if targetID == "" {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, "message_id must not be empty"), nil
		}
		if err := validateReactionEmoji(in.Emoji); err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}

		chat, err := resolveRecipient(chatArg)
		if err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}
		if chat.Server == types.NewsletterServer {
			// Newsletters need NewsletterSendReaction and a MessageServerID the
			// cache does not capture, so a normal reaction stanza would be
			// silently dropped. Fail loudly instead of pretending it worked.
			return mcp.ErrorResult(mcp.ErrInvalidArgument,
				"reacting to newsletter/channel messages is not supported yet (needs NewsletterSendReaction)"), nil
		}

		targetSender, resolveErr := resolveReactionTarget(ctx, deps.Cache, chat.String(), targetID, in.SenderJID)
		if resolveErr != nil {
			// Each failure mode gets its own code: a caller retrying after
			// cache_sync only helps for the uncached case, and a DB failure
			// must not read as "the message doesn't exist".
			return mcp.ErrorResult(resolveErr.code, resolveErr.Error()), nil
		}

		msg := deps.WA.BuildReaction(chat, targetSender, types.MessageID(targetID), in.Emoji)
		if msg == nil {
			return mcp.NotPairedError(), nil
		}

		resp, err := deps.WA.SendMessage(ctx, chat, msg)
		if err != nil {
			if errors.Is(err, wa.ErrNotLoggedIn) {
				return mcp.NotPairedError(), nil
			}
			return mcp.ErrorResult(mcp.ErrInternal, fmt.Sprintf("send reaction: %v", err)), nil
		}

		chatJID := chat.String()
		ts := resp.Timestamp
		if ts.IsZero() {
			ts = time.Now().UTC()
		}

		if err := mirrorReaction(ctx, deps.Cache, chatJID, targetID, in.Emoji, ts); err != nil {
			return mcp.ErrorResult(mcp.ErrInternal, fmt.Sprintf("cache reaction: %v", err)), nil
		}

		action := "add"
		if in.Emoji == "" {
			action = "remove"
		}
		return SendReactionResult{
			MessageID: string(resp.ID),
			ChatJID:   chatJID,
			TargetID:  targetID,
			Emoji:     in.Emoji,
			Action:    action,
			SentTS:    ts.Unix(),
		}, nil
	}
}

// codedError carries the structured error code a resolution failure should be
// reported under, so the caller does not have to re-classify a bare error.
type codedError struct {
	code mcp.ErrorCode
	msg  string
}

func (e *codedError) Error() string { return e.msg }

func codedErrorf(code mcp.ErrorCode, format string, args ...any) *codedError {
	return &codedError{code: code, msg: fmt.Sprintf(format, args...)}
}

// resolveReactionTarget determines the author of the message being reacted to.
// whatsmeow's BuildReaction uses it to set MessageKey.FromMe and, in groups,
// MessageKey.Participant — passing our own JID for someone else's message
// builds a from-me key and misattributes the reaction.
//
// An explicit sender_jid wins; otherwise the cache is consulted, and a message
// we sent resolves to the empty JID (whatsmeow's "this is mine" signal). A
// message that is neither supplied nor cached is an error rather than a guess.
//
// The returned error is always a *codedError: only a genuinely un-cached
// message is not_found (the one case where cache_sync is the fix); a bad JID is
// the caller's mistake, and a failed lookup is ours.
func resolveReactionTarget(ctx context.Context, store *cache.Store, chatJID, targetID, explicit string) (types.JID, *codedError) {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		jid, err := types.ParseJID(explicit)
		if err != nil {
			return types.JID{}, codedErrorf(mcp.ErrInvalidArgument, "sender_jid %q is not a valid JID: %v", explicit, err)
		}
		// ParseJID is permissive: it turns a bare string into
		// "<string>@s.whatsapp.net" and accepts an empty user or server. Any of
		// those would silently key the reaction off a nonexistent identity
		// rather than reporting the typo, so require both halves explicitly.
		if !strings.Contains(explicit, "@") || jid.User == "" || jid.Server == "" {
			return types.JID{}, codedErrorf(mcp.ErrInvalidArgument,
				"sender_jid %q must be a full JID (e.g. '15551234567@s.whatsapp.net')", explicit)
		}
		return jid, nil
	}
	if store == nil {
		return types.JID{}, codedErrorf(mcp.ErrNotFound, "message %q is not cached; pass sender_jid explicitly", targetID)
	}
	sender, isFromMe, err := store.GetMessageSender(ctx, chatJID, targetID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return types.JID{}, codedErrorf(mcp.ErrNotFound,
				"message %q not found in chat %q; run cache_sync first or pass sender_jid explicitly", targetID, chatJID)
		}
		return types.JID{}, codedErrorf(mcp.ErrInternal, "look up message %q: %v", targetID, err)
	}
	if isFromMe || sender == "" {
		// An empty JID is how whatsmeow is told the target is our own message.
		return types.JID{}, nil
	}
	jid, err := types.ParseJID(sender)
	if err != nil {
		// The cache holds something that is not a JID — our bug, not theirs.
		return types.JID{}, codedErrorf(mcp.ErrInternal,
			"cached sender %q for message %q is not a valid JID: %v", sender, targetID, err)
	}
	return jid, nil
}

// mirrorReaction writes our own reaction into the cache so the read tools
// reflect it without waiting for the echo event, mirroring what
// mirrorOutbound does for sends. Our reactions are keyed by the canonical
// empty sender (cache.ReactionSenderKey), so this row is the same one the live
// and history-sync paths would write.
func mirrorReaction(ctx context.Context, store *cache.Store, chatJID, targetID, emoji string, ts time.Time) error {
	if store == nil {
		return nil
	}
	if emoji == "" {
		return store.DeleteReaction(ctx, chatJID, targetID, cache.ReactionSenderKey("", true))
	}
	return store.UpsertReaction(ctx, cache.Reaction{
		ChatJID:   chatJID,
		TargetID:  targetID,
		Emoji:     emoji,
		Timestamp: ts,
		IsFromMe:  true,
	})
}

// validateReactionEmoji guards the one field whatsmeow will happily accept
// anything for. The empty string is legal — it is WhatsApp's "remove my
// reaction" signal.
func validateReactionEmoji(emoji string) error {
	if emoji == "" {
		return nil
	}
	if strings.TrimSpace(emoji) == "" {
		return errors.New("emoji must not be whitespace (use an empty string to remove a reaction)")
	}
	if len(emoji) > maxReactionEmojiBytes {
		return fmt.Errorf("emoji must be at most %d bytes; got %d", maxReactionEmojiBytes, len(emoji))
	}
	for _, r := range emoji {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("emoji must not contain whitespace or control characters")
		}
	}
	return nil
}
