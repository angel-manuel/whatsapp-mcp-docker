package cache

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// schemaVersionDDL creates the bookkeeping table. Kept inline so that a brand-new
// database can record the baseline migration's version without chicken-and-egg.
const schemaVersionDDL = `CREATE TABLE IF NOT EXISTS _schema_version (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
)`

type migration struct {
	version int
	name    string
	up      string
	down    string
}

var migrationFileRe = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.(up|down)\.sql$`)

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("cache: read embedded migrations: %w", err)
	}
	byVersion := make(map[int]*migration)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		m := migrationFileRe.FindStringSubmatch(entry.Name())
		if m == nil {
			return nil, fmt.Errorf("cache: migration file %q does not match <version>_<name>.(up|down).sql", entry.Name())
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("cache: parse migration version from %q: %w", entry.Name(), err)
		}
		name := m[2]
		direction := m[3]
		body, err := fs.ReadFile(migrationsFS, path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("cache: read %s: %w", entry.Name(), err)
		}
		mig, ok := byVersion[version]
		if !ok {
			mig = &migration{version: version, name: name}
			byVersion[version] = mig
		} else if mig.name != name {
			return nil, fmt.Errorf("cache: migration version %d has mismatched names %q and %q", version, mig.name, name)
		}
		switch direction {
		case "up":
			mig.up = string(body)
		case "down":
			mig.down = string(body)
		}
	}
	out := make([]migration, 0, len(byVersion))
	for _, mig := range byVersion {
		if strings.TrimSpace(mig.up) == "" {
			return nil, fmt.Errorf("cache: migration version %d missing up SQL", mig.version)
		}
		if strings.TrimSpace(mig.down) == "" {
			return nil, fmt.Errorf("cache: migration version %d missing down SQL", mig.version)
		}
		out = append(out, *mig)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// Migrate applies every pending up migration in order. Each migration runs in
// its own transaction; the version bump and DDL are committed together.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaVersionDDL); err != nil {
		return fmt.Errorf("cache: create _schema_version: %w", err)
	}
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range migs {
		applied, err := s.isApplied(ctx, m.version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		if err := s.applyUp(ctx, m); err != nil {
			return fmt.Errorf("cache: apply migration %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

// MigrateDown rolls the schema back to (and including) the target version's
// predecessor — i.e. everything strictly greater than `to` is reverted in
// descending order. Pass 0 to undo every migration.
func (s *Store) MigrateDown(ctx context.Context, to int) error {
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version > migs[j].version })
	for _, m := range migs {
		if m.version <= to {
			break
		}
		applied, err := s.isApplied(ctx, m.version)
		if err != nil {
			return err
		}
		if !applied {
			continue
		}
		if err := s.applyDown(ctx, m); err != nil {
			return fmt.Errorf("cache: revert migration %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

// CurrentSchemaVersion returns the highest applied migration version, or 0 if none.
func (s *Store) CurrentSchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM _schema_version`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("cache: read schema version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

func (s *Store) isApplied(ctx context.Context, version int) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM _schema_version WHERE version = ?`, version).Scan(&one)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("cache: lookup schema version %d: %w", version, err)
}

func (s *Store) applyUp(ctx context.Context, m migration) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, m.up); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO _schema_version (version) VALUES (?)`, m.version)
		return err
	})
}

func (s *Store) applyDown(ctx context.Context, m migration) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, m.down); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM _schema_version WHERE version = ?`, m.version)
		return err
	})
}

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
