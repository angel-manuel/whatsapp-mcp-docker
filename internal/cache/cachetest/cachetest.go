// Package cachetest opens throwaway cache stores for tests in other
// packages: SQLite by default, Postgres when TEST_DATABASE_URL is set.
package cachetest

import (
	"context"
	"testing"

	"go.mau.fi/util/dbutil"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/pgdb/pgtest"
)

// Open returns an empty, migrated store that is closed when t ends.
func Open(t testing.TB) *cache.Store {
	t.Helper()
	ctx := context.Background()
	var (
		store *cache.Store
		err   error
	)
	if pgtest.Enabled() {
		store, err = cache.OpenWithDB(ctx, pgtest.Schema(t), dbutil.Postgres)
	} else {
		store, err = cache.Open(ctx, t.TempDir())
	}
	if err != nil {
		t.Fatalf("cachetest: open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
