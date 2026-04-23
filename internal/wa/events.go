// Package wa owns the whatsmeow client, its on-disk session, and the typed
// lifecycle events emitted to the rest of the process.
package wa

import "time"

// State describes the high-level lifecycle state of the whatsmeow client.
type State string

// Lifecycle states.
const (
	StateNotPaired      State = "not_paired"
	StateConnecting     State = "connecting"
	StateConnected      State = "connected"
	StateDisconnected   State = "disconnected"
	StateLoggedOut      State = "logged_out"
	StateStreamReplaced State = "stream_replaced"
)

// EventType enumerates the typed lifecycle events this package fans out.
type EventType string

// Lifecycle event types.
const (
	EventConnected         EventType = "connected"
	EventDisconnected      EventType = "disconnected"
	EventLoggedOut         EventType = "logged_out"
	EventStreamReplaced    EventType = "stream_replaced"
	EventTemporaryBan      EventType = "temporary_ban"
	EventConnectionFailure EventType = "connection_failure"
)

// Event is a typed lifecycle event. Only the fields relevant to Type are set.
type Event struct {
	Type      EventType `json:"type"`
	Timestamp time.Time `json:"timestamp"`

	// BanExpire is the remaining duration of a temporary ban (EventTemporaryBan).
	BanExpire time.Duration `json:"ban_expire,omitempty"`
	// BanCode is the whatsmeow TempBanReason integer (EventTemporaryBan).
	BanCode int `json:"ban_code,omitempty"`

	// FailureReason is the whatsmeow ConnectFailureReason code.
	// Populated for EventConnectionFailure and for EventLoggedOut when the
	// logout arrived as a connect-failure frame.
	FailureReason int `json:"failure_reason,omitempty"`
	// FailureMessage is the human-readable failure message from the server
	// (EventConnectionFailure).
	FailureMessage string `json:"failure_message,omitempty"`
}

// PairEventType enumerates the updates emitted during an active pair flow.
type PairEventType string

// Pair event types. These mirror whatsmeow.QRChannelItem with stable,
// admin-friendly names (the "err-" prefix is dropped).
const (
	PairEventCode                      PairEventType = "code"
	PairEventSuccess                   PairEventType = "success"
	PairEventTimeout                   PairEventType = "timeout"
	PairEventError                     PairEventType = "error"
	PairEventClientOutdated            PairEventType = "client-outdated"
	PairEventScannedWithoutMultidevice PairEventType = "scanned-without-multidevice"
)

// PairEvent is a single update from an in-progress pair flow.
type PairEvent struct {
	Type PairEventType `json:"type"`
	// Code is the rotating pairing payload (Type=PairEventCode).
	Code string `json:"code,omitempty"`
	// TimeoutMs is how long the UI has before the next code rotates
	// (Type=PairEventCode).
	TimeoutMs int64 `json:"timeout_ms,omitempty"`
	// Error is the server-reported error message (Type=PairEventError).
	Error string `json:"error,omitempty"`
}

// IsTerminal reports whether this event is the last one the caller should
// expect on a pair stream.
func (e PairEvent) IsTerminal() bool {
	switch e.Type {
	case PairEventSuccess,
		PairEventTimeout,
		PairEventError,
		PairEventClientOutdated,
		PairEventScannedWithoutMultidevice:
		return true
	}
	return false
}

// Status is a point-in-time snapshot of the client's lifecycle state.
type Status struct {
	State     State  `json:"state"`
	Connected bool   `json:"connected"`
	LoggedIn  bool   `json:"logged_in"`
	JID       string `json:"jid,omitempty"`
	Pushname  string `json:"pushname,omitempty"`
	LastEvent *Event `json:"last_event,omitempty"`
}
