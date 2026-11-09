package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/pgdb"
)

// Storage is where a session's durable state lives, as far as the Manager
// needs to know: whether it already exists (Create adopts it), how to erase
// it (Delete), and what exists without a registry entry (startup warning).
// The session directory is passed along because media always lives there,
// whichever backend holds the databases.
type Storage interface {
	// Exists reports whether the session has stored state to adopt.
	Exists(ctx context.Context, id, dir string) (bool, error)
	// Wipe erases the session's stored state. Missing state is not an error.
	Wipe(ctx context.Context, id, dir string) error
	// Orphans names stored state that belongs to none of the registered ids.
	Orphans(ctx context.Context, root string, registered map[string]struct{}) ([]string, error)
}

// FSStorage keeps each session in its directory: session.db, cache.db and
// media/. It is the default.
type FSStorage struct{}

// Exists reports whether the session directory exists.
func (FSStorage) Exists(_ context.Context, _, dir string) (bool, error) {
	_, err := os.Stat(dir)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Wipe removes the session directory.
func (FSStorage) Wipe(_ context.Context, _, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("sessions: wipe %s: %w", dir, err)
	}
	return nil
}

// Orphans lists directories under root with no registry entry.
func (FSStorage) Orphans(_ context.Context, root string, registered map[string]struct{}) ([]string, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, de := range ents {
		if !de.IsDir() {
			continue
		}
		if _, ok := registered[de.Name()]; !ok {
			out = append(out, de.Name())
		}
	}
	return out, nil
}

// PostgresStorage keeps each session's whatsmeow and cache tables in its own
// schema (see pgdb.SessionSchema); media stays in the session directory.
type PostgresStorage struct {
	// DB administers schemas. Any pool on the database works; the registry's
	// is the natural one.
	DB *sql.DB
}

// Exists reports whether the session's schema exists.
func (p PostgresStorage) Exists(ctx context.Context, id, _ string) (bool, error) {
	return pgdb.SchemaExists(ctx, p.DB, pgdb.SessionSchema(id))
}

// Wipe drops the session's schema and removes its media directory.
func (p PostgresStorage) Wipe(ctx context.Context, id, dir string) error {
	if err := pgdb.DropSchema(ctx, p.DB, pgdb.SessionSchema(id)); err != nil {
		return err
	}
	return FSStorage{}.Wipe(ctx, id, dir)
}

// Orphans lists session schemas with no registry entry.
func (p PostgresStorage) Orphans(ctx context.Context, _ string, registered map[string]struct{}) ([]string, error) {
	schemas, err := pgdb.SchemasWithPrefix(ctx, p.DB, pgdb.SessionSchemaPrefix)
	if err != nil {
		return nil, err
	}
	known := make(map[string]struct{}, len(registered))
	for id := range registered {
		known[pgdb.SessionSchema(id)] = struct{}{}
	}
	var out []string
	for _, s := range schemas {
		if _, ok := known[s]; !ok {
			out = append(out, s)
		}
	}
	return out, nil
}
