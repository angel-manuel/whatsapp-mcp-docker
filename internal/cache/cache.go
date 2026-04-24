// Package cache owns the local SQLite mirror of WhatsApp chats, messages,
// contacts, and the nickname store. It is the read side for the MCP tool
// surface and the write side for the event ingestor.
package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // sqlite driver
)

// Store is a handle to the cache SQLite database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (creating if needed) `dataDir/cache.db`, applies any pending
// migrations, and returns a ready-to-use Store.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("cache: dataDir is required")
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("cache: create data dir: %w", err)
	}
	path := filepath.Join(dataDir, "cache.db")
	return openAt(ctx, path)
}

// OpenInMemory opens an in-memory SQLite database. Intended for tests.
func OpenInMemory(ctx context.Context) (*Store, error) {
	return openAt(ctx, ":memory:")
}

func openAt(ctx context.Context, path string) (*Store, error) {
	dsn := buildDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cache: open %s: %w", path, err)
	}
	// modernc.org/sqlite pool: keep a single conn for :memory: so all
	// migrations and queries see the same schema; plenty of conns otherwise.
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cache: ping: %w", err)
	}
	s := &Store{db: db, path: path}
	if err := s.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func buildDSN(path string) string {
	if path == ":memory:" {
		v := url.Values{}
		v.Set("_pragma", "foreign_keys(1)")
		v.Add("_pragma", "journal_mode(MEMORY)")
		return "file::memory:?cache=shared&" + v.Encode()
	}
	v := url.Values{}
	v.Set("_pragma", "journal_mode(WAL)")
	v.Add("_pragma", "foreign_keys(1)")
	v.Add("_pragma", "busy_timeout(5000)")
	v.Add("_pragma", "synchronous(NORMAL)")
	return "file:" + path + "?" + v.Encode()
}

// DB returns the underlying database handle. Intended for read-side tool
// implementations; writers should use the typed helpers in this package.
func (s *Store) DB() *sql.DB { return s.db }

// Path returns the filesystem path the store was opened at, or ":memory:".
func (s *Store) Path() string { return s.path }

// Close closes the underlying database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
