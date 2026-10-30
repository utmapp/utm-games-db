package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/igdb"
	"github.com/utmapp/utm-games-db/internal/status"
)

// ListParams filters the public game list.
type ListParams struct {
	Query   string
	Status  string // "" or a rating
	Sort    string // stars | reports | recent | name
	Page    int
	PerPage int
}

// ListGames returns one page of active games plus the total count.
func (s *Store) ListGames(ctx context.Context, p ListParams) ([]GameCard, int, error) {
	if p.PerPage <= 0 {
		p.PerPage = 24
	}
	if p.Page <= 0 {
		p.Page = 1
	}
	where := []string{"a.report_count > 0"}
	var args []any
	q := strings.TrimSpace(p.Query)
	if q != "" {
		match := ftsQuery(q)
		if match == "" {
			return nil, 0, nil
		}
		where = append(where, `a.igdb_game_id IN (SELECT rowid FROM igdb_games_fts WHERE igdb_games_fts MATCH ?)`)
		args = append(args, match)
	}
	if p.Status != "" && status.Rating(p.Status).Valid() {
		where = append(where, `a.status = ?`)
		args = append(args, p.Status)
	}
	wh := " WHERE " + strings.Join(where, " AND ")
	var total int
	if err := s.DB.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM games a`+wh, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "a.updated_at DESC"
	switch p.Sort {
	case "stars":
		order = "a.star_count DESC, a.report_count DESC, g.name COLLATE NOCASE"
	case "reports":
		order = "a.report_count DESC, a.star_count DESC, g.name COLLATE NOCASE"
	case "name":
		order = "g.name COLLATE NOCASE"
	}
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT a.igdb_game_id, g.name, g.slug, COALESCE(g.first_release_date, 0), a.status, a.report_count, a.star_count, a.triage_available, a.updated_at,
			COALESCE(c.local_small, ''), v.id, v.version, v.v_major, v.v_minor, v.v_patch, v.is_prerelease
		FROM games a JOIN igdb_games g ON g.id = a.igdb_game_id
		LEFT JOIN igdb_covers c ON c.game_id = a.igdb_game_id
		LEFT JOIN utm_versions v ON v.id = a.status_version_id`+wh+` ORDER BY `+order+` LIMIT ? OFFSET ?`,
		append(args, p.PerPage, (p.Page-1)*p.PerPage)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []GameCard
	for rows.Next() {
		var g GameCard
		var rel, upd int64
		var v nullVersion
		if err := rows.Scan(append([]any{&g.ID, &g.Name, &g.Slug, &rel, &g.Status, &g.ReportCount, &g.StarCount, &g.TriageAvailable, &upd, &g.CoverSmall}, v.dest()...)...); err != nil {
			return nil, 0, err
		}
		if rel > 0 {
			g.Year = unix(rel).Year()
		}
		g.UpdatedAt = unix(upd)
		g.StatusVersion = v.get()
		out = append(out, g)
	}
	return out, total, rows.Err()
}

// ftsQuery turns free text into a prefix-matching FTS5 query, quoting every
// token so user input cannot inject FTS syntax.
func ftsQuery(q string) string {
	var parts []string
	for _, tok := range strings.Fields(q) {
		tok = strings.ReplaceAll(tok, `"`, "")
		if tok == "" {
			continue
		}
		parts = append(parts, `"`+tok+`"*`)
	}
	return strings.Join(parts, " ")
}

// SearchCatalog searches the full IGDB mirror for the report form.
func (s *Store) SearchCatalog(ctx context.Context, q string, limit int) ([]CatalogHit, error) {
	match := ftsQuery(q)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	// Rank: FTS relevance, then main games before DLC/mods, then newest.
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT g.id, g.name, g.slug, COALESCE(g.first_release_date, 0), COALESCE(t.type, ''), a.igdb_game_id IS NOT NULL
		FROM igdb_games_fts f JOIN igdb_games g ON g.id = f.rowid
		LEFT JOIN igdb_game_types t ON t.id = g.game_type_id
		LEFT JOIN games a ON a.igdb_game_id = g.id
		WHERE igdb_games_fts MATCH ?
		ORDER BY bm25(igdb_games_fts), (g.game_type_id = 0) DESC, (a.igdb_game_id IS NOT NULL) DESC, g.first_release_date DESC
		LIMIT ?`, match, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CatalogHit
	for rows.Next() {
		var h CatalogHit
		var rel int64
		if err := rows.Scan(&h.ID, &h.Name, &h.Slug, &rel, &h.GameType, &h.Active); err != nil {
			return nil, err
		}
		if rel > 0 {
			h.Year = unix(rel).Year()
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// GetGame loads the detail view. Works for inactive catalog games too (Active=false).
func (s *Store) GetGame(ctx context.Context, id int64) (*GameDetail, error) {
	var d GameDetail
	var rel, upd, act sql.NullInt64
	var summary, igdbURL, gameType sql.NullString
	var genresJSON, enginesJSON string
	var st sql.NullString
	var rc, sc sql.NullInt64
	var ta sql.NullBool
	var v nullVersion
	var coverBig, coverSmall, coverImg sql.NullString
	var triageSource sql.NullString
	err := s.DB.Reader.QueryRowContext(ctx, `SELECT g.id, g.name, g.slug, COALESCE(g.first_release_date, 0), g.summary, g.url, t.type, g.genres_json, g.game_engines_json,
			a.status, a.report_count, a.star_count, a.triage_available, a.updated_at, a.activated_at,
			v.id, v.version, v.v_major, v.v_minor, v.v_patch, v.is_prerelease,
			c.local_big, c.local_small, c.image_id, ta.source
		FROM igdb_games g
		LEFT JOIN igdb_game_types t ON t.id = g.game_type_id
		LEFT JOIN games a ON a.igdb_game_id = g.id
		LEFT JOIN utm_versions v ON v.id = a.status_version_id
		LEFT JOIN igdb_covers c ON c.game_id = g.id
		LEFT JOIN triage_availability ta ON ta.game_id = g.id
		WHERE g.id = ?`, id).Scan(append(append([]any{&d.ID, &d.Name, &d.Slug, &rel, &summary, &igdbURL, &gameType, &genresJSON, &enginesJSON,
		&st, &rc, &sc, &ta, &upd, &act}, v.dest()...), &coverBig, &coverSmall, &coverImg, &triageSource)...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if rel.Int64 > 0 {
		d.Year = unix(rel.Int64).Year()
	}
	d.Summary = summary.String
	d.IGDBURL = igdbURL.String
	d.GameType = gameType.String
	d.Active = st.Valid && rc.Int64 > 0
	d.Status = status.Rating(st.String)
	if !d.Active {
		d.Status = status.Unknown
	}
	d.ReportCount = int(rc.Int64)
	d.StarCount = int(sc.Int64)
	d.TriageAvailable = ta.Bool
	d.TriageSource = triageSource.String
	d.UpdatedAt = unix(upd.Int64)
	d.ActivatedAt = unix(act.Int64)
	d.StatusVersion = v.get()
	d.CoverBig = coverBig.String
	d.CoverSmall = coverSmall.String
	d.CoverImageID = coverImg.String
	d.Genres, _ = s.names(ctx, "igdb_genres", genresJSON)
	d.Engines, _ = s.names(ctx, "igdb_game_engines", enginesJSON)

	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT storefront, COALESCE(storefront_id, ''), url FROM igdb_websites WHERE game_id = ? AND storefront IS NOT NULL ORDER BY type_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var w Website
		if err := rows.Scan(&w.Storefront, &w.StorefrontID, &w.URL); err != nil {
			return nil, err
		}
		if seen[w.Storefront] {
			continue
		}
		seen[w.Storefront] = true
		w.Label = igdb.StorefrontLabel(w.Storefront)
		d.Websites = append(d.Websites, w)
	}
	return &d, rows.Err()
}

func (s *Store) names(ctx context.Context, table, idsJSON string) ([]string, error) {
	var ids []int64
	if err := json.Unmarshal([]byte(idsJSON), &ids); err != nil || len(ids) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT name FROM `+table+` WHERE id IN (`+strings.Join(ph, ",")+`) ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// activateGame inserts the games row for a catalog game if it is not active yet.
func activateGame(ctx context.Context, tx *sql.Tx, gameID int64, now int64) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM igdb_games WHERE id = ?`, gameID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("game %d: %w", gameID, ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO games (igdb_game_id, activated_at, updated_at) VALUES (?, ?, ?) ON CONFLICT(igdb_game_id) DO NOTHING`, gameID, now, now); err != nil {
		return err
	}
	return igdb.MatchOwned(ctx, tx, gameID)
}

// recomputeGame refreshes the denormalized counters and status of a game.
func recomputeGame(ctx context.Context, tx *sql.Tx, gameID int64, now int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT r.rating, r.created_at, r.plus_one_count, v.id, v.v_major, v.v_minor, v.v_patch
		FROM reports r JOIN configurations c ON c.id = r.config_id JOIN utm_versions v ON v.id = c.utm_version_id
		WHERE r.game_id = ? AND r.visibility = 'visible'`, gameID)
	if err != nil {
		return err
	}
	var votes []status.Vote
	for rows.Next() {
		var v status.Vote
		var at int64
		var plusOnes int
		if err := rows.Scan(&v.Rating, &at, &plusOnes, &v.VersionID, &v.Version.Major, &v.Version.Minor, &v.Version.Patch); err != nil {
			rows.Close()
			return err
		}
		v.At = time.Unix(at, 0)
		v.Weight = 1 + plusOnes // the report itself plus everyone who agreed
		votes = append(votes, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	res := status.Compute(votes)
	var verID any
	if res.VersionID != 0 {
		verID = res.VersionID
	}
	_, err = tx.ExecContext(ctx, `UPDATE games SET
			status = ?, status_version_id = ?,
			report_count = (SELECT COUNT(*) FROM reports WHERE game_id = games.igdb_game_id AND visibility = 'visible'),
			star_count = (SELECT COUNT(*) FROM stars WHERE game_id = games.igdb_game_id),
			updated_at = ?
		WHERE igdb_game_id = ?`, string(res.Rating), verID, now, gameID)
	return err
}

// RecomputeGame is the exported form used by moderation endpoints.
func (s *Store) RecomputeGame(ctx context.Context, gameID int64) error {
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		return recomputeGame(ctx, tx, gameID, time.Now().Unix())
	})
}

// ErrGameInactive is returned when starring a game with no reports.
var ErrGameInactive = errors.New("game has no reports yet")

// ToggleStar stars or unstars an active game and returns the new state.
func (s *Store) ToggleStar(ctx context.Context, userID, gameID int64) (starred bool, starCount int, err error) {
	now := time.Now().Unix()
	err = s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM games WHERE igdb_game_id = ? AND report_count > 0`, gameID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrGameInactive
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM stars WHERE user_id = ? AND game_id = ?`, userID, gameID)
		if err != nil {
			return err
		}
		if removed, _ := res.RowsAffected(); removed == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO stars (user_id, game_id, created_at) VALUES (?, ?, ?)`, userID, gameID, now); err != nil {
				return err
			}
			starred = true
		}
		if _, err := tx.ExecContext(ctx, `UPDATE games SET star_count = (SELECT COUNT(*) FROM stars WHERE game_id = ?), updated_at = ? WHERE igdb_game_id = ?`, gameID, now, gameID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT star_count FROM games WHERE igdb_game_id = ?`, gameID).Scan(&starCount)
	})
	return
}

// UserStars lists the game ids a user has starred.
func (s *Store) UserStars(ctx context.Context, userID int64) ([]int64, error) {
	return queryInt64s(ctx, s.DB.Reader, `SELECT game_id FROM stars WHERE user_id = ? ORDER BY created_at DESC`, userID)
}

// UserStarredGames lists starred games as cards (account page).
func (s *Store) UserStarredGames(ctx context.Context, userID int64) ([]GameCard, error) {
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT a.igdb_game_id, g.name, g.slug, a.status, a.report_count, a.star_count, a.triage_available, COALESCE(c.local_small, '')
		FROM stars s JOIN games a ON a.igdb_game_id = s.game_id JOIN igdb_games g ON g.id = a.igdb_game_id LEFT JOIN igdb_covers c ON c.game_id = a.igdb_game_id
		WHERE s.user_id = ? ORDER BY s.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GameCard
	for rows.Next() {
		var g GameCard
		if err := rows.Scan(&g.ID, &g.Name, &g.Slug, &g.Status, &g.ReportCount, &g.StarCount, &g.TriageAvailable, &g.CoverSmall); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryColumn runs a single-column query and collects the values.
func queryColumn[T any](ctx context.Context, q querier, query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var v T
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func queryInt64s(ctx context.Context, q querier, query string, args ...any) ([]int64, error) {
	return queryColumn[int64](ctx, q, query, args...)
}

func queryStrings(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	return queryColumn[string](ctx, q, query, args...)
}

// Stats for the list page header.
type Stats struct {
	Games   int
	Reports int
}

// Stats returns global counts.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	if err := s.DB.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(report_count), 0) FROM games WHERE report_count > 0`).Scan(&st.Games, &st.Reports); err != nil {
		return st, err
	}
	return st, nil
}
