package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// statusMessageMaxRunes bounds the "About" text. WhatsApp's own clients cap
// the field at 139 characters; the server silently truncates beyond that, so
// we reject rather than let the caller believe it set something it did not.
const statusMessageMaxRunes = 139

var setStatusMessageSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "text": {
      "type": "string",
      "maxLength": 139,
      "description": "New About text, 0-139 characters. An empty string clears the About line."
    }
  },
  "required": ["text"],
  "additionalProperties": false
}`)

var sendPresenceSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "presence": {
      "type": "string",
      "enum": ["available", "unavailable"],
      "description": "'available' marks the account online; 'unavailable' marks it offline."
    }
  },
  "required": ["presence"],
  "additionalProperties": false
}`)

var sendChatPresenceSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "chat_jid": {
      "type": "string",
      "minLength": 1,
      "description": "Chat to publish the indicator in: a user JID ('…@s.whatsapp.net' or '…@lid'), a group JID ('…@g.us'), or a raw phone number with country code."
    },
    "state": {
      "type": "string",
      "enum": ["composing", "paused"],
      "description": "'composing' shows the typing/recording indicator; 'paused' clears it."
    },
    "media": {
      "type": "string",
      "enum": ["text", "audio"],
      "default": "text",
      "description": "Which composing indicator to show: 'text' for \"typing…\", 'audio' for \"recording audio…\". Only valid with state='composing'."
    }
  },
  "required": ["chat_jid", "state"],
  "additionalProperties": false
}`)

var subscribePresenceSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "jid": {
      "type": "string",
      "minLength": 1,
      "description": "User JID to watch ('…@s.whatsapp.net' or '…@lid'), or a raw phone number with country code. Groups do not publish presence."
    }
  },
  "required": ["jid"],
  "additionalProperties": false
}`)

// SetStatusMessageResult echoes the About text now stored on the account.
type SetStatusMessageResult struct {
	Text string `json:"text"`
}

// SendPresenceResult echoes the global availability now published.
type SendPresenceResult struct {
	Presence string `json:"presence"`
}

// SendChatPresenceResult echoes the per-chat indicator now published.
type SendChatPresenceResult struct {
	ChatJID string `json:"chat_jid"`
	State   string `json:"state"`
	Media   string `json:"media"`
}

// SubscribePresenceResult confirms the subscription request. Presence itself
// arrives asynchronously as *events.Presence and is persisted by the cache
// ingestor; read it back with get_contact_details.
type SubscribePresenceResult struct {
	JID string `json:"jid"`
}

// setStatusMessage is the handler for set_status_message. The About string
// is account-wide and visible to everyone the account's privacy settings
// allow, so the length limit is enforced here rather than left to the
// server's silent truncation.
func setStatusMessage(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			Text string `json:"text"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		if n := len([]rune(in.Text)); n > statusMessageMaxRunes {
			return mcp.InvalidArgumentError(
				fmt.Sprintf("text is %d characters; WhatsApp allows at most %d", n, statusMessageMaxRunes)), nil
		}
		if err := deps.WA.SetStatusMessage(ctx, in.Text); err != nil {
			return accountOpError("set status message", err), nil
		}
		return SetStatusMessageResult{Text: in.Text}, nil
	}
}

// sendPresence is the handler for send_presence. Marking the account
// available also re-enables delivery of presence updates the account is
// subscribed to — the server stops pushing them while this device is offline.
func sendPresence(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			Presence string `json:"presence"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		state, err := parsePresence(in.Presence)
		if err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		if err := deps.WA.SendPresence(ctx, state); err != nil {
			return accountOpError("send presence", err), nil
		}
		return SendPresenceResult{Presence: string(state)}, nil
	}
}

// sendChatPresence is the handler for send_chat_presence. WhatsApp expires
// the composing indicator on its own after a few seconds, so an agent that
// wants to look like it is typing has to re-send it; 'paused' clears it
// immediately.
func sendChatPresence(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			ChatJID string `json:"chat_jid"`
			State   string `json:"state"`
			Media   string `json:"media"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		if strings.TrimSpace(in.ChatJID) == "" {
			return mcp.InvalidArgumentError("chat_jid must not be empty"), nil
		}
		chat, err := resolveRecipient(strings.TrimSpace(in.ChatJID))
		if err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		switch chat.Server {
		case types.DefaultUserServer, types.HiddenUserServer, types.GroupServer:
		default:
			return mcp.InvalidArgumentError(
				fmt.Sprintf("chat_jid %q is a @%s chat, which has no typing indicator", in.ChatJID, chat.Server)), nil
		}

		state, err := parseChatPresence(in.State)
		if err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		media, err := parseChatPresenceMedia(in.Media)
		if err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		// whatsmeow drops the media attribute for anything but composing.
		// Rejecting is more honest than silently ignoring half the request.
		if state != types.ChatPresenceComposing && media != types.ChatPresenceMediaText {
			return mcp.InvalidArgumentError(
				fmt.Sprintf("media=%q is only valid with state='composing'", in.Media)), nil
		}

		if err := deps.WA.SendChatPresence(ctx, chat, state, media); err != nil {
			return accountOpError("send chat presence", err), nil
		}
		return SendChatPresenceResult{
			ChatJID: chat.String(),
			State:   string(state),
			Media:   presenceMediaName(media),
		}, nil
	}
}

// subscribePresence is the handler for subscribe_presence. The subscription
// is server-side and outlives the call; the events it produces are ingested
// into the contact's cache row.
func subscribePresence(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			JID string `json:"jid"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		if strings.TrimSpace(in.JID) == "" {
			return mcp.InvalidArgumentError("jid must not be empty"), nil
		}
		target, err := resolveRecipient(strings.TrimSpace(in.JID))
		if err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		switch target.Server {
		case types.DefaultUserServer, types.HiddenUserServer:
		default:
			return mcp.InvalidArgumentError(
				fmt.Sprintf("jid %q is not a user JID; only individual users publish presence", in.JID)), nil
		}

		if err := deps.WA.SubscribePresence(ctx, target.ToNonAD()); err != nil {
			// A missing privacy token means the account has never been in
			// contact with this user — whatsmeow only hard-fails on it when
			// ErrorOnSubscribePresenceWithoutToken is set, but map it to a
			// caller-facing error either way.
			if errors.Is(err, whatsmeow.ErrNoPrivacyToken) {
				return mcp.InvalidArgumentError(
					fmt.Sprintf("no privacy token for %s: the account must have exchanged messages with them before subscribing", target)), nil
			}
			return accountOpError("subscribe presence", err), nil
		}
		return SubscribePresenceResult{JID: target.ToNonAD().String()}, nil
	}
}

// parsePresence maps the tool's presence enum onto whatsmeow's type. The
// JSONSchema advertises the same enum, but mcp-go does not validate against
// it, so this is the real gate.
func parsePresence(in string) (types.Presence, error) {
	switch strings.ToLower(strings.TrimSpace(in)) {
	case string(types.PresenceAvailable):
		return types.PresenceAvailable, nil
	case string(types.PresenceUnavailable):
		return types.PresenceUnavailable, nil
	default:
		return "", fmt.Errorf("presence %q is not one of: available, unavailable", in)
	}
}

func parseChatPresence(in string) (types.ChatPresence, error) {
	switch strings.ToLower(strings.TrimSpace(in)) {
	case string(types.ChatPresenceComposing):
		return types.ChatPresenceComposing, nil
	case string(types.ChatPresencePaused):
		return types.ChatPresencePaused, nil
	default:
		return "", fmt.Errorf("state %q is not one of: composing, paused", in)
	}
}

// parseChatPresenceMedia maps the media enum. "text" is whatsmeow's empty
// string, so an omitted field lands on the same value as an explicit "text".
func parseChatPresenceMedia(in string) (types.ChatPresenceMedia, error) {
	switch strings.ToLower(strings.TrimSpace(in)) {
	case "", "text":
		return types.ChatPresenceMediaText, nil
	case string(types.ChatPresenceMediaAudio):
		return types.ChatPresenceMediaAudio, nil
	default:
		return "", fmt.Errorf("media %q is not one of: text, audio", in)
	}
}

// presenceMediaName is the inverse of parseChatPresenceMedia for the result
// payload — whatsmeow spells "text" as the empty string.
func presenceMediaName(m types.ChatPresenceMedia) string {
	if m == types.ChatPresenceMediaText {
		return "text"
	}
	return string(m)
}

// accountOpError maps a failure from one of the account-mutating whatsmeow
// calls onto the structured tool-error taxonomy. op is a short verb phrase
// used verbatim in the message ("send presence", "mark read").
func accountOpError(op string, err error) any {
	switch {
	case errors.Is(err, wa.ErrNotLoggedIn),
		errors.Is(err, whatsmeow.ErrNotLoggedIn),
		errors.Is(err, whatsmeow.ErrClientIsNil):
		return mcp.NotPairedError()
	case errors.Is(err, whatsmeow.ErrNoPushName):
		// The device is paired but has not finished its first connect, so
		// whatsmeow has no push name to attach to the presence stanza.
		return mcp.InternalError(op + ": the client has not received its push name yet; retry once the connection is established")
	default:
		return mcp.InternalError(fmt.Sprintf("%s: %v", op, err))
	}
}
