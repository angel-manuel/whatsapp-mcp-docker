package tools

import (
	"errors"
	"fmt"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

// Register adds every tool implemented in this package to reg. It
// returns an error if any required dependency is missing or if a tool
// name collides with one already registered.
func Register(reg *mcp.Registry, deps Deps) error {
	if reg == nil {
		return errors.New("tools: registry must not be nil")
	}
	if deps.Cache == nil {
		return errors.New("tools: Deps.Cache is required")
	}
	if deps.WA == nil {
		return errors.New("tools: Deps.WA is required")
	}

	entries := []mcp.Tool{
		{
			Name:        "search_contacts",
			Description: "Search cached WhatsApp contacts by name, push name, business name, nickname, or phone. Paginated.",
			InputSchema: searchContactsSchema,
			Handler:     searchContacts(deps),
		},
		{
			Name:        "list_all_contacts",
			Description: "List all cached WhatsApp contacts in display-name order. Paginated.",
			InputSchema: listAllContactsSchema,
			Handler:     listAllContacts(deps),
		},
		{
			Name:        "get_contact_details",
			Description: "Fetch details for a WhatsApp contact. Merges cache + live whatsmeow USync (status, profile picture, is_on_whatsapp).",
			InputSchema: getContactDetailsSchema,
			Handler:     getContactDetails(deps),
		},
		{
			Name:        "get_group_info",
			Description: "Fetch authoritative group metadata via whatsmeow GetGroupInfo (name, topic, participants, flags).",
			InputSchema: getGroupInfoSchema,
			Handler:     getGroupInfo(deps),
		},
		{
			Name:        "send_message",
			Description: "Send a WhatsApp text message to a user or group chat. Supports optional quote-reply.",
			InputSchema: sendMessageSchema,
			Handler:     sendMessage(deps),
		},
		{
			Name:        "pairing_start",
			Description: "Start a WhatsApp pair flow and return the first rotating QR payload. Pass `phone` to also obtain a phone-number linking code. Exempt from the not_paired gate.",
			InputSchema: pairingStartSchema,
			Handler:     pairingStart(deps),
		},
		{
			Name:        "pairing_complete",
			Description: "Poll an in-progress pair flow. Blocks up to wait_seconds (default 60, max 120) for a terminal event; wait_seconds=0 returns the latest cached event without blocking. Exempt from the not_paired gate.",
			InputSchema: pairingCompleteSchema,
			Handler:     pairingComplete(deps),
		},
		{
			Name:        "cache_sync_status",
			Description: "Diagnostic snapshot of the local cache: chat / message / contact counts and the timestamp of the most recent ingested whatsmeow event. Exempt from the not_paired gate; safe to call before linking.",
			InputSchema: cacheSyncStatusSchema,
			Handler:     cacheSyncStatus(deps),
		},
	}
	for _, t := range entries {
		if err := reg.Register(t); err != nil {
			return fmt.Errorf("tools: register %s: %w", t.Name, err)
		}
	}
	return nil
}
