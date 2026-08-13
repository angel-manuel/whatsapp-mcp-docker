package tools_test

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

const (
	pollChatJID  = "1234567890@s.whatsapp.net"
	pollGroupJID = "1234567890-1600000000@g.us"
	pollOwnJID   = "9990001111@s.whatsapp.net"
)

// pollMock is a mockWA primed for the poll tools: paired, with an own JID and
// a send response carrying an id, since every poll tool keys its cache writes
// off those two.
func pollMock(sendID string) *mockWA {
	// A device (AD) JID, matching what a paired client reports: the tools have
	// to strip the device suffix before it reaches the cache or the wire.
	own, _ := types.ParseJID("9990001111.0:3@s.whatsapp.net")
	return &mockWA{
		ownJID:   own,
		loggedIn: true,
		sendResp: whatsmeow.SendResponse{
			ID:        types.MessageID(sendID),
			Timestamp: time.Unix(1_700_000_000, 0).UTC(),
		},
	}
}

// seedPoll stores a poll as if it had been ingested from an incoming event.
func seedPoll(chatJID, messageID, sender string, selectable int, options ...string) func(*cache.Store) {
	return func(s *cache.Store) {
		ctx := context.Background()
		p := cache.Poll{
			ChatJID:         chatJID,
			MessageID:       messageID,
			Question:        "Lunch?",
			SelectableCount: selectable,
			SenderJID:       sender,
			Timestamp:       time.Unix(1_700_000_000, 0).UTC(),
		}
		for _, o := range options {
			p.Options = append(p.Options, cache.PollOption{Name: o})
		}
		_ = s.UpsertPoll(ctx, p)
	}
}

func TestSendPoll_SendsStoresSecretAndMirrorsBallot(t *testing.T) {
	t.Parallel()
	mock := pollMock("wamid.POLL1")
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "send_poll", map[string]any{
		"recipient":        "1234567890",
		"question":         "Lunch?",
		"options":          []any{"Pizza", "Sushi"},
		"selectable_count": 1,
	})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	if got := s["message_id"]; got != "wamid.POLL1" {
		t.Errorf("message_id = %v", got)
	}
	if got := s["chat_jid"]; got != pollChatJID {
		t.Errorf("chat_jid = %v, want %s", got, pollChatJID)
	}

	if mock.sendCalls != 1 {
		t.Fatalf("sendCalls = %d, want 1", mock.sendCalls)
	}
	poll := mock.lastSendMs.GetPollCreationMessage()
	if poll == nil {
		t.Fatalf("sent message is not a poll creation: %+v", mock.lastSendMs)
	}
	if poll.GetName() != "Lunch?" || len(poll.GetOptions()) != 2 {
		t.Errorf("sent poll = %+v", poll)
	}

	// Without the stored secret, votes on this poll could never be decrypted,
	// so the tool must persist it against the recipient chat and our own JID.
	if len(mock.secrets) != 1 {
		t.Fatalf("StoreMessageSecret calls = %d, want 1", len(mock.secrets))
	}
	sec := mock.secrets[0]
	if sec.chat.String() != pollChatJID || sec.id != "wamid.POLL1" || len(sec.secret) == 0 {
		t.Errorf("stored secret = %+v", sec)
	}
	if sec.sender.ToNonAD().String() != pollOwnJID {
		t.Errorf("secret sender = %s, want our own JID %s", sec.sender, pollOwnJID)
	}

	// The poll is mirrored so it is addressable without waiting for an event.
	cached, err := h.store.GetPoll(context.Background(), pollChatJID, "wamid.POLL1")
	if err != nil {
		t.Fatalf("GetPoll: %v", err)
	}
	if len(cached.Options) != 2 || cached.Options[0].Name != "Pizza" {
		t.Errorf("cached ballot = %+v", cached.Options)
	}
	if !cached.IsFromMe {
		t.Error("cached poll is_from_me = false, want true")
	}
}

func TestSendPoll_RejectsBadBallots(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args map[string]any
	}{
		{"one option", map[string]any{"recipient": "1234567890", "question": "Q", "options": []any{"only"}}},
		{"duplicate options", map[string]any{"recipient": "1234567890", "question": "Q", "options": []any{"Pizza", "Pizza"}}},
		{"blank option", map[string]any{"recipient": "1234567890", "question": "Q", "options": []any{"Pizza", "  "}}},
		{"empty question", map[string]any{"recipient": "1234567890", "question": " ", "options": []any{"Pizza", "Sushi"}}},
		{"selectable above ballot", map[string]any{"recipient": "1234567890", "question": "Q", "options": []any{"Pizza", "Sushi"}, "selectable_count": 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := pollMock("wamid.POLL1")
			h := newHarness(t, true, nil, mock)
			res := callTool(t, h, "send_poll", tc.args)
			expectError(t, res, mcp.ErrInvalidArgument)
			if mock.sendCalls != 0 {
				t.Errorf("sendCalls = %d, want 0: nothing should reach the wire", mock.sendCalls)
			}
		})
	}
}

func TestVotePoll_SendsVoteAndRecordsOwnSelection(t *testing.T) {
	t.Parallel()
	mock := pollMock("wamid.VOTE1")
	h := newHarness(t, true, seedPoll(pollGroupJID, "wamid.POLL1", pollChatJID, 1, "Pizza", "Sushi"), mock)

	res := callTool(t, h, "vote_poll", map[string]any{
		"chat_jid":   pollGroupJID,
		"message_id": "wamid.POLL1",
		"options":    []any{"Sushi"},
	})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	if got := s["poll_message_id"]; got != "wamid.POLL1" {
		t.Errorf("poll_message_id = %v", got)
	}

	// whatsmeow keys the vote's encryption off the poll's own MessageInfo, so
	// the reconstructed chat/sender/group flags have to survive the round trip.
	if mock.lastPollInfo == nil {
		t.Fatal("BuildPollVote was not called")
	}
	if mock.lastPollInfo.Chat.String() != pollGroupJID {
		t.Errorf("poll info chat = %s, want %s", mock.lastPollInfo.Chat, pollGroupJID)
	}
	if mock.lastPollInfo.Sender.String() != pollChatJID {
		t.Errorf("poll info sender = %s, want %s", mock.lastPollInfo.Sender, pollChatJID)
	}
	if !mock.lastPollInfo.IsGroup {
		t.Error("poll info is_group = false for a @g.us chat")
	}
	if len(mock.lastPollOptions) != 1 || mock.lastPollOptions[0] != "Sushi" {
		t.Errorf("voted options = %v", mock.lastPollOptions)
	}
	if mock.lastSendTo.String() != pollGroupJID {
		t.Errorf("vote sent to %s, want the poll's chat %s", mock.lastSendTo, pollGroupJID)
	}

	// WhatsApp never echoes our own vote back to us, so the tool has to record
	// it or the tally would omit this device.
	votes, err := h.store.ListPollVotes(context.Background(), pollGroupJID, "wamid.POLL1")
	if err != nil {
		t.Fatalf("ListPollVotes: %v", err)
	}
	if len(votes) != 1 || len(votes[0].SelectedHashes) != 1 ||
		votes[0].SelectedHashes[0] != cache.HashPollOption("Sushi") {
		t.Fatalf("own vote = %+v, want the Sushi hash", votes)
	}
	if votes[0].VoterJID != pollOwnJID {
		t.Errorf("voter = %s, want our own non-AD JID %s", votes[0].VoterJID, pollOwnJID)
	}
}

func TestVotePoll_EmptyOptionsWithdrawsVote(t *testing.T) {
	t.Parallel()
	mock := pollMock("wamid.VOTE2")
	h := newHarness(t, true, seedPoll(pollChatJID, "wamid.POLL1", pollChatJID, 1, "Pizza", "Sushi"), mock)

	res := callTool(t, h, "vote_poll", map[string]any{
		"chat_jid":   pollChatJID,
		"message_id": "wamid.POLL1",
		"options":    []any{},
	})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	votes, err := h.store.ListPollVotes(context.Background(), pollChatJID, "wamid.POLL1")
	if err != nil {
		t.Fatalf("ListPollVotes: %v", err)
	}
	if len(votes) != 1 || len(votes[0].SelectedHashes) != 0 {
		t.Fatalf("withdrawal = %+v, want a row with an empty selection", votes)
	}
}

func TestVotePoll_RejectsOptionNotOnBallot(t *testing.T) {
	t.Parallel()
	mock := pollMock("wamid.VOTE1")
	h := newHarness(t, true, seedPoll(pollChatJID, "wamid.POLL1", pollChatJID, 1, "Pizza", "Sushi"), mock)

	res := callTool(t, h, "vote_poll", map[string]any{
		"chat_jid":   pollChatJID,
		"message_id": "wamid.POLL1",
		"options":    []any{"pizza"},
	})
	// Options travel as hashes of their exact text, so a near-miss would be a
	// perfectly valid vote for nothing at all — it must not reach the wire.
	expectError(t, res, mcp.ErrInvalidArgument)
	if mock.sendCalls != 0 {
		t.Errorf("sendCalls = %d, want 0", mock.sendCalls)
	}
}

func TestVotePoll_RejectsMoreSelectionsThanAllowed(t *testing.T) {
	t.Parallel()
	mock := pollMock("wamid.VOTE1")
	h := newHarness(t, true, seedPoll(pollChatJID, "wamid.POLL1", pollChatJID, 1, "Pizza", "Sushi"), mock)

	res := callTool(t, h, "vote_poll", map[string]any{
		"chat_jid":   pollChatJID,
		"message_id": "wamid.POLL1",
		"options":    []any{"Pizza", "Sushi"},
	})
	expectError(t, res, mcp.ErrInvalidArgument)
	if mock.sendCalls != 0 {
		t.Errorf("sendCalls = %d, want 0", mock.sendCalls)
	}
}

func TestVotePoll_UnknownPollIsNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil, pollMock("wamid.VOTE1"))

	res := callTool(t, h, "vote_poll", map[string]any{
		"chat_jid":   pollChatJID,
		"message_id": "wamid.NOPE",
		"options":    []any{"Pizza"},
	})
	expectError(t, res, mcp.ErrNotFound)
}

func TestVotePoll_MissingSecretIsReportedNotFound(t *testing.T) {
	t.Parallel()
	mock := pollMock("wamid.VOTE1")
	mock.buildPollVoteErr = whatsmeow.ErrOriginalMessageSecretNotFound
	h := newHarness(t, true, seedPoll(pollChatJID, "wamid.POLL1", pollChatJID, 1, "Pizza", "Sushi"), mock)

	res := callTool(t, h, "vote_poll", map[string]any{
		"chat_jid":   pollChatJID,
		"message_id": "wamid.POLL1",
		"options":    []any{"Pizza"},
	})
	s := expectError(t, res, mcp.ErrNotFound)
	if msg, _ := s["message"].(string); msg == "" {
		t.Error("expected a message explaining that the poll's secret was never held")
	}
	if mock.sendCalls != 0 {
		t.Errorf("sendCalls = %d, want 0", mock.sendCalls)
	}
}

func TestGetPollResults_TalliesVotesAndNamesVoters(t *testing.T) {
	t.Parallel()
	seed := func(s *cache.Store) {
		ctx := context.Background()
		seedContacts(s)
		seedPoll(pollGroupJID, "wamid.POLL1", "111@s.whatsapp.net", 1, "Pizza", "Sushi")(s)
		_ = s.UpsertPollVote(ctx, cache.PollVote{
			ChatJID: pollGroupJID, PollMessageID: "wamid.POLL1", VoterJID: "111@s.whatsapp.net",
			SelectedHashes: []string{cache.HashPollOption("Pizza")}, Timestamp: time.Unix(1_700_000_100, 0),
		})
		_ = s.UpsertPollVote(ctx, cache.PollVote{
			ChatJID: pollGroupJID, PollMessageID: "wamid.POLL1", VoterJID: "222@s.whatsapp.net",
			SelectedHashes: []string{cache.HashPollOption("Pizza")}, Timestamp: time.Unix(1_700_000_200, 0),
		})
		// Withdrew their vote: on record, but counts for nothing.
		_ = s.UpsertPollVote(ctx, cache.PollVote{
			ChatJID: pollGroupJID, PollMessageID: "wamid.POLL1", VoterJID: "333@s.whatsapp.net",
			SelectedHashes: []string{}, Timestamp: time.Unix(1_700_000_300, 0),
		})
		// A hash matching no cached option — surfaced rather than dropped.
		_ = s.UpsertPollVote(ctx, cache.PollVote{
			ChatJID: pollGroupJID, PollMessageID: "wamid.POLL1", VoterJID: "444@s.whatsapp.net",
			SelectedHashes: []string{cache.HashPollOption("Tacos")}, Timestamp: time.Unix(1_700_000_400, 0),
		})
	}
	h := newHarness(t, true, seed, pollMock("wamid.POLL1"))

	res := callTool(t, h, "get_poll_results", map[string]any{
		"chat_jid":   pollGroupJID,
		"message_id": "wamid.POLL1",
	})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	if got := s["question"]; got != "Lunch?" {
		t.Errorf("question = %v", got)
	}
	if got := s["total_voters"]; got != float64(3) {
		t.Errorf("total_voters = %v, want 3 (the withdrawal does not count)", got)
	}
	if got := s["unknown_option_votes"]; got != float64(1) {
		t.Errorf("unknown_option_votes = %v, want 1", got)
	}
	options, _ := s["options"].([]any)
	if len(options) != 2 {
		t.Fatalf("options = %+v, want 2", options)
	}
	pizza, _ := options[0].(map[string]any)
	if pizza["option"] != "Pizza" || pizza["votes"] != float64(2) {
		t.Errorf("pizza row = %+v, want 2 votes", pizza)
	}
	voters, _ := pizza["voters"].([]any)
	if len(voters) != 2 {
		t.Fatalf("pizza voters = %+v, want 2", voters)
	}
	first, _ := voters[0].(map[string]any)
	if first["name"] != "Ali" {
		t.Errorf("first voter name = %v, want the cached contact name", first["name"])
	}
	sushi, _ := options[1].(map[string]any)
	if sushi["votes"] != float64(0) {
		t.Errorf("sushi votes = %v, want 0", sushi["votes"])
	}
	if got, _ := s["caveat"].(string); got == "" {
		t.Error("expected the tally caveat to be stated in the result")
	}
}

func TestGetPollResults_UnknownPollIsNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil, pollMock("wamid.POLL1"))

	res := callTool(t, h, "get_poll_results", map[string]any{
		"chat_jid":   pollChatJID,
		"message_id": "wamid.NOPE",
	})
	expectError(t, res, mcp.ErrNotFound)
}

func TestPollTools_GatedOnPairing(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"send_poll", "vote_poll", "get_poll_results"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, false, nil, nil)
			res := callTool(t, h, name, map[string]any{
				"recipient": "1234567890", "question": "Q", "options": []any{"Pizza", "Sushi"},
				"chat_jid": pollChatJID, "message_id": "wamid.POLL1",
			})
			expectError(t, res, mcp.ErrNotPaired)
		})
	}
}
