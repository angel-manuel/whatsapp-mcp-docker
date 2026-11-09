// Package pgtest gives tests a throwaway Postgres schema. Tests that use it
// run against Postgres only when TEST_DATABASE_URL is set; otherwise they
// keep using SQLite, so `go test ./...` needs no database.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/pgdb"
)

// EnvURL names the variable holding the test database URL.
const EnvURL = "TEST_DATABASE_URL"

// URL returns the test database URL, or "" when Postgres tests are off.
func URL() string { return os.Getenv(EnvURL) }

// Enabled reports whether Postgres tests are on.
func Enabled() bool { return URL() != "" }

// SkipUnlessEnabled skips t when TEST_DATABASE_URL is unset.
func SkipUnlessEnabled(t testing.TB) {
	t.Helper()
	if !Enabled() {
		t.Skip(EnvURL + " not set")
	}
}

// RandomName returns prefix followed by random hex, unique enough to keep
// parallel test runs on one database apart.
func RandomName(t testing.TB, prefix string) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random name: %v", err)
	}
	return prefix + hex.EncodeToString(b)
}

// Schema creates a fresh schema and returns a pool scoped to it. The schema
// is dropped when the test ends.
func Schema(t testing.TB) *sql.DB {
	t.Helper()
	SkipUnlessEnabled(t)
	ctx := context.Background()
	schema := RandomName(t, "test_")
	db, err := pgdb.Open(ctx, URL(), schema, 4)
	if err != nil {
		t.Fatalf("pgtest: open: %v", err)
	}
	t.Cleanup(func() {
		_ = pgdb.DropSchema(context.Background(), db, schema)
		_ = db.Close()
	})
	return db
}
