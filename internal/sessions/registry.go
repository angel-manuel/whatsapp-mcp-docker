package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // sqlite driver
)

// Record is one registered session as persisted: never the token itself.
type Record struct {
	ID        string
	Hash      TokenHash
	CreatedAt time.Time
}

// Registry persists the session id → token-hash mapping in a small SQLite
// database at the data root. Per-session state lives in each session's own
// directory, not here.
type Registry struct {
	db *sql.DB
}

const registrySchema = `
CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT    PRIMARY KEY,
	token_hash BLOB    NOT NULL UNIQUE,
	created_at INTEGER NOT NULL
) STRICT;
`

// OpenRegistry opens (creating if needed) the registry database at path.
func OpenRegistry(ctx context.Context, path string) (*Registry, error) {
	v := url.Values{}
	v.Set("_pragma", "journal_mode(WAL)")
	v.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+v.Encode())
	if err != nil {
		return nil, fmt.Errorf("sessions: open registry %s: %w", path, err)
	}
	if _, err := db.ExecContext(ctx, registrySchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sessions: migrate registry: %w", err)
	}
	return &Registry{db: db}, nil
}

// ErrDuplicate is returned by Insert when the id (or, astronomically
// unlikely, the token hash) is already registered.
var ErrDuplicate = errors.New("sessions: already registered")

// Insert registers a session.
func (r *Registry) Insert(ctx context.Context, rec Record) error {
	var n int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE id = ? OR token_hash = ?`,
		rec.ID, rec.Hash[:]).Scan(&n); err != nil {
		return fmt.Errorf("sessions: registry lookup: %w", err)
	}
	if n > 0 {
		return ErrDuplicate
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO sessions (id, token_hash, created_at) VALUES (?, ?, ?)`,
		rec.ID, rec.Hash[:], rec.CreatedAt.Unix()); err != nil {
		return fmt.Errorf("sessions: registry insert: %w", err)
	}
	return nil
}

// Delete unregisters a session. Deleting an unknown id is not an error.
func (r *Registry) Delete(ctx context.Context, id string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("sessions: registry delete: %w", err)
	}
	return nil
}

// All returns every registered session, ordered by id.
func (r *Registry) All(ctx context.Context) ([]Record, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, token_hash, created_at FROM sessions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("sessions: registry list: %w", err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var (
			rec  Record
			hash []byte
			ts   int64
		)
		if err := rows.Scan(&rec.ID, &hash, &ts); err != nil {
			return nil, fmt.Errorf("sessions: registry scan: %w", err)
		}
		if len(hash) != len(rec.Hash) {
			return nil, fmt.Errorf("sessions: registry row %q has a %d-byte hash", rec.ID, len(hash))
		}
		copy(rec.Hash[:], hash)
		rec.CreatedAt = time.Unix(ts, 0).UTC()
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Close closes the database.
func (r *Registry) Close() error { return r.db.Close() }
