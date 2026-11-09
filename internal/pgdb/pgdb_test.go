package pgdb_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/pgdb"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/pgdb/pgtest"
)

func TestSessionSchema(t *testing.T) {
	if got := pgdb.SessionSchema("default"); got != "wa_default" {
		t.Fatalf("SessionSchema(default) = %q", got)
	}
	// The longest legal id (63 chars) would overflow Postgres's 63-byte
	// identifier limit once prefixed, and silent truncation would let two
	// ids share a schema.
	long1 := strings.Repeat("a", 62) + "1"
	long2 := strings.Repeat("a", 62) + "2"
	s1, s2 := pgdb.SessionSchema(long1), pgdb.SessionSchema(long2)
	if len(s1) > 63 || len(s2) > 63 {
		t.Fatalf("schema names too long: %d, %d", len(s1), len(s2))
	}
	if s1 == s2 {
		t.Fatalf("distinct ids share schema %q", s1)
	}
	if pgdb.SessionSchema(long1) != s1 {
		t.Fatal("SessionSchema is not deterministic")
	}
	// Exactly at the limit stays verbatim.
	fits := strings.Repeat("b", 60)
	if got := pgdb.SessionSchema(fits); got != "wa_"+fits {
		t.Fatalf("SessionSchema(60 chars) = %q, want verbatim", got)
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := pgdb.QuoteIdent(`wa_a-b"c`); got != `"wa_a-b""c"` {
		t.Fatalf("QuoteIdent = %s", got)
	}
}

func TestOpen_ScopesToSchema(t *testing.T) {
	pgtest.SkipUnlessEnabled(t)
	ctx := context.Background()
	schema := pgtest.RandomName(t, "test_scope-")
	db, err := pgdb.Open(ctx, pgtest.URL(), schema, 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		_ = pgdb.DropSchema(context.Background(), db, schema)
		_ = db.Close()
	})

	if ok, err := pgdb.SchemaExists(ctx, db, schema); err != nil || !ok {
		t.Fatalf("SchemaExists = %v, %v; want true", ok, err)
	}
	var cur string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&cur); err != nil {
		t.Fatalf("current_schema: %v", err)
	}
	if cur != schema {
		t.Fatalf("current_schema = %q, want %q", cur, schema)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (x int)`); err != nil {
		t.Fatalf("create unqualified table: %v", err)
	}
	var where string
	if err := db.QueryRowContext(ctx,
		`SELECT schemaname FROM pg_tables WHERE tablename = 't' AND schemaname = $1`, schema).Scan(&where); err != nil {
		t.Fatalf("table not created in the session schema: %v", err)
	}

	names, err := pgdb.SchemasWithPrefix(ctx, db, "test_scope-")
	if err != nil {
		t.Fatalf("SchemasWithPrefix: %v", err)
	}
	found := false
	for _, n := range names {
		found = found || n == schema
	}
	if !found {
		t.Fatalf("SchemasWithPrefix = %v, missing %q", names, schema)
	}

	if err := pgdb.DropSchema(ctx, db, schema); err != nil {
		t.Fatalf("DropSchema: %v", err)
	}
	if ok, _ := pgdb.SchemaExists(ctx, db, schema); ok {
		t.Fatal("schema survived DropSchema")
	}
	if err := pgdb.DropSchema(ctx, db, schema); err != nil {
		t.Fatalf("DropSchema on missing schema: %v", err)
	}
}

func TestTryLock_IsExclusiveUntilReleased(t *testing.T) {
	pgtest.SkipUnlessEnabled(t)
	ctx := context.Background()
	db := pgtest.Schema(t)
	key := pgtest.RandomName(t, "lock-")

	first, err := pgdb.TryLock(ctx, db, key)
	if err != nil {
		t.Fatalf("first TryLock: %v", err)
	}
	if _, err := pgdb.TryLock(ctx, db, key); !errors.Is(err, pgdb.ErrLockHeld) {
		t.Fatalf("second TryLock = %v, want ErrLockHeld", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	again, err := pgdb.TryLock(ctx, db, key)
	if err != nil {
		t.Fatalf("TryLock after release: %v", err)
	}
	_ = again.Release()
	if err := (*pgdb.Lock)(nil).Release(); err != nil {
		t.Fatalf("nil Release: %v", err)
	}
}
