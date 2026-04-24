package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
