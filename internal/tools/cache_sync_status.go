package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

var cacheSyncStatusSchema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

// CacheSyncStatus is the structured output of cache_sync_status. Counts come
// from SELECT COUNT(*) on each cache table; LastEventAt and LastEventAgoSec
// come from the Ingestor's atomic heartbeat. When no event has been ingested
// yet (fresh boot, no whatsmeow events recognized), both are nil.
type CacheSyncStatus struct {
	ChatCount       int `json:"chat_count"`
	MessageCount    int `json:"message_count"`
	ContactCount    int `json:"contact_count"`
	LastEventAt     any `json:"last_event_at"`          // ISO-8601 string | null
	LastEventAgoSec any `json:"last_event_ago_seconds"` // integer  | null
}

func cacheSyncStatus(deps Deps) mcp.Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		var out CacheSyncStatus
		db := deps.Cache.DB()
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chats`).Scan(&out.ChatCount); err != nil {
			return mcp.InternalError(fmt.Sprintf("count chats: %v", err)), nil
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&out.MessageCount); err != nil {
			return mcp.InternalError(fmt.Sprintf("count messages: %v", err)), nil
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM contacts`).Scan(&out.ContactCount); err != nil {
			return mcp.InternalError(fmt.Sprintf("count contacts: %v", err)), nil
		}
		if deps.Ingestor != nil {
			if t := deps.Ingestor.LastEventAt(); !t.IsZero() {
				out.LastEventAt = t.Format(time.RFC3339)
				out.LastEventAgoSec = int(time.Since(t).Seconds())
			}
		}
		return out, nil
	}
}
