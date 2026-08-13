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
	if deps.Sync == nil {
		return errors.New("tools: Deps.Sync is required")
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
			Name:        "resolve_jid",
			Description: "Resolve any value send_message accepts as its recipient — phone JID, @lid, group, newsletter, or bare phone number — to a readable identity: display name, canonical phone JID and E.164 phone number. Follows the lid<->phone alias map, so a @lid never reports its own digits as a phone number. Cache-first, so it is cheap enough to call while rendering a send approval. A JID nothing is known about returns empty fields rather than an error.",
			InputSchema: resolveJIDSchema,
			Handler:     resolveJID(deps),
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
			Name: "send_reaction",
			Description: "React to a WhatsApp message with an emoji. An empty emoji removes your reaction; a new one replaces it (WhatsApp allows one reaction per person per message). " +
				"The target message's author is resolved from the local cache; pass sender_jid when it is not cached. Newsletter/channel messages are not supported.",
			InputSchema: sendReactionSchema,
			Handler:     sendReaction(deps),
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
			Description: "Diagnostic snapshot of the local cache: chat / message / contact counts, the timestamp of the most recent ingested whatsmeow event, and the most recent (or in-progress) sync run. Exempt from the not_paired gate; safe to call before linking.",
			InputSchema: cacheSyncStatusSchema,
			Handler:     cacheSyncStatus(deps),
		},
		{
			Name:        "download_media",
			Description: "Download the attachment of a WhatsApp message (image, video, audio, document, sticker) and return a descriptor pointing at it. Returns JSON only — never bytes and never base64; fetch `media_path` over HTTP with the same bearer token used for /mcp.",
			InputSchema: downloadMediaSchema,
			Handler:     downloadMedia(deps),
		},
		{
			Name:        "cache_sync",
			Description: "Trigger reconciliation of the local cache against authoritative whatsmeow endpoints (joined groups, subscribed newsletters, app state). Returns immediately with a sync_id; per-stage progress is reported by cache_sync_status.",
			InputSchema: cacheSyncSchema,
			Handler:     cacheSyncHandler(deps),
		},
	}
	for _, t := range entries {
		if err := reg.Register(t); err != nil {
			return fmt.Errorf("tools: register %s: %w", t.Name, err)
		}
	}
	return nil
}
