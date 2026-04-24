// Package tools wires read-side MCP tool handlers (contacts, groups, ...)
// to their backing stores. Handlers are pure functions that take decoded
// JSON arguments and return JSON-serialisable payloads, plumbed into the
// internal/mcp registry by Register.
//
// The package deliberately sits above internal/cache and internal/wa so
// that tool handlers stay side-effect-free from a transport point of view
// and can be tested with seeded stores + mocked whatsmeow clients.
package tools

import (
	"context"

	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
)

// WAClient is the subset of the whatsmeow surface this package needs. It
// is satisfied by *wa.Client in production and by test mocks. Keeping the
// interface narrow makes it trivial to stub group / user lookups without
// spinning up the full whatsmeow stack.
type WAClient interface {
	GroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error)
	UserInfo(ctx context.Context, jids []types.JID) (map[types.JID]types.UserInfo, error)
	IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error)
	ProfilePictureURL(ctx context.Context, jid types.JID) (string, error)
}

// Deps is the wiring carried into each tool handler. Fields are optional
// at the struct level but individual tools document which they require.
type Deps struct {
	Cache *cache.Store
	WA    WAClient
}
