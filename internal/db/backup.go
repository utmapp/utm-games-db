package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
)

// CopyTables writes a database at destPath holding the schema of this one
// and the rows of the named tables, taken in a single read transaction so
// they are consistent with each other. fixups are statements run against
// the copy before it is closed. The cost is proportional to the copied
// tables, not to the database as a whole.
func (d *DB) CopyTables(ctx context.Context, destPath string, tables, fixups []string) error {
	os.Remove(destPath)
	// Create the destination with the full schema (migrations), then turn its
	// write-ahead log off so the result is a single file.
	dest, err := Open(destPath)
	if err != nil {
		return fmt.Errorf("create backup database: %w", err)
	}
	if _, err := dest.Writer.ExecContext(ctx, `PRAGMA journal_mode=DELETE`); err != nil {
		dest.Close()
		return err
	}
	if err := dest.Close(); err != nil {
		return err
	}

	// One pinned connection: ATTACH, the transaction and the pragmas are all
	// per connection.
	conn, err := d.Reader.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// The copy's foreign keys point at tables that stay empty (the catalog),
	// so enforcement is off for this connection while it copies.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS backup`, destPath); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `DETACH DATABASE backup`)
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	for _, t := range tables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM backup.`+t); err != nil {
			tx.Rollback()
			return fmt.Errorf("clear %s: %w", t, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO backup.`+t+` SELECT * FROM main.`+t); err != nil {
			tx.Rollback()
			return fmt.Errorf("copy %s: %w", t, err)
		}
	}
	for _, q := range fixups {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
