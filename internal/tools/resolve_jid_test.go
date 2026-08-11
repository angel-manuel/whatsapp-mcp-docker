package tools_test

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

// seedIdentities builds the fixture every resolve_jid test reads from:
// the shared contact rows plus a lid<->phone alias for Alice, a group
// chat with a learned name, and a newsletter chat.
//
// 999@lid is deliberately unaliased — it is the case that used to make
// get_contact_details report "999" as a phone number.
func seedIdentities(s *cache.Store) {
	ctx := context.Background()
	seedContacts(s)
	_ = s.UpsertJIDAlias(ctx, "888@lid", "111@s.whatsapp.net")
	_ = s.UpsertChat(ctx, cache.Chat{
		JID: "chatid@g.us", Name: "Weekend Plans", IsGroup: true, Type: cache.ChatTypeGroup,
	})
	_ = s.UpsertChat(ctx, cache.Chat{
		JID: "12345@newsletter", Name: "Daily Digest", Type: cache.ChatTypeNewsletter,
	})
}

// resolved calls resolve_jid and returns the structured payload, failing
// the test if the tool reported an error.
func resolved(t *testing.T, h *testHarness, jid string) map[string]any {
	t.Helper()
	res := callTool(t, h, "resolve_jid", map[string]any{"jid": jid})
	if res.IsError {
		t.Fatalf("resolve_jid(%q) tool error: %+v", jid, res)
	}
	return structured(t, res)
}

func assertField(t *testing.T, s map[string]any, field string, want any) {
	t.Helper()
	if got := s[field]; got != want {
		t.Errorf("%s=%v, want %v", field, got, want)
	}
}

func TestResolveJID_LIDWithKnownAliasResolvesToPhoneIdentity(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, seedIdentities, nil)

	s := resolved(t, h, "888@lid")
	assertField(t, s, "jid", "888@lid")
	assertField(t, s, "canonical_jid", "111@s.whatsapp.net")
	assertField(t, s, "kind", "user")
	assertField(t, s, "name", "Ali") // nickname shadows the cascade
	assertField(t, s, "phone", "+111")
}

func TestResolveJID_LIDWithoutAliasNeverReportsLIDDigitsAsPhone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, seedIdentities, nil)

	s := resolved(t, h, "999@lid")
	assertField(t, s, "jid", "999@lid")
	// No alias: the LID stays canonical and there is nothing to name it by.
	assertField(t, s, "canonical_jid", "999@lid")
	assertField(t, s, "kind", "user")
	assertField(t, s, "name", "")
	if got := s["phone"]; got != "" {
		t.Errorf("phone=%v, want empty (LID digits are not a phone number)", got)
	}
}

func TestResolveJID_PhoneJIDReadsContactDirectly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, seedIdentities, nil)

	s := resolved(t, h, "222@s.whatsapp.net")
	assertField(t, s, "jid", "222@s.whatsapp.net")
	assertField(t, s, "canonical_jid", "222@s.whatsapp.net")
	assertField(t, s, "kind", "user")
	assertField(t, s, "name", "Bob Builder")
	assertField(t, s, "phone", "+222")
}

// A device-suffixed JID canonicalises to its non-AD form, so an approval
// prompt renders the same identity however the sender addressed it.
func TestResolveJID_StripsDeviceSuffix(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, seedIdentities, nil)

	s := resolved(t, h, "222.0:3@s.whatsapp.net")
	assertField(t, s, "jid", "222@s.whatsapp.net")
	assertField(t, s, "name", "Bob Builder")
}

// Bare digits are what send_message accepts as a recipient, so resolve_jid
// must answer for them too.
func TestResolveJID_BarePhoneNumberIsTreatedAsPhoneJID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, seedIdentities, nil)

	s := resolved(t, h, "222")
	assertField(t, s, "jid", "222@s.whatsapp.net")
	assertField(t, s, "kind", "user")
	assertField(t, s, "name", "Bob Builder")
	assertField(t, s, "phone", "+222")
}

func TestResolveJID_GroupUsesCachedChatNameWithoutLiveCall(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, seedIdentities, mock)

	s := resolved(t, h, "chatid@g.us")
	assertField(t, s, "jid", "chatid@g.us")
	assertField(t, s, "canonical_jid", "chatid@g.us")
	assertField(t, s, "kind", "group")
	assertField(t, s, "name", "Weekend Plans")
	assertField(t, s, "phone", "")
	if mock.groupInfoCall != 0 {
		t.Errorf("GroupInfo calls=%d, want 0 (cache answered)", mock.groupInfoCall)
	}
}

func TestResolveJID_GroupFallsBackToGroupInfoWhenCacheHasNoName(t *testing.T) {
	t.Parallel()
	groupJID := types.NewJID("unknowngroup", types.GroupServer)
	mock := &mockWA{
		groupInfo: map[string]*types.GroupInfo{
			groupJID.String(): {JID: groupJID, GroupName: types.GroupName{Name: "Book Club"}},
		},
	}
	h := newHarness(t, true, seedIdentities, mock)

	s := resolved(t, h, groupJID.String())
	assertField(t, s, "kind", "group")
	assertField(t, s, "name", "Book Club")
	assertField(t, s, "phone", "")
}

func TestResolveJID_NewsletterNamedFromCachedChat(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, seedIdentities, nil)

	s := resolved(t, h, "12345@newsletter")
	assertField(t, s, "jid", "12345@newsletter")
	assertField(t, s, "kind", "newsletter")
	assertField(t, s, "name", "Daily Digest")
	assertField(t, s, "phone", "")
}

// An entirely unknown JID is a successful, empty answer — not an error.
// Callers render approval prompts with it and must always get a payload.
func TestResolveJID_UnknownJIDReturnsEmptySuccess(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, seedIdentities, nil)

	s := resolved(t, h, "404@s.whatsapp.net")
	assertField(t, s, "jid", "404@s.whatsapp.net")
	assertField(t, s, "canonical_jid", "404@s.whatsapp.net")
	assertField(t, s, "kind", "user")
	assertField(t, s, "name", "")
	assertField(t, s, "phone", "+404")

	// A group nobody has heard of resolves the same way.
	s = resolved(t, h, "nosuch@g.us")
	assertField(t, s, "kind", "group")
	assertField(t, s, "name", "")
	assertField(t, s, "phone", "")
}

// Servers outside the user / group / newsletter families are reported as
// "unknown" rather than being forced into a category.
func TestResolveJID_UnrecognisedServerIsKindUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, seedIdentities, nil)

	s := resolved(t, h, "1234@broadcast")
	assertField(t, s, "kind", "unknown")
	assertField(t, s, "name", "")
	assertField(t, s, "phone", "")
}

func TestResolveJID_RejectsInvalidJID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, seedIdentities, nil)

	res := callTool(t, h, "resolve_jid", map[string]any{"jid": "   "})
	expectError(t, res, mcp.ErrInvalidArgument)

	// A JID with no user part names nobody.
	res = callTool(t, h, "resolve_jid", map[string]any{"jid": "@s.whatsapp.net"})
	expectError(t, res, mcp.ErrInvalidArgument)
}
