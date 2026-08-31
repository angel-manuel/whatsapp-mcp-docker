package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// maxPollOptions is WhatsApp's own ceiling on a poll's ballot. Rejecting a
// longer list here turns a silent server-side truncation into a clear error.
const maxPollOptions = 12

// pollTallyCaveat explains the one thing a caller must understand about poll
// results before acting on them: they are accumulated from live vote events,
// because whatsmeow (and the WhatsApp protocol) offers no way to ask the
// server for a poll's current standings. It is returned on every successful
// tally (PollResults.Caveat) and embedded in the not-found error, so a caller
// that never reads the tool description still gets it.
//
// No trailing period: it is embedded in error strings, and ST1005 wants those
// to end without punctuation.
const pollTallyCaveat = "the tally is what this device observed — WhatsApp never replays poll votes, " +
	"so votes cast before the device was linked, or while the container was down, are not counted"

var sendPollSchema = json.RawMessage(`{
  "type": "object",
  "required": ["recipient", "question", "options"],
  "properties": {
    "recipient": {
      "type": "string",
      "description": "Destination chat: a JID ('user@s.whatsapp.net' or 'group@g.us') or a raw phone number with country code (digits only, no + or spaces)."
    },
    "question": {
      "type": "string",
      "description": "The poll question.",
      "minLength": 1
    },
    "options": {
      "type": "array",
      "description": "Answer options, in display order. Must be unique: options are identified on the wire by a hash of their text, so two identical options are indistinguishable when votes come back.",
      "items": {"type": "string", "minLength": 1},
      "minItems": 2,
      "maxItems": 12
    },
    "selectable_count": {
      "type": "integer",
      "description": "How many options one voter may pick. Default 1 (single choice); 0 means no limit. Must not exceed the number of options.",
      "minimum": 0,
      "maximum": 12
    }
  },
  "additionalProperties": false
}`)

var votePollSchema = json.RawMessage(`{
  "type": "object",
  "required": ["chat_jid", "message_id", "options"],
  "properties": {
    "chat_jid": {
      "type": "string",
      "description": "JID of the chat holding the poll (e.g. 34600111222@s.whatsapp.net or 1234567890-1600000000@g.us)."
    },
    "message_id": {
      "type": "string",
      "description": "Stanza id of the poll creation message, as returned by list_messages / get_message_context (kind 'poll') or by send_poll."
    },
    "options": {
      "type": "array",
      "description": "Option texts to vote for, exactly as they appear on the ballot. Pass an empty array to withdraw a previous vote.",
      "items": {"type": "string"},
      "maxItems": 12
    }
  },
  "additionalProperties": false
}`)

var getPollResultsSchema = json.RawMessage(`{
  "type": "object",
  "required": ["chat_jid", "message_id"],
  "properties": {
    "chat_jid": {
      "type": "string",
      "description": "JID of the chat holding the poll."
    },
    "message_id": {
      "type": "string",
      "description": "Stanza id of the poll creation message, as returned by list_messages / get_message_context (kind 'poll') or by send_poll."
    }
  },
  "additionalProperties": false
}`)

// SendPollResult is the structured output of send_poll. MessageID is the
// poll's own stanza id — the handle vote_poll and get_poll_results take.
type SendPollResult struct {
	MessageID       string   `json:"message_id"`
	ChatJID         string   `json:"chat_jid"`
	SentTS          int64    `json:"sent_ts"`
	Question        string   `json:"question"`
	Options         []string `json:"options"`
	SelectableCount int      `json:"selectable_count"`
}

// VotePollResult is the structured output of vote_poll. MessageID is the id
// of the vote stanza, which is not addressable by any other tool; PollMessageID
// is the poll it applies to.
type VotePollResult struct {
	MessageID       string   `json:"message_id"`
	PollMessageID   string   `json:"poll_message_id"`
	ChatJID         string   `json:"chat_jid"`
	SelectedOptions []string `json:"selected_options"`
	SentTS          int64    `json:"sent_ts"`
}

// PollVoterView identifies one voter. Name is empty when no contact name is
// known — the JID is never echoed as a pseudo-name, matching resolve_jid.
type PollVoterView struct {
	JID  string `json:"jid"`
	Name string `json:"name"`
}

// PollOptionResult is one row of the tally.
type PollOptionResult struct {
	Option string          `json:"option"`
	Votes  int             `json:"votes"`
	Voters []PollVoterView `json:"voters"`
}

// PollResults is the structured output of get_poll_results.
//
// UnknownOptionVotes counts selections whose hash matches no option on the
// cached ballot. It should be 0; a non-zero value means the poll was edited or
// its ballot was ingested incompletely, and the tally below it is short by
// that many selections. Surfacing it beats silently dropping them.
type PollResults struct {
	ChatJID            string             `json:"chat_jid"`
	MessageID          string             `json:"message_id"`
	Question           string             `json:"question"`
	SelectableCount    int                `json:"selectable_count"`
	CreatedBy          PollVoterView      `json:"created_by"`
	IsFromMe           bool               `json:"is_from_me"`
	Options            []PollOptionResult `json:"options"`
	TotalVoters        int                `json:"total_voters"`
	UnknownOptionVotes int                `json:"unknown_option_votes"`
	Caveat             string             `json:"caveat"`
}

// sendPoll is the handler for the send_poll MCP tool. Beyond the send itself
// it does two things that are easy to miss and impossible to recover from
// later: it persists the poll's message secret (whatsmeow only stores secrets
// for received messages, so without this no vote on our own poll would ever
// decrypt), and it mirrors the ballot into the cache so vote_poll and
// get_poll_results can resolve the poll without a round trip.
func sendPoll(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			Recipient       string   `json:"recipient"`
			Question        string   `json:"question"`
			Options         []string `json:"options"`
			SelectableCount *int     `json:"selectable_count"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}
		recipient := strings.TrimSpace(in.Recipient)
		if recipient == "" {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, "recipient must not be empty"), nil
		}
		question := strings.TrimSpace(in.Question)
		if question == "" {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, "question must not be empty"), nil
		}
		options, err := normalisePollOptions(in.Options)
		if err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}
		selectable := 1
		if in.SelectableCount != nil {
			selectable = *in.SelectableCount
		}
		if selectable < 0 || selectable > len(options) {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, fmt.Sprintf(
				"selectable_count %d is out of range for a poll with %d options (0 means no limit)",
				selectable, len(options))), nil
		}

		to, err := resolveRecipient(recipient)
		if err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}

		msg, err := deps.WA.BuildPollCreation(question, options, selectable)
		if err != nil {
			if errors.Is(err, wa.ErrNotLoggedIn) {
				return mcp.NotConnectedError(), nil
			}
			return mcp.ErrorResult(mcp.ErrInternal, fmt.Sprintf("build poll: %v", err)), nil
		}

		resp, err := deps.WA.SendMessage(ctx, to, msg)
		if err != nil {
			if errors.Is(err, wa.ErrNotLoggedIn) {
				return mcp.NotConnectedError(), nil
			}
			return mcp.ErrorResult(mcp.ErrInternal, fmt.Sprintf("send poll: %v", err)), nil
		}

		ts := resp.Timestamp
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		ownJID := deps.WA.OwnJID()

		// Persisting the secret is not bookkeeping — it is the only thing that
		// makes the poll answerable. A failure here would leave a poll whose
		// votes are permanently undecryptable, so it is an error, not a warning.
		if err := deps.WA.StoreMessageSecret(ctx, to, ownJID, resp.ID,
			msg.GetMessageContextInfo().GetMessageSecret()); err != nil {
			return mcp.ErrorResult(mcp.ErrInternal, fmt.Sprintf(
				"poll sent as %s but its message secret could not be stored, so incoming votes will not be readable: %v",
				resp.ID, err)), nil
		}

		if err := mirrorOutboundPoll(ctx, deps.Cache, to, ownJID, string(resp.ID), question, options, selectable, ts); err != nil {
			return mcp.ErrorResult(mcp.ErrInternal, fmt.Sprintf("cache outbound poll: %v", err)), nil
		}

		return SendPollResult{
			MessageID:       string(resp.ID),
			ChatJID:         to.String(),
			SentTS:          ts.Unix(),
			Question:        question,
			Options:         options,
			SelectableCount: selectable,
		}, nil
	}
}

// votePoll is the handler for the vote_poll MCP tool. The ballot is validated
// against the cached poll before anything goes on the wire: options travel as
// hashes of their text, so a typo would otherwise be sent as a perfectly valid
// vote for nothing at all.
func votePoll(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			ChatJID   string   `json:"chat_jid"`
			MessageID string   `json:"message_id"`
			Options   []string `json:"options"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}
		chatJID := strings.TrimSpace(in.ChatJID)
		messageID := strings.TrimSpace(in.MessageID)
		if chatJID == "" {
			return mcp.InvalidArgumentError("chat_jid is required"), nil
		}
		if messageID == "" {
			return mcp.InvalidArgumentError("message_id is required"), nil
		}

		poll, err := deps.Cache.GetPoll(ctx, chatJID, messageID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return mcp.NotFoundError(fmt.Sprintf(
					"no cached poll %s in chat %s; only polls this device received can be voted on, and WhatsApp does not replay older ones",
					messageID, chatJID)), nil
			}
			return mcp.InternalError(err.Error()), nil
		}

		selected, err := matchPollOptions(poll, in.Options)
		if err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}
		if poll.SelectableCount > 0 && len(selected) > poll.SelectableCount {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, fmt.Sprintf(
				"poll allows at most %d selection(s), got %d", poll.SelectableCount, len(selected))), nil
		}

		info, err := pollMessageInfo(poll)
		if err != nil {
			return mcp.InternalError(err.Error()), nil
		}

		msg, err := deps.WA.BuildPollVote(ctx, info, selected)
		if err != nil {
			switch {
			case errors.Is(err, wa.ErrNotLoggedIn):
				return mcp.NotConnectedError(), nil
			case errors.Is(err, whatsmeow.ErrOriginalMessageSecretNotFound):
				return mcp.NotFoundError(fmt.Sprintf(
					"poll %s cannot be voted on: this device never held its encryption secret, "+
						"which happens when the poll predates pairing or arrived without one", messageID)), nil
			}
			return mcp.InternalError(fmt.Sprintf("build poll vote: %v", err)), nil
		}

		to, err := types.ParseJID(poll.ChatJID)
		if err != nil {
			return mcp.InternalError(fmt.Sprintf("cached poll has unparseable chat jid %q: %v", poll.ChatJID, err)), nil
		}
		resp, err := deps.WA.SendMessage(ctx, to, msg)
		if err != nil {
			if errors.Is(err, wa.ErrNotLoggedIn) {
				return mcp.NotConnectedError(), nil
			}
			return mcp.InternalError(fmt.Sprintf("send poll vote: %v", err)), nil
		}
		ts := resp.Timestamp
		if ts.IsZero() {
			ts = time.Now().UTC()
		}

		// Our own vote never comes back as an event, so get_poll_results would
		// not otherwise see it.
		if err := mirrorOwnVote(ctx, deps, poll, selected, ts); err != nil {
			return mcp.InternalError(fmt.Sprintf("cache own vote: %v", err)), nil
		}

		return VotePollResult{
			MessageID:       string(resp.ID),
			PollMessageID:   poll.MessageID,
			ChatJID:         poll.ChatJID,
			SelectedOptions: selected,
			SentTS:          ts.Unix(),
		}, nil
	}
}

// getPollResults is the handler for the get_poll_results MCP tool. It reads
// only the cache: there is no server-side query for a poll's standings, so the
// locally accumulated votes are the whole of what is knowable.
func getPollResults(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			ChatJID   string `json:"chat_jid"`
			MessageID string `json:"message_id"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}
		chatJID := strings.TrimSpace(in.ChatJID)
		messageID := strings.TrimSpace(in.MessageID)
		if chatJID == "" {
			return mcp.InvalidArgumentError("chat_jid is required"), nil
		}
		if messageID == "" {
			return mcp.InvalidArgumentError("message_id is required"), nil
		}

		poll, err := deps.Cache.GetPoll(ctx, chatJID, messageID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return mcp.NotFoundError(fmt.Sprintf(
					"no cached poll %s in chat %s; %s", messageID, chatJID, pollTallyCaveat)), nil
			}
			return mcp.InternalError(err.Error()), nil
		}
		votes, err := deps.Cache.ListPollVotes(ctx, chatJID, messageID)
		if err != nil {
			return mcp.InternalError(err.Error()), nil
		}

		out := PollResults{
			ChatJID:         poll.ChatJID,
			MessageID:       poll.MessageID,
			Question:        poll.Question,
			SelectableCount: poll.SelectableCount,
			IsFromMe:        poll.IsFromMe,
			Options:         make([]PollOptionResult, 0, len(poll.Options)),
			Caveat:          pollTallyCaveat,
		}
		out.CreatedBy = voterView(ctx, deps, poll.SenderJID)

		byHash := make(map[string]int, len(poll.Options))
		for idx, opt := range poll.Options {
			byHash[opt.Hash] = idx
			out.Options = append(out.Options, PollOptionResult{Option: opt.Name, Voters: []PollVoterView{}})
		}

		for _, vote := range votes {
			if len(vote.SelectedHashes) == 0 {
				// A withdrawn vote: the voter is on record but counts nowhere.
				continue
			}
			out.TotalVoters++
			voter := voterView(ctx, deps, vote.VoterJID)
			for _, hash := range vote.SelectedHashes {
				idx, ok := byHash[hash]
				if !ok {
					out.UnknownOptionVotes++
					continue
				}
				out.Options[idx].Votes++
				out.Options[idx].Voters = append(out.Options[idx].Voters, voter)
			}
		}
		return out, nil
	}
}

// normalisePollOptions trims and validates a proposed ballot. Duplicates are
// rejected rather than deduplicated: an option is identified on the wire by the
// hash of its text, so two equal options would share a vote count and neither
// the sender nor the voter could tell them apart.
func normalisePollOptions(in []string) ([]string, error) {
	if len(in) < 2 {
		return nil, fmt.Errorf("a poll needs at least 2 options, got %d", len(in))
	}
	if len(in) > maxPollOptions {
		return nil, fmt.Errorf("a poll accepts at most %d options, got %d", maxPollOptions, len(in))
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for i, raw := range in {
		opt := strings.TrimSpace(raw)
		if opt == "" {
			return nil, fmt.Errorf("option %d must not be empty", i+1)
		}
		if _, dup := seen[opt]; dup {
			return nil, fmt.Errorf("option %q appears twice; poll options must be unique", opt)
		}
		seen[opt] = struct{}{}
		out = append(out, opt)
	}
	return out, nil
}

// matchPollOptions resolves the caller's option texts against the cached
// ballot, returning them in ballot order. An unrecognised option is an error
// listing what is actually on the ballot — the caller cannot guess the exact
// text, and a hash of the wrong text votes for nothing.
func matchPollOptions(poll cache.Poll, requested []string) ([]string, error) {
	if len(requested) == 0 {
		// Withdrawing a vote is a real operation, expressed as an empty
		// selection; the slice is non-nil so it marshals as [] rather than null.
		return []string{}, nil
	}
	byName := make(map[string]int, len(poll.Options))
	names := make([]string, 0, len(poll.Options))
	for idx, opt := range poll.Options {
		byName[opt.Name] = idx
		names = append(names, opt.Name)
	}
	chosen := make(map[int]struct{}, len(requested))
	for _, raw := range requested {
		opt := strings.TrimSpace(raw)
		idx, ok := byName[opt]
		if !ok {
			return nil, fmt.Errorf("option %q is not on this poll's ballot; valid options are %q", opt, names)
		}
		if _, dup := chosen[idx]; dup {
			return nil, fmt.Errorf("option %q was selected twice", opt)
		}
		chosen[idx] = struct{}{}
	}
	out := make([]string, 0, len(chosen))
	for idx, opt := range poll.Options {
		if _, ok := chosen[idx]; ok {
			out = append(out, opt.Name)
		}
	}
	return out, nil
}

// pollMessageInfo rebuilds the types.MessageInfo whatsmeow needs to key a
// vote's encryption against the poll creation message. Only Chat, Sender, ID,
// IsFromMe and IsGroup take part in that derivation, which is exactly what the
// polls table keeps.
func pollMessageInfo(poll cache.Poll) (*types.MessageInfo, error) {
	chat, err := types.ParseJID(poll.ChatJID)
	if err != nil {
		return nil, fmt.Errorf("cached poll has unparseable chat jid %q: %w", poll.ChatJID, err)
	}
	info := &types.MessageInfo{
		ID: poll.MessageID,
		MessageSource: types.MessageSource{
			Chat:     chat,
			IsFromMe: poll.IsFromMe,
			IsGroup:  chat.Server == types.GroupServer,
		},
		Timestamp: poll.Timestamp,
	}
	if poll.SenderJID != "" {
		sender, err := types.ParseJID(poll.SenderJID)
		if err != nil {
			return nil, fmt.Errorf("cached poll has unparseable sender jid %q: %w", poll.SenderJID, err)
		}
		info.Sender = sender
	}
	return info, nil
}

// mirrorOutboundPoll writes the poll we just sent into the cache: the chat and
// messages row via the shared outbound mirror (so it shows up in list_messages
// like any other message, which is where a caller gets the id it needs), plus
// the ballot, which only polls have.
func mirrorOutboundPoll(ctx context.Context, store *cache.Store, to, ownJID types.JID, msgID, question string, options []string, selectable int, ts time.Time) error {
	if store == nil {
		return nil
	}
	chatJID := to.String()
	senderJID := ownSenderJID(ownJID)
	if err := mirrorOutboundRow(ctx, store, to, cache.Message{
		ID:        msgID,
		ChatJID:   chatJID,
		SenderJID: senderJID,
		Timestamp: ts,
		Kind:      cache.KindPoll,
		Body:      question,
		IsFromMe:  true,
	}); err != nil {
		return err
	}
	poll := cache.Poll{
		ChatJID:         chatJID,
		MessageID:       msgID,
		Question:        question,
		SelectableCount: selectable,
		SenderJID:       senderJID,
		IsFromMe:        true,
		Timestamp:       ts,
	}
	for _, opt := range options {
		poll.Options = append(poll.Options, cache.PollOption{Name: opt})
	}
	if err := store.UpsertPoll(ctx, poll); err != nil {
		return fmt.Errorf("upsert poll: %w", err)
	}
	return nil
}

// mirrorOwnVote records the vote this device just cast. WhatsApp echoes a
// vote to the other participants but not back to its sender, so without this
// the tally would omit us.
func mirrorOwnVote(ctx context.Context, deps Deps, poll cache.Poll, selected []string, ts time.Time) error {
	if deps.Cache == nil {
		return nil
	}
	voter := deps.WA.OwnJID()
	if voter.IsEmpty() {
		// Nothing to key the vote on; the send already succeeded, so this is
		// reported rather than swallowed.
		return errors.New("own JID unknown")
	}
	hashes := make([]string, 0, len(selected))
	for _, opt := range selected {
		hashes = append(hashes, cache.HashPollOption(opt))
	}
	return deps.Cache.UpsertPollVote(ctx, cache.PollVote{
		ChatJID:        poll.ChatJID,
		PollMessageID:  poll.MessageID,
		VoterJID:       voter.ToNonAD().String(),
		SelectedHashes: hashes,
		Timestamp:      ts,
	})
}

// voterView resolves a JID to a displayable identity, following the same
// lid → phone alias hop resolve_jid does so a voter who appears under their
// LID still gets their contact name.
func voterView(ctx context.Context, deps Deps, jid string) PollVoterView {
	out := PollVoterView{JID: jid}
	if jid == "" || deps.Cache == nil {
		return out
	}
	parsed, err := types.ParseJID(jid)
	if err != nil {
		return out
	}
	phoneJID, hasPhone, err := phoneJIDFor(ctx, deps.Cache, parsed)
	if err != nil {
		return out
	}
	row, ok, err := lookupContact(ctx, deps.Cache, contactIdentities(parsed, phoneJID, hasPhone))
	if err != nil || !ok {
		return out
	}
	out.Name = realDisplayName(row)
	return out
}
