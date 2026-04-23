package cache

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
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

	var kind, body, mime, url string
	var length int64
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT kind, body, media_mime, media_url, media_length FROM messages WHERE chat_jid = ? AND id = ?`,
		chat.String(), "wamid.IMG1").Scan(&kind, &body, &mime, &url, &length); err != nil {
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
