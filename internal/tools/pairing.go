package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// pairing_start: opens a pair flow and returns the first observed event.
// When `phone` is supplied, also requests a phone-link code (mirroring
// /admin/pair/phone, which requires that the QR flow be open first).
var pairingStartSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "phone": {
      "type": "string",
      "description": "Optional E.164 phone number (digits only, no '+'). When set, returns a phone linking code in addition to the rotating QR payload."
    },
    "device_name": {
      "type": "string",
      "description": "Override the label shown on the user's phone after pairing. Defaults to the WHATSAPP_DEVICE_NAME env var or 'whatsapp-mcp'."
    }
  },
  "additionalProperties": false
}`)

// pairing_complete: polls for the next pair event, optionally blocking up
// to wait_seconds. wait_seconds=0 returns the latest cached event without
// blocking (acts as a status snapshot).
var pairingCompleteSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "wait_seconds": {
      "type": "integer",
      "minimum": 0,
      "maximum": 120,
      "default": 60,
      "description": "Maximum seconds to wait for a terminal pair event. 0 returns the latest cached event without blocking (status snapshot)."
    }
  },
  "additionalProperties": false
}`)

// PairStatus is a stable string the agent branches on. Mirrors
// wa.PairEventType for the terminal cases plus extra states that only
// make sense in the request/response shape.
const (
	pairStatusAwaitingScan              = "awaiting_scan"
	pairStatusAwaitingPhoneLink         = "awaiting_phone_link"
	pairStatusPending                   = "pending"
	pairStatusNotPairing                = "not_pairing"
	pairStatusSuccess                   = "success"
	pairStatusTimeout                   = "timeout"
	pairStatusError                     = "error"
	pairStatusClientOutdated            = "client_outdated"
	pairStatusScannedWithoutMultidevice = "scanned_without_multidevice"
)

// PairingStartResult is the structured output of pairing_start.
type PairingStartResult struct {
	Status      string `json:"status"`
	Code        string `json:"code,omitempty"`
	LinkingCode string `json:"linking_code,omitempty"`
	TimeoutMs   int64  `json:"timeout_ms,omitempty"`
}

// PairingCompleteResult is the structured output of pairing_complete.
type PairingCompleteResult struct {
	Status    string `json:"status"`
	Code      string `json:"code,omitempty"`
	TimeoutMs int64  `json:"timeout_ms,omitempty"`
	Error     string `json:"error,omitempty"`
	JID       string `json:"jid,omitempty"`
	Pushname  string `json:"pushname,omitempty"`
}

// firstEventTimeout bounds how long pairing_start waits for whatsmeow's
// first QR emission. The pair flow itself outlives this — only the read
// of the first event is bounded so a wedged connect cannot hang the
// request.
const firstEventTimeout = 5 * time.Second

func pairingStart(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			Phone      string `json:"phone,omitempty"`
			DeviceName string `json:"device_name,omitempty"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}

		// The pair flow must outlive the request that opened it
		// (subsequent pairing_complete calls drain it). Using
		// context.Background() here is deliberate; teardown is owned
		// by wa.Client (terminal event or Unpair).
		ch, err := deps.WA.StartPairing(context.Background(), in.DeviceName)
		if err != nil {
			switch {
			case errors.Is(err, wa.ErrAlreadyPaired):
				return mcp.AlreadyPairedError(err.Error()), nil
			case errors.Is(err, wa.ErrPairInProgress):
				return mcp.PairInProgressError(err.Error()), nil
			default:
				return mcp.InternalError(fmt.Sprintf("start pairing: %v", err)), nil
			}
		}
		// The wa goroutine fans events to both the observation cond
		// (which pairing_complete drains) AND the returned channel
		// (intended for the admin SSE consumer). The MCP path has no
		// SSE consumer, so without a drainer the channel fills at 8
		// rotations and the producer wedges on send — stalling the
		// pair flow entirely. Run a no-op drainer for the lifetime of
		// the channel; it exits when wa closes it on terminal/cancel.
		if ch != nil {
			go func() {
				for range ch {
				}
			}()
		}

		// Drain the first event with a bounded wait so a stuck
		// GetQRChannel doesn't hang the request indefinitely. If the
		// wait elapses before the first QR arrives, fall through with
		// evt.Type == "" — the response then advertises `pending` (QR
		// mode) so the caller polls `pairing_complete` for the code,
		// or `awaiting_phone_link` (phone mode) where the linking
		// code is the actual deliverable and the QR is incidental.
		waitCtx, cancel := context.WithTimeout(ctx, firstEventTimeout)
		defer cancel()
		evt, active, _ := deps.WA.PairWaitNext(waitCtx)
		if !active {
			// StartPairing succeeded above; if no session is observable
			// now it was torn down (e.g. concurrent Unpair). Surface
			// internal so the agent retries rather than treating this
			// as a normal not_pairing.
			return mcp.InternalError("pair flow vanished after start"), nil
		}

		result := PairingStartResult{
			Status:    pairStatusAwaitingScan,
			Code:      evt.Code,
			TimeoutMs: evt.TimeoutMs,
		}
		if evt.Type == "" {
			// First QR has not been emitted yet within the bounded
			// wait. The flow is still alive; the agent should poll
			// `pairing_complete` to retrieve the code.
			result.Status = pairStatusPending
		}

		if in.Phone != "" {
			code, err := deps.WA.PairPhone(ctx, in.Phone)
			if err != nil {
				switch {
				case errors.Is(err, wa.ErrNotPairing):
					return mcp.NotPairingError(err.Error()), nil
				case errors.Is(err, wa.ErrAlreadyPaired):
					return mcp.AlreadyPairedError(err.Error()), nil
				default:
					return mcp.InternalError(fmt.Sprintf("pair phone: %v", err)), nil
				}
			}
			// phone mode: linking_code is the deliverable; QR may
			// still be empty if it hasn't rotated yet, which is OK.
			result.Status = pairStatusAwaitingPhoneLink
			result.LinkingCode = code
		}
		return result, nil
	}
}

func pairingComplete(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			WaitSeconds *int `json:"wait_seconds,omitempty"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		wait := 60
		if in.WaitSeconds != nil {
			wait = *in.WaitSeconds
		}
		if wait < 0 || wait > 120 {
			return mcp.InvalidArgumentError("wait_seconds must be between 0 and 120"), nil
		}

		var (
			evt    wa.PairEvent
			active bool
		)
		if wait == 0 {
			evt, active = deps.WA.PairLatest()
		} else {
			waitCtx, cancel := context.WithTimeout(ctx, time.Duration(wait)*time.Second)
			defer cancel()
			evt, active, _ = deps.WA.PairWaitNext(waitCtx)
		}

		if !active {
			return PairingCompleteResult{Status: pairStatusNotPairing}, nil
		}

		// No event has been emitted yet on this fresh flow.
		if evt.Type == "" {
			return PairingCompleteResult{Status: pairStatusPending}, nil
		}

		if !evt.IsTerminal() {
			return PairingCompleteResult{
				Status:    pairStatusPending,
				Code:      evt.Code,
				TimeoutMs: evt.TimeoutMs,
			}, nil
		}

		out := PairingCompleteResult{Status: terminalToStatus(evt.Type), Error: evt.Error}
		if evt.Type == wa.PairEventSuccess {
			st := deps.WA.Status()
			out.JID = st.JID
			out.Pushname = st.Pushname
		}
		return out, nil
	}
}

// terminalToStatus maps a wa.PairEventType (the underscore-vs-hyphen
// difference is intentional: MCP statuses use snake_case throughout).
func terminalToStatus(t wa.PairEventType) string {
	switch t {
	case wa.PairEventSuccess:
		return pairStatusSuccess
	case wa.PairEventTimeout:
		return pairStatusTimeout
	case wa.PairEventError:
		return pairStatusError
	case wa.PairEventClientOutdated:
		return pairStatusClientOutdated
	case wa.PairEventScannedWithoutMultidevice:
		return pairStatusScannedWithoutMultidevice
	default:
		return pairStatusError
	}
}
