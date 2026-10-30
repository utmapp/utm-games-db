package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/igdb"
)

// CreateTriageRequest records a gift/demo offer for a game.
func (s *Store) CreateTriageRequest(ctx context.Context, gameID, userID int64, kind, storefront, demoURL, notes string) (int64, error) {
	now := time.Now().Unix()
	var id int64
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM games WHERE igdb_game_id = ? AND report_count > 0`, gameID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrGameInactive
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO triage_requests (game_id, user_id, kind, storefront, demo_url, notes, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			gameID, userID, kind, nullStr(storefront), nullStr(demoURL), nullStr(notes), now)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return nil
	})
	return id, err
}

// ListTriageRequests returns requests, newest first, optionally filtered by
// status.
func (s *Store) ListTriageRequests(ctx context.Context, st string, limit int) ([]TriageRequest, error) {
	if st == "" {
		return s.triageRequests(ctx, `1`, `t.id DESC`, clampLimit(limit))
	}
	return s.triageRequests(ctx, `t.status = ?`, `t.id DESC`, clampLimit(limit), st)
}

// triageRequests runs the triage request query with a filter and order;
// limit < 0 means no limit.
func (s *Store) triageRequests(ctx context.Context, where, order string, limit int, args ...any) ([]TriageRequest, error) {
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT t.id, t.game_id, g.name, t.user_id, t.kind, COALESCE(t.storefront, ''), COALESCE(t.demo_url, ''), COALESCE(t.notes, ''), t.status, t.created_at
		FROM triage_requests t JOIN igdb_games g ON g.id = t.game_id WHERE `+where+` ORDER BY `+order+` LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TriageRequest
	for rows.Next() {
		var t TriageRequest
		var created int64
		if err := rows.Scan(&t.ID, &t.GameID, &t.GameName, &t.UserID, &t.Kind, &t.Storefront, &t.DemoURL, &t.Notes, &t.Status, &created); err != nil {
			return nil, err
		}
		t.CreatedAt = unix(created)
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetTriageRequestStatus updates a request (open | done | rejected).
func (s *Store) SetTriageRequestStatus(ctx context.Context, id int64, st string) error {
	if st != "open" && st != "done" && st != "rejected" {
		return errors.New("invalid status")
	}
	res, err := s.DB.Writer.ExecContext(ctx, `UPDATE triage_requests SET status = ? WHERE id = ?`, st, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReplaceOwned replaces the owned-games list and rematches availability.
func (s *Store) ReplaceOwned(ctx context.Context, entries []OwnedEntry) (matched int, err error) {
	now := time.Now().Unix()
	err = s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM triage_owned`); err != nil {
			return err
		}
		for _, e := range entries {
			store := strings.ToLower(strings.TrimSpace(e.Storefront))
			id := strings.TrimSpace(e.StorefrontID)
			if e.IGDBGameID != 0 && store == "" {
				store = "igdb"
			}
			if store == "igdb" && id == "" && e.IGDBGameID != 0 {
				id = strconv.FormatInt(e.IGDBGameID, 10)
			}
			if store == "" || id == "" {
				continue
			}
			if store != "igdb" {
				id = strings.ToLower(id)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO triage_owned (storefront, storefront_id, free, received_at) VALUES (?, ?, ?, ?) ON CONFLICT(storefront, storefront_id) DO UPDATE SET free = excluded.free, received_at = excluded.received_at`, store, id, e.Free, now); err != nil {
				return err
			}
		}
		if err := igdb.MatchOwned(ctx, tx, 0); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM triage_owned WHERE igdb_game_id IS NOT NULL`).Scan(&matched)
	})
	return
}

// SetManualAvailability marks or unmarks a game as triage-available by hand.
func (s *Store) SetManualAvailability(ctx context.Context, gameID int64, on bool) error {
	now := time.Now().Unix()
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if on {
			if _, err := tx.ExecContext(ctx, `INSERT INTO triage_availability (game_id, source, updated_at) VALUES (?, 'manual', ?) ON CONFLICT(game_id) DO UPDATE SET source = 'manual', storefront = NULL, storefront_id = NULL, updated_at = excluded.updated_at`, gameID, now); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `DELETE FROM triage_availability WHERE game_id = ? AND source = 'manual'`, gameID); err != nil {
				return err
			}
		}
		return igdb.RebuildAvailability(ctx, tx, gameID)
	})
}

// ListOwned returns the raw owned list with match state.
func (s *Store) ListOwned(ctx context.Context) ([]OwnedEntry, error) {
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT storefront, storefront_id, free, COALESCE(igdb_game_id, 0) FROM triage_owned ORDER BY storefront, storefront_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OwnedEntry
	for rows.Next() {
		var e OwnedEntry
		if err := rows.Scan(&e.Storefront, &e.StorefrontID, &e.Free, &e.IGDBGameID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
