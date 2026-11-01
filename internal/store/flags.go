package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Flags are reports of problematic content, kept for the maintainers only.

// Flag kinds.
const (
	FlagReport  = "report"
	FlagComment = "comment"
)

// ErrOwnContent is returned when a user flags their own report or comment.
var ErrOwnContent = errors.New("you cannot report your own content")

// Flag records that user reported the content. A second flag of the same
// content by the same user changes nothing. Content that is not visible, or
// the user's own, cannot be flagged; content by a deleted account can.
func (s *Store) Flag(ctx context.Context, kind string, targetID, userID int64) error {
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var author sql.NullInt64
		var err error
		switch kind {
		case FlagReport:
			err = tx.QueryRowContext(ctx, `SELECT user_id FROM reports WHERE id = ? AND visibility = 'visible'`, targetID).Scan(&author)
		case FlagComment:
			err = tx.QueryRowContext(ctx, `SELECT user_id FROM comments WHERE id = ? AND visibility = 'visible'`, targetID).Scan(&author)
		default:
			return ErrNotFound
		}
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if author.Valid && author.Int64 == userID {
			return ErrOwnContent
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO content_flags (kind, target_id, user_id, created_at) VALUES (?, ?, ?, ?)`, kind, targetID, userID, time.Now().Unix())
		return err
	})
}

// FlaggedItem is one piece of content with its flags, for the internal API.
type FlaggedItem struct {
	Kind      string    `json:"kind"`
	TargetID  int64     `json:"target_id"`
	ReportID  int64     `json:"report_id"` // the report itself, or the comment's report
	GameID    int64     `json:"game_id"`
	GameSlug  string    `json:"-"`
	Flags     int       `json:"flags"`
	FirstAt   time.Time `json:"first_at"`
	LastAt    time.Time `json:"last_at"`
	Author    string    `json:"author"`
	AuthorID  int64     `json:"author_id,omitempty"`
	Excerpt   string    `json:"excerpt"`
	Path      string    `json:"path"`
	Reporters []int64   `json:"reporter_ids"`
}

// ListFlags returns flagged content, most recently flagged first.
func (s *Store) ListFlags(ctx context.Context, limit int) ([]FlaggedItem, error) {
	rows, err := s.DB.Reader.QueryContext(ctx, `
		SELECT f.kind, f.target_id, COUNT(*), MIN(f.created_at), MAX(f.created_at), GROUP_CONCAT(f.user_id),
		       CASE f.kind WHEN 'report' THEN r.id ELSE c.report_id END,
		       COALESCE(r.game_id, rc.game_id), COALESCE(gr.slug, grc.slug),
		       COALESCE(CASE f.kind WHEN 'report' THEN ur.display_name ELSE uc.display_name END, 'Auto-Triage'),
		       COALESCE(CASE f.kind WHEN 'report' THEN r.user_id ELSE c.user_id END, 0),
		       COALESCE(CASE f.kind WHEN 'report' THEN substr(r.description_md, 1, 200) ELSE substr(c.body_md, 1, 200) END, '')
		FROM content_flags f
		LEFT JOIN reports r ON f.kind = 'report' AND r.id = f.target_id
		LEFT JOIN users ur ON ur.id = r.user_id
		LEFT JOIN igdb_games gr ON gr.id = r.game_id
		LEFT JOIN comments c ON f.kind = 'comment' AND c.id = f.target_id
		LEFT JOIN users uc ON uc.id = c.user_id
		LEFT JOIN reports rc ON rc.id = c.report_id
		LEFT JOIN igdb_games grc ON grc.id = rc.game_id
		GROUP BY f.kind, f.target_id
		ORDER BY MAX(f.created_at) DESC
		LIMIT ?`, clampLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlaggedItem
	for rows.Next() {
		var it FlaggedItem
		var first, last int64
		var reporters string
		var reportID, gameID sql.NullInt64
		var slug sql.NullString
		if err := rows.Scan(&it.Kind, &it.TargetID, &it.Flags, &first, &last, &reporters, &reportID, &gameID, &slug, &it.Author, &it.AuthorID, &it.Excerpt); err != nil {
			return nil, err
		}
		it.FirstAt, it.LastAt = unix(first), unix(last)
		it.ReportID, it.GameID, it.GameSlug = reportID.Int64, gameID.Int64, slug.String
		it.Path = ReportPath(it.GameID, it.GameSlug, it.ReportID)
		if it.Kind == FlagComment {
			it.Path += "#comment-" + itoa(it.TargetID)
		}
		it.Reporters = splitInt64s(reporters)
		out = append(out, it)
	}
	return out, rows.Err()
}

// IgnoreFlags deletes every flag on the content: nothing tells the author or
// the reporters, and the reporters may flag it again.
func (s *Store) IgnoreFlags(ctx context.Context, kind string, targetID int64) (int64, error) {
	res, err := s.DB.Writer.ExecContext(ctx, `DELETE FROM content_flags WHERE kind = ? AND target_id = ?`, kind, targetID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// deleteReportFlags removes the flags of a report and of its comments; it is
// called wherever a report is deleted or removed.
func deleteReportFlags(ctx context.Context, tx *sql.Tx, reportID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM content_flags WHERE (kind = 'report' AND target_id = ?) OR (kind = 'comment' AND target_id IN (SELECT id FROM comments WHERE report_id = ?))`, reportID, reportID); err != nil {
		return err
	}
	return nil
}

// deleteCommentFlags removes the flags of a comment.
func deleteCommentFlags(ctx context.Context, tx *sql.Tx, commentID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM content_flags WHERE kind = 'comment' AND target_id = ?`, commentID)
	return err
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func splitInt64s(csv string) []int64 {
	var out []int64
	for _, p := range strings.Split(csv, ",") {
		if n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// CommentReport returns the report a comment belongs to.
func (s *Store) CommentReport(ctx context.Context, commentID int64) (int64, error) {
	var id int64
	err := s.DB.Reader.QueryRowContext(ctx, `SELECT report_id FROM comments WHERE id = ?`, commentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}
