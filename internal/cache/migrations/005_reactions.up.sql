-- reactions stores emoji reactions attached to a message. Reactions are not
-- messages: WhatsApp allows exactly one reaction per sender per message, a new
-- emoji replaces the sender's previous one, and an empty emoji removes it. That
-- maps to a row keyed by (chat_jid, target_id, sender_jid) rather than a column
-- on messages or a new messages.kind.
--
-- NOTE: there is deliberately no foreign key to messages. A reaction can arrive
-- for a message the cache has not ingested yet (history gaps, out-of-order
-- delivery); keeping the row means the reaction shows up as soon as its target
-- is backfilled.
--
-- Removals DELETE the row rather than storing an empty emoji, so "no reaction"
-- has exactly one representation.
CREATE TABLE reactions (
    chat_jid   TEXT    NOT NULL,
    target_id  TEXT    NOT NULL,  -- messages.id of the message reacted to
    sender_jid TEXT    NOT NULL,  -- the reactor ('' when unknown)
    emoji      TEXT    NOT NULL,
    ts         INTEGER NOT NULL,  -- the reaction's own timestamp, unix seconds
    is_from_me INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_jid, target_id, sender_jid)
);

-- The primary key already covers the (chat_jid, target_id) prefix every read
-- uses to fetch a message's reactions, so no additional index is needed.
