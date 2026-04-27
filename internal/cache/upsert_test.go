package cache

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestUpsertChat_MergesAndKeepsLatestTimestamp(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	earlier := time.Unix(1_700_000_000, 0)
	later := time.Unix(1_700_001_000, 0)

	if err := store.UpsertChat(ctx, Chat{JID: "g1@g.us", Name: "Team", IsGroup: true, LastMessageTS: later}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Second upsert has an empty name and an older timestamp — neither should win.
	if err := store.UpsertChat(ctx, Chat{JID: "g1@g.us", Name: "", IsGroup: true, LastMessageTS: earlier, UnreadCount: 3}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	var (
		name        string
		isGroup     int
		lastTS      int64
		unreadCount int
		chatType    string
	)
	row := store.DB().QueryRowContext(ctx, `SELECT name, is_group, last_message_ts, unread_count, chat_type FROM chats WHERE jid = ?`, "g1@g.us")
	if err := row.Scan(&name, &isGroup, &lastTS, &unreadCount, &chatType); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if name != "Team" {
		t.Fatalf("name = %q, want %q (empty upsert must not clobber)", name, "Team")
	}
	if isGroup != 1 {
		t.Fatalf("is_group = %d, want 1", isGroup)
	}
	if lastTS != later.Unix() {
		t.Fatalf("last_message_ts = %d, want %d (MAX must preserve later)", lastTS, later.Unix())
	}
	if unreadCount != 3 {
		t.Fatalf("unread_count = %d, want 3", unreadCount)
	}
	if chatType != "group" {
		t.Fatalf("chat_type = %q, want group (derived from is_group)", chatType)
	}
}

// A subsequent upsert that does not set Type must not regress a previously
// classified type. Mirrors the empty-name preservation rule.
func TestUpsertChat_TypeNotRegressedByPartialUpdate(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	jid := "120363999000000001@newsletter"
	if err := store.UpsertChat(ctx, Chat{JID: jid, Name: "Brief", Type: ChatTypeNewsletter}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Second upsert leaves Type empty + flips IsGroup true. Without the
	// CASE-preserve clause this would regress chat_type to 'group'.
	if err := store.UpsertChat(ctx, Chat{JID: jid, Name: "", IsGroup: true}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	var chatType string
	if err := store.DB().QueryRowContext(ctx, `SELECT chat_type FROM chats WHERE jid = ?`, jid).Scan(&chatType); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if chatType != "newsletter" {
		t.Fatalf("chat_type = %q, want newsletter (must not regress)", chatType)
	}
}

func TestUpsertContact_EmptyFieldsDoNotOverwrite(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.UpsertContact(ctx, Contact{JID: "u@s.whatsapp.net", PushName: "Alice", FullName: "Alice Example"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Subsequent event only knows business name; PushName/FullName must survive.
	if err := store.UpsertContact(ctx, Contact{JID: "u@s.whatsapp.net", BusinessName: "Alice Co"}); err != nil {
		t.Fatalf("second: %v", err)
	}

	var push, full, biz string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT push_name, full_name, business_name FROM contacts WHERE jid = ?`, "u@s.whatsapp.net").
		Scan(&push, &full, &biz); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if push != "Alice" || full != "Alice Example" || biz != "Alice Co" {
		t.Fatalf("got push=%q full=%q biz=%q", push, full, biz)
	}
}

func TestInsertMessage_IgnoresReplayAndKeepsEditDeletedFlags(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	ts := time.Unix(1_700_000_500, 0)
	if err := store.InsertMessage(ctx, Message{
		ID: "wamid.1", ChatJID: "c@s.whatsapp.net", SenderJID: "c@s.whatsapp.net",
		Timestamp: ts, Kind: KindText, Body: "original",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.MarkMessageEdited(ctx, "c@s.whatsapp.net", "wamid.1", "edited body", ts.Add(time.Minute)); err != nil {
		t.Fatalf("edit: %v", err)
	}
	// History sync arrives later and tries to reinsert the same id — must not overwrite the edit.
	if err := store.InsertMessage(ctx, Message{
		ID: "wamid.1", ChatJID: "c@s.whatsapp.net", SenderJID: "c@s.whatsapp.net",
		Timestamp: ts, Kind: KindText, Body: "original",
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}

	var body string
	var edited int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT body, edited FROM messages WHERE chat_jid = ? AND id = ?`,
		"c@s.whatsapp.net", "wamid.1").Scan(&body, &edited); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if body != "edited body" {
		t.Fatalf("body = %q, want %q (replay must not clobber edit)", body, "edited body")
	}
	if edited != 1 {
		t.Fatalf("edited flag lost on replay")
	}
}

func TestMarkMessageDeleted_PreservesRowAndClearsBody(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.InsertMessage(ctx, Message{
		ID: "m1", ChatJID: "c@s", Timestamp: time.Unix(1, 0), Body: "secret",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.MarkMessageDeleted(ctx, "c@s", "m1", time.Unix(2, 0)); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var body string
	var deleted int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT body, deleted FROM messages WHERE chat_jid = ? AND id = ?`, "c@s", "m1").
		Scan(&body, &deleted); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if body != "" || deleted != 1 {
		t.Fatalf("after delete: body=%q deleted=%d", body, deleted)
	}
}

func TestMarkEditedOrDeleted_MissingRowReturnsErrNoRows(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	err := store.MarkMessageEdited(ctx, "c@s", "missing", "new", time.Unix(1, 0))
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("MarkMessageEdited missing: got %v, want sql.ErrNoRows", err)
	}
	err = store.MarkMessageDeleted(ctx, "c@s", "missing", time.Unix(1, 0))
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("MarkMessageDeleted missing: got %v, want sql.ErrNoRows", err)
	}
}

func TestUpsertNickname_RoundTripAndDelete(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.UpsertNickname(ctx, Nickname{JID: "u@s", Nickname: "Boss", Note: "team-lead"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var nick, note string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT nickname, note FROM nicknames WHERE jid = ?`, "u@s").Scan(&nick, &note); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if nick != "Boss" || note != "team-lead" {
		t.Fatalf("got nick=%q note=%q", nick, note)
	}
	if err := store.DeleteNickname(ctx, "u@s"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM nicknames WHERE jid = ?`, "u@s").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("nickname not deleted, count = %d", count)
	}
}
