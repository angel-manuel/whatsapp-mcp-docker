package cache

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// PollDecrypter unwraps the encrypted vote carried by a poll update event.
// *whatsmeow.Client satisfies it via DecryptPollVote, and internal/wa forwards
// to it; the Ingestor keeps it as an interface so poll ingestion can be tested
// without a live WhatsApp session.
//
// Decryption needs the poll creation message's secret, which whatsmeow only
// holds for polls this device actually saw. A vote on an older poll therefore
// fails here permanently — there is nothing to retry.
type PollDecrypter interface {
	DecryptPollVote(ctx context.Context, vote *events.Message) (*waE2E.PollVoteMessage, error)
}

// Ingestor subscribes to whatsmeow events (and session lifecycle events
// re-dispatched by the server) and persists their payloads into the cache
// store.
//
// The single public entry point is HandleEvent(any) so it can be attached to
// `whatsmeow.Client.AddEventHandler` as well as forwarded to from an internal
// lifecycle dispatcher without wrapping.
type Ingestor struct {
	store       *Store
	logger      *slog.Logger
	lastEventTS atomic.Int64 // unix seconds of the most recent recognized event

	// pollDecrypter is installed after construction because the whatsmeow
	// client that satisfies it is opened *after* the ingestor (the client
	// needs the ingestor's HandleEvent as its event hook). Nil means poll
	// votes are counted as unreadable rather than ingested.
	pollDecrypter atomic.Pointer[PollDecrypter]

	// Per-app-state-event counters used by the cache_sync orchestrator's
	// app_state stage to estimate items processed per FetchAppState patch.
	// Bumped inside the matching handlers; concurrent live events also bump
	// them, so attribution is approximate but good enough for v1 progress.
	markRead atomic.Int64
	pin      atomic.Int64
	archive  atomic.Int64
	contact  atomic.Int64
}

// LastEventAt returns the timestamp of the last successfully ingested event,
// or zero time when none has been seen. Used by the cache_sync_status tool
// to expose a freshness heartbeat.
func (i *Ingestor) LastEventAt() time.Time {
	sec := i.lastEventTS.Load()
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// AppStateCounts returns running totals of the four event types
// FetchAppState typically dispatches: MarkChatAsRead, Pin, Archive,
// Contact. The sync orchestrator snapshots these around each
// FetchAppState call to estimate per-stage progress.
func (i *Ingestor) AppStateCounts() (markRead, pin, archive, contact int64) {
	return i.markRead.Load(), i.pin.Load(), i.archive.Load(), i.contact.Load()
}

// NewIngestor constructs an Ingestor backed by store. A nil logger is replaced
// with a discarding one so callers that don't care about diagnostics can pass
// nil.
func NewIngestor(store *Store, logger *slog.Logger) *Ingestor {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Ingestor{store: store, logger: logger}
}

// SetPollDecrypter installs the surface used to decrypt incoming poll votes.
// It must be called before the whatsmeow client connects: a vote that arrives
// while no decrypter is installed is dropped for good, and the reconnect that
// flushes WhatsApp's offline queue is exactly when votes arrive in bulk.
//
// A nil argument clears the decrypter rather than installing a nil interface
// that would panic on the next vote. Nothing in production clears it — the wa
// client survives pair/unpair (only its inner whatsmeow client is swapped, and
// internal/wa resolves that per call) — but the setter must not turn a
// caller's nil into a crash.
func (i *Ingestor) SetPollDecrypter(d PollDecrypter) {
	if d == nil {
		i.pollDecrypter.Store(nil)
		return
	}
	i.pollDecrypter.Store(&d)
}

// HandleEvent dispatches on the concrete whatsmeow event type. Unknown events
// are ignored silently — any event we can't persist today simply isn't
// mirrored. This method is safe to register directly with
// `whatsmeow.Client.AddEventHandler`.
func (i *Ingestor) HandleEvent(evt any) {
	ctx := context.Background()
	recognized := true
	switch v := evt.(type) {
	case *events.Message:
		i.handleMessage(ctx, v)
	case *events.HistorySync:
		i.handleHistorySync(ctx, v)
	case *events.Contact:
		i.handleContact(ctx, v)
	case *events.PushName:
		i.handlePushName(ctx, v)
	case *events.BusinessName:
		i.handleBusinessName(ctx, v)
	case *events.GroupInfo:
		i.handleGroupInfo(ctx, v)
	case *events.JoinedGroup:
		i.handleJoinedGroup(ctx, v)
	case *events.NewsletterJoin:
		i.handleNewsletterJoin(ctx, v)
	case *events.NewsletterLeave:
		i.handleNewsletterLeave(ctx, v)
	case *events.MarkChatAsRead:
		i.handleMarkChatAsRead(ctx, v)
	case *events.Pin:
		i.handlePin(ctx, v)
	case *events.Archive:
		i.handleArchive(ctx, v)
	case *events.Star:
		i.handleStar(ctx, v)
	case *events.Presence:
		i.handlePresence(ctx, v)
	default:
		recognized = false
	}
	if recognized {
		i.lastEventTS.Store(time.Now().Unix())
	}
}

func (i *Ingestor) handleMessage(ctx context.Context, evt *events.Message) {
	if evt == nil || evt.Info.ID == "" || evt.Message == nil {
		return
	}

	chatJID := evt.Info.Chat.String()
	senderJID := evt.Info.Sender.ToNonAD().String()
	ts := evt.Info.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	// Protocol-level edits and revokes come in first: they target an
	// existing message id rather than inserting a new one.
	if proto := evt.Message.GetProtocolMessage(); proto != nil {
		if i.handleProtocolMessage(ctx, chatJID, ts, proto) {
			return
		}
	}

	// Newer edits are unwrapped by UnwrapRaw into evt.Message with IsEdit=true.
	// evt.Info.ID already points at the original message id in that case.
	if evt.IsEdit {
		body := extractTextBody(evt.Message)
		if err := i.store.MarkMessageEdited(ctx, chatJID, evt.Info.ID, body, ts); err != nil {
			i.logger.Warn("cache: edit target not found; inserting new row",
				slog.String("chat_jid", chatJID), slog.String("message_id", evt.Info.ID))
			// Fall through: store as a fresh row so context isn't lost.
		} else {
			return
		}
	}

	// Reactions target an existing message rather than inserting a new row,
	// and must not touch the chat: bumping last_message_ts would reorder the
	// chat list and make list_chats report the reaction as the last message.
	if react := evt.Message.GetReactionMessage(); react != nil {
		i.handleReaction(ctx, evt, react, ts)
		return
	}

	// A poll update is a vote, not a message: it has no body and never appears
	// in a chat transcript, so like a reaction it must not bump the chat's
	// last_message_ts. Unlike a reaction it does record who sent it — the
	// tally names its voters, and a group member who votes without ever having
	// sent a message would otherwise be unnameable.
	if vote := evt.Message.GetPollUpdateMessage(); vote != nil {
		i.recordSenderIdentity(ctx, evt)
		i.handlePollVote(ctx, evt, vote)
		return
	}

	if err := i.store.UpsertChat(ctx, Chat{
		JID:           chatJID,
		IsGroup:       evt.Info.IsGroup,
		LastMessageTS: ts,
	}); err != nil {
		i.logger.Warn("cache: upsert chat", slog.String("chat_jid", chatJID), slog.String("err", err.Error()))
	}

	i.recordSenderIdentity(ctx, evt)

	msg := buildMessageRow(chatJID, senderJID, evt.Info.ID, evt.Info.PushName, ts, evt.Info.IsFromMe, evt.Message)
	if msg == nil {
		return
	}
	if err := i.store.InsertMessage(ctx, *msg); err != nil {
		i.logger.Warn("cache: insert message",
			slog.String("chat_jid", chatJID), slog.String("message_id", evt.Info.ID), slog.String("err", err.Error()))
	}

	i.persistPollCreation(ctx, chatJID, senderJID, evt.Info.ID, ts, evt.Info.IsFromMe, evt.Message)
}

// handleReaction persists an emoji reaction carried in a ReactionMessage
// envelope. An empty Text is WhatsApp's "remove my reaction" signal, so it
// deletes the row instead of storing an empty emoji.
//
// The reactor's contact and lid<->phone alias are recorded exactly as they are
// on the message path, since a reaction is often the first (or only) thing a
// given identity sends us. The chat row is deliberately left alone — see the
// call site in handleMessage.
func (i *Ingestor) handleReaction(ctx context.Context, evt *events.Message, react *waE2E.ReactionMessage, fallbackTS time.Time) {
	key := react.GetKey()
	if key == nil || key.GetID() == "" {
		return
	}
	chatJID := evt.Info.Chat.String()
	if remote := key.GetRemoteJID(); remote != "" {
		chatJID = remote
	}
	senderJID := evt.Info.Sender.ToNonAD().String()

	if senderJID != "" && !evt.Info.IsFromMe {
		if err := i.store.UpsertContact(ctx, Contact{JID: senderJID, PushName: evt.Info.PushName}); err != nil {
			i.logger.Warn("cache: upsert reaction contact", slog.String("jid", senderJID), slog.String("err", err.Error()))
		}
	}
	i.recordJIDAlias(ctx, evt.Info.Sender, evt.Info.SenderAlt)

	// The reaction carries its own timestamp, which is what orders competing
	// reactions from the same sender; the envelope time is only a fallback.
	ts := fallbackTS
	if ms := react.GetSenderTimestampMS(); ms > 0 {
		ts = time.UnixMilli(ms)
	}

	i.persistReaction(ctx, Reaction{
		ChatJID:   chatJID,
		TargetID:  key.GetID(),
		SenderJID: senderJID,
		Emoji:     react.GetText(),
		Timestamp: ts,
		IsFromMe:  evt.Info.IsFromMe,
	})
}

// persistReaction writes one reaction row, routing the empty-emoji removal to
// a delete. Shared by the live and history-sync ingest paths; failures are
// logged and swallowed like every other ingest write.
func (i *Ingestor) persistReaction(ctx context.Context, r Reaction) {
	if r.ChatJID == "" || r.TargetID == "" {
		return
	}
	if r.Emoji == "" {
		if err := i.store.DeleteReaction(ctx, r.ChatJID, r.TargetID, ReactionSenderKey(r.SenderJID, r.IsFromMe)); err != nil {
			i.logger.Warn("cache: delete reaction",
				slog.String("chat_jid", r.ChatJID), slog.String("target_id", r.TargetID), slog.String("err", err.Error()))
		}
		return
	}
	if err := i.store.UpsertReaction(ctx, r); err != nil {
		i.logger.Warn("cache: upsert reaction",
			slog.String("chat_jid", r.ChatJID), slog.String("target_id", r.TargetID), slog.String("err", err.Error()))
	}
}

// recordSenderIdentity persists what an inbound stanza reveals about who sent
// it: the sender's contact row (push name, verified business name) and the
// lid<->phone pairing whatsmeow attaches as the alternate address.
//
// It is deliberately separate from the chat upsert so that stanzas which must
// not reorder the chat list — a poll vote, say — can still contribute the
// identity facts the read side needs to name people.
func (i *Ingestor) recordSenderIdentity(ctx context.Context, evt *events.Message) {
	senderJID := evt.Info.Sender.ToNonAD().String()
	if senderJID != "" && !evt.Info.IsFromMe {
		contact := Contact{JID: senderJID, PushName: evt.Info.PushName}
		if evt.Info.VerifiedName != nil && evt.Info.VerifiedName.Details != nil {
			contact.BusinessName = evt.Info.VerifiedName.Details.GetVerifiedName()
		}
		if err := i.store.UpsertContact(ctx, contact); err != nil {
			i.logger.Warn("cache: upsert contact", slog.String("jid", senderJID), slog.String("err", err.Error()))
		}
	}
	// whatsmeow attaches the sender's alternate address (the LID when Sender
	// is the phone JID, and vice-versa). Record the pairing so the read side
	// can merge a contact's split phone/LID threads (see get_conversation).
	i.recordJIDAlias(ctx, evt.Info.Sender, evt.Info.SenderAlt)
}

// recordJIDAlias persists the link between a contact's phone-number JID and
// privacy LID when a message carries both addresses. a and b are the sender's
// primary and alternate identities (in either order); the pair is stored only
// when exactly one is a phone JID and the other a LID. Anything else (same
// server, group, empty, broadcast) is ignored.
func (i *Ingestor) recordJIDAlias(ctx context.Context, a, b types.JID) {
	if a.IsEmpty() || b.IsEmpty() {
		return
	}
	var lid, pn types.JID
	switch {
	case a.Server == types.HiddenUserServer && b.Server == types.DefaultUserServer:
		lid, pn = a, b
	case a.Server == types.DefaultUserServer && b.Server == types.HiddenUserServer:
		lid, pn = b, a
	default:
		return
	}
	if err := i.store.UpsertJIDAlias(ctx, lid.ToNonAD().String(), pn.ToNonAD().String()); err != nil {
		i.logger.Warn("cache: record jid alias",
			slog.String("lid", lid.String()), slog.String("pn", pn.String()), slog.String("err", err.Error()))
	}
}

// handleProtocolMessage persists edits/revokes carried in a legacy-style
// ProtocolMessage envelope. Returns true if the envelope was fully handled
// and the caller should stop.
func (i *Ingestor) handleProtocolMessage(ctx context.Context, chatJID string, ts time.Time, proto *waE2E.ProtocolMessage) bool {
	key := proto.GetKey()
	if key == nil || key.GetID() == "" {
		return false
	}
	targetID := key.GetID()
	targetChat := chatJID
	if remote := key.GetRemoteJID(); remote != "" {
		targetChat = remote
	}
	switch proto.GetType() {
	case waE2E.ProtocolMessage_REVOKE:
		if err := i.store.MarkMessageDeleted(ctx, targetChat, targetID, ts); err != nil {
			i.logger.Warn("cache: mark deleted",
				slog.String("chat_jid", targetChat), slog.String("message_id", targetID), slog.String("err", err.Error()))
		}
		return true
	case waE2E.ProtocolMessage_MESSAGE_EDIT:
		body := extractTextBody(proto.GetEditedMessage())
		if err := i.store.MarkMessageEdited(ctx, targetChat, targetID, body, ts); err != nil {
			i.logger.Warn("cache: mark edited",
				slog.String("chat_jid", targetChat), slog.String("message_id", targetID), slog.String("err", err.Error()))
		}
		return true
	}
	return false
}

func (i *Ingestor) handleHistorySync(ctx context.Context, evt *events.HistorySync) {
	if evt == nil || evt.Data == nil {
		return
	}
	for _, conv := range evt.Data.GetConversations() {
		if conv == nil || conv.GetID() == "" {
			continue
		}
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil {
			i.logger.Warn("cache: history sync invalid chat jid", slog.String("jid", conv.GetID()), slog.String("err", err.Error()))
			continue
		}
		chatJIDStr := chatJID.String()

		var latestTS time.Time
		for _, wm := range conv.GetMessages() {
			if wm == nil || wm.GetMessage() == nil {
				continue
			}
			web := wm.GetMessage()
			rowTS := time.Unix(int64(web.GetMessageTimestamp()), 0) //nolint:gosec // whatsapp server timestamp, bounded
			if rowTS.After(latestTS) {
				latestTS = rowTS
			}
			key := web.GetKey()
			if key == nil || key.GetID() == "" || web.GetMessage() == nil {
				continue
			}
			var senderJID string
			if key.GetFromMe() {
				senderJID = chatJIDStr
			} else if part := key.GetParticipant(); part != "" {
				if pj, err := types.ParseJID(part); err == nil {
					senderJID = pj.ToNonAD().String()
				} else {
					senderJID = part
				}
			} else {
				senderJID = chatJIDStr
			}
			// A nil row means an envelope kind we don't surface. Its
			// reactions are still worth keeping, so they are ingested
			// below regardless.
			if row := buildMessageRow(chatJIDStr, senderJID, key.GetID(), web.GetPushName(), rowTS, key.GetFromMe(), web.GetMessage()); row != nil {
				if err := i.store.InsertMessage(ctx, *row); err != nil {
					i.logger.Warn("cache: history insert message",
						slog.String("chat_jid", chatJIDStr), slog.String("message_id", key.GetID()), slog.String("err", err.Error()))
				}
			}
			// Both of these are independent of the row above: a poll's ballot
			// and a message's reactions are worth keeping even for an
			// envelope kind we don't surface as a message.
			i.persistPollCreation(ctx, chatJIDStr, senderJID, key.GetID(), rowTS, key.GetFromMe(), web.GetMessage())
			i.ingestHistoryReactions(ctx, chatJIDStr, key.GetID(), rowTS, web.GetReactions())
		}

		chat := Chat{JID: chatJIDStr, IsGroup: chatJID.Server == types.GroupServer, LastMessageTS: latestTS}
		if name := conv.GetName(); name != "" {
			chat.Name = name
		} else if dn := conv.GetDisplayName(); dn != "" {
			chat.Name = dn
		}
		if ua := conv.GetUnreadCount(); ua > 0 {
			chat.UnreadCount = int(ua)
		}
		if err := i.store.UpsertChat(ctx, chat); err != nil {
			i.logger.Warn("cache: history upsert chat", slog.String("chat_jid", chatJIDStr), slog.String("err", err.Error()))
		}
	}
}

// ingestHistoryReactions persists the reactions history sync attaches to a
// message. Unlike a live ReactionMessage — where the envelope's own sender is
// the reactor — here each reaction carries its own key, and the *enclosing*
// message id is the target.
func (i *Ingestor) ingestHistoryReactions(ctx context.Context, chatJID, targetID string, fallbackTS time.Time, reactions []*waWeb.Reaction) {
	for _, react := range reactions {
		if react == nil {
			continue
		}
		key := react.GetKey()
		if key == nil {
			continue
		}
		senderJID := ""
		if part := key.GetParticipant(); part != "" {
			if pj, err := types.ParseJID(part); err == nil {
				senderJID = pj.ToNonAD().String()
			} else {
				senderJID = part
			}
		} else if !key.GetFromMe() {
			// A 1:1 chat carries no participant; the other party is the chat.
			senderJID = chatJID
		}
		ts := fallbackTS
		if ms := react.GetSenderTimestampMS(); ms > 0 {
			ts = time.UnixMilli(ms)
		}
		i.persistReaction(ctx, Reaction{
			ChatJID:   chatJID,
			TargetID:  targetID,
			SenderJID: senderJID,
			Emoji:     react.GetText(),
			Timestamp: ts,
			IsFromMe:  key.GetFromMe(),
		})
	}
}

func (i *Ingestor) handleContact(ctx context.Context, evt *events.Contact) {
	if evt == nil || evt.Action == nil {
		return
	}
	i.contact.Add(1)
	jid := evt.JID.ToNonAD().String()
	if jid == "" {
		return
	}
	c := Contact{
		JID:       jid,
		FullName:  evt.Action.GetFullName(),
		FirstName: evt.Action.GetFirstName(),
	}
	if err := i.store.UpsertContact(ctx, c); err != nil {
		i.logger.Warn("cache: contact event", slog.String("jid", jid), slog.String("err", err.Error()))
	}
}

func (i *Ingestor) handlePushName(ctx context.Context, evt *events.PushName) {
	if evt == nil {
		return
	}
	jid := evt.JID.ToNonAD().String()
	if jid == "" {
		return
	}
	if err := i.store.UpsertContact(ctx, Contact{JID: jid, PushName: evt.NewPushName}); err != nil {
		i.logger.Warn("cache: push name event", slog.String("jid", jid), slog.String("err", err.Error()))
	}
}

func (i *Ingestor) handleBusinessName(ctx context.Context, evt *events.BusinessName) {
	if evt == nil {
		return
	}
	jid := evt.JID.ToNonAD().String()
	if jid == "" {
		return
	}
	if err := i.store.UpsertContact(ctx, Contact{JID: jid, BusinessName: evt.NewBusinessName}); err != nil {
		i.logger.Warn("cache: business name event", slog.String("jid", jid), slog.String("err", err.Error()))
	}
}

func (i *Ingestor) handleGroupInfo(ctx context.Context, evt *events.GroupInfo) {
	if evt == nil {
		return
	}
	chat := Chat{JID: evt.JID.String(), IsGroup: true}
	if evt.Name != nil {
		chat.Name = evt.Name.Name
	}
	if !evt.Timestamp.IsZero() {
		chat.LastMessageTS = evt.Timestamp
	}
	if err := i.store.UpsertChat(ctx, chat); err != nil {
		i.logger.Warn("cache: group info event", slog.String("jid", evt.JID.String()), slog.String("err", err.Error()))
	}
}

// handleJoinedGroup creates the chat row when the user joins or is added to a
// group, before any message has flowed through it. Communities (parent groups)
// get chat_type=community; ordinary groups and community subgroups get group.
func (i *Ingestor) handleJoinedGroup(ctx context.Context, evt *events.JoinedGroup) {
	if evt == nil || evt.JID.User == "" {
		return
	}
	chatType := ChatTypeGroup
	if evt.IsParent {
		chatType = ChatTypeCommunity
	}
	chat := Chat{
		JID:     evt.JID.String(),
		IsGroup: true,
		Type:    chatType,
		Name:    evt.Name,
	}
	if !evt.NameSetAt.IsZero() {
		chat.LastMessageTS = evt.NameSetAt
	}
	if err := i.store.UpsertChat(ctx, chat); err != nil {
		i.logger.Warn("cache: joined group event", slog.String("jid", evt.JID.String()), slog.String("err", err.Error()))
	}
}

func (i *Ingestor) handleNewsletterJoin(ctx context.Context, evt *events.NewsletterJoin) {
	if evt == nil || evt.ID.User == "" {
		return
	}
	chat := Chat{
		JID:  evt.ID.String(),
		Type: ChatTypeNewsletter,
		Name: evt.ThreadMeta.Name.Text,
	}
	if t := evt.ThreadMeta.CreationTime.Time; !t.IsZero() {
		chat.LastMessageTS = t
	}
	if err := i.store.UpsertChat(ctx, chat); err != nil {
		i.logger.Warn("cache: newsletter join event", slog.String("jid", evt.ID.String()), slog.String("err", err.Error()))
	}
}

// handleNewsletterLeave records that the user unsubscribed but keeps the row
// so historical messages stay queryable. A `subscribed` flag on chats would
// let readers filter out left newsletters; deferred to a follow-up.
func (i *Ingestor) handleNewsletterLeave(ctx context.Context, evt *events.NewsletterLeave) {
	if evt == nil || evt.ID.User == "" {
		return
	}
	chat := Chat{JID: evt.ID.String(), Type: ChatTypeNewsletter}
	if err := i.store.UpsertChat(ctx, chat); err != nil {
		i.logger.Warn("cache: newsletter leave event", slog.String("jid", evt.ID.String()), slog.String("err", err.Error()))
	}
}

func (i *Ingestor) handleMarkChatAsRead(ctx context.Context, evt *events.MarkChatAsRead) {
	if evt == nil || evt.JID.User == "" {
		return
	}
	i.markRead.Add(1)
	read := evt.Action != nil && evt.Action.GetRead()
	if err := i.store.SetChatUnread(ctx, evt.JID.String(), evt.JID.Server == types.GroupServer, !read); err != nil {
		i.logger.Warn("cache: mark chat as read", slog.String("jid", evt.JID.String()), slog.String("err", err.Error()))
	}
}

func (i *Ingestor) handlePin(ctx context.Context, evt *events.Pin) {
	if evt == nil || evt.JID.User == "" {
		return
	}
	i.pin.Add(1)
	pinned := evt.Action != nil && evt.Action.GetPinned()
	if err := i.store.SetChatPinned(ctx, evt.JID.String(), evt.JID.Server == types.GroupServer, pinned); err != nil {
		i.logger.Warn("cache: pin event", slog.String("jid", evt.JID.String()), slog.String("err", err.Error()))
	}
}

func (i *Ingestor) handleArchive(ctx context.Context, evt *events.Archive) {
	if evt == nil || evt.JID.User == "" {
		return
	}
	i.archive.Add(1)
	archived := evt.Action != nil && evt.Action.GetArchived()
	if err := i.store.SetChatArchived(ctx, evt.JID.String(), evt.JID.Server == types.GroupServer, archived); err != nil {
		i.logger.Warn("cache: archive event", slog.String("jid", evt.JID.String()), slog.String("err", err.Error()))
	}
}

// handleStar surfaces the chat row when a message is starred from another
// device, so the chat list can include it. The starred flag itself is not
// persisted — the messages table has no starred column today; deferred to a
// follow-up that adds it.
func (i *Ingestor) handleStar(ctx context.Context, evt *events.Star) {
	if evt == nil || evt.ChatJID.User == "" {
		return
	}
	chat := Chat{
		JID:     evt.ChatJID.String(),
		IsGroup: evt.ChatJID.Server == types.GroupServer,
	}
	if err := i.store.UpsertChat(ctx, chat); err != nil {
		i.logger.Warn("cache: star event", slog.String("jid", evt.ChatJID.String()), slog.String("err", err.Error()))
	}
}

// persistPollCreation mirrors the ballot of a poll creation message into the
// poll tables. It is a no-op for every other envelope, so both the live and
// the history-sync ingest paths can call it unconditionally.
func (i *Ingestor) persistPollCreation(ctx context.Context, chatJID, senderJID, msgID string, ts time.Time, isFromMe bool, msg *waE2E.Message) {
	poll := extractPollCreation(msg)
	if poll == nil {
		return
	}
	row := Poll{
		ChatJID:         chatJID,
		MessageID:       msgID,
		Question:        poll.GetName(),
		SelectableCount: int(poll.GetSelectableOptionsCount()),
		SenderJID:       senderJID,
		IsFromMe:        isFromMe,
		Timestamp:       ts,
	}
	for _, opt := range poll.GetOptions() {
		row.Options = append(row.Options, PollOption{Name: opt.GetOptionName()})
	}
	if err := i.store.UpsertPoll(ctx, row); err != nil {
		i.logger.Warn("cache: upsert poll",
			slog.String("chat_jid", chatJID), slog.String("message_id", msgID), slog.String("err", err.Error()))
	}
}

// handlePollVote decrypts an incoming poll update and records the voter's
// selection. The vote is stored against the chat the *event* arrived in, not
// the one named by the poll key: in a direct chat each side's key points at
// the other party, so only the event's own chat matches the poll row.
//
// A vote we cannot decrypt is dropped with a warning. That is terminal, not
// transient — the poll's secret is either in whatsmeow's store or it never
// will be — so retrying or persisting the ciphertext would buy nothing.
func (i *Ingestor) handlePollVote(ctx context.Context, evt *events.Message, vote *waE2E.PollUpdateMessage) {
	pollID := vote.GetPollCreationMessageKey().GetID()
	if pollID == "" {
		return
	}
	chatJID := evt.Info.Chat.String()

	dec := i.pollDecrypter.Load()
	if dec == nil {
		i.logger.Warn("cache: poll vote dropped; no decrypter installed",
			slog.String("chat_jid", chatJID), slog.String("poll_message_id", pollID))
		return
	}
	decrypted, err := (*dec).DecryptPollVote(ctx, evt)
	if err != nil {
		i.logger.Warn("cache: decrypt poll vote",
			slog.String("chat_jid", chatJID), slog.String("poll_message_id", pollID),
			slog.String("err", err.Error()))
		return
	}

	// An empty selection is a voter clearing their vote, which is exactly why
	// the slice is built even when there is nothing to append: a nil would be
	// indistinguishable from "never voted" downstream.
	selected := make([]string, 0, len(decrypted.GetSelectedOptions()))
	for _, hash := range decrypted.GetSelectedOptions() {
		selected = append(selected, hex.EncodeToString(hash))
	}

	ts := evt.Info.Timestamp
	if ms := vote.GetSenderTimestampMS(); ms > 0 {
		ts = time.UnixMilli(ms)
	}
	if ts.IsZero() {
		ts = time.Now()
	}

	if err := i.store.UpsertPollVote(ctx, PollVote{
		ChatJID:        chatJID,
		PollMessageID:  pollID,
		VoterJID:       evt.Info.Sender.ToNonAD().String(),
		SelectedHashes: selected,
		Timestamp:      ts,
	}); err != nil {
		i.logger.Warn("cache: upsert poll vote",
			slog.String("chat_jid", chatJID), slog.String("poll_message_id", pollID),
			slog.String("err", err.Error()))
	}
}

// extractPollCreation returns the poll carried by a message, whichever of the
// numbered PollCreationMessage fields it arrived in. WhatsApp has revised the
// envelope repeatedly (V2, V3, V5, V6 are the same message type; V4 wraps it
// in a FutureProofMessage) while keeping the payload identical, so all of them
// are worth reading.
func extractPollCreation(msg *waE2E.Message) *waE2E.PollCreationMessage {
	if p := directPollCreation(msg); p != nil {
		return p
	}
	return directPollCreation(msg.GetPollCreationMessageV4().GetMessage())
}

func directPollCreation(msg *waE2E.Message) *waE2E.PollCreationMessage {
	if msg == nil {
		return nil
	}
	for _, p := range []*waE2E.PollCreationMessage{
		msg.GetPollCreationMessage(),
		msg.GetPollCreationMessageV2(),
		msg.GetPollCreationMessageV3(),
		msg.GetPollCreationMessageV5(),
		msg.GetPollCreationMessageV6(),
	} {
		if p != nil {
			return p
		}
	}
	return nil
}

// handlePresence records an availability update for a contact. These events
// only arrive for JIDs this device explicitly subscribed to (the
// subscribe_presence tool) and only while this device is itself online, so
// the presence columns stay at their defaults for every other contact.
//
// evt.LastSeen is zero when the contact hides their last-seen time; the store
// keeps any previously observed value in that case rather than zeroing it.
func (i *Ingestor) handlePresence(ctx context.Context, evt *events.Presence) {
	if evt == nil || evt.From.User == "" {
		return
	}
	jid := evt.From.ToNonAD().String()
	if err := i.store.UpsertPresence(ctx, jid, !evt.Unavailable, evt.LastSeen); err != nil {
		i.logger.Warn("cache: presence event", slog.String("jid", jid), slog.String("err", err.Error()))
	}
}

// buildMessageRow pulls the shape of a Message row out of a waE2E.Message.
// Returns nil when the envelope carries nothing worth persisting.
func buildMessageRow(chatJID, senderJID, id, pushName string, ts time.Time, isFromMe bool, msg *waE2E.Message) *Message {
	if msg == nil {
		return nil
	}
	body := extractTextBody(msg)
	kind, media, caption := extractEnvelope(msg)
	if caption != "" && body == "" {
		body = caption
	}
	if kind == KindOther && body == "" && media == nil {
		return nil
	}
	replyTo := extractReplyTo(msg)
	return &Message{
		ID:             id,
		ChatJID:        chatJID,
		SenderJID:      senderJID,
		SenderPushName: pushName,
		Timestamp:      ts,
		Kind:           kind,
		Body:           body,
		ReplyToID:      replyTo,
		IsFromMe:       isFromMe,
		Media:          media,
	}
}

// extractTextBody returns the plain text from a message, handling both the
// bare Conversation variant and the ExtendedTextMessage variant that carries
// formatting and mentions.
func extractTextBody(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	if c := msg.GetConversation(); c != "" {
		return c
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	return ""
}

// extractEnvelope reports the message kind, media metadata, and any secondary
// text (a media caption, a poll question) that should be promoted into the
// searchable body column when the envelope carries no body of its own.
func extractEnvelope(msg *waE2E.Message) (MessageKind, *Media, string) {
	if msg == nil {
		return KindOther, nil, ""
	}
	if msg.GetConversation() != "" || msg.GetExtendedTextMessage() != nil {
		return KindText, nil, ""
	}
	if img := msg.GetImageMessage(); img != nil {
		return KindImage, &Media{
			Mime:       img.GetMimetype(),
			URL:        img.GetURL(),
			DirectPath: img.GetDirectPath(),
			Key:        img.GetMediaKey(),
			SHA256:     img.GetFileSHA256(),
			EncSHA256:  img.GetFileEncSHA256(),
			Length:     img.GetFileLength(),
		}, img.GetCaption()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return KindVideo, &Media{
			Mime:       vid.GetMimetype(),
			URL:        vid.GetURL(),
			DirectPath: vid.GetDirectPath(),
			Key:        vid.GetMediaKey(),
			SHA256:     vid.GetFileSHA256(),
			EncSHA256:  vid.GetFileEncSHA256(),
			Length:     vid.GetFileLength(),
		}, vid.GetCaption()
	}
	if aud := msg.GetAudioMessage(); aud != nil {
		return KindAudio, &Media{
			Mime:       aud.GetMimetype(),
			URL:        aud.GetURL(),
			DirectPath: aud.GetDirectPath(),
			Key:        aud.GetMediaKey(),
			SHA256:     aud.GetFileSHA256(),
			EncSHA256:  aud.GetFileEncSHA256(),
			Length:     aud.GetFileLength(),
		}, ""
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return KindDocument, &Media{
			Mime:       doc.GetMimetype(),
			Filename:   doc.GetFileName(),
			URL:        doc.GetURL(),
			DirectPath: doc.GetDirectPath(),
			Key:        doc.GetMediaKey(),
			SHA256:     doc.GetFileSHA256(),
			EncSHA256:  doc.GetFileEncSHA256(),
			Length:     doc.GetFileLength(),
		}, doc.GetCaption()
	}
	if poll := extractPollCreation(msg); poll != nil {
		// The poll question is promoted into the body so a poll is legible in
		// list_messages and reachable by full-text search; the ballot and the
		// tally live in the poll tables.
		return KindPoll, nil, poll.GetName()
	}
	if st := msg.GetStickerMessage(); st != nil {
		return KindSticker, &Media{
			Mime:       st.GetMimetype(),
			URL:        st.GetURL(),
			DirectPath: st.GetDirectPath(),
			Key:        st.GetMediaKey(),
			SHA256:     st.GetFileSHA256(),
			EncSHA256:  st.GetFileEncSHA256(),
			Length:     st.GetFileLength(),
		}, ""
	}
	return KindOther, nil, ""
}

// extractReplyTo returns the stanza id of the message being replied to, if any.
func extractReplyTo(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		if ci := ext.GetContextInfo(); ci != nil {
			return ci.GetStanzaID()
		}
	}
	if img := msg.GetImageMessage(); img != nil {
		if ci := img.GetContextInfo(); ci != nil {
			return ci.GetStanzaID()
		}
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		if ci := vid.GetContextInfo(); ci != nil {
			return ci.GetStanzaID()
		}
	}
	if aud := msg.GetAudioMessage(); aud != nil {
		if ci := aud.GetContextInfo(); ci != nil {
			return ci.GetStanzaID()
		}
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		if ci := doc.GetContextInfo(); ci != nil {
			return ci.GetStanzaID()
		}
	}
	return ""
}
