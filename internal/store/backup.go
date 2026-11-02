package store

import "context"

// BackupTables are the tables a backup carries: everything users and
// maintainers produced, in an order that satisfies the foreign keys between
// them. The IGDB mirror (igdb_* tables) is left out: it is re-imported from
// IGDB, and is most of the database. login_codes are transient and left
// out; sessions are cheap and kept so a restore does not sign everyone out.
var BackupTables = []string{
	"schema_migrations",
	"users", "sessions",
	"utm_versions",
	"games", "stars", "configurations", "reports", "comments", "plus_ones", "attachments",
	"upload_ledger", "spam_log",
	"triage_owned", "triage_availability", "triage_requests", "deletion_requests", "content_flags",
}

// BackupFixups adjust the copy for a restore without the catalog: covers
// and storefront links are re-resolved from IGDB once the catalog is back.
var BackupFixups = []string{
	`UPDATE backup.games SET details_fetched_at = NULL`,
}

// TableCounts returns the row count of each table, for the backup manifest.
func (s *Store) TableCounts(ctx context.Context, tables []string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range tables {
		var n int64
		if err := s.DB.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t).Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}

// CountUntriaged returns how many visible reports have no triage status.
func (s *Store) CountUntriaged(ctx context.Context) (int, error) {
	var n int
	err := s.DB.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM reports WHERE visibility = 'visible' AND triage_status IS NULL`).Scan(&n)
	return n, err
}
