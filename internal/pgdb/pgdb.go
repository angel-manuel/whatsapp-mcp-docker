// Package pgdb holds the Postgres plumbing shared by the session registry,
// the whatsmeow store and the cache: one schema per session, connection
// pools scoped to that schema, and advisory locks that stand in for the
// flock a SQLite volume uses.
package pgdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// DriverName is the database/sql driver name, and the dialect name dbutil
// and whatsmeow's sqlstore accept for Postgres.
const DriverName = "pgx"

// RegistrySchema holds the session registry.
const RegistrySchema = "whatsapp_mcp"

// SessionSchemaPrefix prefixes every per-session schema.
const SessionSchemaPrefix = "wa_"

// maxIdentLen is Postgres's NAMEDATALEN-1: longer identifiers are silently
// truncated, which would let two long session ids collide.
const maxIdentLen = 63

// sessionIDKeep is how much of a long id survives in its schema name, so
// that prefix + kept + "_" + 7 hex chars fits in maxIdentLen.
const sessionIDKeep = maxIdentLen - len(SessionSchemaPrefix) - 1 - 7

// IsURL reports whether s is a Postgres connection URL.
func IsURL(s string) bool {
	return strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://")
}

// SessionSchema returns the schema holding a session's whatsmeow and cache
// tables. Ids short enough are kept verbatim; longer ones are truncated and
// suffixed with a digest of the full id so distinct ids never collide.
func SessionSchema(id string) string {
	if len(SessionSchemaPrefix)+len(id) <= maxIdentLen {
		return SessionSchemaPrefix + id
	}
	sum := sha256.Sum256([]byte(id))
	return SessionSchemaPrefix + id[:sessionIDKeep] + "_" + hex.EncodeToString(sum[:])[:7]
}

// QuoteIdent quotes an identifier for interpolation into SQL.
func QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// Open returns a pool whose connections all resolve unqualified names in
// schema, creating the schema first if it is missing. An empty schema
// leaves the server's default search_path alone.
//
// search_path is set as a startup parameter, so a pgbouncer in front of the
// server must run in session pooling mode.
func Open(ctx context.Context, url, schema string, maxConns int) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("pgdb: parse DATABASE_URL: %w", err)
	}
	if schema != "" {
		cfg.RuntimeParams["search_path"] = QuoteIdent(schema)
	}
	db := stdlib.OpenDB(*cfg)
	if maxConns > 0 {
		db.SetMaxOpenConns(maxConns)
		db.SetMaxIdleConns(maxConns)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pgdb: connect: %w", err)
	}
	if schema != "" {
		if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+QuoteIdent(schema)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("pgdb: create schema %s: %w", schema, err)
		}
	}
	return db, nil
}

// SchemaExists reports whether schema exists.
func SchemaExists(ctx context.Context, db *sql.DB, schema string) (bool, error) {
	var ok bool
	err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("pgdb: look up schema %s: %w", schema, err)
	}
	return ok, nil
}

// DropSchema drops schema and everything in it. A missing schema is not an
// error.
func DropSchema(ctx context.Context, db *sql.DB, schema string) error {
	if _, err := db.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+QuoteIdent(schema)+" CASCADE"); err != nil {
		return fmt.Errorf("pgdb: drop schema %s: %w", schema, err)
	}
	return nil
}

// SchemasWithPrefix lists the schemas whose name starts with prefix.
func SchemasWithPrefix(ctx context.Context, db *sql.DB, prefix string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT nspname FROM pg_namespace WHERE starts_with(nspname, $1) ORDER BY nspname`, prefix)
	if err != nil {
		return nil, fmt.Errorf("pgdb: list schemas: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ErrLockHeld is returned when another process holds an advisory lock.
var ErrLockHeld = errors.New("pgdb: advisory lock is already held")

// Lock is a session-level advisory lock, held on a connection reserved for
// it until Release. It is the Postgres counterpart of the data-dir flock:
// whatsmeow ratchets rotate on every message, so two processes driving the
// same session would corrupt it.
type Lock struct {
	conn *sql.Conn
	key  string
}

// TryLock takes the advisory lock named key without blocking, failing with
// ErrLockHeld when another session holds it. The lock lives as long as the
// reserved connection, so it is released even if the process dies.
func TryLock(ctx context.Context, db *sql.DB, key string) (*Lock, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("pgdb: reserve lock connection: %w", err)
	}
	var ok bool
	if err := conn.QueryRowContext(ctx,
		`SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&ok); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("pgdb: advisory lock %s: %w", key, err)
	}
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %s", ErrLockHeld, key)
	}
	return &Lock{conn: conn, key: key}, nil
}

// Release drops the lock and returns its connection. Safe on a nil Lock.
func (l *Lock) Release() error {
	if l == nil || l.conn == nil {
		return nil
	}
	_, uerr := l.conn.ExecContext(context.Background(),
		`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, l.key)
	cerr := l.conn.Close()
	l.conn = nil
	return errors.Join(uerr, cerr)
}
