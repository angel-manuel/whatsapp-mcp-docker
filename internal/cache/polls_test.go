package cache

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// stubDecrypter returns a canned selection (or an error) for every poll
// update, standing in for whatsmeow's DecryptPollVote.
type stubDecrypter struct {
	options [][]byte
	err     error
	calls   int
}

func (s *stubDecrypter) DecryptPollVote(context.Context, *events.Message) (*waE2E.PollVoteMessage, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &waE2E.PollVoteMessage{SelectedOptions: s.options}, nil
}

func buildPollCreationEvent(chat, sender types.JID, id, question string, options []string, selectable int, ts time.Time) *events.Message {
	opts := make([]*waE2E.PollCreationMessage_Option, len(options))
	for i, o := range options {
		opts[i] = &waE2E.PollCreationMessage_Option{OptionName: proto.String(o)}
	}
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: chat.Server == types.GroupServer},
			ID:            id,
			Timestamp:     ts,
		},
		Message: &waE2E.Message{
			PollCreationMessage: &waE2E.PollCreationMessage{
				Name:                   proto.String(question),
				Options:                opts,
				SelectableOptionsCount: proto.Uint32(uint32(selectable)), //nolint:gosec // test constant
			},
		},
	}
}

func buildPollVoteEvent(chat, voter types.JID, id, pollID string, ts time.Time) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: voter, IsGroup: chat.Server == types.GroupServer},
			ID:            id,
			Timestamp:     ts,
		},
		Message: &waE2E.Message{
			PollUpdateMessage: &waE2E.PollUpdateMessage{
				PollCreationMessageKey: &waCommon.MessageKey{ID: proto.String(pollID)},
				Vote:                   &waE2E.PollEncValue{EncPayload: []byte("ciphertext"), EncIV: []byte("iv")},
				SenderTimestampMS:      proto.Int64(ts.UnixMilli()),
			},
		},
	}
}

func TestHandleEvent_PollCreation_PersistsBallotAndMessageRow(t *testing.T) {
	ingest, store := newTestIngestor(t)
	ctx := context.Background()

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	ts := time.Unix(1_700_000_000, 0).UTC()
	ingest.HandleEvent(buildPollCreationEvent(chat, chat, "wamid.POLL1", "Lunch?", []string{"Pizza", "Sushi"}, 1, ts))

	// The poll must also land in messages: that is where a caller gets the id
	// vote_poll and get_poll_results take.
	var kind, body string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT kind, body FROM messages WHERE chat_jid = ? AND id = ?`,
		chat.String(), "wamid.POLL1").Scan(&kind, &body); err != nil {
		t.Fatalf("scan message: %v", err)
	}
	if kind != string(KindPoll) {
		t.Errorf("kind = %q, want %q", kind, KindPoll)
	}
	if body != "Lunch?" {
		t.Errorf("body = %q, want the poll question", body)
	}

	poll, err := store.GetPoll(ctx, chat.String(), "wamid.POLL1")
	if err != nil {
		t.Fatalf("GetPoll: %v", err)
	}
	if poll.Question != "Lunch?" || poll.SelectableCount != 1 {
		t.Errorf("poll = %+v", poll)
	}
	if len(poll.Options) != 2 || poll.Options[0].Name != "Pizza" || poll.Options[1].Name != "Sushi" {
		t.Fatalf("options = %+v, want [Pizza Sushi] in order", poll.Options)
	}
	if poll.Options[0].Hash != HashPollOption("Pizza") {
		t.Errorf("option hash = %q, want the whatsmeow digest of the name", poll.Options[0].Hash)
	}
	if poll.SenderJID != chat.String() {
		t.Errorf("sender_jid = %q, want %q", poll.SenderJID, chat.String())
	}
}

func TestHandleEvent_PollVote_DecryptsAndRecordsSelection(t *testing.T) {
	ingest, store := newTestIngestor(t)
	ctx := context.Background()

	chat := mustParseJID(t, "1234567890-1600000000@g.us")
	author := mustParseJID(t, "1234567890@s.whatsapp.net")
	voter := mustParseJID(t, "5550001111@s.whatsapp.net")
	ts := time.Unix(1_700_000_000, 0).UTC()
	ingest.HandleEvent(buildPollCreationEvent(chat, author, "wamid.POLL1", "Lunch?", []string{"Pizza", "Sushi"}, 1, ts))

	pizza, err := hex.DecodeString(HashPollOption("Pizza"))
	if err != nil {
		t.Fatalf("decode hash: %v", err)
	}
	dec := &stubDecrypter{options: [][]byte{pizza}}
	ingest.SetPollDecrypter(dec)

	ingest.HandleEvent(buildPollVoteEvent(chat, voter, "wamid.VOTE1", "wamid.POLL1", ts.Add(time.Minute)))

	if dec.calls != 1 {
		t.Fatalf("decrypter calls = %d, want 1", dec.calls)
	}
	votes, err := store.ListPollVotes(ctx, chat.String(), "wamid.POLL1")
	if err != nil {
		t.Fatalf("ListPollVotes: %v", err)
	}
	if len(votes) != 1 {
		t.Fatalf("votes = %+v, want 1", votes)
	}
	if votes[0].VoterJID != voter.String() {
		t.Errorf("voter = %q, want %q", votes[0].VoterJID, voter.String())
	}
	if len(votes[0].SelectedHashes) != 1 || votes[0].SelectedHashes[0] != HashPollOption("Pizza") {
		t.Errorf("selection = %+v, want the Pizza hash", votes[0].SelectedHashes)
	}

	// A vote is not a message: it must not show up in the transcript.
	var count int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE id = ?`, "wamid.VOTE1").Scan(&count); err != nil {
		t.Fatalf("count vote messages: %v", err)
	}
	if count != 0 {
		t.Errorf("vote stanza stored as a message row (%d rows)", count)
	}
}

func TestHandleEvent_PollVote_ReplacesEarlierSelection(t *testing.T) {
	ingest, store := newTestIngestor(t)
	ctx := context.Background()

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	ts := time.Unix(1_700_000_000, 0).UTC()
	ingest.HandleEvent(buildPollCreationEvent(chat, chat, "wamid.POLL1", "Lunch?", []string{"Pizza", "Sushi"}, 1, ts))

	pizza, _ := hex.DecodeString(HashPollOption("Pizza"))
	sushi, _ := hex.DecodeString(HashPollOption("Sushi"))

	dec := &stubDecrypter{options: [][]byte{pizza}}
	ingest.SetPollDecrypter(dec)
	ingest.HandleEvent(buildPollVoteEvent(chat, chat, "wamid.VOTE1", "wamid.POLL1", ts.Add(time.Minute)))

	// Changing the vote replaces the selection outright rather than adding to it.
	dec.options = [][]byte{sushi}
	ingest.HandleEvent(buildPollVoteEvent(chat, chat, "wamid.VOTE2", "wamid.POLL1", ts.Add(2*time.Minute)))

	votes, err := store.ListPollVotes(ctx, chat.String(), "wamid.POLL1")
	if err != nil {
		t.Fatalf("ListPollVotes: %v", err)
	}
	if len(votes) != 1 {
		t.Fatalf("votes = %+v, want a single row per voter", votes)
	}
	if len(votes[0].SelectedHashes) != 1 || votes[0].SelectedHashes[0] != HashPollOption("Sushi") {
		t.Fatalf("selection = %+v, want only the Sushi hash", votes[0].SelectedHashes)
	}

	// A replayed older update must not resurrect the earlier selection.
	dec.options = [][]byte{pizza}
	ingest.HandleEvent(buildPollVoteEvent(chat, chat, "wamid.VOTE1", "wamid.POLL1", ts.Add(time.Minute)))
	votes, err = store.ListPollVotes(ctx, chat.String(), "wamid.POLL1")
	if err != nil {
		t.Fatalf("ListPollVotes: %v", err)
	}
	if len(votes[0].SelectedHashes) != 1 || votes[0].SelectedHashes[0] != HashPollOption("Sushi") {
		t.Fatalf("stale update overwrote the newer vote: %+v", votes[0].SelectedHashes)
	}
}

func TestHandleEvent_PollVote_EmptySelectionIsRecordedAsWithdrawal(t *testing.T) {
	ingest, store := newTestIngestor(t)
	ctx := context.Background()

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	ts := time.Unix(1_700_000_000, 0).UTC()
	ingest.HandleEvent(buildPollCreationEvent(chat, chat, "wamid.POLL1", "Lunch?", []string{"Pizza", "Sushi"}, 1, ts))

	ingest.SetPollDecrypter(&stubDecrypter{options: nil})
	ingest.HandleEvent(buildPollVoteEvent(chat, chat, "wamid.VOTE1", "wamid.POLL1", ts.Add(time.Minute)))

	votes, err := store.ListPollVotes(ctx, chat.String(), "wamid.POLL1")
	if err != nil {
		t.Fatalf("ListPollVotes: %v", err)
	}
	if len(votes) != 1 {
		t.Fatalf("votes = %+v, want the withdrawal on record", votes)
	}
	if len(votes[0].SelectedHashes) != 0 {
		t.Fatalf("selection = %+v, want empty", votes[0].SelectedHashes)
	}
}

func TestHandleEvent_PollVote_UndecryptableIsDropped(t *testing.T) {
	ingest, store := newTestIngestor(t)
	ctx := context.Background()

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	ts := time.Unix(1_700_000_000, 0).UTC()
	ingest.SetPollDecrypter(&stubDecrypter{err: errors.New("original message secret key not found")})
	ingest.HandleEvent(buildPollVoteEvent(chat, chat, "wamid.VOTE1", "wamid.POLL1", ts))

	votes, err := store.ListPollVotes(ctx, chat.String(), "wamid.POLL1")
	if err != nil {
		t.Fatalf("ListPollVotes: %v", err)
	}
	if len(votes) != 0 {
		t.Fatalf("votes = %+v, want none: an undecryptable vote carries no information", votes)
	}
}

func TestHandleEvent_PollVote_NoDecrypterIsDropped(t *testing.T) {
	ingest, store := newTestIngestor(t)
	ctx := context.Background()

	chat := mustParseJID(t, "1234567890@s.whatsapp.net")
	ingest.HandleEvent(buildPollVoteEvent(chat, chat, "wamid.VOTE1", "wamid.POLL1", time.Unix(1_700_000_000, 0).UTC()))

	votes, err := store.ListPollVotes(ctx, chat.String(), "wamid.POLL1")
	if err != nil {
		t.Fatalf("ListPollVotes: %v", err)
	}
	if len(votes) != 0 {
		t.Fatalf("votes = %+v, want none", votes)
	}
}

func TestUpsertPoll_ReplacesBallotOnReingest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	base := Poll{
		ChatJID:   "1234567890@s.whatsapp.net",
		MessageID: "wamid.POLL1",
		Question:  "Lunch?",
		Options:   []PollOption{{Name: "Pizza"}, {Name: "Sushi"}, {Name: "Salad"}},
	}
	if err := store.UpsertPoll(ctx, base); err != nil {
		t.Fatalf("UpsertPoll: %v", err)
	}
	base.Options = []PollOption{{Name: "Pizza"}, {Name: "Sushi"}}
	if err := store.UpsertPoll(ctx, base); err != nil {
		t.Fatalf("UpsertPoll (re-ingest): %v", err)
	}

	got, err := store.GetPoll(ctx, base.ChatJID, base.MessageID)
	if err != nil {
		t.Fatalf("GetPoll: %v", err)
	}
	if len(got.Options) != 2 {
		t.Fatalf("options = %+v, want the shorter ballot with no leftovers", got.Options)
	}
}

func TestGetPoll_UnknownPollReportsNoRows(t *testing.T) {
	store := newTestStore(t)
	_, err := store.GetPoll(context.Background(), "1234567890@s.whatsapp.net", "wamid.NOPE")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}
