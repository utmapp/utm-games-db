// Package db opens the SQLite database and applies embedded migrations.
//
// Two pools share one file: Writer is limited to a single connection so that
// web handlers and the IGDB sync serialize their transactions instead of
// racing into SQLITE_BUSY; Reader serves everything else.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var migrations embed.FS

// DB bundles the reader and writer pools.
type DB struct {
	Reader *sql.DB
	Writer *sql.DB
}

// Open opens (creating if needed) the database at path and runs migrations.
func Open(path string) (*DB, error) {
	dsn := func() string {
		q := url.Values{}
		q.Add("_pragma", "busy_timeout(10000)")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "foreign_keys(1)")
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Add("_pragma", "temp_store(MEMORY)")
		q.Add("_pragma", "cache_size(-32000)")
		return "file:" + path + "?" + q.Encode()
	}
	w, err := sql.Open("sqlite", dsn())
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	r, err := sql.Open("sqlite", dsn())
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(8)
	r.SetMaxIdleConns(8)
	r.SetConnMaxLifetime(0)
	d := &DB{Reader: r, Writer: w}
	if err := d.migrate(context.Background()); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// Close closes both pools.
func (d *DB) Close() error {
	e1 := d.Reader.Close()
	e2 := d.Writer.Close()
	if e1 != nil {
		return e1
	}
	return e2
}

// IsUniqueViolation reports whether err is a UNIQUE or PRIMARY KEY
// constraint failure.
func IsUniqueViolation(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

// WriteTx runs fn inside a write transaction on the single writer connection.
func (d *DB) WriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// migrate applies the embedded migrations that are not yet recorded in
// schema_migrations, in file-name order, each in its own transaction. A
// future migration that rebuilds a referenced table (rename, copy, drop)
// must run with PRAGMA foreign_keys=OFF on the writer connection, set
// outside the transaction, and check pragma_foreign_key_check afterwards.
func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.Writer.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var exists int
		if err := d.Writer.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, name).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		err = d.WriteTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, unixepoch())`, name)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
