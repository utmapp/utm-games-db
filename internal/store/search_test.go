package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/utmapp/utm-games-db/internal/db"
)

// Search offers base games before DLC, expansions and bundles, and leaves
// seasons, packs, updates and the like out entirely.
func TestSearchCatalogTypes(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	rows := []struct {
		id   int64
		name string
		typ  any
	}{
		{1, "Overwatch 2: Season 14 - Hazard", 7},
		{2, "Overwatch 2: Hero Pack", 13},
		{3, "Overwatch 2: Invasion", 1},
		{4, "Overwatch 2", 0},
		{5, "Overwatch 2: Balance Update", 14},
		{6, "Overwatch 2 Remastered", 9},
		{7, "Overwatch 2: Legacy", nil}, // imported before main games carried an explicit type
		{9, "Overwatch 2 Trilogy", 3},   // a bundle: offered, after base games
	}
	// IGDB renamed Overwatch 2 to "Overwatch"; its alternative names still say so.
	if _, err := d.Writer.ExecContext(ctx, `INSERT INTO igdb_games (id, name, alt_names, slug, game_type_id, first_release_date, raw_json, synced_at) VALUES (8, 'Overwatch', 'Overwatch II' || char(10) || 'OW2' || char(10) || 'Overwatch 2', 'overwatch--1', 0, 1700000000, '{}', 0)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := d.Writer.ExecContext(ctx, `INSERT INTO igdb_games (id, name, slug, game_type_id, first_release_date, raw_json, synced_at) VALUES (?, ?, ?, ?, ?, '{}', 0)`, r.id, r.name, r.name, r.typ, 1600000000+r.id); err != nil {
			t.Fatal(err)
		}
	}
	for _, tn := range []struct {
		id   int
		name string
	}{{0, "Main Game"}, {1, "DLC"}, {7, "Season"}, {9, "Remaster"}, {13, "Pack / Addon"}, {14, "Update"}} {
		d.Writer.ExecContext(ctx, `INSERT INTO igdb_game_types (id, type) VALUES (?, ?)`, tn.id, tn.name)
	}
	hits, err := New(d).SearchCatalog(ctx, "overwatch 2", 20)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, h := range hits {
		got = append(got, h.ID)
	}
	// Base-game tier (ids 4, 6, 7 and, by alternative name, 8) before the
	// DLC (3) and the bundle (9); 1, 2 and 5 absent.
	if len(got) != 6 {
		t.Fatalf("got %v; want four base entries, then the DLC and the bundle", got)
	}
	for _, id := range got[:4] {
		if id != 4 && id != 6 && id != 7 && id != 8 {
			t.Fatalf("unexpected %d among base games: %v", id, got)
		}
	}
	for _, id := range got[4:] {
		if id != 3 && id != 9 {
			t.Fatalf("unexpected %d in the add-on tier: %v", id, got)
		}
	}
	if hits, _ := New(d).SearchCatalog(ctx, "ow2", 5); len(hits) != 1 || hits[0].ID != 8 {
		t.Fatalf("acronym search: %+v", hits)
	}
	// The exact title comes first, then titles starting with the query,
	// whatever the full-text score says (alternative names such as
	// executable names would otherwise push "PortalBox" above "Portal").
	for _, r := range []struct {
		id   int64
		name string
		alt  string
	}{{20, "Portal", "传送门\nhl2.exe"}, {21, "PortalBox", "PortalBox.exe"}, {22, "Portal 2", "Portal2.exe"}, {23, "Ecto Portal", ""}} {
		if _, err := d.Writer.ExecContext(ctx, `INSERT INTO igdb_games (id, name, alt_names, slug, game_type_id, first_release_date, raw_json, synced_at) VALUES (?, ?, ?, ?, 0, ?, '{}', 0)`, r.id, r.name, r.alt, r.name, 1500000000+r.id); err != nil {
			t.Fatal(err)
		}
	}
	hits, _ = New(d).SearchCatalog(ctx, "portal", 10)
	got = got[:0]
	for _, h := range hits {
		got = append(got, h.ID)
	}
	if len(got) < 4 || got[0] != 20 || got[1] != 22 {
		t.Fatalf("portal search order %v; want Portal, then Portal 2 first", got)
	}
}
