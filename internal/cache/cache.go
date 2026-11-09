// Package cache owns the local mirror of WhatsApp chats, messages, contacts,
// and the nickname store. It is the read side for the MCP tool surface and
// the write side for the event ingestor. It runs on SQLite (one cache.db per
// session) or on Postgres (a pool scoped to the session's schema).
package cache

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"go.mau.fi/util/dbutil"

	_ "modernc.org/sqlite" // sqlite driver
)

//go:embed upgrades/*.sql
var upgradesFS embed.FS

// upgradeTable is the cache schema history, applied by dbutil.
var upgradeTable = dbutil.BuildUpgradeTable().WithFSPath(upgradesFS, "upgrades").Finish()

// VersionTable records the cache schema version. It is distinct from
// whatsmeow's own version table so both can share one Postgres schema.
const VersionTable = "cache_version"

// legacyTables are the tables written by the pre-dbutil migration system.
// A cache.db that still has _schema_version is dropped and rebuilt: the
// cache is a re-syncable mirror, so starting empty loses nothing durable.
var legacyTables = []string{
	"messages_fts", "poll_votes", "poll_options", "polls", "reactions",
	"jid_aliases", "nicknames", "contacts", "messages", "chats", "_schema_version",
}

// Store is a handle to the cache database.
type Store struct {
	db     *DB
	dbu    *dbutil.Database
	path   string
	ownsDB bool
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
	return openSQLite(ctx, path)
}

// OpenInMemory opens an in-memory SQLite database. Intended for tests.
func OpenInMemory(ctx context.Context) (*Store, error) {
	return openSQLite(ctx, ":memory:")
}

// OpenWithDB runs the cache on an existing pool, such as a session's
// Postgres pool shared with whatsmeow. The caller keeps ownership: Close
// does not close db.
func OpenWithDB(ctx context.Context, db *sql.DB, dialect dbutil.Dialect) (*Store, error) {
	s, err := newStore(db, dialect, "", false)
	if err != nil {
		return nil, err
	}
	if err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func openSQLite(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", buildDSN(path))
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
	s, err := newStore(db, dbutil.SQLite, path, true)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func newStore(db *sql.DB, dialect dbutil.Dialect, path string, owns bool) (*Store, error) {
	dbu, err := dbutil.NewWithDB(db, dialect.String())
	if err != nil {
		return nil, fmt.Errorf("cache: wrap db: %w", err)
	}
	dbu.VersionTable = VersionTable
	dbu.UpgradeTable = upgradeTable
	return &Store{
		db:     &DB{raw: db, dialect: dialect},
		dbu:    dbu,
		path:   path,
		ownsDB: owns,
	}, nil
}

// Migrate brings the schema up to date. A cache written by the pre-dbutil
// migration system is dropped first and rebuilt empty.
func (s *Store) Migrate(ctx context.Context) error {
	if s.db.dialect == dbutil.SQLite {
		legacy, err := s.dbu.TableExists(ctx, "_schema_version")
		if err != nil {
			return fmt.Errorf("cache: check legacy schema: %w", err)
		}
		if legacy {
			for _, t := range legacyTables {
				if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+t); err != nil {
					return fmt.Errorf("cache: drop legacy table %s: %w", t, err)
				}
			}
		}
	}
	// dbutil decides whether the version table exists by asking
	// information_schema, which on Postgres spans every schema in the
	// database. With one schema per session, another session's table would
	// answer for this one, so create it here, in the current schema, first.
	if _, err := s.db.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS "+VersionTable+" (version INTEGER, compat INTEGER)"); err != nil {
		return fmt.Errorf("cache: create version table: %w", err)
	}
	if err := s.dbu.Upgrade(ctx); err != nil {
		return fmt.Errorf("cache: upgrade schema: %w", err)
	}
	return nil
}

// CurrentSchemaVersion returns the applied schema version, or 0 if none.
func (s *Store) CurrentSchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM `+VersionTable).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("cache: read schema version: %w", err)
	}
	return int(v.Int64), nil
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

// DB returns the query handle. Intended for read-side tool implementations;
// writers should use the typed helpers in this package. Queries use `?`
// placeholders on every dialect.
func (s *Store) DB() *DB { return s.db }

// Dialect reports which database engine backs the store.
func (s *Store) Dialect() dbutil.Dialect { return s.db.dialect }

// Path returns the filesystem path the store was opened at, ":memory:", or
// "" when it runs on a caller-provided pool.
func (s *Store) Path() string { return s.path }

// Close closes the underlying database handle if the store opened it.
func (s *Store) Close() error {
	if s == nil || s.db == nil || !s.ownsDB {
		return nil
	}
	return s.db.raw.Close()
}

func (s *Store) inTx(ctx context.Context, fn func(*Tx) error) error {
	return s.db.inTx(ctx, fn)
}
