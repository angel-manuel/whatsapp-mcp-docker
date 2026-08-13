-- Presence observed for a contact via SubscribePresence. The WhatsApp server
-- only pushes *events.Presence for JIDs this device has explicitly subscribed
-- to (and only while this device is itself online), so these columns stay at
-- their defaults for every contact nobody ever subscribed to.
--
-- presence_updated_at is what distinguishes "never observed" (0) from
-- "observed, and they were offline" (non-zero with presence_online = 0).
-- presence_last_seen_ts stays 0 when the contact hides their last-seen time,
-- which is independent of whether the presence event itself arrived.
ALTER TABLE contacts ADD COLUMN presence_online       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE contacts ADD COLUMN presence_last_seen_ts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE contacts ADD COLUMN presence_updated_at   INTEGER NOT NULL DEFAULT 0;
