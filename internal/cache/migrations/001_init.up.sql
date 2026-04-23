CREATE TABLE chats (
    jid             TEXT PRIMARY KEY,
    name            TEXT NOT NULL DEFAULT '',
    is_group        INTEGER NOT NULL DEFAULT 0,
    last_message_ts INTEGER NOT NULL DEFAULT 0,
    unread_count    INTEGER NOT NULL DEFAULT 0,
    archived        INTEGER NOT NULL DEFAULT 0,
    pinned          INTEGER NOT NULL DEFAULT 0,
    muted_until     INTEGER NOT NULL DEFAULT 0,
    updated_at      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE messages (
    chat_jid          TEXT NOT NULL,
    id                TEXT NOT NULL,
    sender_jid        TEXT NOT NULL DEFAULT '',
    sender_push_name  TEXT NOT NULL DEFAULT '',
    ts                INTEGER NOT NULL,
    kind              TEXT NOT NULL DEFAULT 'text',
    body              TEXT NOT NULL DEFAULT '',
    reply_to_id       TEXT NOT NULL DEFAULT '',
    is_from_me        INTEGER NOT NULL DEFAULT 0,
    edited            INTEGER NOT NULL DEFAULT 0,
    edited_at         INTEGER NOT NULL DEFAULT 0,
    deleted           INTEGER NOT NULL DEFAULT 0,
    deleted_at        INTEGER NOT NULL DEFAULT 0,
    media_mime        TEXT NOT NULL DEFAULT '',
    media_filename    TEXT NOT NULL DEFAULT '',
    media_url         TEXT NOT NULL DEFAULT '',
    media_key         BLOB,
    media_sha256      BLOB,
    media_enc_sha256  BLOB,
    media_length      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_jid, id)
);

CREATE INDEX idx_messages_chat_ts   ON messages(chat_jid, ts);
CREATE INDEX idx_messages_sender_ts ON messages(sender_jid, ts);

CREATE TABLE contacts (
    jid           TEXT PRIMARY KEY,
    push_name     TEXT NOT NULL DEFAULT '',
    business_name TEXT NOT NULL DEFAULT '',
    first_name    TEXT NOT NULL DEFAULT '',
    full_name     TEXT NOT NULL DEFAULT '',
    updated_at    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE nicknames (
    jid        TEXT PRIMARY KEY,
    nickname   TEXT NOT NULL DEFAULT '',
    note       TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0
);

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
