package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

// disappearingTimers is the complete, ordered set of timers WhatsApp accepts
// and the single source of truth for the two tools below: the schema enums,
// the rejection message and the parser are all derived from it, so adding or
// removing a timer is a one-line change that cannot leave the surfaces
// disagreeing.
//
// whatsmeow's ParseDisappearingTimerString takes a much wider vocabulary of
// synonyms, and its SetDisappearingTimer will happily forward an arbitrary
// duration that official clients then ignore (and that the server rejects in
// groups). Exposing exactly the four real values keeps callers from
// "succeeding" at setting a timer nobody honours.
var disappearingTimers = []struct {
	name  string
	value time.Duration
}{
	{"off", whatsmeow.DisappearingTimerOff},
	{"24h", whatsmeow.DisappearingTimer24Hours},
	{"7d", whatsmeow.DisappearingTimer7Days},
	{"90d", whatsmeow.DisappearingTimer90Days},
}

// Derived from disappearingTimers at init. Package-level var initialisation
// is dependency-ordered, so the schemas below see the finished strings.
var (
	// disappearingEnumJSON is the JSONSchema array body, e.g. `"off", "24h"`.
	disappearingEnumJSON = disappearingNames(`"%s"`, ", ")
	// disappearingNameList is the human-readable list used in error text.
	disappearingNameList = disappearingNames("%s", ", ")

	disappearingDurationDesc = "Timer to apply: one of " + disappearingNameList +
		". These are the only values WhatsApp honours."
)

// disappearingNames renders every timer name through format, joined by sep.
func disappearingNames(format, sep string) string {
	parts := make([]string, 0, len(disappearingTimers))
	for _, t := range disappearingTimers {
		parts = append(parts, fmt.Sprintf(format, t.name))
	}
	return strings.Join(parts, sep)
}

var setDisappearingTimerSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "chat_jid": {
      "type": "string",
      "minLength": 1,
      "description": "Chat to change: ` + chatJIDForms + `."
    },
    "duration": {
      "type": "string",
      "enum": [` + disappearingEnumJSON + `],
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
      "enum": [` + disappearingEnumJSON + `],
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
		chat, kind, err := chatTarget(in.ChatJID)
		if err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		if kind != jidKindUser && kind != jidKindGroup {
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
	want := strings.ToLower(strings.TrimSpace(in))
	for _, t := range disappearingTimers {
		if t.name == want {
			return t.value, nil
		}
	}
	return 0, fmt.Errorf("duration %q is not one of: %s", in, disappearingNameList)
}

// canonicalDisappearingName is the inverse lookup used in result payloads so
// callers see the canonical spelling regardless of how they cased the input.
func canonicalDisappearingName(d time.Duration) string {
	for _, t := range disappearingTimers {
		if t.value == d {
			return t.name
		}
	}
	return d.String()
}
