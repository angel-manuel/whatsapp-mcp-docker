-- Add chat_type column to distinguish direct chats, regular groups, newsletters,
-- and community parents. is_group stays for back-compat; chat_type is the
-- preferred discriminator going forward.
ALTER TABLE chats ADD COLUMN chat_type TEXT NOT NULL DEFAULT 'direct';

-- Backfill from is_group + JID suffix. Newsletters (@newsletter) and any
-- @g.us JID classed as group; status broadcast and @lid JIDs fall through to
-- 'direct'.
UPDATE chats
   SET chat_type = CASE
       WHEN is_group = 1            THEN 'group'
       WHEN jid LIKE '%@newsletter' THEN 'newsletter'
       ELSE                              'direct'
   END;

CREATE INDEX idx_chats_chat_type ON chats(chat_type);
