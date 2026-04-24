package cache

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// seedContacts populates the test store with a small mix of contacts,
// one group chat (to prove group JIDs are excluded), and two nicknames
// (one for an existing contact, one that is an orphan).
func seedContacts(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	contacts := []Contact{
		{JID: "111@s.whatsapp.net", PushName: "Alice Lastname", FullName: "Alice Anderson", FirstName: "Alice"},
		{JID: "222@s.whatsapp.net", PushName: "", FullName: "Bob Builder", BusinessName: "Bob's Hardware"},
		{JID: "333@s.whatsapp.net", PushName: "Carol", FullName: "", FirstName: "Carol"},
		{JID: "444@s.whatsapp.net"}, // phone-only
	}
	for _, c := range contacts {
		if err := s.UpsertContact(ctx, c); err != nil {
			t.Fatalf("UpsertContact %s: %v", c.JID, err)
		}
	}

	// Group chat should never show up in the contact read paths.
	if err := s.UpsertChat(ctx, Chat{JID: "groupid@g.us", Name: "Weekend Plans", IsGroup: true}); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}

	// Nickname attached to Alice; and one nickname-only row with no backing contact.
	if err := s.UpsertNickname(ctx, Nickname{JID: "111@s.whatsapp.net", Nickname: "Ali"}); err != nil {
		t.Fatalf("UpsertNickname alice: %v", err)
	}
	if err := s.UpsertNickname(ctx, Nickname{JID: "999@s.whatsapp.net", Nickname: "Ghost"}); err != nil {
		t.Fatalf("UpsertNickname ghost: %v", err)
	}
}

func TestListAllContacts_ExcludesGroupsAndOrdersByDisplayName(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedContacts(t, s)

	rows, err := s.ListAllContacts(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ListAllContacts: %v", err)
	}
	// Alice full_name, Bob full_name, Carol push_name, 444 (jid fallback).
	if got, want := len(rows), 4; got != want {
		t.Fatalf("len=%d, want %d (rows=%+v)", got, want, rows)
	}
	// Sorted ascending by display name cascade. 444 has no rich display
	// fields so the JID fallback (`4...`) sorts ahead of the letters.
	want := []string{"444@s.whatsapp.net", "111@s.whatsapp.net", "222@s.whatsapp.net", "333@s.whatsapp.net"}
	for i, r := range rows {
		if r.JID != want[i] {
			t.Errorf("rows[%d].JID=%s, want %s", i, r.JID, want[i])
		}
	}
	// Alice's row carries her locally-set nickname.
	var alice ContactRow
	for _, r := range rows {
		if r.JID == "111@s.whatsapp.net" {
			alice = r
		}
	}
	if alice.Nickname != "Ali" {
		t.Errorf("alice nickname=%q, want Ali", alice.Nickname)
	}
}

func TestListAllContacts_Pagination(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedContacts(t, s)

	page0, err := s.ListAllContacts(context.Background(), 2, 0)
	if err != nil {
		t.Fatalf("ListAllContacts page0: %v", err)
	}
	page1, err := s.ListAllContacts(context.Background(), 2, 1)
	if err != nil {
		t.Fatalf("ListAllContacts page1: %v", err)
	}

	if len(page0) != 2 || len(page1) != 2 {
		t.Fatalf("page sizes = %d/%d, want 2/2", len(page0), len(page1))
	}
	if page0[0].JID == page1[0].JID {
		t.Errorf("page 0 and page 1 overlap: %s", page0[0].JID)
	}
}

func TestSearchContacts_MatchesAcrossFields(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedContacts(t, s)

	cases := []struct {
		name    string
		query   string
		wantJID string
	}{
		{"push name", "carol", "333@s.whatsapp.net"},
		{"full name", "anderson", "111@s.whatsapp.net"},
		{"business", "hardware", "222@s.whatsapp.net"},
		{"nickname", "ali", "111@s.whatsapp.net"},
		{"phone jid", "444", "444@s.whatsapp.net"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.SearchContacts(context.Background(), tc.query, 0, 0)
			if err != nil {
				t.Fatalf("SearchContacts: %v", err)
			}
			var found bool
			for _, r := range rows {
				if r.JID == tc.wantJID {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("search %q did not return %s (got=%+v)", tc.query, tc.wantJID, rows)
			}
		})
	}
}

func TestSearchContacts_ExcludesGroups(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedContacts(t, s)

	// Group chats have no contact row, so they wouldn't appear anyway;
	// also upsert a contact with a @g.us JID to prove the filter holds.
	if err := s.UpsertContact(context.Background(), Contact{JID: "xxx@g.us", PushName: "Leaky Group"}); err != nil {
		t.Fatalf("UpsertContact group: %v", err)
	}
	rows, err := s.SearchContacts(context.Background(), "leaky", 0, 0)
	if err != nil {
		t.Fatalf("SearchContacts: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected group-JID excluded, got %+v", rows)
	}
}

func TestGetContactByJID_NotFoundReturnsErrNoRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.GetContactByJID(context.Background(), "missing@s.whatsapp.net")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err=%v, want sql.ErrNoRows", err)
	}
}

func TestGetContactByJID_MergesNickname(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedContacts(t, s)

	c, err := s.GetContactByJID(context.Background(), "111@s.whatsapp.net")
	if err != nil {
		t.Fatalf("GetContactByJID: %v", err)
	}
	if c.Nickname != "Ali" {
		t.Errorf("nickname=%q, want Ali", c.Nickname)
	}
	if c.FullName != "Alice Anderson" {
		t.Errorf("full_name=%q, want Alice Anderson", c.FullName)
	}
}

func TestGetNicknameByJID_OrphanNickname(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedContacts(t, s)

	nick, err := s.GetNicknameByJID(context.Background(), "999@s.whatsapp.net")
	if err != nil {
		t.Fatalf("GetNicknameByJID: %v", err)
	}
	if nick != "Ghost" {
		t.Errorf("nickname=%q, want Ghost", nick)
	}

	// missing nickname → "", nil
	nick2, err := s.GetNicknameByJID(context.Background(), "no-such@s.whatsapp.net")
	if err != nil {
		t.Fatalf("GetNicknameByJID missing: %v", err)
	}
	if nick2 != "" {
		t.Errorf("expected empty nickname, got %q", nick2)
	}
}

func TestContactRow_Phone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		jid  string
		want string
	}{
		{"1234@s.whatsapp.net", "1234"},
		{"abcd@lid", "abcd"},
		{"noserver", "noserver"},
	}
	for _, tc := range cases {
		if got := (ContactRow{JID: tc.jid}).Phone(); got != tc.want {
			t.Errorf("Phone(%q)=%q, want %q", tc.jid, got, tc.want)
		}
	}
}
