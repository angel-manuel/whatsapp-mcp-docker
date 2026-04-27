package tools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

var cacheSyncSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "force": {
      "type": "boolean",
      "default": false,
      "description": "Reserved. Today force is a no-op while a sync is running (the in-progress sync_id is returned regardless)."
    }
  },
  "additionalProperties": false
}`)

// CacheSyncResult is the structured output of cache_sync. It is
// intentionally a subset of cache.SyncReport — full per-stage state lives
// in cache_sync_status, which polls the same orchestrator.
type CacheSyncResult struct {
	SyncID    string `json:"sync_id"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at"`
}

func cacheSyncHandler(deps Deps) mcp.Handler {
	return func(_ context.Context, _ json.RawMessage) (any, error) {
		if !deps.WA.IsLoggedIn() {
			return mcp.NotPairedError(), nil
		}
		// tools.WAClient is a superset of cache.SyncWAClient; pass it
		// directly. Go's structural typing makes this safe at compile
		// time and keeps internal/cache free of an internal/tools dep.
		report, _, err := deps.Sync.Start(deps.WA)
		if err != nil {
			return mcp.InternalError(err.Error()), nil
		}
		return CacheSyncResult{
			SyncID:    report.SyncID,
			Status:    string(report.Status),
			StartedAt: report.StartedAt.UTC().Format(time.RFC3339),
		}, nil
	}
}
