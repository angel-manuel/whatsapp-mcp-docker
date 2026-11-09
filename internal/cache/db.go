package cache

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"go.mau.fi/util/dbutil"
)

// DB is the cache's query handle. Queries are written once with `?`
// placeholders; on Postgres they are rewritten to `$1…$n` before they reach
// the driver, so the read-side tools stay dialect-agnostic even where they
// assemble WHERE clauses dynamically.
type DB struct {
	raw     *sql.DB
	dialect dbutil.Dialect
}

// Dialect reports which database engine backs this handle.
func (d *DB) Dialect() dbutil.Dialect { return d.dialect }

// ExecContext runs a statement.
func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.raw.ExecContext(ctx, rebind(d.dialect, query), args...)
}

// QueryContext runs a query returning rows.
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.raw.QueryContext(ctx, rebind(d.dialect, query), args...)
}

// QueryRowContext runs a query returning at most one row.
func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.raw.QueryRowContext(ctx, rebind(d.dialect, query), args...)
}

// Tx is a transaction on a DB, with the same placeholder rewriting.
type Tx struct {
	tx      *sql.Tx
	dialect dbutil.Dialect
}

// ExecContext runs a statement inside the transaction.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, rebind(t.dialect, query), args...)
}

// inTx runs fn in a transaction, committing on success.
func (d *DB) inTx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := d.raw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(&Tx{tx: tx, dialect: d.dialect}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// rebind rewrites `?` placeholders as `$1…$n` for Postgres. Question marks
// inside string literals, quoted identifiers and comments are left alone.
// SQLite accepts `?` natively, so its queries pass through untouched.
func rebind(dialect dbutil.Dialect, query string) string {
	if dialect != dbutil.Postgres || !strings.Contains(query, "?") {
		return query
	}
	var (
		b strings.Builder
		n int
	)
	b.Grow(len(query) + 8)
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'' || c == '"':
			// Copy through the closing quote; a doubled quote is an escape
			// and keeps the literal open.
			j := i + 1
			for j < len(query) {
				if query[j] == c {
					if j+1 < len(query) && query[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			end := min(j+1, len(query))
			b.WriteString(query[i:end])
			i = end - 1
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			j := strings.IndexByte(query[i:], '\n')
			if j < 0 {
				b.WriteString(query[i:])
				return b.String()
			}
			b.WriteString(query[i : i+j])
			i += j - 1
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			j := strings.Index(query[i+2:], "*/")
			if j < 0 {
				b.WriteString(query[i:])
				return b.String()
			}
			end := i + 2 + j + 2
			b.WriteString(query[i:end])
			i = end - 1
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
