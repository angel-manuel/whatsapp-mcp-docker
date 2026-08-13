-- Poll support. WhatsApp poll tallies cannot be fetched on demand: whatsmeow
-- exposes no "read the current results" call, and the server never replays
-- them. Votes only ever arrive as `*events.Message` carrying an encrypted
-- PollUpdateMessage, so the running tally has to be accumulated locally as
-- those events land (see internal/cache/handler.go::handlePollVote).
--
-- Consequence worth stating plainly: the tally is "as observed by this
-- device". Votes cast while the container was down, or before the device was
-- linked, are lost — WhatsApp will not resend them.

-- polls holds the poll question itself, keyed exactly like a messages row so
-- callers can pass the (chat_jid, message_id) pair they already get back from
-- list_messages / get_message_context. sender_jid + is_from_me are kept
-- because whatsmeow's BuildPollVote needs a types.MessageInfo for the poll
-- creation message, and those two fields are the part that cannot be
-- reconstructed from the JID alone.
CREATE TABLE polls (
    chat_jid         TEXT NOT NULL,
    message_id       TEXT NOT NULL,
    question         TEXT NOT NULL DEFAULT '',
    selectable_count INTEGER NOT NULL DEFAULT 0,
    sender_jid       TEXT NOT NULL DEFAULT '',
    is_from_me       INTEGER NOT NULL DEFAULT 0,
    ts               INTEGER NOT NULL DEFAULT 0,
    updated_at       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_jid, message_id)
);

-- poll_options stores the ballot in display order. `hash` is the lowercase
-- hex SHA-256 of `name` — the same digest whatsmeow.HashPollOptions puts on
-- the wire — because a decrypted vote identifies its selections by hash and
-- never by name.
CREATE TABLE poll_options (
    chat_jid   TEXT NOT NULL,
    message_id TEXT NOT NULL,
    idx        INTEGER NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    hash       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (chat_jid, message_id, idx)
);

CREATE INDEX idx_poll_options_hash ON poll_options(chat_jid, message_id, hash);

-- poll_votes holds one row per voter, not per selection: a WhatsApp poll
-- update always carries the voter's *complete* current selection, so the
-- newest event replaces the previous one wholesale. selected_hashes is that
-- selection as a JSON array of hex hashes; an empty array is a real state
-- (the voter cleared their vote) and must not be confused with "never voted".
CREATE TABLE poll_votes (
    chat_jid        TEXT NOT NULL,
    poll_message_id TEXT NOT NULL,
    voter_jid       TEXT NOT NULL,
    selected_hashes TEXT NOT NULL DEFAULT '[]',
    ts              INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_jid, poll_message_id, voter_jid)
);

CREATE INDEX idx_poll_votes_poll ON poll_votes(chat_jid, poll_message_id);
