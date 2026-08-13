package wa

import (
	"context"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// This file holds the write-side whatsmeow wrappers that mutate state other
// people can observe: the account's "About" text, its online presence, the
// per-chat typing indicator, disappearing-message timers, and read receipts.
// They sit alongside the read-side wrappers in lookups.go and follow the same
// contract — resolve the whatsmeow pointer per call (a re-pair swaps it out)
// and surface ErrNotLoggedIn rather than panicking on a nil client.

// SetStatusMessage updates the account's "About" text — the string shown in
// the profile of everyone allowed to see it. This is not a status broadcast
// ("story"); it is the persistent one-line bio.
func (c *Client) SetStatusMessage(ctx context.Context, msg string) error {
	wm := c.snapshotWM()
	if wm == nil || !wm.IsLoggedIn() {
		return ErrNotLoggedIn
	}
	return wm.SetStatusMessage(ctx, msg)
}

// SendPresence publishes the account's global availability
// (types.PresenceAvailable / types.PresenceUnavailable). Contacts allowed to
// see last-seen observe the change immediately.
//
// whatsmeow refuses to send presence before it knows the account's push name
// (whatsmeow.ErrNoPushName), which is only populated once the client has been
// connected; the caller is expected to surface that verbatim.
func (c *Client) SendPresence(ctx context.Context, state types.Presence) error {
	wm := c.snapshotWM()
	if wm == nil || !wm.IsLoggedIn() {
		return ErrNotLoggedIn
	}
	return wm.SendPresence(ctx, state)
}

// SendChatPresence publishes the per-chat typing indicator. media is only
// meaningful for types.ChatPresenceComposing, where it switches the indicator
// between "typing…" (types.ChatPresenceMediaText) and "recording audio…"
// (types.ChatPresenceMediaAudio).
func (c *Client) SendChatPresence(ctx context.Context, jid types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error {
	wm := c.snapshotWM()
	if wm == nil || !wm.IsLoggedIn() {
		return ErrNotLoggedIn
	}
	return wm.SendChatPresence(ctx, jid, state, media)
}

// SubscribePresence asks the server to start delivering *events.Presence for
// jid. The resulting events flow through the dispatcher's EventHook into the
// cache ingestor, which records them against the contact row.
//
// The server only honours the subscription while this device is itself
// online, so callers normally SendPresence(available) first.
func (c *Client) SubscribePresence(ctx context.Context, jid types.JID) error {
	wm := c.snapshotWM()
	if wm == nil || !wm.IsLoggedIn() {
		return ErrNotLoggedIn
	}
	return wm.SubscribePresence(ctx, jid)
}

// SetDisappearingTimer sets the disappearing-message timer for a single chat.
// Both participants see the change, and it is announced in the chat itself.
// timer must be one of whatsmeow's DisappearingTimer* constants; the server
// rejects anything else in groups and official clients ignore it in DMs.
//
// The setting timestamp is left to whatsmeow, which stamps "now" for the DM
// path and does not use it for groups.
func (c *Client) SetDisappearingTimer(ctx context.Context, chat types.JID, timer time.Duration) error {
	wm := c.snapshotWM()
	if wm == nil || !wm.IsLoggedIn() {
		return ErrNotLoggedIn
	}
	return wm.SetDisappearingTimer(ctx, chat, timer, time.Time{})
}

// SetDefaultDisappearingTimer sets the account-wide default applied to newly
// started chats. Existing chats keep whatever timer they already have.
func (c *Client) SetDefaultDisappearingTimer(ctx context.Context, timer time.Duration) error {
	wm := c.snapshotWM()
	if wm == nil || !wm.IsLoggedIn() {
		return ErrNotLoggedIn
	}
	return wm.SetDefaultDisappearingTimer(ctx, timer)
}

// MarkRead sends a read receipt (the sender's blue ticks) for ids. chat is
// always the chat JID; sender must be the author's JID in group chats and is
// ignored in DMs. All ids must belong to the same author — whatsmeow encodes
// a single participant per receipt.
func (c *Client) MarkRead(ctx context.Context, ids []types.MessageID, timestamp time.Time, chat, sender types.JID) error {
	wm := c.snapshotWM()
	if wm == nil || !wm.IsLoggedIn() {
		return ErrNotLoggedIn
	}
	return wm.MarkRead(ctx, ids, timestamp, chat, sender)
}
