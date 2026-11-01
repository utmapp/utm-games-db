package igdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/utmapp/utm-games-db/internal/db"
)

// Syncer mirrors the IGDB catalog into the local database.
type Syncer struct {
	DB        *db.DB
	Client    *Client
	Log       *slog.Logger
	Platforms []int64
	CoverDir  string // DATA_DIR/covers

	mu      sync.Mutex
	running bool
	status  Status
}

// Status is reported on the internal API.
type Status struct {
	Running      bool      `json:"running"`
	LastStart    time.Time `json:"last_start,omitempty"`
	LastEnd      time.Time `json:"last_end,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	LastSummary  *Summary  `json:"last_summary,omitempty"`
	Watermark    int64     `json:"watermark"`
	CatalogCount int64     `json:"catalog_count"`
}

// Summary describes one sync run.
type Summary struct {
	Full      bool          `json:"full"`
	Pages     int           `json:"pages"`
	Fetched   int           `json:"fetched"`
	Inserted  int           `json:"inserted"`
	Updated   int           `json:"updated"`
	Unchanged int           `json:"unchanged"`
	Refreshed int           `json:"active_refreshed"`
	Duration  time.Duration `json:"duration"`
}

// ErrAlreadyRunning is returned when a sync overlaps another.
var ErrAlreadyRunning = errors.New("igdb: sync already running")

// Status returns a snapshot of the sync state.
func (s *Syncer) Status(ctx context.Context) Status {
	s.mu.Lock()
	st := s.status
	st.Running = s.running
	s.mu.Unlock()
	st.Watermark, _ = s.getInt(ctx, "watermark")
	s.DB.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM igdb_games`).Scan(&st.CatalogCount)
	return st
}

func (s *Syncer) platformFilter() string {
	parts := make([]string, len(s.Platforms))
	for i, p := range s.Platforms {
		parts[i] = strconv.FormatInt(p, 10)
	}
	return "(" + strings.Join(parts, ",") + ")"
}

// Run performs a catalog sync. full forces a complete walk keyed by id;
// otherwise only games with updated_at past the stored watermark are fetched.
// maxPages > 0 limits the number of pages (for testing).
func (s *Syncer) Run(ctx context.Context, full bool, maxPages int) (*Summary, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil, ErrAlreadyRunning
	}
	s.running = true
	s.status.LastStart = time.Now()
	s.status.LastError = ""
	s.mu.Unlock()

	sum, err := s.run(ctx, full, maxPages)

	s.mu.Lock()
	s.running = false
	s.status.LastEnd = time.Now()
	if err != nil {
		s.status.LastError = err.Error()
	}
	if sum != nil {
		s.status.LastSummary = sum
	}
	s.mu.Unlock()
	return sum, err
}

func (s *Syncer) run(ctx context.Context, full bool, maxPages int) (*Summary, error) {
	start := time.Now()
	sum := &Summary{Full: full}
	if !s.Client.Configured() {
		return sum, errors.New("igdb: credentials not configured")
	}
	if err := s.syncLookups(ctx); err != nil {
		return sum, err
	}
	var count int64
	if err := s.DB.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM igdb_games`).Scan(&count); err != nil {
		return sum, err
	}
	watermark, _ := s.getInt(ctx, "watermark")
	if count == 0 || watermark == 0 {
		full = true
		sum.Full = true
	}

	var maxUpdated int64 = watermark
	var lastID int64
	cursor := watermark
	pageLimit := func() bool { return maxPages > 0 && sum.Pages >= maxPages }
	fetch := func(body string) ([]Game, error) {
		var games []Game
		if err := s.Client.Query(ctx, "games", body, &games); err != nil {
			return nil, err
		}
		if len(games) == 0 {
			return nil, nil
		}
		sum.Pages++
		sum.Fetched += len(games)
		ins, upd, unch, refreshed, err := s.upsertPage(ctx, games)
		if err != nil {
			return nil, err
		}
		sum.Inserted += ins
		sum.Updated += upd
		sum.Unchanged += unch
		sum.Refreshed += refreshed
		for _, g := range games {
			if g.UpdatedAt > maxUpdated {
				maxUpdated = g.UpdatedAt
			}
			if g.ID > lastID {
				lastID = g.ID
			}
		}
		s.Log.Info("igdb sync page", "page", sum.Pages, "fetched", len(games), "last_id", lastID, "max_updated", maxUpdated)
		return games, nil
	}
	for !pageLimit() {
		var body string
		if full {
			body = fmt.Sprintf("fields %s; where platforms = %s & id > %d; sort id asc; limit %d;", GameFields, s.platformFilter(), lastID, pageSize)
		} else {
			body = fmt.Sprintf("fields %s; where platforms = %s & updated_at > %d; sort updated_at asc; limit %d;", GameFields, s.platformFilter(), cursor, pageSize)
		}
		games, err := fetch(body)
		if err != nil {
			return sum, err
		}
		if len(games) < pageSize {
			break
		}
		if full {
			continue
		}
		// A full page ended on timestamp T. Other games may share T but fall
		// outside this page; drain them by id before moving past T, otherwise
		// they would be skipped forever once the watermark advances. IGDB does
		// not order ties by id, so the page's T-games are not necessarily the
		// lowest ids: walk all of T from the start (re-fetched rows are
		// recognised as unchanged).
		boundary := games[len(games)-1].UpdatedAt
		var boundaryID int64
		for !pageLimit() {
			more, err := fetch(fmt.Sprintf("fields %s; where platforms = %s & updated_at = %d & id > %d; sort id asc; limit %d;", GameFields, s.platformFilter(), boundary, boundaryID, pageSize))
			if err != nil {
				return sum, err
			}
			if len(more) == 0 {
				break
			}
			boundaryID = more[len(more)-1].ID
			if len(more) < pageSize {
				break
			}
		}
		cursor = boundary
	}
	// Only advance the watermark on a complete run; a page-limited test run
	// must not skip data.
	if maxPages <= 0 && maxUpdated > watermark {
		if err := s.setInt(ctx, "watermark", maxUpdated); err != nil {
			return sum, err
		}
	}
	sum.Duration = time.Since(start)
	s.Log.Info("igdb sync done", "full", full, "pages", sum.Pages, "fetched", sum.Fetched, "inserted", sum.Inserted, "updated", sum.Updated, "unchanged", sum.Unchanged, "duration", sum.Duration)
	return sum, nil
}

// upsertPage writes one page, skipping rows whose stored JSON is unchanged,
// and re-resolves details for active games that changed.
func (s *Syncer) upsertPage(ctx context.Context, games []Game) (inserted, updated, unchanged, refreshed int, err error) {
	ids := make([]any, len(games))
	ph := make([]string, len(games))
	for i, g := range games {
		ids[i] = g.ID
		ph[i] = "?"
	}
	existing := map[int64]string{}
	active := map[int64]bool{}
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT g.id, g.raw_json, a.igdb_game_id IS NOT NULL FROM igdb_games g LEFT JOIN games a ON a.igdb_game_id = g.id WHERE g.id IN (`+strings.Join(ph, ",")+`)`, ids...)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	for rows.Next() {
		var id int64
		var raw string
		var isActive bool
		if err := rows.Scan(&id, &raw, &isActive); err != nil {
			rows.Close()
			return 0, 0, 0, 0, err
		}
		existing[id] = raw
		active[id] = isActive
	}
	rows.Close()

	var changedActive []int64
	now := time.Now().Unix()
	err = s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO igdb_games (id, checksum, name, alt_names, slug, summary, url, first_release_date, game_type_id, cover_id, created_at, updated_at, genres_json, game_engines_json, raw_json, synced_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET checksum=excluded.checksum, name=excluded.name, alt_names=excluded.alt_names, slug=excluded.slug, summary=excluded.summary, url=excluded.url, first_release_date=excluded.first_release_date, game_type_id=excluded.game_type_id, cover_id=excluded.cover_id, created_at=excluded.created_at, updated_at=excluded.updated_at, genres_json=excluded.genres_json, game_engines_json=excluded.game_engines_json, raw_json=excluded.raw_json, synced_at=excluded.synced_at`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, g := range games {
			raw, _ := json.Marshal(g)
			if old, ok := existing[g.ID]; ok {
				if old == string(raw) {
					unchanged++
					continue
				}
				updated++
				if active[g.ID] {
					changedActive = append(changedActive, g.ID)
				}
			} else {
				inserted++
			}
			if _, err := stmt.ExecContext(ctx, g.ID, g.Checksum, g.Name, g.AltNames(), g.Slug, g.Summary, g.URL, nz(g.FirstReleaseDate), g.GameType, nz(g.Cover), nz(g.CreatedAt), nz(g.UpdatedAt),
				jsonArr(g.Genres), jsonArr(g.GameEngines), string(raw), now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, 0, 0, err
	}
	for _, id := range changedActive {
		if err := s.ResolveDetails(ctx, id); err != nil {
			s.Log.Warn("igdb: refresh active game details", "game", id, "err", err)
			continue
		}
		refreshed++
	}
	return inserted, updated, unchanged, refreshed, nil
}

func nz(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func jsonArr(v []int64) string {
	if len(v) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// syncLookups refreshes the small reference tables in full.
func (s *Syncer) syncLookups(ctx context.Context) error {
	if err := syncLookup(ctx, s, "game_types", "type", `INSERT INTO igdb_game_types (id, type) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET type = excluded.type`,
		func(r Typed) (int64, []any) { return r.ID, []any{r.ID, r.Type} }); err != nil {
		return err
	}
	for _, t := range []struct{ endpoint, table string }{{"genres", "igdb_genres"}, {"game_engines", "igdb_game_engines"}} {
		if err := syncLookup(ctx, s, t.endpoint, "name,slug", `INSERT INTO `+t.table+` (id, name, slug) VALUES (?, ?, ?) ON CONFLICT(id) DO UPDATE SET name = excluded.name, slug = excluded.slug`,
			func(r Named) (int64, []any) { return r.ID, []any{r.ID, r.Name, r.Slug} }); err != nil {
			return err
		}
	}
	return nil
}

// syncLookup pages through a lookup endpoint by id and upserts every row
// with upsert, whose arguments row returns along with the row's id.
func syncLookup[T any](ctx context.Context, s *Syncer, endpoint, fields, upsert string, row func(T) (int64, []any)) error {
	var lastID int64 = -1
	for {
		var rows []T
		if err := s.Client.Query(ctx, endpoint, fmt.Sprintf("fields %s; where id > %d; sort id asc; limit %d;", fields, lastID, pageSize), &rows); err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
			for _, r := range rows {
				id, args := row(r)
				if _, err := tx.ExecContext(ctx, upsert, args...); err != nil {
					return err
				}
				lastID = max(lastID, id)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(rows) < pageSize {
			return nil
		}
	}
}

// --- sync_state helpers ----------------------------------------------------

func (s *Syncer) getInt(ctx context.Context, key string) (int64, error) {
	var v string
	err := s.DB.Reader.QueryRowContext(ctx, `SELECT value FROM igdb_sync_state WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

func (s *Syncer) setInt(ctx context.Context, key string, v int64) error {
	_, err := s.DB.Writer.ExecContext(ctx, `INSERT INTO igdb_sync_state (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, strconv.FormatInt(v, 10))
	return err
}

// DBTokenStore persists the OAuth token in igdb_sync_state.
type DBTokenStore struct{ DB *db.DB }

// GetToken implements TokenStore.
func (t DBTokenStore) GetToken(ctx context.Context) (string, time.Time, error) {
	var tok, exp string
	err := t.DB.Reader.QueryRowContext(ctx, `SELECT value FROM igdb_sync_state WHERE key = 'token'`).Scan(&tok)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := t.DB.Reader.QueryRowContext(ctx, `SELECT value FROM igdb_sync_state WHERE key = 'token_expires'`).Scan(&exp); err != nil {
		return "", time.Time{}, err
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		return "", time.Time{}, err
	}
	return tok, time.Unix(n, 0), nil
}

// SetToken implements TokenStore.
func (t DBTokenStore) SetToken(ctx context.Context, token string, expiresAt time.Time) error {
	return t.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		for k, v := range map[string]string{"token": token, "token_expires": strconv.FormatInt(expiresAt.Unix(), 10)} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO igdb_sync_state (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return err
			}
		}
		return nil
	})
}
