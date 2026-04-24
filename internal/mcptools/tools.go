// Package mcptools implements the read-side MCP tool surface backed by
// the local cache.db (chats, messages, contacts). It is the Go-native
// equivalent of the `whatsapp-mcp-extended/whatsapp-mcp-server` Python
// reference for the seven query tools listed in REQUIREMENTS.md.
//
// Every tool here only reads; the cache is populated by the ingestor in
// internal/cache and by whatsmeow-driven handlers elsewhere.
package mcptools

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

// MaxPageSize caps any `limit` parameter so a malicious or accidental
// caller can't exhaust memory pulling the entire cache at once.
const MaxPageSize = 200

// Register wires every read-side tool into reg against store. Callers
// must pass the same *cache.Store used by the ingestor; the tools issue
// plain reads and do not mutate state.
func Register(reg *mcp.Registry, store *cache.Store) error {
	if reg == nil {
		return errors.New("mcptools: registry required")
	}
	if store == nil {
		return errors.New("mcptools: cache store required")
	}
	registrations := []func(*mcp.Registry, *cache.Store) error{
		registerListChats,
		registerGetChat,
		registerListMessages,
		registerGetMessageContext,
		registerGetDirectChatByContact,
		registerGetContactChats,
		registerGetLastInteraction,
	}
	for _, fn := range registrations {
		if err := fn(reg, store); err != nil {
			return err
		}
	}
	return nil
}

// validatePagination enforces `limit` and `page` bounds and returns the
// effective offset. Zero-valued limits fall back to def so callers can
// treat `limit=0` as "use the tool default".
func validatePagination(limit, page, def int) (int, int, *mcpError) {
	if limit == 0 {
		limit = def
	}
	if limit < 1 {
		return 0, 0, newMCPError(mcp.ErrInvalidArgument, fmt.Sprintf("limit must be >= 1, got %d", limit))
	}
	if limit > MaxPageSize {
		return 0, 0, newMCPError(mcp.ErrInvalidArgument, fmt.Sprintf("limit must be <= %d, got %d", MaxPageSize, limit))
	}
	if page < 0 {
		return 0, 0, newMCPError(mcp.ErrInvalidArgument, fmt.Sprintf("page must be >= 0, got %d", page))
	}
	return limit, page * limit, nil
}

// mcpError carries a structured MCP error code + message out of a helper
// so each tool can convert it into a *mcp.ErrorResult without re-deriving
// the code/message plumbing at every call site.
type mcpError struct {
	Code    mcp.ErrorCode
	Message string
}

func newMCPError(code mcp.ErrorCode, msg string) *mcpError {
	return &mcpError{Code: code, Message: msg}
}

func (e *mcpError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// parseISOTime parses an ISO-8601 timestamp into a UTC time. Empty input
// returns the zero Time with nil error so callers can treat "unset" as
// "no lower/upper bound".
func parseISOTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	// Accept both "Z" RFC3339 and fractional variants; Go's RFC3339Nano
	// accepts both.
	t, err := time.Parse(time.RFC3339Nano, s)
	if err == nil {
		return t.UTC(), nil
	}
	// Fallback: date-only (YYYY-MM-DD) — Python's fromisoformat accepts
	// these and the reference doesn't.
	if t, err2 := time.Parse("2006-01-02", s); err2 == nil {
		return t.UTC(), nil
	}
	return time.Time{}, err
}

// stringOrNil returns nil when s is empty, otherwise *s. Used to emit
// JSON null rather than "" for fields the Python reference surfaces as
// None.
func stringOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// tsISOOrNil returns the time as an ISO-8601 UTC string, or JSON null if
// the underlying unix-seconds value was zero.
func tsISOOrNil(sec int64) any {
	if sec == 0 {
		return nil
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}
