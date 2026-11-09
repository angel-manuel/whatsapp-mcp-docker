-- v0 -> v1: Latest cache schema
-- One schema for SQLite and Postgres. Lines that differ are fenced with
-- dbutil's "-- only: <dialect>" directives. Integer columns are BIGINT
-- everywhere: SQLite gives them INTEGER affinity, and on Postgres unix
-- timestamps and media lengths must not be capped at int4.

CREATE TABLE chats (
    jid             TEXT PRIMARY KEY,
    name            TEXT   NOT NULL DEFAULT '',
    is_group        BIGINT NOT NULL DEFAULT 0,
    -- chat_type distinguishes direct chats, regular groups, newsletters and
    -- community parents. is_group stays for back-compat.
    chat_type       TEXT   NOT NULL DEFAULT 'direct',
    last_message_ts BIGINT NOT NULL DEFAULT 0,
    unread_count    BIGINT NOT NULL DEFAULT 0,
    archived        BIGINT NOT NULL DEFAULT 0,
    pinned          BIGINT NOT NULL DEFAULT 0,
    muted_until     BIGINT NOT NULL DEFAULT 0,
    updated_at      BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX idx_chats_chat_type ON chats(chat_type);

CREATE TABLE messages (
    chat_jid          TEXT   NOT NULL,
    id                TEXT   NOT NULL,
    sender_jid        TEXT   NOT NULL DEFAULT '',
    sender_push_name  TEXT   NOT NULL DEFAULT '',
    ts                BIGINT NOT NULL,
    kind              TEXT   NOT NULL DEFAULT 'text',
    body              TEXT   NOT NULL DEFAULT '',
    reply_to_id       TEXT   NOT NULL DEFAULT '',
    is_from_me        BIGINT NOT NULL DEFAULT 0,
    edited            BIGINT NOT NULL DEFAULT 0,
    edited_at         BIGINT NOT NULL DEFAULT 0,
    deleted           BIGINT NOT NULL DEFAULT 0,
    deleted_at        BIGINT NOT NULL DEFAULT 0,
    media_mime        TEXT   NOT NULL DEFAULT '',
    media_filename    TEXT   NOT NULL DEFAULT '',
    media_url         TEXT   NOT NULL DEFAULT '',
    -- only: sqlite for next 3 lines
    media_key         BLOB,
    media_sha256      BLOB,
    media_enc_sha256  BLOB,
    -- only: postgres for next 3 lines
    media_key         BYTEA,
    media_sha256      BYTEA,
    media_enc_sha256  BYTEA,
    media_length      BIGINT NOT NULL DEFAULT 0,
    -- media_direct_path is the CDN-relative path whatsmeow needs to
    -- re-request an attachment; plain media_url values go stale.
    media_direct_path TEXT   NOT NULL DEFAULT '',
    -- Full-text search on Postgres. SQLite uses the messages_fts table below.
    -- only: postgres
    body_tsv          tsvector GENERATED ALWAYS AS (to_tsvector('simple', body)) STORED,
    PRIMARY KEY (chat_jid, id)
);

CREATE INDEX idx_messages_chat_ts   ON messages(chat_jid, ts);
CREATE INDEX idx_messages_sender_ts ON messages(sender_jid, ts);
-- only: postgres
CREATE INDEX idx_messages_body_tsv ON messages USING GIN (body_tsv);

-- only: sqlite until "end only"
CREATE VIRTUAL TABLE messages_fts USING fts5(
    body,
    content='messages',
    content_rowid='rowid'
);

CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, body) VALUES (new.rowid, new.body);
END;

CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, body) VALUES ('delete', old.rowid, old.body);
END;

CREATE TRIGGER messages_au AFTER UPDATE OF body ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, body) VALUES ('delete', old.rowid, old.body);
    INSERT INTO messages_fts(rowid, body) VALUES (new.rowid, new.body);
END;
-- end only sqlite

CREATE TABLE contacts (
    jid                   TEXT PRIMARY KEY,
    push_name             TEXT   NOT NULL DEFAULT '',
    business_name         TEXT   NOT NULL DEFAULT '',
    first_name            TEXT   NOT NULL DEFAULT '',
    full_name             TEXT   NOT NULL DEFAULT '',
    updated_at            BIGINT NOT NULL DEFAULT 0,
    -- Presence observed via SubscribePresence. presence_updated_at = 0 means
    -- "never observed"; presence_last_seen_ts = 0 means last-seen is hidden.
    presence_online       BIGINT NOT NULL DEFAULT 0,
    presence_last_seen_ts BIGINT NOT NULL DEFAULT 0,
    presence_updated_at   BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE nicknames (
    jid        TEXT PRIMARY KEY,
    nickname   TEXT   NOT NULL DEFAULT '',
    note       TEXT   NOT NULL DEFAULT '',
    updated_at BIGINT NOT NULL DEFAULT 0
);

-- jid_aliases links a contact's phone-number JID and privacy LID, so the
-- read side can merge one human's split conversation (see get_conversation).
CREATE TABLE jid_aliases (
    lid_jid    TEXT   NOT NULL,
    pn_jid     TEXT   NOT NULL,
    updated_at BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (lid_jid, pn_jid)
);

CREATE INDEX idx_jid_aliases_pn ON jid_aliases(pn_jid);

-- One reaction per sender per message; removals delete the row. No foreign
-- key to messages: a reaction may arrive before its target is ingested.
CREATE TABLE reactions (
    chat_jid   TEXT   NOT NULL,
    target_id  TEXT   NOT NULL,
    sender_jid TEXT   NOT NULL,
    emoji      TEXT   NOT NULL,
    ts         BIGINT NOT NULL,
    is_from_me BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_jid, target_id, sender_jid)
);

-- Polls. Tallies are accumulated locally from vote events as this device
-- observes them; WhatsApp never replays them.
CREATE TABLE polls (
    chat_jid         TEXT   NOT NULL,
    message_id       TEXT   NOT NULL,
    question         TEXT   NOT NULL DEFAULT '',
    selectable_count BIGINT NOT NULL DEFAULT 0,
    sender_jid       TEXT   NOT NULL DEFAULT '',
    is_from_me       BIGINT NOT NULL DEFAULT 0,
    ts               BIGINT NOT NULL DEFAULT 0,
    updated_at       BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_jid, message_id)
);

-- hash is the lowercase hex SHA-256 of name, as whatsmeow.HashPollOptions.
CREATE TABLE poll_options (
    chat_jid   TEXT   NOT NULL,
    message_id TEXT   NOT NULL,
    idx        BIGINT NOT NULL,
    name       TEXT   NOT NULL DEFAULT '',
    hash       TEXT   NOT NULL DEFAULT '',
    PRIMARY KEY (chat_jid, message_id, idx)
);

CREATE INDEX idx_poll_options_hash ON poll_options(chat_jid, message_id, hash);

-- One row per voter; selected_hashes is the voter's complete current
-- selection as a JSON array ('[]' = cleared vote, not "never voted").
CREATE TABLE poll_votes (
    chat_jid        TEXT   NOT NULL,
    poll_message_id TEXT   NOT NULL,
    voter_jid       TEXT   NOT NULL,
    selected_hashes TEXT   NOT NULL DEFAULT '[]',
    ts              BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_jid, poll_message_id, voter_jid)
);

CREATE INDEX idx_poll_votes_poll ON poll_votes(chat_jid, poll_message_id);
