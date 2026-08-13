package tools_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// Tests for the account-mutating tools: set_status_message, send_presence,
// send_chat_presence, subscribe_presence, set_disappearing_timer,
// set_default_disappearing_timer and mark_read. Every one of these is visible
// to other WhatsApp users, so the assertions are mostly about what does NOT
// reach whatsmeow.

// --- set_status_message ---------------------------------------------------

func TestSetStatusMessage_ForwardsText(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "set_status_message", map[string]any{"text": "Out for lunch 🍜"})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	assertField(t, structured(t, res), "text", "Out for lunch 🍜")
	if mock.statusMsg != "Out for lunch 🍜" {
		t.Errorf("forwarded %q, want %q", mock.statusMsg, "Out for lunch 🍜")
	}
}

// An empty string is a legitimate request: it clears the About line.
func TestSetStatusMessage_EmptyStringClears(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "set_status_message", map[string]any{"text": ""})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	if mock.statusMsgCalls != 1 || mock.statusMsg != "" {
		t.Errorf("calls=%d text=%q, want 1 call with an empty string", mock.statusMsgCalls, mock.statusMsg)
	}
}

// The server truncates past 139 characters instead of failing, which would
// leave the caller believing it set something it did not.
func TestSetStatusMessage_RejectsOverlongText(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "set_status_message", map[string]any{"text": strings.Repeat("a", 140)})
	expectError(t, res, mcp.ErrInvalidArgument)
	if mock.statusMsgCalls != 0 {
		t.Errorf("statusMsgCalls=%d, want 0 — the call must not reach whatsmeow", mock.statusMsgCalls)
	}
}

// The limit is in characters, not bytes: 139 multi-byte runes must pass.
func TestSetStatusMessage_CountsRunesNotBytes(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "set_status_message", map[string]any{"text": strings.Repeat("é", 139)})
	if res.IsError {
		t.Fatalf("139 runes must be accepted, got %+v", res)
	}
}

func TestSetStatusMessage_NotLoggedInMapsToNotPaired(t *testing.T) {
	t.Parallel()
	mock := &mockWA{statusMsgErr: wa.ErrNotLoggedIn}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "set_status_message", map[string]any{"text": "hi"})
	expectError(t, res, mcp.ErrNotPaired)
}

// --- send_presence --------------------------------------------------------

func TestSendPresence_ForwardsEnum(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want types.Presence
	}{
		{"available", types.PresenceAvailable},
		{"unavailable", types.PresenceUnavailable},
	} {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			mock := &mockWA{}
			h := newHarness(t, true, nil, mock)

			res := callTool(t, h, "send_presence", map[string]any{"presence": tc.in})
			if res.IsError {
				t.Fatalf("tool error: %+v", res)
			}
			assertField(t, structured(t, res), "presence", tc.in)
			if mock.presence != tc.want {
				t.Errorf("forwarded %q, want %q", mock.presence, tc.want)
			}
		})
	}
}

// The JSONSchema advertises an enum but mcp-go does not enforce it, so the
// handler is the only real gate.
func TestSendPresence_RejectsUnknownState(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "send_presence", map[string]any{"presence": "online"})
	expectError(t, res, mcp.ErrInvalidArgument)
	if mock.presenceCalls != 0 {
		t.Errorf("presenceCalls=%d, want 0", mock.presenceCalls)
	}
}

// A paired-but-not-yet-connected client has no push name; the caller should
// get an actionable message rather than a bare whatsmeow error.
func TestSendPresence_NoPushNameIsInternalWithGuidance(t *testing.T) {
	t.Parallel()
	mock := &mockWA{presenceErr: whatsmeow.ErrNoPushName}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "send_presence", map[string]any{"presence": "available"})
	s := expectError(t, res, mcp.ErrInternal)
	if msg, _ := s["message"].(string); !strings.Contains(msg, "push name") {
		t.Errorf("message=%q, want it to mention the push name", msg)
	}
}

// --- send_chat_presence ---------------------------------------------------

func TestSendChatPresence_ComposingWithAudio(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "send_chat_presence", map[string]any{
		"chat_jid": "111@s.whatsapp.net",
		"state":    "composing",
		"media":    "audio",
	})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	assertField(t, s, "chat_jid", "111@s.whatsapp.net")
	assertField(t, s, "state", "composing")
	assertField(t, s, "media", "audio")
	if mock.chatPresenceState != types.ChatPresenceComposing || mock.chatPresenceMedia != types.ChatPresenceMediaAudio {
		t.Errorf("forwarded state=%q media=%q", mock.chatPresenceState, mock.chatPresenceMedia)
	}
}

// Omitting media is the same as asking for the text indicator, which
// whatsmeow spells as the empty string.
func TestSendChatPresence_DefaultsToTextMedia(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "send_chat_presence", map[string]any{
		"chat_jid": "120363000000000001@g.us",
		"state":    "paused",
	})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	assertField(t, structured(t, res), "media", "text")
	if mock.chatPresenceMedia != types.ChatPresenceMediaText {
		t.Errorf("media=%q, want the empty (text) value", mock.chatPresenceMedia)
	}
}

// whatsmeow drops the media attribute unless the state is composing —
// silently honouring half the request would be worse than refusing it.
func TestSendChatPresence_RejectsAudioWithPaused(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "send_chat_presence", map[string]any{
		"chat_jid": "111@s.whatsapp.net",
		"state":    "paused",
		"media":    "audio",
	})
	expectError(t, res, mcp.ErrInvalidArgument)
	if mock.chatPresenceCalls != 0 {
		t.Errorf("chatPresenceCalls=%d, want 0", mock.chatPresenceCalls)
	}
}

func TestSendChatPresence_RejectsNewsletterChat(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "send_chat_presence", map[string]any{
		"chat_jid": "120363000000000009@newsletter",
		"state":    "composing",
	})
	expectError(t, res, mcp.ErrInvalidArgument)
	if mock.chatPresenceCalls != 0 {
		t.Errorf("chatPresenceCalls=%d, want 0", mock.chatPresenceCalls)
	}
}

// The recipient grammar matches send_message, so a bare phone number works.
func TestSendChatPresence_AcceptsBarePhoneNumber(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "send_chat_presence", map[string]any{
		"chat_jid": "+34 600 111 222",
		"state":    "composing",
	})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	if got := mock.chatPresenceJID.String(); got != "34600111222@s.whatsapp.net" {
		t.Errorf("chat jid=%q, want 34600111222@s.whatsapp.net", got)
	}
}

// --- subscribe_presence ---------------------------------------------------

func TestSubscribePresence_ForwardsUserJID(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "subscribe_presence", map[string]any{"jid": "111@s.whatsapp.net"})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	assertField(t, structured(t, res), "jid", "111@s.whatsapp.net")
	if got := mock.subscribedJID.String(); got != "111@s.whatsapp.net" {
		t.Errorf("subscribed to %q", got)
	}
}

func TestSubscribePresence_RejectsGroupJID(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "subscribe_presence", map[string]any{"jid": "120363000000000001@g.us"})
	expectError(t, res, mcp.ErrInvalidArgument)
	if mock.subscribeCalls != 0 {
		t.Errorf("subscribeCalls=%d, want 0 — groups do not publish presence", mock.subscribeCalls)
	}
}

// No privacy token means the account has never interacted with the target;
// that is the caller's problem to fix, not an internal failure.
func TestSubscribePresence_NoPrivacyTokenIsInvalidArgument(t *testing.T) {
	t.Parallel()
	mock := &mockWA{subscribeErr: whatsmeow.ErrNoPrivacyToken}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "subscribe_presence", map[string]any{"jid": "111@s.whatsapp.net"})
	expectError(t, res, mcp.ErrInvalidArgument)
}

// --- set_disappearing_timer -----------------------------------------------

func TestSetDisappearingTimer_MapsEveryAllowedDuration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"off", 0},
		{"24h", 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"90d", 90 * 24 * time.Hour},
	} {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			mock := &mockWA{}
			h := newHarness(t, true, nil, mock)

			res := callTool(t, h, "set_disappearing_timer", map[string]any{
				"chat_jid": "111@s.whatsapp.net",
				"duration": tc.in,
			})
			if res.IsError {
				t.Fatalf("tool error: %+v", res)
			}
			s := structured(t, res)
			assertField(t, s, "duration", tc.in)
			if got, _ := s["duration_seconds"].(float64); int64(got) != int64(tc.want.Seconds()) {
				t.Errorf("duration_seconds=%v, want %v", got, tc.want.Seconds())
			}
			if mock.timer != tc.want {
				t.Errorf("forwarded %v, want %v", mock.timer, tc.want)
			}
		})
	}
}

// WhatsApp honours exactly four timers. Anything else is either ignored by
// official clients or rejected by the server, so it must not be forwarded.
func TestSetDisappearingTimer_RejectsUnsupportedDuration(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"1h", "30d", "0", "forever", ""} {
		mock := &mockWA{}
		h := newHarness(t, true, nil, mock)

		res := callTool(t, h, "set_disappearing_timer", map[string]any{
			"chat_jid": "111@s.whatsapp.net",
			"duration": in,
		})
		expectError(t, res, mcp.ErrInvalidArgument)
		if mock.timerCalls != 0 {
			t.Errorf("duration %q: timerCalls=%d, want 0", in, mock.timerCalls)
		}
	}
}

func TestSetDisappearingTimer_RejectsNewsletterChat(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "set_disappearing_timer", map[string]any{
		"chat_jid": "120363000000000009@newsletter",
		"duration": "24h",
	})
	expectError(t, res, mcp.ErrInvalidArgument)
	if mock.timerCalls != 0 {
		t.Errorf("timerCalls=%d, want 0", mock.timerCalls)
	}
}

// A group whose server rejects the timer is a caller-visible bad request,
// not an internal error.
func TestSetDisappearingTimer_ServerRejectionIsInvalidArgument(t *testing.T) {
	t.Parallel()
	mock := &mockWA{timerErr: whatsmeow.ErrInvalidDisappearingTimer}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "set_disappearing_timer", map[string]any{
		"chat_jid": "120363000000000001@g.us",
		"duration": "90d",
	})
	expectError(t, res, mcp.ErrInvalidArgument)
}

// --- set_default_disappearing_timer ---------------------------------------

func TestSetDefaultDisappearingTimer_Forwards(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "set_default_disappearing_timer", map[string]any{"duration": "7d"})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	assertField(t, structured(t, res), "duration", "7d")
	if mock.defaultTimer != 7*24*time.Hour {
		t.Errorf("forwarded %v, want 168h", mock.defaultTimer)
	}
}

func TestSetDefaultDisappearingTimer_RejectsUnsupportedDuration(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "set_default_disappearing_timer", map[string]any{"duration": "1week"})
	expectError(t, res, mcp.ErrInvalidArgument)
	if mock.defaultTimerCalls != 0 {
		t.Errorf("defaultTimerCalls=%d, want 0", mock.defaultTimerCalls)
	}
}

// --- mark_read ------------------------------------------------------------

// seedUnreadChats leaves one DM and one group flagged unread so the tests can
// watch mark_read clear the flag the MarkChatAsRead ingest path sets.
func seedUnreadChats(s *cache.Store) {
	ctx := context.Background()
	_ = s.SetChatUnread(ctx, "111@s.whatsapp.net", false, true)
	_ = s.SetChatUnread(ctx, "120363000000000001@g.us", true, true)
}

func unreadCount(t *testing.T, h *testHarness, jid string) int {
	t.Helper()
	var n int
	if err := h.store.DB().QueryRowContext(context.Background(),
		`SELECT unread_count FROM chats WHERE jid = ?`, jid).Scan(&n); err != nil {
		t.Fatalf("scan unread_count for %s: %v", jid, err)
	}
	return n
}

func TestMarkRead_SendsReceiptAndClearsCachedUnread(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, seedUnreadChats, mock)

	if got := unreadCount(t, h, "111@s.whatsapp.net"); got != 1 {
		t.Fatalf("precondition: unread_count=%d, want 1", got)
	}

	res := callTool(t, h, "mark_read", map[string]any{
		"chat_jid":    "111@s.whatsapp.net",
		"message_ids": []any{"MSG1", "MSG2"},
	})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	assertField(t, s, "chat_jid", "111@s.whatsapp.net")
	if got, _ := s["count"].(float64); int(got) != 2 {
		t.Errorf("count=%v, want 2", got)
	}
	if got, _ := s["read_ts"].(float64); int64(got) == 0 {
		t.Error("read_ts=0, want the receipt timestamp")
	}
	if len(mock.markReadIDs) != 2 || mock.markReadIDs[0] != "MSG1" || mock.markReadIDs[1] != "MSG2" {
		t.Errorf("forwarded ids=%v, want [MSG1 MSG2] in order", mock.markReadIDs)
	}
	if !mock.markReadSender.IsEmpty() {
		t.Errorf("sender=%q, want empty in a direct chat", mock.markReadSender)
	}
	if got := unreadCount(t, h, "111@s.whatsapp.net"); got != 0 {
		t.Errorf("unread_count=%d after mark_read, want 0", got)
	}
}

func TestMarkRead_GroupRequiresSenderJID(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, seedUnreadChats, mock)

	res := callTool(t, h, "mark_read", map[string]any{
		"chat_jid":    "120363000000000001@g.us",
		"message_ids": []any{"MSG1"},
	})
	expectError(t, res, mcp.ErrInvalidArgument)
	if mock.markReadCalls != 0 {
		t.Errorf("markReadCalls=%d, want 0", mock.markReadCalls)
	}
	if got := unreadCount(t, h, "120363000000000001@g.us"); got != 1 {
		t.Errorf("unread_count=%d, want the flag untouched after a rejected call", got)
	}
}

func TestMarkRead_GroupForwardsSenderJID(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, seedUnreadChats, mock)

	res := callTool(t, h, "mark_read", map[string]any{
		"chat_jid":    "120363000000000001@g.us",
		"message_ids": []any{"MSG1"},
		"sender_jid":  "111@s.whatsapp.net",
	})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	if got := mock.markReadSender.String(); got != "111@s.whatsapp.net" {
		t.Errorf("sender=%q, want 111@s.whatsapp.net", got)
	}
	if got := unreadCount(t, h, "120363000000000001@g.us"); got != 0 {
		t.Errorf("unread_count=%d, want 0", got)
	}
}

// A repeated id in one stanza is wasted bytes; order of first appearance is
// what whatsmeow relies on (ids[0] becomes the stanza id).
func TestMarkRead_DedupesMessageIDs(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, seedUnreadChats, mock)

	res := callTool(t, h, "mark_read", map[string]any{
		"chat_jid":    "111@s.whatsapp.net",
		"message_ids": []any{"MSG1", " MSG2 ", "MSG1"},
	})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	if len(mock.markReadIDs) != 2 || mock.markReadIDs[1] != "MSG2" {
		t.Errorf("forwarded ids=%v, want [MSG1 MSG2]", mock.markReadIDs)
	}
}

func TestMarkRead_RejectsEmptyAndBlankIDs(t *testing.T) {
	t.Parallel()
	for _, ids := range [][]any{{}, {"  "}, {"MSG1", ""}} {
		mock := &mockWA{}
		h := newHarness(t, true, seedUnreadChats, mock)

		res := callTool(t, h, "mark_read", map[string]any{
			"chat_jid":    "111@s.whatsapp.net",
			"message_ids": ids,
		})
		expectError(t, res, mcp.ErrInvalidArgument)
		if mock.markReadCalls != 0 {
			t.Errorf("ids=%v: markReadCalls=%d, want 0", ids, mock.markReadCalls)
		}
	}
}

// The receipt is irreversible once sent, so a failed send must leave the
// cached unread flag alone rather than pretending the chat was read.
func TestMarkRead_FailedReceiptLeavesUnreadFlag(t *testing.T) {
	t.Parallel()
	mock := &mockWA{markReadErr: wa.ErrNotLoggedIn}
	h := newHarness(t, true, seedUnreadChats, mock)

	res := callTool(t, h, "mark_read", map[string]any{
		"chat_jid":    "111@s.whatsapp.net",
		"message_ids": []any{"MSG1"},
	})
	expectError(t, res, mcp.ErrNotPaired)
	if got := unreadCount(t, h, "111@s.whatsapp.net"); got != 1 {
		t.Errorf("unread_count=%d, want 1 — the receipt never went out", got)
	}
}

func TestMarkRead_RejectsUnsupportedChatServer(t *testing.T) {
	t.Parallel()
	mock := &mockWA{}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "mark_read", map[string]any{
		"chat_jid":    "status@broadcast",
		"message_ids": []any{"MSG1"},
	})
	expectError(t, res, mcp.ErrInvalidArgument)
	if mock.markReadCalls != 0 {
		t.Errorf("markReadCalls=%d, want 0", mock.markReadCalls)
	}
}

// --- pairing gate ---------------------------------------------------------

// None of these tools are exempt from the not_paired middleware: they all
// mutate account state and are meaningless before the device is linked.
func TestAccountOpTools_GatedOnPairing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"set_status_message", map[string]any{"text": "hi"}},
		{"send_presence", map[string]any{"presence": "available"}},
		{"send_chat_presence", map[string]any{"chat_jid": "111@s.whatsapp.net", "state": "composing"}},
		{"subscribe_presence", map[string]any{"jid": "111@s.whatsapp.net"}},
		{"set_disappearing_timer", map[string]any{"chat_jid": "111@s.whatsapp.net", "duration": "24h"}},
		{"set_default_disappearing_timer", map[string]any{"duration": "24h"}},
		{"mark_read", map[string]any{"chat_jid": "111@s.whatsapp.net", "message_ids": []any{"MSG1"}}},
	}
	for _, tc := range cases {
		mock := &mockWA{}
		h := newHarness(t, false, nil, mock)
		res := callTool(t, h, tc.tool, tc.args)
		expectError(t, res, mcp.ErrNotPaired)
	}
}

// --- presence read-back ---------------------------------------------------

// The loop subscribe_presence opens: the events it produces are ingested
// against the contact and surface through get_contact_details.
func TestGetContactDetails_SurfacesIngestedPresence(t *testing.T) {
	t.Parallel()
	lastSeen := time.Unix(1700000000, 0).UTC()
	target := types.NewJID("111", types.DefaultUserServer)
	mock := &mockWA{userInfo: map[types.JID]types.UserInfo{target: {}}}
	h := newHarness(t, true, func(s *cache.Store) {
		seedContacts(s)
		_ = s.UpsertPresence(context.Background(), "111@s.whatsapp.net", false, lastSeen)
	}, mock)

	res := callTool(t, h, "get_contact_details", map[string]any{"jid": "111@s.whatsapp.net"})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	if observed, _ := s["presence_observed"].(bool); !observed {
		t.Fatalf("presence_observed=false, want true (payload=%+v)", s)
	}
	if online, _ := s["is_online"].(bool); online {
		t.Error("is_online=true, want false")
	}
	if got, _ := s["last_seen_ts"].(float64); int64(got) != lastSeen.Unix() {
		t.Errorf("last_seen_ts=%v, want %d", got, lastSeen.Unix())
	}
	if got, _ := s["presence_updated_ts"].(float64); int64(got) == 0 {
		t.Error("presence_updated_ts=0, want the ingest timestamp")
	}
}

// Presence only arrives for explicit subscriptions, so an ordinary contact
// reports presence_observed=false rather than a misleading "offline".
func TestGetContactDetails_UnsubscribedContactReportsNoPresence(t *testing.T) {
	t.Parallel()
	target := types.NewJID("222", types.DefaultUserServer)
	mock := &mockWA{userInfo: map[types.JID]types.UserInfo{target: {}}}
	h := newHarness(t, true, seedContacts, mock)

	res := callTool(t, h, "get_contact_details", map[string]any{"jid": "222@s.whatsapp.net"})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	if observed, _ := s["presence_observed"].(bool); observed {
		t.Error("presence_observed=true, want false")
	}
	if _, ok := s["last_seen_ts"]; ok {
		t.Error("last_seen_ts present, want it omitted when nothing was observed")
	}
}

// A @lid resolves through jid_aliases to the phone JID the presence row is
// keyed on, matching how the rest of get_contact_details reads the cache.
func TestGetContactDetails_PresenceFollowsLIDAlias(t *testing.T) {
	t.Parallel()
	lid := types.NewJID("888", types.HiddenUserServer)
	mock := &mockWA{userInfo: map[types.JID]types.UserInfo{lid: {}}}
	h := newHarness(t, true, func(s *cache.Store) {
		seedIdentities(s)
		_ = s.UpsertPresence(context.Background(), "111@s.whatsapp.net", true, time.Time{})
	}, mock)

	res := callTool(t, h, "get_contact_details", map[string]any{"jid": lid.String()})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	if online, _ := s["is_online"].(bool); !online {
		t.Errorf("is_online=false, want true (payload=%+v)", s)
	}
}
