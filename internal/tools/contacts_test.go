package tools_test

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
)

// get_contact_details used to look the raw @lid up in the contacts table
// (which is keyed on the phone JID) and miss the row entirely. It must
// follow jid_aliases instead.
func TestGetContactDetails_LIDFollowsAliasToCachedContact(t *testing.T) {
	t.Parallel()
	lid := types.NewJID("888", types.HiddenUserServer)
	mock := &mockWA{
		userInfo: map[types.JID]types.UserInfo{lid: {Status: "Available"}},
	}
	h := newHarness(t, true, seedIdentities, mock)

	res := callTool(t, h, "get_contact_details", map[string]any{"jid": lid.String()})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	assertField(t, s, "jid", "888@lid")
	assertField(t, s, "full_name", "Alice Anderson")
	assertField(t, s, "nickname", "Ali")
	assertField(t, s, "name", "Ali")
	// The phone number comes from the aliased …@s.whatsapp.net JID, never
	// from the LID's own digits.
	assertField(t, s, "phone", "111")
	if got, _ := s["is_on_whatsapp"].(bool); !got {
		t.Errorf("is_on_whatsapp=%v, want true", got)
	}
}

// The bug this guards: a @lid with no recorded alias reported its LID
// digits as a phone number, which is confidently wrong.
func TestGetContactDetails_LIDWithoutAliasLeavesPhoneEmpty(t *testing.T) {
	t.Parallel()
	lid := types.NewJID("999", types.HiddenUserServer)
	mock := &mockWA{
		// USync knows the JID exists, so the handler gets past the
		// not-found gate and we can inspect the phone field.
		userInfo: map[types.JID]types.UserInfo{lid: {Status: "hi"}},
	}
	h := newHarness(t, true, seedIdentities, mock)

	res := callTool(t, h, "get_contact_details", map[string]any{"jid": lid.String()})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	assertField(t, s, "jid", "999@lid")
	if got := s["phone"]; got != "" {
		t.Errorf("phone=%v, want empty (999 is a LID, not a phone number)", got)
	}
	assertField(t, s, "name", "")
}

// A LID with no alias must not be handed to IsOnWhatsApp as if its digits
// were a phone number — that lookup could match an unrelated person.
func TestGetContactDetails_LIDWithoutAliasSkipsPhoneRegistrationCheck(t *testing.T) {
	t.Parallel()
	mock := &mockWA{
		isOnWhatsApp: []types.IsOnWhatsAppResponse{{IsIn: true}},
	}
	h := newHarness(t, true, seedIdentities, mock)

	res := callTool(t, h, "get_contact_details", map[string]any{"jid": "999@lid"})
	if !res.IsError {
		t.Fatalf("expected not_found: the LID is unknown to cache and USync, got %+v", res)
	}
}

// name is derived from the same cascade ContactView exposes, so callers do
// not have to re-derive it.
func TestGetContactDetails_ExposesDisplayName(t *testing.T) {
	t.Parallel()
	target := types.NewJID("222", types.DefaultUserServer)
	mock := &mockWA{
		userInfo: map[types.JID]types.UserInfo{target: {}},
	}
	h := newHarness(t, true, seedContacts, mock)

	res := callTool(t, h, "get_contact_details", map[string]any{"jid": target.String()})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	assertField(t, s, "name", "Bob Builder")
	assertField(t, s, "phone", "222")
}
