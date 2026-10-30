package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenMigrateAndFTS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = d.Writer.ExecContext(ctx, `INSERT INTO igdb_games (id, name, slug, raw_json, synced_at) VALUES (1, 'The Legend of Zélda', 'zelda', '{}', 0), (2, 'Half-Life 2', 'half-life-2', '{}', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM igdb_games_fts WHERE igdb_games_fts MATCH 'zelda'`).Scan(&n); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 fts hit, got %d", n)
	}
	var mode string
	if err := d.Reader.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode=%q err=%v", mode, err)
	}
	var applied int
	if err := d.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	d.Close()
	// Re-opening the same file must not re-apply migrations (a second run of
	// any CREATE would fail) and must keep the data.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer d2.Close()
	var again, games int
	d2.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&again)
	d2.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM igdb_games`).Scan(&games)
	if again != applied || games != 2 {
		t.Fatalf("after re-open: migrations %d -> %d, games = %d", applied, again, games)
	}
}
