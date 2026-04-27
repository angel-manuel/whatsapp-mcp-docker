package cache

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

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

// NewIngestor constructs an Ingestor backed by store. A nil logger is replaced
// with a discarding one so callers that don't care about diagnostics can pass
// nil.
func NewIngestor(store *Store, logger *slog.Logger) *Ingestor {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Ingestor{store: store, logger: logger}
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

	if err := i.store.UpsertChat(ctx, Chat{
		JID:           chatJID,
		IsGroup:       evt.Info.IsGroup,
		LastMessageTS: ts,
	}); err != nil {
		i.logger.Warn("cache: upsert chat", slog.String("chat_jid", chatJID), slog.String("err", err.Error()))
	}

	if senderJID != "" && !evt.Info.IsFromMe {
		contact := Contact{JID: senderJID, PushName: evt.Info.PushName}
		if evt.Info.VerifiedName != nil && evt.Info.VerifiedName.Details != nil {
			contact.BusinessName = evt.Info.VerifiedName.Details.GetVerifiedName()
		}
		if err := i.store.UpsertContact(ctx, contact); err != nil {
			i.logger.Warn("cache: upsert contact", slog.String("jid", senderJID), slog.String("err", err.Error()))
		}
	}

	msg := buildMessageRow(chatJID, senderJID, evt.Info.ID, evt.Info.PushName, ts, evt.Info.IsFromMe, evt.Message)
	if msg == nil {
		return
	}
	if err := i.store.InsertMessage(ctx, *msg); err != nil {
		i.logger.Warn("cache: insert message",
			slog.String("chat_jid", chatJID), slog.String("message_id", evt.Info.ID), slog.String("err", err.Error()))
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
			row := buildMessageRow(chatJIDStr, senderJID, key.GetID(), web.GetPushName(), rowTS, key.GetFromMe(), web.GetMessage())
			if row == nil {
				continue
			}
			if err := i.store.InsertMessage(ctx, *row); err != nil {
				i.logger.Warn("cache: history insert message",
					slog.String("chat_jid", chatJIDStr), slog.String("message_id", key.GetID()), slog.String("err", err.Error()))
			}
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

func (i *Ingestor) handleContact(ctx context.Context, evt *events.Contact) {
	if evt == nil || evt.Action == nil {
		return
	}
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
	if evt.GroupParent.IsParent {
		chatType = ChatTypeCommunity
	}
	chat := Chat{
		JID:     evt.JID.String(),
		IsGroup: true,
		Type:    chatType,
		Name:    evt.GroupName.Name,
	}
	if !evt.GroupName.NameSetAt.IsZero() {
		chat.LastMessageTS = evt.GroupName.NameSetAt
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
	read := evt.Action != nil && evt.Action.GetRead()
	if err := i.store.SetChatUnread(ctx, evt.JID.String(), evt.JID.Server == types.GroupServer, !read); err != nil {
		i.logger.Warn("cache: mark chat as read", slog.String("jid", evt.JID.String()), slog.String("err", err.Error()))
	}
}

func (i *Ingestor) handlePin(ctx context.Context, evt *events.Pin) {
	if evt == nil || evt.JID.User == "" {
		return
	}
	pinned := evt.Action != nil && evt.Action.GetPinned()
	if err := i.store.SetChatPinned(ctx, evt.JID.String(), evt.JID.Server == types.GroupServer, pinned); err != nil {
		i.logger.Warn("cache: pin event", slog.String("jid", evt.JID.String()), slog.String("err", err.Error()))
	}
}

func (i *Ingestor) handleArchive(ctx context.Context, evt *events.Archive) {
	if evt == nil || evt.JID.User == "" {
		return
	}
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

// extractEnvelope reports the message kind, media metadata, and any media
// caption we should promote into the searchable body column.
func extractEnvelope(msg *waE2E.Message) (MessageKind, *Media, string) {
	if msg == nil {
		return KindOther, nil, ""
	}
	if msg.GetConversation() != "" || msg.GetExtendedTextMessage() != nil {
		return KindText, nil, ""
	}
	if img := msg.GetImageMessage(); img != nil {
		return KindImage, &Media{
			Mime:      img.GetMimetype(),
			URL:       img.GetURL(),
			Key:       img.GetMediaKey(),
			SHA256:    img.GetFileSHA256(),
			EncSHA256: img.GetFileEncSHA256(),
			Length:    img.GetFileLength(),
		}, img.GetCaption()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return KindVideo, &Media{
			Mime:      vid.GetMimetype(),
			URL:       vid.GetURL(),
			Key:       vid.GetMediaKey(),
			SHA256:    vid.GetFileSHA256(),
			EncSHA256: vid.GetFileEncSHA256(),
			Length:    vid.GetFileLength(),
		}, vid.GetCaption()
	}
	if aud := msg.GetAudioMessage(); aud != nil {
		return KindAudio, &Media{
			Mime:      aud.GetMimetype(),
			URL:       aud.GetURL(),
			Key:       aud.GetMediaKey(),
			SHA256:    aud.GetFileSHA256(),
			EncSHA256: aud.GetFileEncSHA256(),
			Length:    aud.GetFileLength(),
		}, ""
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return KindDocument, &Media{
			Mime:      doc.GetMimetype(),
			Filename:  doc.GetFileName(),
			URL:       doc.GetURL(),
			Key:       doc.GetMediaKey(),
			SHA256:    doc.GetFileSHA256(),
			EncSHA256: doc.GetFileEncSHA256(),
			Length:    doc.GetFileLength(),
		}, doc.GetCaption()
	}
	if st := msg.GetStickerMessage(); st != nil {
		return KindSticker, &Media{
			Mime:      st.GetMimetype(),
			URL:       st.GetURL(),
			Key:       st.GetMediaKey(),
			SHA256:    st.GetFileSHA256(),
			EncSHA256: st.GetFileEncSHA256(),
			Length:    st.GetFileLength(),
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
