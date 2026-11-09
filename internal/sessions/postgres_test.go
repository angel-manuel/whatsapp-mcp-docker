package sessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/pgdb"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/pgdb/pgtest"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// pgOptions configures real sessions against the test database.
func pgOptions() Options {
	return Options{
		Name:               "whatsapp-mcp-test",
		Version:            "test",
		Transport:          mcp.TransportHTTP,
		MediaSweepInterval: time.Hour,
		DatabaseURL:        pgtest.URL(),
		Logger:             quietLogger(),
	}
}

// pgID returns a session id unique to this test run, and drops its schema
// when the test ends: session schemas live at the database root, outside
// any per-test schema.
func pgID(t *testing.T, prefix string) string {
	t.Helper()
	id := pgtest.RandomName(t, prefix)
	t.Cleanup(func() {
		db, err := pgdb.Open(context.Background(), pgtest.URL(), "", 1)
		if err != nil {
			return
		}
		defer db.Close()
		_ = pgdb.DropSchema(context.Background(), db, pgdb.SessionSchema(id))
	})
	return id
}

// tablesIn returns which of the named tables exist in schema.
func tablesIn(t *testing.T, schema string, names ...string) map[string]bool {
	t.Helper()
	db, err := pgdb.Open(context.Background(), pgtest.URL(), "", 1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	out := map[string]bool{}
	for _, n := range names {
		var ok bool
		if err := db.QueryRowContext(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = $1 AND tablename = $2)`,
			schema, n).Scan(&ok); err != nil {
			t.Fatalf("look up %s.%s: %v", schema, n, err)
		}
		out[n] = ok
	}
	return out
}

// Two sessions in one database must each get a complete schema. dbutil
// checks for its version tables through information_schema, which spans
// every schema; without pre-creating them per schema the second session
// would see the first one's tables and skip its own migrations.
func TestPostgres_TwoSessionsShareADatabase(t *testing.T) {
	pgtest.SkipUnlessEnabled(t)
	root := t.TempDir()
	for _, prefix := range []string{"a", "b"} {
		id := pgID(t, prefix)
		sess, err := Open(id, filepath.Join(root, id), pgOptions())
		if err != nil {
			t.Fatalf("Open %s: %v", id, err)
		}
		t.Cleanup(func() { _ = sess.Close() })
		if st := sess.Status(); st != StatusUnpaired {
			t.Errorf("%s status = %s, want unpaired", id, st)
		}
		got := tablesIn(t, pgdb.SessionSchema(id),
			"whatsmeow_version", "whatsmeow_device", "cache_version", "chats", "messages")
		for table, ok := range got {
			if !ok {
				t.Errorf("%s: table %s missing from its schema", id, table)
			}
		}
	}
}

func TestPostgres_SessionIsExclusive(t *testing.T) {
	pgtest.SkipUnlessEnabled(t)
	id := pgID(t, "x")
	dir := filepath.Join(t.TempDir(), id)

	first, err := Open(id, dir, pgOptions())
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := Open(id, dir+"-elsewhere", pgOptions()); !errors.Is(err, wa.ErrLockHeld) {
		t.Fatalf("second Open = %v, want wa.ErrLockHeld", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Closing releases the lock: the session reopens.
	again, err := Open(id, dir, pgOptions())
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	_ = again.Close()
}

func TestPostgres_ManagerCreateAdoptDelete(t *testing.T) {
	pgtest.SkipUnlessEnabled(t)
	ctx := context.Background()

	regSchema := pgtest.RandomName(t, "test_registry_")
	reg, err := openPostgresRegistry(ctx, pgtest.URL(), regSchema)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() {
		_ = pgdb.DropSchema(context.Background(), reg.DB(), regSchema)
		_ = reg.Close()
	})

	root := t.TempDir()
	opts := pgOptions()
	mgr := NewManager(reg, root, 5,
		func(id, dir string) (Instance, error) { return Open(id, dir, opts) },
		quietLogger()).WithStorage(PostgresStorage{DB: reg.DB()})
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	id := pgID(t, "m")
	schema := pgdb.SessionSchema(id)
	created, err := mgr.Create(ctx, id)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Adopted {
		t.Error("fresh session reported as adopted")
	}
	if ok, _ := pgdb.SchemaExists(ctx, reg.DB(), schema); !ok {
		t.Fatalf("schema %s not created", schema)
	}
	if _, inst, found := mgr.Lookup(created.Token); !found || inst == nil {
		t.Fatal("token does not resolve to the session")
	}
	recs, err := reg.All(ctx)
	if err != nil || len(recs) != 1 || recs[0].ID != id {
		t.Fatalf("registry = %+v, %v; want one row for %s", recs, err, id)
	}

	// The schema outlives a restart; a new Manager would adopt it, and
	// until then it is an orphan.
	if _, err := mgr.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := pgdb.SchemaExists(ctx, reg.DB(), schema); ok {
		t.Fatal("Delete left the schema behind")
	}
	if _, err := os.Stat(filepath.Join(root, id)); !os.IsNotExist(err) {
		t.Fatalf("Delete left the media directory behind: %v", err)
	}

	// Pre-existing state is adopted, not overwritten.
	pre, err := Open(id, filepath.Join(root, id), opts)
	if err != nil {
		t.Fatalf("pre-create session state: %v", err)
	}
	_ = pre.Close()
	orphans, err := (PostgresStorage{DB: reg.DB()}).Orphans(ctx, root, map[string]struct{}{})
	if err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	foundOrphan := false
	for _, o := range orphans {
		foundOrphan = foundOrphan || o == schema
	}
	if !foundOrphan {
		t.Errorf("Orphans = %v, want it to list %s", orphans, schema)
	}
	adopted, err := mgr.Create(ctx, id)
	if err != nil {
		t.Fatalf("Create over existing schema: %v", err)
	}
	if !adopted.Adopted {
		t.Error("existing schema not reported as adopted")
	}
}
