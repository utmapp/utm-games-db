package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/utmapp/utm-games-db/internal/upload"
)

// UserExport is everything the site holds about one user, in the shape
// written to data.json inside an export archive.
type UserExport struct {
	ExportedAt time.Time `json:"exported_at"`
	User       struct {
		ID          int64      `json:"id"`
		DisplayName string     `json:"display_name"`
		CreatedAt   time.Time  `json:"created_at"`
		LastLoginAt time.Time  `json:"last_login_at"`
		Host        HostConfig `json:"host"` // the Mac saved on the profile
	} `json:"user"`
	Reports          []Report           `json:"reports"`  // with comments and attachment metadata
	Comments         []Comment          `json:"comments"` // the user's comments on any report
	PlusOnes         []int64            `json:"plus_one_report_ids"`
	Stars            []GameCard         `json:"starred_games"`
	DonationRequests []TriageRequest    `json:"donation_requests"`
	DeletionRequests []DeletionRequest  `json:"deletion_requests"`
	Attachments      []ExportAttachment `json:"attachments"`
}

// ExportAttachment is attachment metadata plus whether the file itself is in
// the archive (files younger than the minimum age are listed but withheld).
type ExportAttachment struct {
	Attachment
	ArchivePath string `json:"archive_path,omitempty"`
	Included    bool   `json:"included_in_archive"`
}

// ExportUserData gathers a user's data. Attachment files created after
// attachmentCutoff are listed but marked as not included.
func (s *Store) ExportUserData(ctx context.Context, userID int64, attachmentCutoff time.Time) (*UserExport, error) {
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	ex := &UserExport{ExportedAt: time.Now()}
	ex.User.ID, ex.User.DisplayName, ex.User.CreatedAt, ex.User.LastLoginAt, ex.User.Host = u.ID, u.DisplayName, u.CreatedAt, u.LastLoginAt, u.Host

	ids, err := queryInt64s(ctx, s.DB.Reader, `SELECT id FROM reports WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		r, err := s.GetReport(ctx, id)
		if err != nil {
			return nil, err
		}
		ex.Reports = append(ex.Reports, *r)
	}
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT id, report_id, body_md, visibility, created_at FROM comments WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c Comment
		var at int64
		if err := rows.Scan(&c.ID, &c.ReportID, &c.Body, &c.Visibility, &at); err != nil {
			rows.Close()
			return nil, err
		}
		c.UserID, c.Author, c.AuthorKind, c.CreatedAt = userID, u.DisplayName, "user", unix(at)
		ex.Comments = append(ex.Comments, c)
	}
	rows.Close()
	if ex.PlusOnes, err = s.UserPlusOnes(ctx, userID); err != nil {
		return nil, err
	}
	if ex.Stars, err = s.UserStarredGames(ctx, userID); err != nil {
		return nil, err
	}
	if ex.DonationRequests, err = s.userTriageRequests(ctx, userID); err != nil {
		return nil, err
	}
	if ex.DeletionRequests, err = s.userDeletionRequests(ctx, userID); err != nil {
		return nil, err
	}
	atts, err := s.UserAttachments(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, a := range atts {
		ea := ExportAttachment{Attachment: a, Included: a.CreatedAt.Before(attachmentCutoff)}
		if ea.Included {
			ea.ArchivePath = archivePath(a)
		}
		ex.Attachments = append(ex.Attachments, ea)
	}
	return ex, nil
}

// archivePath names an attachment inside the export archive. The uploaded
// file name is sanitized so no entry can point outside the archive.
func archivePath(a Attachment) string {
	return "attachments/report-" + strconv.FormatInt(a.ReportID, 10) + "/" + strconv.FormatInt(a.ID, 10) + "-" + upload.SafeName(a.Filename)
}

// UserAttachments lists a user's live attachments, oldest first.
func (s *Store) UserAttachments(ctx context.Context, userID int64) ([]Attachment, error) {
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT id, report_id, user_id, filename, mime, size_bytes, sha256, storage_path, created_at FROM attachments WHERE user_id = ? AND deleted_at IS NULL ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		var a Attachment
		var at int64
		if err := rows.Scan(&a.ID, &a.ReportID, &a.UserID, &a.Filename, &a.MIME, &a.Size, &a.SHA256, &a.Path, &at); err != nil {
			return nil, err
		}
		a.CreatedAt = unix(at)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) userTriageRequests(ctx context.Context, userID int64) ([]TriageRequest, error) {
	return s.triageRequests(ctx, `t.user_id = ?`, `t.id`, -1, userID)
}

func (s *Store) userDeletionRequests(ctx context.Context, userID int64) ([]DeletionRequest, error) {
	return s.deletionRequests(ctx, `d.user_id = ?`, `d.id`, -1, userID)
}

// ErrExportTooSoon is returned when the per-user export interval has not
// elapsed; Next is when the next export is allowed.
type ErrExportTooSoon struct{ Next time.Time }

func (e ErrExportTooSoon) Error() string {
	return "export not available until " + e.Next.UTC().Format(time.RFC1123)
}

// ClaimExport records an export attempt if the interval since the previous
// one has elapsed, so concurrent requests cannot both proceed.
func (s *Store) ClaimExport(ctx context.Context, userID int64, interval time.Duration) error {
	now := time.Now()
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var last sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT last_export_at FROM users WHERE id = ?`, userID).Scan(&last); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if last.Valid && now.Sub(unix(last.Int64)) < interval {
			return ErrExportTooSoon{Next: unix(last.Int64).Add(interval)}
		}
		_, err := tx.ExecContext(ctx, `UPDATE users SET last_export_at = ? WHERE id = ?`, now.Unix(), userID)
		return err
	})
}

// ReleaseExport clears the export timestamp after a failed export so the
// user can try again.
func (s *Store) ReleaseExport(ctx context.Context, userID int64) error {
	_, err := s.DB.Writer.ExecContext(ctx, `UPDATE users SET last_export_at = NULL WHERE id = ?`, userID)
	return err
}

// DeleteAllUserAttachments marks every attachment the user uploaded as
// deleted and returns the storage paths to unlink. Upload quotas are not
// refunded.
func (s *Store) DeleteAllUserAttachments(ctx context.Context, userID int64) (paths []string, err error) {
	now := time.Now().Unix()
	err = s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		paths, err = deleteAttachments(ctx, tx, now, `user_id = ?`, userID)
		return err
	})
	return paths, err
}

// DeletedUserName is how anonymized content is attributed. It is reserved so
// nobody can impersonate it.
const DeletedUserName = "Deleted user"

// deletedNamePrefix starts the placeholder name ("deleted-<id>") a deleted
// account's row keeps. It is reserved so nobody can take a placeholder in
// advance and make that account's deletion fail on the unique name.
const deletedNamePrefix = "deleted-"

// DeleteUser deletes everything the user could have deleted themselves:
// comments, +1s, stars, attachments, requests, and reports that still
// qualify for direct deletion (no comments from others and younger than
// DirectDeleteWindow). Reports beyond that point, which a user can only ask
// a maintainer to remove, are kept so the conversation survives: flagged
// anonymized (rendered as "Deleted user") with attachments removed. The
// user row itself is kept, marked deleted, with its email identity, so the
// surviving posts stay traceable for moderation and a ban still bites if
// the same address signs in again. Sessions, pending sign-in codes and spam-log
// rows go; the upload ledger stays, so deleting the account and signing in
// again does not reset upload quotas. It returns the attachment paths to unlink and the public
// pages (game and report paths) that showed the user's name or content.
func (s *Store) DeleteUser(ctx context.Context, userID int64) (paths []string, pages []string, err error) {
	now := time.Now().Unix()
	err = s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var emailHash string
		if err := tx.QueryRowContext(ctx, `SELECT email_hash FROM users WHERE id = ? AND deleted_at IS NULL`, userID).Scan(&emailHash); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		// Games affected: own reports, reports the user +1'd (weighted votes),
		// and starred games (star counts).
		games, err := queryInt64s(ctx, tx, `SELECT DISTINCT game_id FROM reports WHERE user_id = ?
			UNION SELECT DISTINCT r.game_id FROM plus_ones p JOIN reports r ON r.id = p.report_id WHERE p.user_id = ?
			UNION SELECT game_id FROM stars WHERE user_id = ?`, userID, userID, userID)
		if err != nil {
			return err
		}
		// Pages to purge from the CDN, collected before anything is deleted:
		// the user's reports, reports they commented on or +1'd, and the pages
		// of those games and of starred games (counts change).
		pageRows, err := tx.QueryContext(ctx, `SELECT r.game_id, g.slug, r.id FROM reports r JOIN igdb_games g ON g.id = r.game_id
				WHERE r.user_id = ? OR r.id IN (SELECT report_id FROM comments WHERE user_id = ?) OR r.id IN (SELECT report_id FROM plus_ones WHERE user_id = ?)
			UNION SELECT st.game_id, g.slug, 0 FROM stars st JOIN igdb_games g ON g.id = st.game_id WHERE st.user_id = ?`, userID, userID, userID, userID)
		if err != nil {
			return err
		}
		for pageRows.Next() {
			var gid, rid int64
			var slug string
			if err := pageRows.Scan(&gid, &slug, &rid); err != nil {
				pageRows.Close()
				return err
			}
			if rid != 0 {
				pages = append(pages, PagesForReport(gid, slug, rid)...)
			} else {
				pages = append(pages, PagesForGame(gid, slug)...)
			}
		}
		pageRows.Close()
		paths, err = queryStrings(ctx, tx, `SELECT storage_path FROM attachments WHERE deleted_at IS NULL AND (user_id = ? OR report_id IN (SELECT id FROM reports WHERE user_id = ?))`, userID, userID)
		if err != nil {
			return err
		}
		// Reports the user could still delete directly are deleted; the rest
		// are kept and anonymized.
		cutoff := now - int64(DirectDeleteWindow.Seconds())
		deletable, err := queryInt64s(ctx, tx, `SELECT id FROM reports WHERE user_id = ? AND created_at > ? AND NOT EXISTS (SELECT 1 FROM comments c WHERE c.report_id = reports.id AND c.visibility <> 'removed' AND (c.user_id IS NULL OR c.user_id <> ?))`, userID, cutoff, userID)
		if err != nil {
			return err
		}
		// Reports elsewhere whose counts change when this user's comments and
		// +1s disappear.
		commented, err := queryInt64s(ctx, tx, `SELECT DISTINCT report_id FROM comments WHERE user_id = ? AND report_id NOT IN (SELECT id FROM reports WHERE user_id = ?)`, userID, userID)
		if err != nil {
			return err
		}
		plusOned, err := queryInt64s(ctx, tx, `SELECT report_id FROM plus_ones WHERE user_id = ? AND report_id NOT IN (SELECT id FROM reports WHERE user_id = ?)`, userID, userID)
		if err != nil {
			return err
		}
		stmts := []struct {
			q    string
			args []any
		}{
			{`DELETE FROM attachments WHERE user_id = ? OR report_id IN (SELECT id FROM reports WHERE user_id = ?)`, []any{userID, userID}},
			{`DELETE FROM content_flags WHERE user_id = ? OR (kind = 'comment' AND target_id IN (SELECT id FROM comments WHERE user_id = ?))`, []any{userID, userID}},
			{`DELETE FROM comments WHERE user_id = ?`, []any{userID}},
			{`DELETE FROM plus_ones WHERE user_id = ?`, []any{userID}},
		}
		for _, id := range deletable {
			stmts = append(stmts, struct {
				q    string
				args []any
			}{`DELETE FROM content_flags WHERE (kind = 'report' AND target_id = ?) OR (kind = 'comment' AND target_id IN (SELECT id FROM comments WHERE report_id = ?))`, []any{id, id}},
				struct {
					q    string
					args []any
				}{`DELETE FROM reports WHERE id = ?`, []any{id}}) // comments, +1s, requests cascade
		}
		stmts = append(stmts, []struct {
			q    string
			args []any
		}{
			{`UPDATE reports SET anonymized = 1, updated_at = ? WHERE user_id = ?`, []any{now, userID}},
			{`DELETE FROM triage_requests WHERE user_id = ?`, []any{userID}},
			{`DELETE FROM stars WHERE user_id = ?`, []any{userID}},
			{`DELETE FROM spam_log WHERE user_id = ?`, []any{userID}},
			{`DELETE FROM sessions WHERE user_id = ?`, []any{userID}},
			{`DELETE FROM login_codes WHERE email_hash = ?`, []any{emailHash}},
			{`UPDATE users SET deleted_at = ?, display_name = ? || id WHERE id = ?`, []any{now, deletedNamePrefix, userID}},
			{`DELETE FROM configurations WHERE id NOT IN (SELECT config_id FROM reports) AND id NOT IN (SELECT config_id FROM comments WHERE config_id IS NOT NULL) AND id NOT IN (SELECT config_id FROM plus_ones WHERE config_id IS NOT NULL)`, nil},
		}...)
		for _, st := range stmts {
			if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
				return err
			}
		}
		for _, rid := range commented {
			if err := recountComments(ctx, tx, rid, now); err != nil {
				return err
			}
		}
		for _, rid := range plusOned {
			if err := recountPlusOnes(ctx, tx, rid, now); err != nil {
				return err
			}
		}
		for _, g := range games {
			if err := recomputeGame(ctx, tx, g, now); err != nil {
				return err
			}
		}
		return nil
	})
	return paths, pages, err
}
