package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// sendMessageSchema is the JSONSchema exposed to MCP clients for the
// send_message tool. `recipient` accepts either a full JID or a raw
// phone number; `reply_to_id` is optional and, when set, wraps the body
// in an ExtendedTextMessage carrying the quoted stanza id.
var sendMessageSchema = json.RawMessage(`{
  "type": "object",
  "required": ["recipient", "text"],
  "properties": {
    "recipient": {
      "type": "string",
      "description": "Destination chat: a JID ('user@s.whatsapp.net' or 'group@g.us') or a raw phone number with country code (digits only, no + or spaces)."
    },
    "text": {
      "type": "string",
      "description": "Message body. Must be non-empty.",
      "minLength": 1
    },
    "reply_to_id": {
      "type": "string",
      "description": "Optional stanza id of the message to quote-reply to."
    }
  },
  "additionalProperties": false
}`)

// SendMessageResult is the structured output of the send_message tool.
// Timestamps are reported as Unix seconds to match the cache's own
// resolution.
type SendMessageResult struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
	SentTS    int64  `json:"sent_ts"`
}

// sendMessage is the handler for the send_message MCP tool. It
// normalises the recipient (JID or raw phone number), wraps the body in
// ExtendedTextMessage when replying, delegates to whatsmeow.SendMessage
// via deps.WA, and mirrors the outbound row into the cache so
// list_messages sees it.
func sendMessage(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			Recipient string `json:"recipient"`
			Text      string `json:"text"`
			ReplyToID string `json:"reply_to_id,omitempty"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}
		recipient := strings.TrimSpace(in.Recipient)
		if recipient == "" {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, "recipient must not be empty"), nil
		}
		if in.Text == "" {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, "text must not be empty"), nil
		}

		to, err := resolveRecipient(recipient)
		if err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}

		ownJID := deps.WA.OwnJID()
		msg := buildOutboundMessage(in.Text, in.ReplyToID, ownJID)

		resp, err := deps.WA.SendMessage(ctx, to, msg)
		if err != nil {
			if errors.Is(err, wa.ErrNotLoggedIn) {
				return mcp.NotPairedError(), nil
			}
			return mcp.ErrorResult(mcp.ErrInternal, fmt.Sprintf("send message: %v", err)), nil
		}

		chatJID := to.String()
		ts := resp.Timestamp
		if ts.IsZero() {
			ts = time.Now().UTC()
		}

		if err := mirrorOutbound(ctx, deps.Cache, chatJID, ownJID, to, in.Text, string(resp.ID), in.ReplyToID, ts); err != nil {
			return mcp.ErrorResult(mcp.ErrInternal, fmt.Sprintf("cache outbound: %v", err)), nil
		}

		return SendMessageResult{
			MessageID: string(resp.ID),
			ChatJID:   chatJID,
			SentTS:    ts.Unix(),
		}, nil
	}
}

// buildOutboundMessage constructs the proto envelope for a text send.
// Plain sends use Conversation; replies use ExtendedTextMessage so
// ContextInfo can carry the quoted stanza id.
func buildOutboundMessage(text, replyTo string, owner types.JID) *waE2E.Message {
	if replyTo == "" {
		return &waE2E.Message{Conversation: proto.String(text)}
	}
	ci := &waE2E.ContextInfo{StanzaID: proto.String(replyTo)}
	if !owner.IsEmpty() {
		// WhatsApp expects ContextInfo.Participant on a reply; for an
		// outbound send this is always our own non-AD JID.
		ci.Participant = proto.String(owner.ToNonAD().String())
	}
	return &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: ci,
		},
	}
}

func mirrorOutbound(ctx context.Context, store *cache.Store, chatJID string, ownJID, to types.JID, body, msgID, replyTo string, ts time.Time) error {
	if store == nil {
		return nil
	}
	if err := store.UpsertChat(ctx, cache.Chat{
		JID:           chatJID,
		IsGroup:       to.Server == types.GroupServer,
		LastMessageTS: ts,
	}); err != nil {
		return fmt.Errorf("upsert chat: %w", err)
	}
	senderJID := ""
	if !ownJID.IsEmpty() {
		senderJID = ownJID.ToNonAD().String()
	}
	if err := store.InsertMessage(ctx, cache.Message{
		ID:        msgID,
		ChatJID:   chatJID,
		SenderJID: senderJID,
		Timestamp: ts,
		Kind:      cache.KindText,
		Body:      body,
		ReplyToID: replyTo,
		IsFromMe:  true,
	}); err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

// resolveRecipient parses either a full JID or a raw phone number. Raw
// phone numbers are normalised to {digits}@s.whatsapp.net; common UI
// decoration ("+", spaces) is stripped before validation.
func resolveRecipient(in string) (types.JID, error) {
	if strings.Contains(in, "@") {
		return types.ParseJID(in)
	}
	digits := stripNonDigits(in)
	if digits == "" {
		return types.JID{}, fmt.Errorf("recipient %q is neither a JID nor a phone number", in)
	}
	return types.NewJID(digits, types.DefaultUserServer), nil
}

func stripNonDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}
