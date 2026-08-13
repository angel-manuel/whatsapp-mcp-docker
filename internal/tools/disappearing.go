package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

// disappearingDurations is the complete set of timers WhatsApp accepts.
// whatsmeow's ParseDisappearingTimerString takes a much wider vocabulary of
// synonyms, and its SetDisappearingTimer will happily forward an arbitrary
// duration that official clients then ignore (and that the server rejects in
// groups). Exposing exactly the four real values keeps callers from
// "succeeding" at setting a timer nobody honours.
var disappearingDurations = map[string]time.Duration{
	"off": whatsmeow.DisappearingTimerOff,
	"24h": whatsmeow.DisappearingTimer24Hours,
	"7d":  whatsmeow.DisappearingTimer7Days,
	"90d": whatsmeow.DisappearingTimer90Days,
}

const disappearingDurationDesc = "Timer to apply: 'off', '24h', '7d' or '90d'. These are the only values WhatsApp honours."

var setDisappearingTimerSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "chat_jid": {
      "type": "string",
      "minLength": 1,
      "description": "Chat to change: a user JID ('…@s.whatsapp.net' or '…@lid'), a group JID ('…@g.us'), or a raw phone number with country code."
    },
    "duration": {
      "type": "string",
      "enum": ["off", "24h", "7d", "90d"],
      "description": "` + disappearingDurationDesc + `"
    }
  },
  "required": ["chat_jid", "duration"],
  "additionalProperties": false
}`)

var setDefaultDisappearingTimerSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "duration": {
      "type": "string",
      "enum": ["off", "24h", "7d", "90d"],
      "description": "` + disappearingDurationDesc + `"
    }
  },
  "required": ["duration"],
  "additionalProperties": false
}`)

// SetDisappearingTimerResult confirms the timer now set on a single chat.
// DurationSeconds is 0 for "off".
type SetDisappearingTimerResult struct {
	ChatJID         string `json:"chat_jid"`
	Duration        string `json:"duration"`
	DurationSeconds int64  `json:"duration_seconds"`
}

// SetDefaultDisappearingTimerResult confirms the account-wide default.
type SetDefaultDisappearingTimerResult struct {
	Duration        string `json:"duration"`
	DurationSeconds int64  `json:"duration_seconds"`
}

// setDisappearingTimer is the handler for set_disappearing_timer. The change
// is announced inside the chat and applies to every participant, not just
// this device.
func setDisappearingTimer(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			ChatJID  string `json:"chat_jid"`
			Duration string `json:"duration"`
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
				fmt.Sprintf("chat_jid %q is a @%s chat, which has no disappearing-message timer", in.ChatJID, chat.Server)), nil
		}

		timer, err := parseDisappearingDuration(in.Duration)
		if err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}

		if err := deps.WA.SetDisappearingTimer(ctx, chat, timer); err != nil {
			if errors.Is(err, whatsmeow.ErrInvalidDisappearingTimer) {
				return mcp.InvalidArgumentError(
					fmt.Sprintf("the server rejected duration %q for %s", in.Duration, chat)), nil
			}
			return accountOpError("set disappearing timer", err), nil
		}
		return SetDisappearingTimerResult{
			ChatJID:         chat.String(),
			Duration:        canonicalDisappearingName(timer),
			DurationSeconds: int64(timer.Seconds()),
		}, nil
	}
}

// setDefaultDisappearingTimer is the handler for
// set_default_disappearing_timer. It changes the account-wide default used by
// chats started from now on; existing chats keep their own timer.
func setDefaultDisappearingTimer(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			Duration string `json:"duration"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		timer, err := parseDisappearingDuration(in.Duration)
		if err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		if err := deps.WA.SetDefaultDisappearingTimer(ctx, timer); err != nil {
			return accountOpError("set default disappearing timer", err), nil
		}
		return SetDefaultDisappearingTimerResult{
			Duration:        canonicalDisappearingName(timer),
			DurationSeconds: int64(timer.Seconds()),
		}, nil
	}
}

func parseDisappearingDuration(in string) (time.Duration, error) {
	if d, ok := disappearingDurations[strings.ToLower(strings.TrimSpace(in))]; ok {
		return d, nil
	}
	return 0, fmt.Errorf("duration %q is not one of: off, 24h, 7d, 90d", in)
}

// canonicalDisappearingName is the inverse lookup used in result payloads so
// callers see the canonical spelling regardless of how they cased the input.
func canonicalDisappearingName(d time.Duration) string {
	for name, candidate := range disappearingDurations {
		if candidate == d {
			return name
		}
	}
	return d.String()
}
