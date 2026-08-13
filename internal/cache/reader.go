package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ContactRow is the read-side projection of a single contact, merged with
// its optional user-defined nickname. Zero values marshal to empty strings
// and are preserved by the tool layer so clients can distinguish "not
// known" from "known to be empty".
type ContactRow struct {
	// JID is the primary key and always non-empty.
	JID string
	// PushName is the display-name WhatsApp broadcasts in messages.
	PushName string
	// BusinessName is the verified business name (WABA contacts only).
	BusinessName string
	// FirstName / FullName come from the address-book sync action.
	FirstName string
	FullName  string
	// Nickname is the user-defined alias stored locally (may be empty).
	Nickname string
}

// Phone returns the `user` component of the JID, i.e. the phone-number
// portion before the `@server` suffix. Non-LID/non-phone JIDs still parse
// to whatever sits before the `@`, which is the convention the Python
// reference uses.
func (c ContactRow) Phone() string {
	if idx := strings.IndexByte(c.JID, '@'); idx >= 0 {
		return c.JID[:idx]
	}
	return c.JID
}

// DisplayName returns the best human-readable label for the contact using
// the Python reference cascade: nickname > full_name > push_name >
// first_name > business_name > phone number. It never returns empty —
// callers that need to distinguish "real name known" from "JID fallback"
// should compare against Phone() / check the underlying fields.
func (c ContactRow) DisplayName() string {
	switch {
	case c.Nickname != "":
		return c.Nickname
	case c.FullName != "":
		return c.FullName
	case c.PushName != "":
		return c.PushName
	case c.FirstName != "":
		return c.FirstName
	case c.BusinessName != "":
		return c.BusinessName
	default:
		return c.Phone()
	}
}

// contactsSelect is the shared SELECT over contacts + nicknames used by
// every read path. Ordering prefers the richest display field available so
// "list all" and "search" present a stable, human-friendly sort.
const contactsSelect = `
SELECT c.jid,
       c.push_name,
       c.business_name,
       c.first_name,
       c.full_name,
       COALESCE(n.nickname, '') AS nickname
  FROM contacts c
  LEFT JOIN nicknames n ON n.jid = c.jid
`

const contactsOrder = `
ORDER BY
    CASE
        WHEN c.full_name     != '' THEN LOWER(c.full_name)
        WHEN c.push_name     != '' THEN LOWER(c.push_name)
        WHEN c.first_name    != '' THEN LOWER(c.first_name)
        WHEN c.business_name != '' THEN LOWER(c.business_name)
        ELSE LOWER(c.jid)
    END,
    c.jid
`

// ListAllContacts returns contacts ordered by best-available display name,
// skipping group JIDs. `limit` defaults to 100 when <=0 and is capped at
// 500. `page` is zero-indexed; a negative page is treated as 0.
func (s *Store) ListAllContacts(ctx context.Context, limit, page int) ([]ContactRow, error) {
	limit, page = normalisePagination(limit, page, 100, 500)
	q := contactsSelect + `WHERE c.jid NOT LIKE '%@g.us'` + contactsOrder + ` LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, limit, limit*page)
	if err != nil {
		return nil, fmt.Errorf("cache: list contacts: %w", err)
	}
	defer rows.Close()
	return scanContacts(rows)
}

// SearchContacts performs a case-insensitive substring match against
// push_name / full_name / first_name / business_name / nickname / jid
// (the JID match covers phone-number searches since the user component
// of a regular JID is the phone number). Group JIDs are excluded.
func (s *Store) SearchContacts(ctx context.Context, query string, limit, page int) ([]ContactRow, error) {
	limit, page = normalisePagination(limit, page, 50, 200)
	pattern := "%" + strings.ToLower(strings.TrimSpace(query)) + "%"
	q := contactsSelect + `
WHERE c.jid NOT LIKE '%@g.us'
  AND (
      LOWER(c.push_name)              LIKE ?
   OR LOWER(c.full_name)              LIKE ?
   OR LOWER(c.first_name)             LIKE ?
   OR LOWER(c.business_name)          LIKE ?
   OR LOWER(COALESCE(n.nickname, '')) LIKE ?
   OR LOWER(c.jid)                    LIKE ?
  )
` + contactsOrder + ` LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q,
		pattern, pattern, pattern, pattern, pattern, pattern,
		limit, limit*page,
	)
	if err != nil {
		return nil, fmt.Errorf("cache: search contacts: %w", err)
	}
	defer rows.Close()
	return scanContacts(rows)
}

// GetContactByJID returns the contact row for jid, joined with its
// nickname. Returns sql.ErrNoRows when the JID is not present in either
// table (including when only a nickname exists without a backing contact
// row — nicknames-only rows are surfaced separately by GetNicknameByJID).
func (s *Store) GetContactByJID(ctx context.Context, jid string) (ContactRow, error) {
	if jid == "" {
		return ContactRow{}, errors.New("cache: GetContactByJID: jid required")
	}
	q := contactsSelect + `WHERE c.jid = ? LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, jid)
	var out ContactRow
	if err := row.Scan(&out.JID, &out.PushName, &out.BusinessName, &out.FirstName, &out.FullName, &out.Nickname); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ContactRow{}, sql.ErrNoRows
		}
		return ContactRow{}, fmt.Errorf("cache: get contact %s: %w", jid, err)
	}
	return out, nil
}

// GetNicknameByJID returns the locally-stored nickname for jid, or empty
// string if none is set. Nickname-only rows (no matching contact row) are
// still visible through this call.
func (s *Store) GetNicknameByJID(ctx context.Context, jid string) (string, error) {
	if jid == "" {
		return "", errors.New("cache: GetNicknameByJID: jid required")
	}
	var nick string
	err := s.db.QueryRowContext(ctx, `SELECT nickname FROM nicknames WHERE jid = ?`, jid).Scan(&nick)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("cache: get nickname %s: %w", jid, err)
	}
	return nick, nil
}

// GetChatNameByJID returns the cached chat name for jid (used by the
// group_info tool as an opportunistic fallback when whatsmeow is offline
// but the caller only needs the group name). Empty string + nil is
// returned when the chat row exists but has no learned name.
func (s *Store) GetChatNameByJID(ctx context.Context, jid string) (string, error) {
	if jid == "" {
		return "", errors.New("cache: GetChatNameByJID: jid required")
	}
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM chats WHERE jid = ?`, jid).Scan(&name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", sql.ErrNoRows
		}
		return "", fmt.Errorf("cache: get chat %s: %w", jid, err)
	}
	return name, nil
}

// MediaRow is the read-side projection of the media columns on a message
// row: everything needed to re-request the CDN object plus what to call the
// resulting file. Non-media messages yield a row with Kind text/other and a
// nil Key — see HasMedia.
type MediaRow struct {
	ChatJID  string
	ID       string
	Kind     MessageKind
	Mime     string
	Filename string
	// URL is the absolute CDN URL captured at ingest. It expires; DirectPath
	// is the durable locator and should be preferred when non-empty.
	URL string
	// DirectPath is empty for rows ingested before the
	// 004_media_direct_path migration. Those can only be downloaded through
	// URL, and only until it expires.
	DirectPath string
	Key        []byte
	SHA256     []byte
	EncSHA256  []byte
	Length     uint64
	// Timestamp is the message time, used to synthesise a stable download
	// filename for the kinds that carry none (everything but documents).
	Timestamp time.Time
}

// HasMedia reports whether the row carries a downloadable attachment. A
// media kind without a media key is not downloadable (the payload is
// encrypted with it), so both are required.
func (m MediaRow) HasMedia() bool {
	switch m.Kind {
	case KindImage, KindVideo, KindAudio, KindDocument, KindSticker:
		return len(m.Key) > 0
	default:
		return false
	}
}

// GetMessageMedia returns the media locator for the message identified by
// (chatJID, id). Returns sql.ErrNoRows when no such message is cached; a
// message that exists but carries no attachment is returned with HasMedia
// false so callers can tell the two cases apart.
func (s *Store) GetMessageMedia(ctx context.Context, chatJID, id string) (MediaRow, error) {
	if chatJID == "" || id == "" {
		return MediaRow{}, errors.New("cache: GetMessageMedia: chatJID and id required")
	}
	var (
		out    MediaRow
		kind   string
		length int64
		ts     int64
	)
	err := s.db.QueryRowContext(ctx, `
SELECT chat_jid, id, kind, ts, media_mime, media_filename, media_url, media_direct_path,
       media_key, media_sha256, media_enc_sha256, media_length
  FROM messages
 WHERE chat_jid = ? AND id = ?
 LIMIT 1
`, chatJID, id).Scan(
		&out.ChatJID, &out.ID, &kind, &ts, &out.Mime, &out.Filename, &out.URL, &out.DirectPath,
		&out.Key, &out.SHA256, &out.EncSHA256, &length,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MediaRow{}, sql.ErrNoRows
		}
		return MediaRow{}, fmt.Errorf("cache: get message media %s/%s: %w", chatJID, id, err)
	}
	out.Kind = MessageKind(kind)
	// A row with no recorded ts keeps the zero time.Time rather than
	// becoming 1970-01-01, so callers can tell "unknown" from "epoch".
	if ts > 0 {
		out.Timestamp = time.Unix(ts, 0).UTC()
	}
	if length > 0 {
		out.Length = uint64(length)
	}
	return out, nil
}

// GetMessageSender returns the author of the message identified by
// (chatJID, id) — the sender JID together with whether we are that sender.
// Returns sql.ErrNoRows when no such message is cached.
//
// send_reaction needs this because whatsmeow's BuildReaction keys the reaction
// off the *target message's* author, not the reactor: passing our own JID for
// someone else's message builds a FromMe key and misattributes the reaction.
func (s *Store) GetMessageSender(ctx context.Context, chatJID, id string) (string, bool, error) {
	if chatJID == "" || id == "" {
		return "", false, errors.New("cache: GetMessageSender: chatJID and id required")
	}
	var (
		sender   string
		isFromMe int
	)
	err := s.db.QueryRowContext(ctx, `
SELECT sender_jid, is_from_me
  FROM messages
 WHERE chat_jid = ? AND id = ?
 LIMIT 1
`, chatJID, id).Scan(&sender, &isFromMe)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, sql.ErrNoRows
		}
		return "", false, fmt.Errorf("cache: get message sender %s/%s: %w", chatJID, id, err)
	}
	return sender, isFromMe != 0, nil
}

// ResolveLinkedJIDs returns jid together with every other identity linked to
// it through jid_aliases — i.e. a contact's phone-number JID and privacy LID.
// The input jid is always returned first (and only once); linked identities
// follow in a stable order. When jid has no recorded alias the result is just
// [jid], so callers can treat the single-identity case uniformly.
func (s *Store) ResolveLinkedJIDs(ctx context.Context, jid string) ([]string, error) {
	if jid == "" {
		return nil, errors.New("cache: ResolveLinkedJIDs: jid required")
	}
	out := []string{jid}
	seen := map[string]bool{jid: true}
	rows, err := s.db.QueryContext(ctx, `
SELECT pn_jid FROM jid_aliases WHERE lid_jid = ?
UNION
SELECT lid_jid FROM jid_aliases WHERE pn_jid = ?
`, jid, jid)
	if err != nil {
		return nil, fmt.Errorf("cache: resolve linked jids for %s: %w", jid, err)
	}
	defer rows.Close()
	for rows.Next() {
		var linked string
		if err := rows.Scan(&linked); err != nil {
			return nil, fmt.Errorf("cache: scan linked jid: %w", err)
		}
		if linked == "" || seen[linked] {
			continue
		}
		seen[linked] = true
		out = append(out, linked)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cache: iterate linked jids: %w", err)
	}
	return out, nil
}

func scanContacts(rows *sql.Rows) ([]ContactRow, error) {
	var out []ContactRow
	for rows.Next() {
		var c ContactRow
		if err := rows.Scan(&c.JID, &c.PushName, &c.BusinessName, &c.FirstName, &c.FullName, &c.Nickname); err != nil {
			return nil, fmt.Errorf("cache: scan contact: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cache: iterate contacts: %w", err)
	}
	return out, nil
}

// normalisePagination clamps limit and page into sensible ranges. Passing
// limit <= 0 yields the default; values above hardMax are capped.
func normalisePagination(limit, page, defaultLimit, hardMax int) (int, int) {
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > hardMax {
		limit = hardMax
	}
	if page < 0 {
		page = 0
	}
	return limit, page
}
