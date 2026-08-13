package cache

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func newTestIngestor(t *testing.T) (*Ingestor, *Store) {
	t.Helper()
	store := newTestStore(t)
	return NewIngestor(store, nil), store
}

func mustParseJID(t *testing.T, s string) types.JID {
	t.Helper()
	jid, err := types.ParseJID(s)
	if err != nil {
		t.Fatalf("ParseJID(%q): %v", s, err)
	}
	return jid
}

func TestHandleEvent_TextMessage_PersistsRowAndChatAndContact(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	sender := chat
	ts := time.Unix(1_700_000_000, 0).UTC()

	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: false},
			ID:            "wamid.TEXT1",
			PushName:      "Alice",
			Timestamp:     ts,
		},
		Message: &waE2E.Message{Conversation: proto.String("hello world")},
	}
	ingest.HandleEvent(evt)

	var body, pushName string
	var storedTS int64
	var isFromMe int
	err := store.DB().QueryRowContext(context.Background(),
		`SELECT body, sender_push_name, ts, is_from_me FROM messages WHERE chat_jid = ? AND id = ?`,
		chat.String(), "wamid.TEXT1").Scan(&body, &pushName, &storedTS, &isFromMe)
	if err != nil {
		t.Fatalf("scan message: %v", err)
	}
	if body != "hello world" {
		t.Fatalf("body = %q", body)
	}
	if pushName != "Alice" {
		t.Fatalf("push_name = %q", pushName)
	}
	if storedTS != ts.Unix() {
		t.Fatalf("ts = %d, want %d", storedTS, ts.Unix())
	}
	if isFromMe != 0 {
		t.Fatalf("is_from_me = %d", isFromMe)
	}

	var chatName string
	var lastTS int64
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT name, last_message_ts FROM chats WHERE jid = ?`, chat.String()).Scan(&chatName, &lastTS); err != nil {
		t.Fatalf("scan chat: %v", err)
	}
	if lastTS != ts.Unix() {
		t.Fatalf("chat last_message_ts = %d, want %d", lastTS, ts.Unix())
	}

	var contactPush string
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT push_name FROM contacts WHERE jid = ?`, sender.ToNonAD().String()).Scan(&contactPush); err != nil {
		t.Fatalf("scan contact: %v", err)
	}
	if contactPush != "Alice" {
		t.Fatalf("contact push_name = %q", contactPush)
	}
}

func TestHandleEvent_ExtendedTextWithReply(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "group@g.us")
	sender := mustParseJID(t, "2222222222@s.whatsapp.net")
	ts := time.Unix(1_700_000_100, 0).UTC()

	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: true},
			ID:            "wamid.EXT1",
			Timestamp:     ts,
		},
		Message: &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String("replying here"),
				ContextInfo: &waE2E.ContextInfo{
					StanzaID: proto.String("wamid.ORIG"),
				},
			},
		},
	}
	ingest.HandleEvent(evt)

	var body, replyTo string
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT body, reply_to_id FROM messages WHERE chat_jid = ? AND id = ?`,
		chat.String(), "wamid.EXT1").Scan(&body, &replyTo); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if body != "replying here" {
		t.Fatalf("body = %q", body)
	}
	if replyTo != "wamid.ORIG" {
		t.Fatalf("reply_to_id = %q", replyTo)
	}

	var isGroup int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT is_group FROM chats WHERE jid = ?`, chat.String()).Scan(&isGroup); err != nil {
		t.Fatalf("scan chat: %v", err)
	}
	if isGroup != 1 {
		t.Fatalf("is_group = %d", isGroup)
	}
}

func TestHandleEvent_ImageMessage_StoresMediaMetadataAndCaption(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")

	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: chat},
			ID:            "wamid.IMG1",
			Timestamp:     time.Unix(1_700_000_200, 0).UTC(),
		},
		Message: &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{
				URL:           proto.String("https://mmg.whatsapp.net/x"),
				DirectPath:    proto.String("/v/t62.7118-24/img"),
				Mimetype:      proto.String("image/jpeg"),
				Caption:       proto.String("a cat"),
				MediaKey:      []byte{0x01, 0x02},
				FileSHA256:    []byte{0xaa},
				FileEncSHA256: []byte{0xbb},
				FileLength:    proto.Uint64(4321),
			},
		},
	}
	ingest.HandleEvent(evt)

	var kind, body, mime, url, directPath string
	var length int64
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT kind, body, media_mime, media_url, media_direct_path, media_length FROM messages WHERE chat_jid = ? AND id = ?`,
		chat.String(), "wamid.IMG1").Scan(&kind, &body, &mime, &url, &directPath, &length); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if kind != string(KindImage) {
		t.Fatalf("kind = %q", kind)
	}
	if body != "a cat" {
		t.Fatalf("caption promoted to body failed: %q", body)
	}
	if mime != "image/jpeg" || url != "https://mmg.whatsapp.net/x" || length != 4321 {
		t.Fatalf("media metadata mismatch: mime=%q url=%q len=%d", mime, url, length)
	}
	if directPath != "/v/t62.7118-24/img" {
		t.Fatalf("media_direct_path = %q", directPath)
	}
}

// TestHandleEvent_MediaKinds_CaptureDirectPath pins the locator capture for
// every media kind. The direct path is the only durable one — media_url
// expires and cannot be refreshed — and it exists solely on the live
// protobuf, so a kind missing from extractEnvelope can never be repaired
// after the fact. That makes per-kind coverage worth the table.
func TestHandleEvent_MediaKinds_CaptureDirectPath(t *testing.T) {
	const (
		wantURL  = "https://mmg.whatsapp.net/blob"
		wantPath = "/v/t62.7118-24/blob"
	)
	key := []byte{0x01, 0x02, 0x03}
	fileSHA := []byte{0xaa, 0xbb}
	encSHA := []byte{0xcc, 0xdd}

	cases := []struct {
		name     string
		id       string
		msg      *waE2E.Message
		wantKind MessageKind
		wantMime string
		wantName string
	}{
		{
			name: "image", id: "wamid.M-IMG", wantKind: KindImage, wantMime: "image/jpeg",
			msg: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				URL: proto.String(wantURL), DirectPath: proto.String(wantPath),
				Mimetype: proto.String("image/jpeg"), MediaKey: key,
				FileSHA256: fileSHA, FileEncSHA256: encSHA, FileLength: proto.Uint64(11),
			}},
		},
		{
			name: "video", id: "wamid.M-VID", wantKind: KindVideo, wantMime: "video/mp4",
			msg: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
				URL: proto.String(wantURL), DirectPath: proto.String(wantPath),
				Mimetype: proto.String("video/mp4"), MediaKey: key,
				FileSHA256: fileSHA, FileEncSHA256: encSHA, FileLength: proto.Uint64(22),
			}},
		},
		{
			name: "audio", id: "wamid.M-AUD", wantKind: KindAudio, wantMime: "audio/ogg; codecs=opus",
			msg: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
				URL: proto.String(wantURL), DirectPath: proto.String(wantPath),
				Mimetype: proto.String("audio/ogg; codecs=opus"), MediaKey: key,
				FileSHA256: fileSHA, FileEncSHA256: encSHA, FileLength: proto.Uint64(33),
			}},
		},
		{
			name: "document", id: "wamid.M-DOC", wantKind: KindDocument, wantMime: "application/pdf",
			wantName: "invoice.pdf",
			msg: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
				URL: proto.String(wantURL), DirectPath: proto.String(wantPath),
				Mimetype: proto.String("application/pdf"), FileName: proto.String("invoice.pdf"),
				MediaKey: key, FileSHA256: fileSHA, FileEncSHA256: encSHA, FileLength: proto.Uint64(44),
			}},
		},
		{
			name: "sticker", id: "wamid.M-STK", wantKind: KindSticker, wantMime: "image/webp",
			msg: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{
				URL: proto.String(wantURL), DirectPath: proto.String(wantPath),
				Mimetype: proto.String("image/webp"), MediaKey: key,
				FileSHA256: fileSHA, FileEncSHA256: encSHA, FileLength: proto.Uint64(55),
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ingest, store := newTestIngestor(t)
			chat := mustParseJID(t, "1234567890@s.whatsapp.net")
			ingest.HandleEvent(&events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{Chat: chat, Sender: chat},
					ID:            tc.id,
					Timestamp:     time.Unix(1_700_000_400, 0).UTC(),
				},
				Message: tc.msg,
			})

			row, err := store.GetMessageMedia(context.Background(), chat.String(), tc.id)
			if err != nil {
				t.Fatalf("GetMessageMedia: %v", err)
			}
			if row.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", row.Kind, tc.wantKind)
			}
			if row.DirectPath != wantPath {
				t.Errorf("DirectPath = %q, want %q", row.DirectPath, wantPath)
			}
			if row.URL != wantURL {
				t.Errorf("URL = %q, want %q", row.URL, wantURL)
			}
			if row.Mime != tc.wantMime {
				t.Errorf("Mime = %q, want %q", row.Mime, tc.wantMime)
			}
			if row.Filename != tc.wantName {
				t.Errorf("Filename = %q, want %q", row.Filename, tc.wantName)
			}
			if !row.HasMedia() {
				t.Errorf("HasMedia() = false, want true")
			}
		})
	}
}

// TestGetMessageMedia_TextMessageHasNoMedia keeps the "no attachment" case
// distinguishable from "message not cached": download_media reports them
// with different error codes.
func TestGetMessageMedia_TextMessageHasNoMedia(t *testing.T) {
	ingest, store := newTestIngestor(t)
	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	ingest.HandleEvent(&events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: chat},
			ID:            "wamid.TXT",
			Timestamp:     time.Unix(1_700_000_500, 0).UTC(),
		},
		Message: &waE2E.Message{Conversation: proto.String("hello")},
	})

	row, err := store.GetMessageMedia(context.Background(), chat.String(), "wamid.TXT")
	if err != nil {
		t.Fatalf("GetMessageMedia: %v", err)
	}
	if row.HasMedia() {
		t.Fatalf("HasMedia() = true for a text message")
	}
	if row.Kind != KindText {
		t.Fatalf("kind = %q, want %q", row.Kind, KindText)
	}

	if _, err := store.GetMessageMedia(context.Background(), chat.String(), "wamid.MISSING"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing message: err = %v, want sql.ErrNoRows", err)
	}
}

// TestGetMessageMedia_MissingTimestampStaysZero keeps "unknown time"
// distinguishable from "the epoch". download_media synthesises a filename
// from this timestamp and needs to fall back when it was never recorded;
// silently returning 1970-01-01 would make that fallback unreachable.
func TestGetMessageMedia_MissingTimestampStaysZero(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	chat := "1234567890@s.whatsapp.net"
	if err := store.InsertMessage(ctx, Message{
		ID: "wamid.NOTS", ChatJID: chat, SenderJID: chat, Kind: KindImage,
		Media: &Media{Mime: "image/jpeg", Key: []byte{0x01}, DirectPath: "/v/x"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	row, err := store.GetMessageMedia(ctx, chat, "wamid.NOTS")
	if err != nil {
		t.Fatalf("GetMessageMedia: %v", err)
	}
	if !row.Timestamp.IsZero() {
		t.Fatalf("Timestamp = %s, want the zero time", row.Timestamp)
	}
}

func TestHandleEvent_ProtocolRevoke_MarksDeletedKeepsRow(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	sender := chat
	if err := store.InsertMessage(context.Background(), Message{
		ID: "wamid.ORIG", ChatJID: chat.String(), SenderJID: sender.ToNonAD().String(),
		Timestamp: time.Unix(1_700_000_000, 0), Kind: KindText, Body: "to be revoked",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	revokeType := waE2E.ProtocolMessage_REVOKE
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender},
			ID:            "wamid.REVOKE",
			Timestamp:     time.Unix(1_700_000_300, 0).UTC(),
		},
		Message: &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{
				Type: &revokeType,
				Key:  &waCommon.MessageKey{ID: proto.String("wamid.ORIG"), RemoteJID: proto.String(chat.String())},
			},
		},
	}
	ingest.HandleEvent(evt)

	var body string
	var deleted int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT body, deleted FROM messages WHERE chat_jid = ? AND id = ?`,
		chat.String(), "wamid.ORIG").Scan(&body, &deleted); err != nil {
		t.Fatalf("scan orig: %v", err)
	}
	if body != "" || deleted != 1 {
		t.Fatalf("revoke not applied: body=%q deleted=%d", body, deleted)
	}

	// The revoke envelope itself must not be persisted as a fresh message row.
	var count int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messages WHERE id = ?`, "wamid.REVOKE").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("revoke envelope persisted as message (count=%d)", count)
	}
}

func TestHandleEvent_ProtocolEdit_RewritesBodyAndFlagsEdited(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	if err := store.InsertMessage(context.Background(), Message{
		ID: "wamid.ORIG", ChatJID: chat.String(), Timestamp: time.Unix(1, 0), Body: "first",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	editType := waE2E.ProtocolMessage_MESSAGE_EDIT
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat},
			ID:            "wamid.EDIT",
			Timestamp:     time.Unix(1_700_000_400, 0).UTC(),
		},
		Message: &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{
				Type:          &editType,
				Key:           &waCommon.MessageKey{ID: proto.String("wamid.ORIG"), RemoteJID: proto.String(chat.String())},
				EditedMessage: &waE2E.Message{Conversation: proto.String("rewritten")},
			},
		},
	}
	ingest.HandleEvent(evt)

	var body string
	var edited int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT body, edited FROM messages WHERE chat_jid = ? AND id = ?`,
		chat.String(), "wamid.ORIG").Scan(&body, &edited); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if body != "rewritten" || edited != 1 {
		t.Fatalf("edit not applied: body=%q edited=%d", body, edited)
	}
}

func TestHandleEvent_UnwrappedEditFlag_MarksExistingRow(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	if err := store.InsertMessage(context.Background(), Message{
		ID: "wamid.UW", ChatJID: chat.String(), Timestamp: time.Unix(1, 0), Body: "original",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	evt := &events.Message{
		IsEdit: true,
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat},
			ID:            "wamid.UW",
			Timestamp:     time.Unix(2, 0).UTC(),
		},
		Message: &waE2E.Message{Conversation: proto.String("edited via unwrap")},
	}
	ingest.HandleEvent(evt)

	var body string
	var edited int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT body, edited FROM messages WHERE chat_jid = ? AND id = ?`,
		chat.String(), "wamid.UW").Scan(&body, &edited); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if body != "edited via unwrap" || edited != 1 {
		t.Fatalf("unwrap-edit not applied: body=%q edited=%d", body, edited)
	}
}

func TestHandleEvent_PushNameUpdatesContact(t *testing.T) {
	ingest, store := newTestIngestor(t)

	jid := mustParseJID(t, "3333333333@s.whatsapp.net")
	ingest.HandleEvent(&events.PushName{JID: jid, NewPushName: "Bob"})

	var push string
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT push_name FROM contacts WHERE jid = ?`, jid.ToNonAD().String()).Scan(&push); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if push != "Bob" {
		t.Fatalf("push_name = %q", push)
	}
}

func TestHandleEvent_ContactActionUpdatesFullName(t *testing.T) {
	ingest, store := newTestIngestor(t)

	jid := mustParseJID(t, "4444444444@s.whatsapp.net")
	// Pre-seed a push name: the contact-action event carries only the names
	// set via the sync-action patch, and must not clobber what PushName gave us.
	if err := store.UpsertContact(context.Background(), Contact{JID: jid.ToNonAD().String(), PushName: "Preseed"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ingest.HandleEvent(buildContactEvent(jid, "Carol Example", "Carol"))

	var push, full, first string
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT push_name, full_name, first_name FROM contacts WHERE jid = ?`,
		jid.ToNonAD().String()).Scan(&push, &full, &first); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if push != "Preseed" {
		t.Fatalf("push_name overwritten: %q", push)
	}
	if full != "Carol Example" || first != "Carol" {
		t.Fatalf("names not applied: full=%q first=%q", full, first)
	}
}

func TestHandleEvent_IgnoresUnknown(t *testing.T) {
	ingest, _ := newTestIngestor(t)
	// A random struct should be silently ignored (no panic, no error).
	ingest.HandleEvent(struct{ X int }{X: 1})
	ingest.HandleEvent(nil)
}

func TestHandleEvent_JoinedGroup_PersistsAsGroup(t *testing.T) {
	ingest, store := newTestIngestor(t)
	jid := mustParseJID(t, "120363000000000001@g.us")
	evt := &events.JoinedGroup{
		Reason: "invite",
		Type:   "new",
		GroupInfo: types.GroupInfo{
			JID:       jid,
			GroupName: types.GroupName{Name: "Weekend Plans"},
		},
	}
	ingest.HandleEvent(evt)

	var name, chatType string
	var isGroup int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT name, is_group, chat_type FROM chats WHERE jid = ?`,
		jid.String()).Scan(&name, &isGroup, &chatType); err != nil {
		t.Fatalf("scan chat: %v", err)
	}
	if name != "Weekend Plans" || isGroup != 1 || chatType != "group" {
		t.Fatalf("got name=%q is_group=%d chat_type=%q", name, isGroup, chatType)
	}
}

func TestHandleEvent_JoinedGroup_CommunityParent(t *testing.T) {
	ingest, store := newTestIngestor(t)
	jid := mustParseJID(t, "120363000000000002@g.us")
	evt := &events.JoinedGroup{
		Type: "new",
		GroupInfo: types.GroupInfo{
			JID:         jid,
			GroupName:   types.GroupName{Name: "Neighborhood"},
			GroupParent: types.GroupParent{IsParent: true},
		},
	}
	ingest.HandleEvent(evt)

	var chatType string
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT chat_type FROM chats WHERE jid = ?`, jid.String()).Scan(&chatType); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if chatType != "community" {
		t.Fatalf("chat_type = %q, want community", chatType)
	}
}

func TestHandleEvent_NewsletterJoin_PersistsAsNewsletter(t *testing.T) {
	ingest, store := newTestIngestor(t)
	jid := mustParseJID(t, "120363000000000003@newsletter")
	meta := types.NewsletterMetadata{ID: jid}
	meta.ThreadMeta.Name = types.NewsletterText{Text: "Daily Brief"}
	evt := &events.NewsletterJoin{NewsletterMetadata: meta}
	ingest.HandleEvent(evt)

	var name, chatType string
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT name, chat_type FROM chats WHERE jid = ?`, jid.String()).Scan(&name, &chatType); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if name != "Daily Brief" || chatType != "newsletter" {
		t.Fatalf("got name=%q chat_type=%q", name, chatType)
	}
}

func TestHandleEvent_MarkChatAsRead_SetsUnread(t *testing.T) {
	ingest, store := newTestIngestor(t)
	jid := mustParseJID(t, "1234567890@s.whatsapp.net")
	read := false
	evt := &events.MarkChatAsRead{
		JID:    jid,
		Action: &waSyncAction.MarkChatAsReadAction{Read: &read},
	}
	ingest.HandleEvent(evt)

	var unread int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT unread_count FROM chats WHERE jid = ?`, jid.String()).Scan(&unread); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if unread != 1 {
		t.Fatalf("unread_count = %d, want 1", unread)
	}
}

func TestHandleEvent_Pin_SetsPinned(t *testing.T) {
	ingest, store := newTestIngestor(t)
	jid := mustParseJID(t, "120363000000000004@g.us")
	pinned := true
	evt := &events.Pin{
		JID:    jid,
		Action: &waSyncAction.PinAction{Pinned: &pinned},
	}
	ingest.HandleEvent(evt)

	var p, isGroup int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT pinned, is_group FROM chats WHERE jid = ?`, jid.String()).Scan(&p, &isGroup); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if p != 1 || isGroup != 1 {
		t.Fatalf("got pinned=%d is_group=%d", p, isGroup)
	}
}

func TestHandleEvent_Archive_SetsArchived(t *testing.T) {
	ingest, store := newTestIngestor(t)
	jid := mustParseJID(t, "9999999999@s.whatsapp.net")
	archived := true
	evt := &events.Archive{
		JID:    jid,
		Action: &waSyncAction.ArchiveChatAction{Archived: &archived},
	}
	ingest.HandleEvent(evt)

	var a int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT archived FROM chats WHERE jid = ?`, jid.String()).Scan(&a); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if a != 1 {
		t.Fatalf("archived = %d, want 1", a)
	}
}

// Pre-seed a chat with a known name and ensure that a partial-info appstate
// event (Pin) leaves the existing name intact.
func TestHandleEvent_AppState_DoesNotClobberExistingName(t *testing.T) {
	ingest, store := newTestIngestor(t)
	jid := mustParseJID(t, "120363000000000005@g.us")
	if err := store.UpsertChat(context.Background(), Chat{
		JID:     jid.String(),
		Name:    "Existing",
		IsGroup: true,
		Type:    ChatTypeGroup,
	}); err != nil {
		t.Fatalf("seed UpsertChat: %v", err)
	}

	pinned := true
	ingest.HandleEvent(&events.Pin{JID: jid, Action: &waSyncAction.PinAction{Pinned: &pinned}})

	var name, chatType string
	var p int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT name, chat_type, pinned FROM chats WHERE jid = ?`, jid.String()).Scan(&name, &chatType, &p); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if name != "Existing" || chatType != "group" || p != 1 {
		t.Fatalf("got name=%q chat_type=%q pinned=%d", name, chatType, p)
	}
}

func TestHandleEvent_BumpsLastEventTimestamp(t *testing.T) {
	ingest, _ := newTestIngestor(t)
	if !ingest.LastEventAt().IsZero() {
		t.Fatalf("expected zero LastEventAt before any event")
	}
	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	ingest.HandleEvent(&events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: chat},
			ID:            "wamid.TS",
			Timestamp:     time.Unix(1_700_000_000, 0).UTC(),
		},
		Message: &waE2E.Message{Conversation: proto.String("hi")},
	})
	if ingest.LastEventAt().IsZero() {
		t.Fatalf("expected LastEventAt to advance after a Message event")
	}
}

// reactionEvent builds the events.Message envelope WhatsApp delivers for a
// reaction: the reactor is the envelope's sender, and the key points at the
// message being reacted to.
func reactionEvent(chat, sender types.JID, targetID, emoji string, isFromMe bool, senderTS time.Time) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: isFromMe},
			ID:            "wamid.REACT-" + targetID + "-" + emoji,
			Timestamp:     senderTS,
		},
		Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
			Key: &waCommon.MessageKey{
				ID:        proto.String(targetID),
				RemoteJID: proto.String(chat.String()),
				FromMe:    proto.Bool(false),
			},
			Text:              proto.String(emoji),
			SenderTimestampMS: proto.Int64(senderTS.UnixMilli()),
		}},
	}
}

func readReactions(t *testing.T, store *Store, chatJID, targetID string) []Reaction {
	t.Helper()
	rows, err := store.DB().QueryContext(context.Background(),
		`SELECT sender_jid, emoji, ts, is_from_me FROM reactions WHERE chat_jid = ? AND target_id = ? ORDER BY sender_jid`,
		chatJID, targetID)
	if err != nil {
		t.Fatalf("query reactions: %v", err)
	}
	defer rows.Close()
	var out []Reaction
	for rows.Next() {
		var (
			r        Reaction
			ts       int64
			isFromMe int
		)
		if err := rows.Scan(&r.SenderJID, &r.Emoji, &ts, &isFromMe); err != nil {
			t.Fatalf("scan reaction: %v", err)
		}
		r.ChatJID, r.TargetID = chatJID, targetID
		r.Timestamp = time.Unix(ts, 0).UTC()
		r.IsFromMe = isFromMe != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reaction rows: %v", err)
	}
	return out
}

func TestHandleEvent_Reaction_PersistsRow(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	ts := time.Unix(1_700_000_100, 0).UTC()
	ingest.HandleEvent(reactionEvent(chat, chat, "wamid.TARGET", "👍", false, ts))

	got := readReactions(t, store, chat.String(), "wamid.TARGET")
	if len(got) != 1 {
		t.Fatalf("want 1 reaction, got %d", len(got))
	}
	if got[0].Emoji != "👍" {
		t.Errorf("emoji = %q, want 👍", got[0].Emoji)
	}
	if got[0].SenderJID != chat.String() {
		t.Errorf("sender = %q, want %q", got[0].SenderJID, chat.String())
	}
	if got[0].IsFromMe {
		t.Error("is_from_me = true, want false")
	}
	if !got[0].Timestamp.Equal(ts) {
		t.Errorf("ts = %v, want %v (the reaction's own SenderTimestampMS)", got[0].Timestamp, ts)
	}
}

func TestHandleEvent_Reaction_ReplacesSameSendersPrevious(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	base := time.Unix(1_700_000_100, 0).UTC()
	ingest.HandleEvent(reactionEvent(chat, chat, "wamid.TARGET", "👍", false, base))
	ingest.HandleEvent(reactionEvent(chat, chat, "wamid.TARGET", "😂", false, base.Add(time.Minute)))

	got := readReactions(t, store, chat.String(), "wamid.TARGET")
	if len(got) != 1 {
		t.Fatalf("want 1 reaction after replacement, got %d", len(got))
	}
	if got[0].Emoji != "😂" {
		t.Errorf("emoji = %q, want 😂", got[0].Emoji)
	}
}

func TestHandleEvent_Reaction_StaleTimestampIsIgnored(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	base := time.Unix(1_700_000_100, 0).UTC()
	ingest.HandleEvent(reactionEvent(chat, chat, "wamid.TARGET", "😂", false, base))
	// Out-of-order delivery: an older reaction arriving after a newer one
	// must not overwrite it.
	ingest.HandleEvent(reactionEvent(chat, chat, "wamid.TARGET", "👍", false, base.Add(-time.Minute)))

	got := readReactions(t, store, chat.String(), "wamid.TARGET")
	if len(got) != 1 {
		t.Fatalf("want 1 reaction, got %d", len(got))
	}
	if got[0].Emoji != "😂" {
		t.Errorf("emoji = %q, want the newer 😂 to survive", got[0].Emoji)
	}
}

func TestHandleEvent_Reaction_EmptyTextRemovesRow(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	base := time.Unix(1_700_000_100, 0).UTC()
	ingest.HandleEvent(reactionEvent(chat, chat, "wamid.TARGET", "👍", false, base))
	ingest.HandleEvent(reactionEvent(chat, chat, "wamid.TARGET", "", false, base.Add(time.Minute)))

	if got := readReactions(t, store, chat.String(), "wamid.TARGET"); len(got) != 0 {
		t.Fatalf("want reaction removed, got %d rows: %+v", len(got), got)
	}
}

func TestHandleEvent_Reaction_OwnReactionUsesCanonicalEmptySender(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	me := mustParseJID(t, "9999999999@s.whatsapp.net")
	base := time.Unix(1_700_000_100, 0).UTC()
	ingest.HandleEvent(reactionEvent(chat, me, "wamid.TARGET", "👍", true, base))

	got := readReactions(t, store, chat.String(), "wamid.TARGET")
	if len(got) != 1 {
		t.Fatalf("want 1 reaction, got %d", len(got))
	}
	if got[0].SenderJID != "" {
		t.Errorf("sender = %q, want the canonical empty sender for our own reaction", got[0].SenderJID)
	}
	if !got[0].IsFromMe {
		t.Error("is_from_me = false, want true")
	}
}

func TestHandleEvent_Reaction_DoesNotTouchChat(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	msgTS := time.Unix(1_700_000_000, 0).UTC()
	ingest.HandleEvent(&events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: chat},
			ID:            "wamid.TARGET",
			Timestamp:     msgTS,
		},
		Message: &waE2E.Message{Conversation: proto.String("hello")},
	})

	// A reaction arriving much later must not bump the chat, or list_chats
	// would reorder and report the reaction as the chat's last message.
	ingest.HandleEvent(reactionEvent(chat, chat, "wamid.TARGET", "👍", false, msgTS.Add(time.Hour)))

	var lastTS int64
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT last_message_ts FROM chats WHERE jid = ?`, chat.String()).Scan(&lastTS); err != nil {
		t.Fatalf("scan chat: %v", err)
	}
	if lastTS != msgTS.Unix() {
		t.Errorf("last_message_ts = %d, want %d (unchanged by the reaction)", lastTS, msgTS.Unix())
	}
}

func TestHandleEvent_Reaction_TargetNeedNotBeCached(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	ts := time.Unix(1_700_000_100, 0).UTC()
	ingest.HandleEvent(reactionEvent(chat, chat, "wamid.NEVER-SEEN", "👍", false, ts))

	// The reaction is kept so it surfaces once the target is backfilled.
	if got := readReactions(t, store, chat.String(), "wamid.NEVER-SEEN"); len(got) != 1 {
		t.Fatalf("want the reaction kept for an uncached target, got %d rows", len(got))
	}
}

func TestHandleEvent_HistorySync_BackfillsReactions(t *testing.T) {
	ingest, store := newTestIngestor(t)

	chat := mustParseJID(t, "120363000000000000@g.us")
	participant := "1112223333@s.whatsapp.net"
	msgTS := uint64(1_700_000_000)

	ingest.HandleEvent(&events.HistorySync{Data: &waHistorySync.HistorySync{
		Conversations: []*waHistorySync.Conversation{{
			ID: proto.String(chat.String()),
			Messages: []*waHistorySync.HistorySyncMsg{{
				Message: &waWeb.WebMessageInfo{
					Key: &waCommon.MessageKey{
						ID:          proto.String("wamid.OLD"),
						RemoteJID:   proto.String(chat.String()),
						FromMe:      proto.Bool(false),
						Participant: proto.String(participant),
					},
					MessageTimestamp: proto.Uint64(msgTS),
					Message:          &waE2E.Message{Conversation: proto.String("old message")},
					Reactions: []*waWeb.Reaction{{
						Key: &waCommon.MessageKey{
							ID:          proto.String("wamid.OLD-REACT"),
							Participant: proto.String(participant),
							FromMe:      proto.Bool(false),
						},
						Text:              proto.String("🎉"),
						SenderTimestampMS: proto.Int64(int64(msgTS) * 1000),
					}},
				},
			}},
		}},
	}})

	got := readReactions(t, store, chat.String(), "wamid.OLD")
	if len(got) != 1 {
		t.Fatalf("want 1 backfilled reaction, got %d", len(got))
	}
	if got[0].Emoji != "🎉" {
		t.Errorf("emoji = %q, want 🎉", got[0].Emoji)
	}
	if got[0].SenderJID != participant {
		t.Errorf("sender = %q, want %q", got[0].SenderJID, participant)
	}
}
