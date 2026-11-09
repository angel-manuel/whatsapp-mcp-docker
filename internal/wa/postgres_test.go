package wa

import (
	"context"
	"testing"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/pgdb/pgtest"
)

// On Postgres, Unpair cannot delete session.db: it empties the session's
// whatsmeow tables instead, leaving the schema and the shared pool usable
// for the fresh device it reopens with.
func TestPostgres_UnpairWipesSessionTables(t *testing.T) {
	db := pgtest.Schema(t)
	ctx := context.Background()

	c, err := Open(ctx, Config{DataDir: t.TempDir(), DB: db})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if c.IsPaired() {
		t.Fatal("fresh schema should not be paired")
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO whatsmeow_lid_map (lid, pn) VALUES ('1@lid', '2@s.whatsapp.net')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := c.Unpair(ctx); err != nil {
		t.Fatalf("Unpair: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM whatsmeow_lid_map`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("whatsmeow_lid_map has %d rows after Unpair, want 0", n)
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT version FROM whatsmeow_version`).Scan(&version); err != nil {
		t.Fatalf("whatsmeow_version lost by Unpair: %v", err)
	}
	if got := c.State(); got != StateNotPaired {
		t.Fatalf("state after Unpair = %q, want %q", got, StateNotPaired)
	}

	// The pool is borrowed: Close must leave it open for its owner.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("Close closed the borrowed pool: %v", err)
	}
}
