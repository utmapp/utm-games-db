package igdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/utmapp/utm-games-db/internal/db"
)

func TestParseStorefront(t *testing.T) {
	cases := []struct {
		typ   int64
		url   string
		store string
		id    string
		ok    bool
	}{
		{WebsiteSteam, "https://store.steampowered.com/app/620/Portal_2/", "steam", "620", true},
		{WebsiteSteam, "https://steamcommunity.com/app/620", "", "", false},
		{WebsiteGOG, "https://www.gog.com/en/game/the_witcher_3_wild_hunt", "gog", "the_witcher_3_wild_hunt", true},
		{WebsiteGOG, "https://www.gog.com/game/Cyberpunk_2077", "gog", "cyberpunk_2077", true},
		{WebsiteEpic, "https://store.epicgames.com/en-US/p/hades", "epic", "hades", true},
		{WebsiteEpic, "https://store.epicgames.com/p/control", "epic", "control", true},
		{WebsiteItch, "https://ncase.itch.io/wbwwb", "itch", "ncase/wbwwb", true},
		{1, "https://example.com", "", "", false},
	}
	for _, c := range cases {
		s, id, ok := ParseStorefront(c.typ, c.url)
		if s != c.store || id != c.id || ok != c.ok {
			t.Errorf("ParseStorefront(%d, %q) = %q,%q,%v; want %q,%q,%v", c.typ, c.url, s, id, ok, c.store, c.id, c.ok)
		}
	}
}

// fakeIGDB serves a fixed catalog with keyset pagination semantics. Like the
// real API, it promises nothing about the order of rows that tie on the sort
// key: ties on updated_at come back in descending id order.
func fakeIGDB(t *testing.T, games []Game) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	idRe := regexp.MustCompile(`id > (\d+)`)
	updRe := regexp.MustCompile(`updated_at > (\d+)`)
	eqRe := regexp.MustCompile(`updated_at = (\d+) & id > (\d+)`)
	limRe := regexp.MustCompile(`limit (\d+)`)
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"tok","expires_in":5000000,"token_type":"bearer"}`)
	})
	mux.HandleFunc("/v4/games", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Client-ID") != "cid" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		q := string(body)
		limit, _ := strconv.Atoi(limRe.FindStringSubmatch(q)[1])
		var out []Game
		if m := eqRe.FindStringSubmatch(q); m != nil {
			ts, _ := strconv.ParseInt(m[1], 10, 64)
			after, _ := strconv.ParseInt(m[2], 10, 64)
			for _, g := range games {
				if g.UpdatedAt == ts && g.ID > after {
					out = append(out, g)
				}
			}
		} else if m := updRe.FindStringSubmatch(q); m != nil && strings.Contains(q, "sort updated_at") {
			after, _ := strconv.ParseInt(m[1], 10, 64)
			for _, g := range games {
				if g.UpdatedAt > after {
					out = append(out, g)
				}
			}
			sort.SliceStable(out, func(i, j int) bool {
				if out[i].UpdatedAt != out[j].UpdatedAt {
					return out[i].UpdatedAt < out[j].UpdatedAt
				}
				return out[i].ID > out[j].ID
			})
		} else if m := idRe.FindStringSubmatch(q); m != nil {
			after, _ := strconv.ParseInt(m[1], 10, 64)
			for _, g := range games {
				if g.ID > after {
					out = append(out, g)
				}
			}
		}
		if len(out) > limit {
			out = out[:limit]
		}
		json.NewEncoder(w).Encode(out)
	})
	for _, ep := range []string{"game_types", "genres", "game_engines", "covers", "websites"} {
		mux.HandleFunc("/v4/"+ep, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "[]") })
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestSyncFullThenIncremental(t *testing.T) {
	var games []Game
	for i := int64(1); i <= 1203; i++ {
		games = append(games, Game{ID: i, Name: fmt.Sprintf("Game %d", i), Slug: fmt.Sprintf("game-%d", i), Platforms: []int64{6}, UpdatedAt: 1000 + i})
	}
	srv, calls := fakeIGDB(t, games)
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c := NewClient("cid", "sec", DBTokenStore{DB: d})
	c.APIBase = srv.URL + "/v4"
	c.TokenURL = srv.URL + "/oauth2/token"
	s := &Syncer{DB: d, Client: c, Log: slog.Default(), Platforms: []int64{6}, CoverDir: t.TempDir()}
	ctx := context.Background()

	sum, err := s.Run(ctx, false, 0) // empty table → forced full
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Full || sum.Fetched != 1203 || sum.Inserted != 1203 || sum.Pages != 3 {
		t.Fatalf("full: %+v", sum)
	}
	var n int
	d.Reader.QueryRow(`SELECT COUNT(*) FROM igdb_games`).Scan(&n)
	if n != 1203 {
		t.Fatalf("catalog rows = %d", n)
	}
	wm, _ := s.getInt(ctx, "watermark")
	if wm != 2203 {
		t.Fatalf("watermark = %d", wm)
	}

	// Change one game and add a new one; incremental must pick up both only.
	games[5].Name = "Renamed"
	games[5].UpdatedAt = 5000
	games = append(games, Game{ID: 9999, Name: "New", Slug: "new", Platforms: []int64{6}, UpdatedAt: 5001})
	srv2, _ := fakeIGDB(t, games)
	c.APIBase = srv2.URL + "/v4"
	before := *calls
	sum, err = s.Run(ctx, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Full || sum.Fetched != 2 || sum.Inserted != 1 || sum.Updated != 1 {
		t.Fatalf("incremental: %+v", sum)
	}
	_ = before
	var name string
	d.Reader.QueryRow(`SELECT name FROM igdb_games WHERE id = 6`).Scan(&name)
	if name != "Renamed" {
		t.Fatalf("name = %q", name)
	}
	d.Reader.QueryRow(`SELECT COUNT(*) FROM igdb_games_fts WHERE igdb_games_fts MATCH 'renamed'`).Scan(&n)
	if n != 1 {
		t.Fatalf("fts not updated: %d", n)
	}
	// Unchanged rows are skipped on a full re-run.
	sum, err = s.Run(ctx, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unchanged != 1204 || sum.Updated != 0 || sum.Inserted != 0 {
		t.Fatalf("full re-run: %+v", sum)
	}
	if !strings.Contains(c.APIBase, srv2.URL) {
		t.Fatal("unexpected")
	}
}

func TestIncrementalDrainsSharedTimestampAtPageBoundary(t *testing.T) {
	var games []Game
	for i := int64(1); i <= 1203; i++ {
		games = append(games, Game{ID: i, Name: fmt.Sprintf("Game %d", i), Slug: fmt.Sprintf("game-%d", i), Platforms: []int64{6}, UpdatedAt: 1000 + i})
	}
	srv, _ := fakeIGDB(t, games)
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c := NewClient("cid", "sec", DBTokenStore{DB: d})
	c.APIBase = srv.URL + "/v4"
	c.TokenURL = srv.URL + "/oauth2/token"
	s := &Syncer{DB: d, Client: c, Log: slog.Default(), Platforms: []int64{6}, CoverDir: t.TempDir()}
	ctx := context.Background()
	if _, err := s.Run(ctx, true, 0); err != nil {
		t.Fatal(err)
	}
	// IGDB bulk-touches 700 games in the same second (straddling a 500-row
	// page), plus one game a second later. Every one of them must be updated.
	for i := 100; i < 800; i++ {
		games[i].Name = "Touched"
		games[i].UpdatedAt = 6000
	}
	games[900].Name = "Later"
	games[900].UpdatedAt = 6001
	srv2, _ := fakeIGDB(t, games)
	c.APIBase = srv2.URL + "/v4"
	sum, err := s.Run(ctx, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Updated != 701 {
		t.Fatalf("expected 701 updates, got %+v", sum)
	}
	var n int
	d.Reader.QueryRow(`SELECT COUNT(*) FROM igdb_games WHERE name = 'Touched'`).Scan(&n)
	if n != 700 {
		t.Fatalf("touched rows = %d", n)
	}
	if wm, _ := s.getInt(ctx, "watermark"); wm != 6001 {
		t.Fatalf("watermark = %d", wm)
	}
}
