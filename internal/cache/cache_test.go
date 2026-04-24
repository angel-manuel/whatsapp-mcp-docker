package cache

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(ctx, dir)
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
	if v1 == 0 {
		t.Fatalf("expected schema to be migrated after Open")
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

func TestMigrateDown_ThenUp(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	startVersion, err := store.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("CurrentSchemaVersion: %v", err)
	}
	if startVersion == 0 {
		t.Fatalf("expected at least one migration applied")
	}

	// Rolling all the way down must drop every managed table.
	if err := store.MigrateDown(ctx, 0); err != nil {
		t.Fatalf("MigrateDown(0): %v", err)
	}
	v, err := store.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("CurrentSchemaVersion after down: %v", err)
	}
	if v != 0 {
		t.Fatalf("schema version after full down = %d, want 0", v)
	}

	// MigrateDown again is a no-op — nothing left to revert.
	if err := store.MigrateDown(ctx, 0); err != nil {
		t.Fatalf("MigrateDown(0) second call: %v", err)
	}

	// Reapply from scratch and confirm we're back at the baseline.
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate after full down: %v", err)
	}
	v2, err := store.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("CurrentSchemaVersion after re-up: %v", err)
	}
	if v2 != startVersion {
		t.Fatalf("schema version after re-up = %d, want %d", v2, startVersion)
	}

	// And the tables should actually exist after re-up.
	var count int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('chats','messages','contacts','nicknames','messages_fts')`).
		Scan(&count); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if count != 5 {
		t.Fatalf("expected 5 tables after re-up, got %d", count)
	}
}

func TestMigrate_ReopenPreservesSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	v1, err := store.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("CurrentSchemaVersion: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store2, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	v2, err := store2.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("CurrentSchemaVersion after reopen: %v", err)
	}
	if v1 != v2 {
		t.Fatalf("version drift on reopen: %d -> %d", v1, v2)
	}
}

func TestFTS5_AvailableAndSearchable(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	mustExec(t, store, `INSERT INTO messages (chat_jid, id, ts, body) VALUES ('c@s', 'm1', 1, 'hello world')`)
	mustExec(t, store, `INSERT INTO messages (chat_jid, id, ts, body) VALUES ('c@s', 'm2', 2, 'unrelated text')`)

	var got string
	err := store.DB().QueryRowContext(ctx,
		`SELECT m.id FROM messages_fts f JOIN messages m ON m.rowid = f.rowid WHERE f.body MATCH 'hello'`).Scan(&got)
	if err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if got != "m1" {
		t.Fatalf("fts match id = %q, want m1", got)
	}
}

func mustExec(t *testing.T, store *Store, query string, args ...any) {
	t.Helper()
	if _, err := store.DB().ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
