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
	Type      EventType
	Timestamp time.Time

	// BanExpire is the remaining duration of a temporary ban (EventTemporaryBan).
	BanExpire time.Duration
	// BanCode is the whatsmeow TempBanReason integer (EventTemporaryBan).
	BanCode int

	// FailureReason is the whatsmeow ConnectFailureReason code.
	// Populated for EventConnectionFailure and for EventLoggedOut when the
	// logout arrived as a connect-failure frame.
	FailureReason int
	// FailureMessage is the human-readable failure message from the server
	// (EventConnectionFailure).
	FailureMessage string
}
