package cache

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
)

// Poll mirrors a polls row plus its ordered ballot. It is the local record of
// a poll creation message: the question, the options, and the identity of the
// message that carried them.
//
// SenderJID and IsFromMe exist for one reason: whatsmeow's BuildPollVote wants
// a types.MessageInfo describing the *poll creation* message, and derives the
// encryption key from its sender. Everything else in that MessageInfo can be
// rebuilt from ChatJID, but the original sender cannot.
type Poll struct {
	ChatJID   string
	MessageID string
	Question  string
	Options   []PollOption
	// SelectableCount is how many options a voter may pick. 0 means the poll
	// carried no limit.
	SelectableCount int
	SenderJID       string
	IsFromMe        bool
	Timestamp       time.Time
}

// PollOption is one entry on the ballot. Hash is the lowercase hex SHA-256 of
// Name; votes reference options by that digest and never by name.
type PollOption struct {
	Name string
	Hash string
}

// PollVote is one voter's complete current selection. WhatsApp resends the
// whole selection on every change, so a newer vote replaces an older one
// rather than adding to it — and an empty SelectedHashes is a voter who
// cleared their vote, not a voter who never voted.
type PollVote struct {
	ChatJID        string
	PollMessageID  string
	VoterJID       string
	SelectedHashes []string
	Timestamp      time.Time
}

// HashPollOption returns the lowercase hex SHA-256 of a poll option name —
// the digest whatsmeow puts on the wire for votes. It delegates to whatsmeow
// so the local ballot and an incoming vote can never disagree about how an
// option is identified.
func HashPollOption(name string) string {
	hashes := whatsmeow.HashPollOptions([]string{name})
	if len(hashes) == 0 {
		return ""
	}
	return hex.EncodeToString(hashes[0])
}

// UpsertPoll records a poll creation message and replaces its ballot. The
// ballot is rewritten wholesale (rather than merged) so a re-ingest of the
// same poll cannot leave stale options behind; option hashes are filled in
// from Name when the caller left them empty.
func (s *Store) UpsertPoll(ctx context.Context, p Poll) error {
	if p.ChatJID == "" || p.MessageID == "" {
		return errors.New("cache: UpsertPoll: ChatJID and MessageID required")
	}
	now := time.Now().Unix()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO polls (chat_jid, message_id, question, selectable_count, sender_jid, is_from_me, ts, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(chat_jid, message_id) DO UPDATE SET
    question         = excluded.question,
    selectable_count = excluded.selectable_count,
    sender_jid       = CASE WHEN excluded.sender_jid != '' THEN excluded.sender_jid ELSE polls.sender_jid END,
    is_from_me       = excluded.is_from_me,
    ts               = excluded.ts,
    updated_at       = excluded.updated_at
`,
			p.ChatJID, p.MessageID, p.Question, p.SelectableCount, p.SenderJID,
			boolToInt(p.IsFromMe), unixSeconds(p.Timestamp), now,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM poll_options WHERE chat_jid = ? AND message_id = ?`,
			p.ChatJID, p.MessageID); err != nil {
			return err
		}
		for i, opt := range p.Options {
			hash := opt.Hash
			if hash == "" {
				hash = HashPollOption(opt.Name)
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO poll_options (chat_jid, message_id, idx, name, hash) VALUES (?, ?, ?, ?, ?)`,
				p.ChatJID, p.MessageID, i, opt.Name, hash); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetPoll returns a poll and its ballot. It reports sql.ErrNoRows when the
// poll was never ingested — which is the normal state for a poll created
// before this device was linked, since WhatsApp never replays it.
func (s *Store) GetPoll(ctx context.Context, chatJID, messageID string) (Poll, error) {
	var (
		p        Poll
		isFromMe int
		ts       int64
	)
	err := s.db.QueryRowContext(ctx, `
SELECT chat_jid, message_id, question, selectable_count, sender_jid, is_from_me, ts
FROM polls WHERE chat_jid = ? AND message_id = ?`, chatJID, messageID).
		Scan(&p.ChatJID, &p.MessageID, &p.Question, &p.SelectableCount, &p.SenderJID, &isFromMe, &ts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Poll{}, err
		}
		return Poll{}, fmt.Errorf("cache: get poll %s/%s: %w", chatJID, messageID, err)
	}
	p.IsFromMe = isFromMe != 0
	if ts > 0 {
		p.Timestamp = time.Unix(ts, 0).UTC()
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT name, hash FROM poll_options WHERE chat_jid = ? AND message_id = ? ORDER BY idx`,
		chatJID, messageID)
	if err != nil {
		return Poll{}, fmt.Errorf("cache: list poll options %s/%s: %w", chatJID, messageID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var opt PollOption
		if err := rows.Scan(&opt.Name, &opt.Hash); err != nil {
			return Poll{}, fmt.Errorf("cache: scan poll option: %w", err)
		}
		p.Options = append(p.Options, opt)
	}
	if err := rows.Err(); err != nil {
		return Poll{}, fmt.Errorf("cache: iterate poll options: %w", err)
	}
	return p, nil
}

// UpsertPollVote records a voter's current selection, keeping the newest one.
// Events can arrive out of order (a retry after a reconnect replays an old
// update), so a vote older than the one already stored is dropped instead of
// overwriting it.
func (s *Store) UpsertPollVote(ctx context.Context, v PollVote) error {
	if v.ChatJID == "" || v.PollMessageID == "" || v.VoterJID == "" {
		return errors.New("cache: UpsertPollVote: ChatJID, PollMessageID and VoterJID required")
	}
	selected := v.SelectedHashes
	if selected == nil {
		selected = []string{}
	}
	encoded, err := json.Marshal(selected)
	if err != nil {
		return fmt.Errorf("cache: encode poll selection: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO poll_votes (chat_jid, poll_message_id, voter_jid, selected_hashes, ts)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(chat_jid, poll_message_id, voter_jid) DO UPDATE SET
    selected_hashes = excluded.selected_hashes,
    ts              = excluded.ts
WHERE excluded.ts >= poll_votes.ts
`, v.ChatJID, v.PollMessageID, v.VoterJID, string(encoded), unixSeconds(v.Timestamp))
	if err != nil {
		return fmt.Errorf("cache: upsert poll vote %s/%s/%s: %w", v.ChatJID, v.PollMessageID, v.VoterJID, err)
	}
	return nil
}

// ListPollVotes returns every recorded vote for a poll, oldest first. The
// result is the tally's raw material: one row per voter, each carrying that
// voter's complete current selection.
func (s *Store) ListPollVotes(ctx context.Context, chatJID, messageID string) ([]PollVote, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT voter_jid, selected_hashes, ts
FROM poll_votes WHERE chat_jid = ? AND poll_message_id = ?
ORDER BY ts, voter_jid`, chatJID, messageID)
	if err != nil {
		return nil, fmt.Errorf("cache: list poll votes %s/%s: %w", chatJID, messageID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []PollVote
	for rows.Next() {
		var (
			v       PollVote
			encoded string
			ts      int64
		)
		if err := rows.Scan(&v.VoterJID, &encoded, &ts); err != nil {
			return nil, fmt.Errorf("cache: scan poll vote: %w", err)
		}
		if err := json.Unmarshal([]byte(encoded), &v.SelectedHashes); err != nil {
			return nil, fmt.Errorf("cache: decode poll selection for %s: %w", v.VoterJID, err)
		}
		v.ChatJID = chatJID
		v.PollMessageID = messageID
		if ts > 0 {
			v.Timestamp = time.Unix(ts, 0).UTC()
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cache: iterate poll votes: %w", err)
	}
	return out, nil
}
