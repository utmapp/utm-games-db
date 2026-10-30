package igdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// ResolveDetails fetches the cover and websites of a game and
// stores them locally. It is idempotent and safe to call repeatedly; it is
// invoked when a game first becomes active and whenever an active game's
// catalog row changes.
func (s *Syncer) ResolveDetails(ctx context.Context, gameID int64) error {
	var coverID sql.NullInt64
	if err := s.DB.Reader.QueryRowContext(ctx, `SELECT cover_id FROM igdb_games WHERE id = ?`, gameID).Scan(&coverID); err != nil {
		return fmt.Errorf("igdb: game %d not in catalog: %w", gameID, err)
	}

	var covers []Cover
	if coverID.Valid {
		if err := s.Client.Query(ctx, "covers", fmt.Sprintf("fields image_id,width,height,game; where id = %d; limit 1;", coverID.Int64), &covers); err != nil {
			return err
		}
	}
	var websites []Website
	if err := s.Client.Query(ctx, "websites", fmt.Sprintf("fields type,url,trusted,game; where game = %d; limit 100;", gameID), &websites); err != nil {
		return err
	}

	var localBig, localSmall string
	if len(covers) > 0 && imageIDRe.MatchString(covers[0].ImageID) {
		var err error
		localBig, err = s.downloadCover(ctx, covers[0].ImageID, "cover_big_2x", "big")
		if err != nil {
			s.Log.Warn("igdb: cover download failed", "game", gameID, "err", err)
		}
		localSmall, err = s.downloadCover(ctx, covers[0].ImageID, "cover_small_2x", "small")
		if err != nil {
			s.Log.Warn("igdb: cover download failed", "game", gameID, "err", err)
		}
	}

	now := time.Now().Unix()
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM igdb_covers WHERE game_id = ?`, gameID); err != nil {
			return err
		}
		for _, c := range covers {
			if _, err := tx.ExecContext(ctx, `INSERT INTO igdb_covers (id, game_id, image_id, width, height, local_big, local_small, fetched_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				c.ID, gameID, c.ImageID, c.Width, c.Height, nullStr(localBig), nullStr(localSmall), now); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM igdb_websites WHERE game_id = ?`, gameID); err != nil {
			return err
		}
		for _, w := range websites {
			store, id, _ := ParseStorefront(w.Type, w.URL)
			if _, err := tx.ExecContext(ctx, `INSERT INTO igdb_websites (id, game_id, type_id, url, trusted, storefront, storefront_id) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				w.ID, gameID, w.Type, w.URL, w.Trusted, nullStr(store), nullStr(id)); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE games SET details_fetched_at = ? WHERE igdb_game_id = ?`, now, gameID); err != nil {
			return err
		}
		// Match any owned-games entries that were waiting for this game's storefront ids.
		return MatchOwned(ctx, tx, gameID)
	})
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// imageIDRe is the shape of IGDB image ids. They become local file names
// and URL path segments, so anything else is refused.
var imageIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// downloadCover stores the cover locally and returns the relative file name.
func (s *Syncer) downloadCover(ctx context.Context, imageID, size, suffix string) (string, error) {
	if s.CoverDir == "" {
		return "", errors.New("cover dir not configured")
	}
	if err := os.MkdirAll(s.CoverDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s_%s.jpg", imageID, suffix)
	dest := filepath.Join(s.CoverDir, name)
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		return name, nil
	}
	tmp, err := os.CreateTemp(s.CoverDir, ".cover-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if err := s.Client.Download(ctx, s.Client.ImageURL(imageID, size), tmp, 8<<20); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return name, nil
}

// MatchOwned links triage_owned rows to the given game (or to all games when
// gameID is 0) using the storefront ids parsed from igdb_websites, and
// rebuilds triage_availability for the affected games.
func MatchOwned(ctx context.Context, tx *sql.Tx, gameID int64) error {
	where := ""
	args := []any{}
	if gameID != 0 {
		where = " AND w.game_id = ?"
		args = append(args, gameID)
	}
	// Direct IGDB ids.
	if _, err := tx.ExecContext(ctx, `UPDATE triage_owned SET igdb_game_id = CAST(storefront_id AS INTEGER) WHERE storefront = 'igdb' AND igdb_game_id IS NULL`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE triage_owned SET igdb_game_id = (
			SELECT w.game_id FROM igdb_websites w WHERE w.storefront = triage_owned.storefront AND w.storefront_id = triage_owned.storefront_id`+where+` LIMIT 1)
		WHERE igdb_game_id IS NULL AND storefront <> 'igdb'`, args...); err != nil {
		return err
	}
	return RebuildAvailability(ctx, tx, gameID)
}

// RebuildAvailability recomputes triage_availability (for one game or all)
// from triage_owned and manual entries, and mirrors the flag onto games.
func RebuildAvailability(ctx context.Context, tx *sql.Tx, gameID int64) error {
	now := time.Now().Unix()
	if gameID != 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM triage_availability WHERE game_id = ? AND source <> 'manual'`, gameID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO triage_availability (game_id, source, storefront, storefront_id, updated_at)
			SELECT igdb_game_id, CASE WHEN free THEN 'free' ELSE 'owned' END, storefront, storefront_id, ? FROM triage_owned
			WHERE igdb_game_id = ? ORDER BY free DESC LIMIT 1
			ON CONFLICT(game_id) DO NOTHING`, now, gameID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE games SET triage_available = EXISTS (SELECT 1 FROM triage_availability t WHERE t.game_id = games.igdb_game_id) WHERE igdb_game_id = ?`, gameID)
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM triage_availability WHERE source <> 'manual'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO triage_availability (game_id, source, storefront, storefront_id, updated_at)
		SELECT igdb_game_id, CASE WHEN MAX(free) THEN 'free' ELSE 'owned' END, MIN(storefront), MIN(storefront_id), ? FROM triage_owned
		WHERE igdb_game_id IS NOT NULL GROUP BY igdb_game_id
		ON CONFLICT(game_id) DO NOTHING`, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE games SET triage_available = EXISTS (SELECT 1 FROM triage_availability t WHERE t.game_id = games.igdb_game_id)`)
	return err
}

// PeekCover returns the cover image id of a catalog game, fetching and
// caching the cover row (without downloading the image) if needed. It is
// used by the report form to show cover art before a game becomes active.
func (s *Syncer) PeekCover(ctx context.Context, gameID int64) (string, error) {
	var imageID string
	err := s.DB.Reader.QueryRowContext(ctx, `SELECT image_id FROM igdb_covers WHERE game_id = ? LIMIT 1`, gameID).Scan(&imageID)
	if err == nil {
		return imageID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var coverID sql.NullInt64
	if err := s.DB.Reader.QueryRowContext(ctx, `SELECT cover_id FROM igdb_games WHERE id = ?`, gameID).Scan(&coverID); err != nil {
		return "", err
	}
	if !coverID.Valid {
		return "", nil
	}
	var covers []Cover
	if err := s.Client.Query(ctx, "covers", fmt.Sprintf("fields image_id,width,height,game; where id = %d; limit 1;", coverID.Int64), &covers); err != nil {
		return "", err
	}
	if len(covers) == 0 || !imageIDRe.MatchString(covers[0].ImageID) {
		return "", nil
	}
	c := covers[0]
	_, err = s.DB.Writer.ExecContext(ctx, `INSERT INTO igdb_covers (id, game_id, image_id, width, height, fetched_at) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, c.ID, gameID, c.ImageID, c.Width, c.Height, time.Now().Unix())
	return c.ImageID, err
}

// ResolvePending resolves details for active games that have none yet
// (e.g. because IGDB was unreachable when they were activated).
func (s *Syncer) ResolvePending(ctx context.Context) (int, error) {
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT igdb_game_id FROM games WHERE details_fetched_at IS NULL ORDER BY activated_at LIMIT 50`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	n := 0
	for _, id := range ids {
		if err := s.ResolveDetails(ctx, id); err != nil {
			s.Log.Warn("igdb: resolve pending details", "game", id, "err", err)
			continue
		}
		n++
	}
	return n, nil
}
