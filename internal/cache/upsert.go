package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UpsertChat inserts or updates a chat row by JID.
//
// Name is only overwritten when the incoming value is non-empty (so a bare
// message event doesn't clobber a previously-learned group name). The
// last_message_ts column advances monotonically — an older timestamp from a
// back-fill never regresses the freshest message we have seen.
func (s *Store) UpsertChat(ctx context.Context, c Chat) error {
	if c.JID == "" {
		return errors.New("cache: UpsertChat: JID required")
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO chats (jid, name, is_group, last_message_ts, unread_count, archived, pinned, muted_until, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(jid) DO UPDATE SET
    name            = CASE WHEN excluded.name != '' THEN excluded.name ELSE chats.name END,
    is_group        = excluded.is_group,
    last_message_ts = MAX(chats.last_message_ts, excluded.last_message_ts),
    unread_count    = excluded.unread_count,
    archived        = excluded.archived,
    pinned          = excluded.pinned,
    muted_until     = excluded.muted_until,
    updated_at      = excluded.updated_at
`,
		c.JID, c.Name, boolToInt(c.IsGroup), unixSeconds(c.LastMessageTS), c.UnreadCount,
		boolToInt(c.Archived), boolToInt(c.Pinned), unixSeconds(c.MutedUntil), now,
	)
	if err != nil {
		return fmt.Errorf("cache: upsert chat %s: %w", c.JID, err)
	}
	return nil
}

// UpsertContact inserts or updates a contact row by JID. Empty incoming
// string fields do NOT overwrite existing non-empty values — contact facts
// arrive piecemeal (PushName from a message, FullName from a contact-action
// sync) and each channel should only set what it knows.
func (s *Store) UpsertContact(ctx context.Context, c Contact) error {
	if c.JID == "" {
		return errors.New("cache: UpsertContact: JID required")
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO contacts (jid, push_name, business_name, first_name, full_name, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(jid) DO UPDATE SET
    push_name     = CASE WHEN excluded.push_name     != '' THEN excluded.push_name     ELSE contacts.push_name     END,
    business_name = CASE WHEN excluded.business_name != '' THEN excluded.business_name ELSE contacts.business_name END,
    first_name    = CASE WHEN excluded.first_name    != '' THEN excluded.first_name    ELSE contacts.first_name    END,
    full_name     = CASE WHEN excluded.full_name     != '' THEN excluded.full_name     ELSE contacts.full_name     END,
    updated_at    = excluded.updated_at
`,
		c.JID, c.PushName, c.BusinessName, c.FirstName, c.FullName, now,
	)
	if err != nil {
		return fmt.Errorf("cache: upsert contact %s: %w", c.JID, err)
	}
	return nil
}

// UpsertNickname inserts or updates a local nickname by JID.
func (s *Store) UpsertNickname(ctx context.Context, n Nickname) error {
	if n.JID == "" {
		return errors.New("cache: UpsertNickname: JID required")
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO nicknames (jid, nickname, note, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(jid) DO UPDATE SET
    nickname   = excluded.nickname,
    note       = excluded.note,
    updated_at = excluded.updated_at
`, n.JID, n.Nickname, n.Note, now)
	if err != nil {
		return fmt.Errorf("cache: upsert nickname %s: %w", n.JID, err)
	}
	return nil
}

// DeleteNickname removes a nickname row. Returns nil if the row didn't exist.
func (s *Store) DeleteNickname(ctx context.Context, jid string) error {
	if jid == "" {
		return errors.New("cache: DeleteNickname: jid required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM nicknames WHERE jid = ?`, jid)
	if err != nil {
		return fmt.Errorf("cache: delete nickname %s: %w", jid, err)
	}
	return nil
}

// InsertMessage persists an incoming message. Conflicts on (chat_jid, id) are
// ignored so replays (e.g. the same message arriving via both live event and
// history sync) don't overwrite an already-stored row — in particular they
// won't clobber a row that has since been marked edited/deleted.
func (s *Store) InsertMessage(ctx context.Context, m Message) error {
	if m.ChatJID == "" || m.ID == "" {
		return errors.New("cache: InsertMessage: ChatJID and ID required")
	}
	kind := m.Kind
	if kind == "" {
		kind = KindText
	}
	var media Media
	if m.Media != nil {
		media = *m.Media
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO messages (
    chat_jid, id, sender_jid, sender_push_name, ts, kind, body, reply_to_id,
    is_from_me,
    media_mime, media_filename, media_url, media_key, media_sha256, media_enc_sha256, media_length
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(chat_jid, id) DO NOTHING
`,
		m.ChatJID, m.ID, m.SenderJID, m.SenderPushName, unixSeconds(m.Timestamp),
		string(kind), m.Body, m.ReplyToID,
		boolToInt(m.IsFromMe),
		media.Mime, media.Filename, media.URL, media.Key, media.SHA256, media.EncSHA256, media.Length,
	)
	if err != nil {
		return fmt.Errorf("cache: insert message %s/%s: %w", m.ChatJID, m.ID, err)
	}
	return nil
}

// MarkMessageEdited flips the edited flag and replaces body with the new
// content. The row is preserved (not deleted) so reply threads and
// get_message_context still resolve. Returns sql.ErrNoRows if the original
// message was never stored.
func (s *Store) MarkMessageEdited(ctx context.Context, chatJID, id, newBody string, editedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE messages
   SET body = ?, edited = 1, edited_at = ?
 WHERE chat_jid = ? AND id = ?
`, newBody, unixSeconds(editedAt), chatJID, id)
	if err != nil {
		return fmt.Errorf("cache: mark edited %s/%s: %w", chatJID, id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cache: mark edited rows %s/%s: %w", chatJID, id, err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkMessageDeleted flips the deleted flag and clears the searchable body,
// but keeps the row so replies / thread context still resolve. Returns
// sql.ErrNoRows if the target was never stored.
func (s *Store) MarkMessageDeleted(ctx context.Context, chatJID, id string, deletedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE messages
   SET body = '', deleted = 1, deleted_at = ?
 WHERE chat_jid = ? AND id = ?
`, unixSeconds(deletedAt), chatJID, id)
	if err != nil {
		return fmt.Errorf("cache: mark deleted %s/%s: %w", chatJID, id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cache: mark deleted rows %s/%s: %w", chatJID, id, err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func unixSeconds(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
