-- jid_aliases links a contact's two WhatsApp identities: the phone-number
-- JID (…@s.whatsapp.net) and the privacy LID (…@lid). WhatsApp splits a
-- single human's messages across both addresses, so this pairing lets the
-- read side merge them into one conversation (see get_conversation).
--
-- Rows are populated opportunistically from the alternate address that
-- whatsmeow attaches to every live message (events.Message.Info.SenderAlt);
-- the table is empty until such a message is seen, and readers degrade to a
-- single-identity view in that case.
CREATE TABLE jid_aliases (
    lid_jid    TEXT NOT NULL,
    pn_jid     TEXT NOT NULL,
    updated_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (lid_jid, pn_jid)
);

CREATE INDEX idx_jid_aliases_pn ON jid_aliases(pn_jid);
