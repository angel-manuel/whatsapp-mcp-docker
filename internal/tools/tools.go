// Package tools wires MCP tool handlers (contacts, groups, outbound
// messaging, ...) to their backing stores. Handlers are pure functions
// that take decoded JSON arguments and return JSON-serialisable
// payloads, plumbed into the internal/mcp registry by Register.
//
// The package deliberately sits above internal/cache and internal/wa so
// that tool handlers stay side-effect-free from a transport point of view
// and can be tested with seeded stores + mocked whatsmeow clients.
package tools

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// WAClient is the subset of the whatsmeow surface this package needs. It
// is satisfied by *wa.Client in production and by test mocks. Keeping the
// interface narrow makes it trivial to stub group / user lookups and
// message sends without spinning up the full whatsmeow stack.
type WAClient interface {
	GroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error)
	UserInfo(ctx context.Context, jids []types.JID) (map[types.JID]types.UserInfo, error)
	IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error)
	ProfilePictureURL(ctx context.Context, jid types.JID) (string, error)
	SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message) (whatsmeow.SendResponse, error)
	OwnJID() types.JID

	// Pairing surface — used by the pairing_start and pairing_complete
	// MCP tools. The lifecycle is owned by *wa.Client; this interface
	// just forwards.
	StartPairing(ctx context.Context, deviceName string) (<-chan wa.PairEvent, error)
	PairPhone(ctx context.Context, phone string) (string, error)
	PairLatest() (wa.PairEvent, bool)
	PairWaitNext(ctx context.Context) (wa.PairEvent, bool, error)
	Status() wa.Status
}

// Deps is the wiring carried into each tool handler. Fields are optional
// at the struct level but individual tools document which they require.
type Deps struct {
	Cache    *cache.Store
	WA       WAClient
	Ingestor *cache.Ingestor // optional; cache_sync_status reads its heartbeat
}
