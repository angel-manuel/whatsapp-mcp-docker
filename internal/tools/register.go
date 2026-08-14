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
			Name: "send_file",
			Description: "Send stored bytes as a WhatsApp media message (image, video, audio, document or sticker). " +
				"Bytes are never passed through MCP: upload them first with 'POST /media' (same port and bearer token as /mcp), " +
				"or reuse the media_path a download_media call returned, and pass that reference as media_path. " +
				"The envelope is picked from the stored mimetype unless media_type says otherwise.",
			InputSchema: sendFileSchema,
			Handler:     sendFile(deps),
		},
		{
			Name: "send_audio_message",
			Description: "Send stored audio as a WhatsApp voice note (PTT). Takes the same '/media/<sha256>' reference as send_file. " +
				"Voice notes must be Ogg/Opus: other formats are transcoded when ffmpeg is available (FFMPEG_PATH; shipped in the -slim image) " +
				"and refused with invalid_argument when it is not, rather than sent as an unplayable note. Use send_file for a plain audio attachment.",
			InputSchema: sendAudioMessageSchema,
			Handler:     sendAudioMessage(deps),
		},
		{
			Name:        "send_poll",
			Description: "Create a WhatsApp poll in a user or group chat. Returns the poll's message_id, which is the handle vote_poll and get_poll_results take.",
			InputSchema: sendPollSchema,
			Handler:     sendPoll(deps),
		},
		{
			Name:        "vote_poll",
			Description: "Cast (or withdraw) this device's vote on a poll. Option texts must match the ballot exactly; pass an empty options array to withdraw. Only polls this device received can be voted on.",
			InputSchema: votePollSchema,
			Handler:     votePoll(deps),
		},
		{
			Name:        "get_poll_results",
			Description: "Read the tally of a poll: per-option vote counts and voters. Answered from the local cache, which accumulates vote events as they arrive — WhatsApp offers no way to query a poll's standings, so votes cast before this device was linked (or while it was down) are not counted.",
			InputSchema: getPollResultsSchema,
			Handler:     getPollResults(deps),
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
			Name:        "set_status_message",
			Description: "MUTATES ACCOUNT STATE: overwrite the account's 'About' text (the one-line bio on the profile), visible to everyone the account's privacy settings allow. This is not a status broadcast/story. Max 139 characters; an empty string clears it. There is no undo — the previous text is not recoverable from here.",
			InputSchema: setStatusMessageSchema,
			Handler:     setStatusMessage(deps),
		},
		{
			Name:        "send_presence",
			Description: "MUTATES ACCOUNT STATE: publish the account's global availability ('available' = online, 'unavailable' = offline). Contacts allowed to see last-seen observe this immediately. Marking available is also a prerequisite for receiving the updates subscribe_presence asks for.",
			InputSchema: sendPresenceSchema,
			Handler:     sendPresence(deps),
		},
		{
			Name:        "send_chat_presence",
			Description: "MUTATES ACCOUNT STATE: show or clear the typing indicator in one chat — 'composing' with media 'text' renders as \"typing…\", with media 'audio' as \"recording audio…\"; 'paused' clears it. The other party sees it live. WhatsApp expires the indicator after a few seconds, so sustained typing needs re-sending.",
			InputSchema: sendChatPresenceSchema,
			Handler:     sendChatPresence(deps),
		},
		{
			Name:        "subscribe_presence",
			Description: "MUTATES ACCOUNT STATE: ask the server to push online/offline updates for one user. The subscription is server-side and visible to WhatsApp as activity from this account. Updates arrive asynchronously and are cached against the contact — read them back with get_contact_details. Requires this account to be marked available (send_presence) for the server to keep delivering them.",
			InputSchema: subscribePresenceSchema,
			Handler:     subscribePresence(deps),
		},
		{
			Name:        "set_disappearing_timer",
			Description: "MUTATES ACCOUNT STATE: change the disappearing-message timer for one chat ('off', '24h', '7d', '90d'). The change applies to every participant, is announced in the chat itself, and causes future messages to be deleted on all devices once the timer elapses.",
			InputSchema: setDisappearingTimerSchema,
			Handler:     setDisappearingTimer(deps),
		},
		{
			Name:        "set_default_disappearing_timer",
			Description: "MUTATES ACCOUNT STATE: change the account-wide default disappearing-message timer ('off', '24h', '7d', '90d') applied to newly started chats. Existing chats keep their own timer.",
			InputSchema: setDefaultDisappearingTimerSchema,
			Handler:     setDefaultDisappearingTimer(deps),
		},
		{
			Name:        "mark_read",
			Description: "MUTATES ACCOUNT STATE: send a read receipt for one or more messages, turning the sender's ticks blue and updating their last-read position. All ids must be from the same author; sender_jid is required in group chats. Also clears the chat's cached unread flag. Cannot be undone.",
			InputSchema: markReadSchema,
			Handler:     markRead(deps),
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
