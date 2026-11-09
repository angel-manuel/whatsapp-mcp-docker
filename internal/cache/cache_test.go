package cache

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"go.mau.fi/util/dbutil"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/pgdb/pgtest"
)

// newTestStore returns an empty, migrated store: a file-backed SQLite cache,
// or a throwaway Postgres schema when TEST_DATABASE_URL is set.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	if pgtest.Enabled() {
		store, err := OpenWithDB(ctx, pgtest.Schema(t), dbutil.Postgres)
		if err != nil {
			t.Fatalf("OpenWithDB: %v", err)
		}
		return store
	}
	store, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestOpen_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if got, want := store.Path(), filepath.Join(dir, "cache.db"); got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}

func TestOpen_RejectsEmptyDataDir(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatalf("expected error for empty dataDir")
	}
}

func TestMigrate_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	v1, err := store.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("CurrentSchemaVersion after Open: %v", err)
	}
	if v1 != len(upgradeTable) {
		t.Fatalf("schema version after Open = %d, want %d", v1, len(upgradeTable))
	}

	// Re-running Migrate on an already-migrated db must be a no-op.
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	v2, err := store.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("CurrentSchemaVersion: %v", err)
	}
	if v1 != v2 {
		t.Fatalf("schema version changed on re-run: %d -> %d", v1, v2)
	}
}

func TestMigrate_ReopenPreservesData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustExec(t, store, `INSERT INTO chats (jid, name) VALUES ('c@s', 'kept')`)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store2, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	if got, err := store2.GetChatNameByJID(ctx, "c@s"); err != nil || got != "kept" {
		t.Fatalf("ChatName after reopen = %q, %v; want kept", got, err)
	}
}

// TestMigrate_DropsLegacySchema covers a cache.db written by the pre-dbutil
// migration system: it is dropped and rebuilt empty rather than upgraded.
func TestMigrate_DropsLegacySchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")

	legacy, err := openRaw(path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	for _, q := range []string{
		`CREATE TABLE _schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER)`,
		`INSERT INTO _schema_version (version) VALUES (7)`,
		`CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO chats (jid, name) VALUES ('old@s', 'stale')`,
	} {
		if _, err := legacy.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed legacy %q: %v", q, err)
		}
	}
	_ = legacy.Close()

	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open over legacy cache: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var n int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM chats`).Scan(&n); err != nil {
		t.Fatalf("count chats: %v", err)
	}
	if n != 0 {
		t.Fatalf("legacy rows survived: %d chats", n)
	}
	if exists, err := store.dbu.TableExists(ctx, "_schema_version"); err != nil || exists {
		t.Fatalf("_schema_version still present (exists=%v, err=%v)", exists, err)
	}
}

func TestFullTextSearchSchema(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	mustExec(t, store, `INSERT INTO messages (chat_jid, id, ts, body) VALUES ('c@s', 'm1', 1, 'hello world')`)
	mustExec(t, store, `INSERT INTO messages (chat_jid, id, ts, body) VALUES ('c@s', 'm2', 2, 'unrelated text')`)

	q := `SELECT m.id FROM messages_fts f JOIN messages m ON m.rowid = f.rowid WHERE f.body MATCH 'hello'`
	if store.Dialect() == dbutil.Postgres {
		q = `SELECT id FROM messages WHERE body_tsv @@ phraseto_tsquery('simple', 'hello')`
	}
	var got string
	if err := store.DB().QueryRowContext(ctx, q).Scan(&got); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if got != "m1" {
		t.Fatalf("fts match id = %q, want m1", got)
	}
}

func TestRebind(t *testing.T) {
	cases := []struct{ in, want string }{
		{`SELECT 1`, `SELECT 1`},
		{`a = ? AND b = ?`, `a = $1 AND b = $2`},
		{`x LIKE '?%' AND y = ?`, `x LIKE '?%' AND y = $1`},
		{`'it''s ?' = ?`, `'it''s ?' = $1`},
		{`"we?ird" = ?`, `"we?ird" = $1`},
		{"-- why?\nx = ?", "-- why?\nx = $1"},
		{`/* ? */ x = ?`, `/* ? */ x = $1`},
		{`ESCAPE '\' AND z = ?`, `ESCAPE '\' AND z = $1`},
		{`unterminated '?`, `unterminated '?`},
	}
	for _, c := range cases {
		if got := rebind(dbutil.Postgres, c.in); got != c.want {
			t.Errorf("rebind(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := rebind(dbutil.SQLite, c.in); got != c.in {
			t.Errorf("SQLite rebind(%q) changed the query to %q", c.in, got)
		}
	}
}

func mustExec(t *testing.T, store *Store, query string, args ...any) {
	t.Helper()
	if _, err := store.DB().ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// openRaw opens a SQLite file without running any migrations.
func openRaw(path string) (*sql.DB, error) {
	return sql.Open("sqlite", buildDSN(path))
}
