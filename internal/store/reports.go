package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/status"
)

// ConfigInput is a validated configuration form.
type ConfigInput struct {
	VersionID       int64
	GuestOS         string
	LinuxDistro     string
	LinuxVersion    string
	OtherOSName     string
	GraphicsBackend string
	MacOSVersion    string
	DeviceModel     string
	Chip            string
	RAMGB           int
}

// ReportInput is a validated report form.
type ReportInput struct {
	GameID             int64
	UserID             int64
	Rating             status.Rating
	PlayedToCompletion bool
	Config             ConfigInput
	IssueType          string
	Description        string
	Repro              string
	WineSameIssue      string
	IP                 string
	UserAgent          string
}

// AttachmentInput is a file already saved to disk.
type AttachmentInput struct {
	Filename string
	MIME     string
	Size     int64
	SHA256   string
	Path     string
}

// Quotas are the private per-user upload limits.
type Quotas struct {
	DailyBytes int64
	DailyFiles int
	TotalBytes int64
}

// ErrQuota is returned when an upload would exceed the user's quota.
var ErrQuota = errors.New("upload limit reached for your account; try again later")

// ErrUnknownVersion is returned when a configuration names a UTM version
// that is not in the curated list.
var ErrUnknownVersion = errors.New("unknown UTM version")

func insertConfig(ctx context.Context, tx *sql.Tx, c ConfigInput, now int64) (int64, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM utm_versions WHERE id = ?`, c.VersionID).Scan(&n); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrUnknownVersion
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO configurations (utm_version_id, guest_os, linux_distro, linux_version, other_os_name, graphics_backend, macos_version, device_model, chip, ram_gb, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.VersionID, c.GuestOS, nullStr(c.LinuxDistro), nullStr(c.LinuxVersion), nullStr(c.OtherOSName), nullStr(c.GraphicsBackend), nullStr(c.MacOSVersion), nullStr(c.DeviceModel), nullStr(c.Chip), nullInt(c.RAMGB), now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// chargeQuota verifies and records an upload against the user's ledger.
func chargeQuota(ctx context.Context, tx *sql.Tx, userID int64, bytes int64, files int, q Quotas) error {
	if files == 0 {
		return nil
	}
	day := time.Now().UTC().Format("2006-01-02")
	var dayBytes int64
	var dayFiles int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(bytes, 0), COALESCE(files, 0) FROM upload_ledger WHERE user_id = ? AND day = ?`, userID, day).Scan(&dayBytes, &dayFiles); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes), 0) FROM upload_ledger WHERE user_id = ?`, userID).Scan(&total); err != nil {
		return err
	}
	if dayBytes+bytes > q.DailyBytes || dayFiles+files > q.DailyFiles || total+bytes > q.TotalBytes {
		return ErrQuota
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO upload_ledger (user_id, day, bytes, files) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, day) DO UPDATE SET bytes = bytes + excluded.bytes, files = files + excluded.files`, userID, day, bytes, files)
	return err
}

// CreateReport stores a report, activating the game and starring it for the
// author in the same transaction.
func (s *Store) CreateReport(ctx context.Context, in ReportInput, files []AttachmentInput, q Quotas) (int64, error) {
	now := time.Now().Unix()
	var id int64
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := activateGame(ctx, tx, in.GameID, now); err != nil {
			return err
		}
		cfgID, err := insertConfig(ctx, tx, in.Config, now)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO reports (game_id, user_id, rating, played_to_completion, config_id, issue_type, description_md, repro_md, wine_same_issue, created_at, updated_at, ip, user_agent)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.GameID, in.UserID, string(in.Rating), in.PlayedToCompletion, cfgID, nullStr(in.IssueType), in.Description, in.Repro, nullStr(in.WineSameIssue), now, now, nullStr(in.IP), nullStr(in.UserAgent))
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		var bytes int64
		for _, f := range files {
			bytes += f.Size
		}
		if err := chargeQuota(ctx, tx, in.UserID, bytes, len(files), q); err != nil {
			return err
		}
		for _, f := range files {
			if _, err := tx.ExecContext(ctx, `INSERT INTO attachments (report_id, user_id, filename, mime, size_bytes, sha256, storage_path, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				id, in.UserID, f.Filename, f.MIME, f.Size, f.SHA256, f.Path, now); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO stars (user_id, game_id, created_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, in.UserID, in.GameID, now); err != nil {
			return err
		}
		return recomputeGame(ctx, tx, in.GameID, now)
	})
	return id, err
}

// UpdateReport replaces the content of an existing report (author edit).
func (s *Store) UpdateReport(ctx context.Context, reportID int64, in ReportInput, files []AttachmentInput, removeAttachmentIDs []int64, q Quotas) ([]string, error) {
	now := time.Now().Unix()
	var removedPaths []string
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var gameID, userID, oldCfg int64
		if err := tx.QueryRowContext(ctx, `SELECT game_id, user_id, config_id FROM reports WHERE id = ?`, reportID).Scan(&gameID, &userID, &oldCfg); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if userID != in.UserID {
			return ErrNotFound
		}
		cfgID, err := insertConfig(ctx, tx, in.Config, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE reports SET rating = ?, played_to_completion = ?, config_id = ?, issue_type = ?, description_md = ?, repro_md = ?, wine_same_issue = ?, updated_at = ?, edited_at = ? WHERE id = ?`,
			string(in.Rating), in.PlayedToCompletion, cfgID, nullStr(in.IssueType), in.Description, in.Repro, nullStr(in.WineSameIssue), now, now, reportID); err != nil {
			return err
		}
		// The replaced configuration is no longer used (configurations are
		// never shared); delete it rather than leave an orphan behind.
		if _, err := tx.ExecContext(ctx, `DELETE FROM configurations WHERE id = ?
				AND NOT EXISTS (SELECT 1 FROM reports WHERE config_id = ?)
				AND NOT EXISTS (SELECT 1 FROM comments WHERE config_id = ?)
				AND NOT EXISTS (SELECT 1 FROM plus_ones WHERE config_id = ?)`, oldCfg, oldCfg, oldCfg, oldCfg); err != nil {
			return err
		}
		for _, aid := range removeAttachmentIDs {
			var path string
			if err := tx.QueryRowContext(ctx, `SELECT storage_path FROM attachments WHERE id = ? AND report_id = ? AND deleted_at IS NULL`, aid, reportID).Scan(&path); err != nil {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE attachments SET deleted_at = ? WHERE id = ?`, now, aid); err != nil {
				return err
			}
			removedPaths = append(removedPaths, path)
		}
		var bytes int64
		for _, f := range files {
			bytes += f.Size
		}
		if err := chargeQuota(ctx, tx, in.UserID, bytes, len(files), q); err != nil {
			return err
		}
		for _, f := range files {
			if _, err := tx.ExecContext(ctx, `INSERT INTO attachments (report_id, user_id, filename, mime, size_bytes, sha256, storage_path, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				reportID, in.UserID, f.Filename, f.MIME, f.Size, f.SHA256, f.Path, now); err != nil {
				return err
			}
		}
		return recomputeGame(ctx, tx, gameID, now)
	})
	return removedPaths, err
}

const reportCols = `r.id, r.game_id, g.name, g.slug, r.user_id, CASE WHEN r.anonymized THEN '` + DeletedUserName + `' ELSE u.display_name END, r.anonymized, r.rating, r.played_to_completion, COALESCE(r.issue_type, ''), r.description_md, r.repro_md, COALESCE(r.wine_same_issue, ''), r.visibility, COALESCE(r.triage_status, ''), r.plus_one_count, r.comment_count, r.created_at, r.updated_at, r.edited_at,
	c.id, c.utm_version_id, v.version, v.v_major, v.v_minor, v.v_patch, v.is_prerelease, c.guest_os, COALESCE(c.linux_distro, ''), COALESCE(c.linux_version, ''), COALESCE(c.other_os_name, ''), COALESCE(c.graphics_backend, ''), COALESCE(c.macos_version, ''), COALESCE(c.device_model, ''), COALESCE(c.chip, ''), COALESCE(c.ram_gb, 0)`

const reportFrom = ` FROM reports r JOIN igdb_games g ON g.id = r.game_id JOIN users u ON u.id = r.user_id JOIN configurations c ON c.id = r.config_id JOIN utm_versions v ON v.id = c.utm_version_id`

func scanReport(row interface{ Scan(...any) error }) (*Report, error) {
	var r Report
	var created, updated int64
	var edited sql.NullInt64
	if err := row.Scan(&r.ID, &r.GameID, &r.GameName, &r.GameSlug, &r.UserID, &r.Author, &r.Anonymized, &r.Rating, &r.PlayedToCompletion, &r.IssueType, &r.Description, &r.Repro, &r.WineSameIssue, &r.Visibility, &r.TriageStatus, &r.PlusOneCount, &r.CommentCount, &created, &updated, &edited,
		&r.Config.ID, &r.Config.VersionID, &r.Config.Version.Version, &r.Config.Version.Major, &r.Config.Version.Minor, &r.Config.Version.Patch, &r.Config.Version.IsPrerelease, &r.Config.GuestOS, &r.Config.LinuxDistro, &r.Config.LinuxVersion, &r.Config.OtherOSName, &r.Config.GraphicsBackend, &r.Config.MacOSVersion, &r.Config.DeviceModel, &r.Config.Chip, &r.Config.RAMGB); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.Config.Version.ID = r.Config.VersionID
	r.CreatedAt = unix(created)
	r.UpdatedAt = unix(updated)
	if edited.Valid {
		r.EditedAt = unixPtr(&edited.Int64)
	}
	return &r, nil
}

// GetReport loads one report with attachments and comments (any visibility;
// callers decide what to show).
func (s *Store) GetReport(ctx context.Context, id int64) (*Report, error) {
	r, err := scanReport(s.DB.Reader.QueryRowContext(ctx, `SELECT `+reportCols+reportFrom+` WHERE r.id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.fillAttachments(ctx, []*Report{r}); err != nil {
		return nil, err
	}
	r.Comments, err = s.ListComments(ctx, id, false)
	return r, err
}

// ReportPaths returns the canonical paths of a report and of its game,
// without loading the report itself.
func (s *Store) ReportPaths(ctx context.Context, id int64) (report, game string, err error) {
	var gameID int64
	var slug string
	err = s.DB.Reader.QueryRowContext(ctx, `SELECT r.game_id, g.slug FROM reports r JOIN igdb_games g ON g.id = r.game_id WHERE r.id = ?`, id).Scan(&gameID, &slug)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	return ReportPath(gameID, slug, id), GamePath(gameID, slug), nil
}

// ListGameReports returns visible reports for a game grouped by version,
// newest version first, for the public game page: attachments, which are
// private, are not loaded.
func (s *Store) ListGameReports(ctx context.Context, gameID int64) ([]VersionGroup, error) {
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT `+reportCols+reportFrom+` WHERE r.game_id = ? AND r.visibility = 'visible' ORDER BY v.v_major DESC, v.v_minor DESC, v.v_patch DESC, r.created_at DESC`, gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []VersionGroup
	var all []*Report
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range all {
		if len(groups) == 0 || groups[len(groups)-1].Version.ID != r.Config.VersionID {
			groups = append(groups, VersionGroup{Version: r.Config.Version, Newest: len(groups) == 0})
		}
		groups[len(groups)-1].Reports = append(groups[len(groups)-1].Reports, *r)
	}
	return groups, nil
}

// SetTriageStatus sets (or clears, with "") the triage status of a report.
func (s *Store) SetTriageStatus(ctx context.Context, reportID int64, st string) error {
	if st != "" && !ValidTriageStatus(st) {
		return fmt.Errorf("invalid triage status %q", st)
	}
	now := time.Now().Unix()
	res, err := s.DB.Writer.ExecContext(ctx, `UPDATE reports SET triage_status = ?, triage_updated_at = ?, updated_at = ? WHERE id = ?`, nullStr(st), now, now, reportID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListReports is the internal listing (any visibility) with filters.
// triageStatus "" means no filter; "none" selects untriaged reports.
func (s *Store) ListReports(ctx context.Context, since time.Time, gameID int64, visibility, triageStatus string, limit int) ([]Report, error) {
	where := []string{"r.updated_at >= ?"}
	args := []any{since.Unix()}
	if gameID != 0 {
		where = append(where, "r.game_id = ?")
		args = append(args, gameID)
	}
	if visibility != "" {
		where = append(where, "r.visibility = ?")
		args = append(args, visibility)
	}
	if triageStatus == "none" {
		where = append(where, "r.triage_status IS NULL")
	} else if triageStatus != "" {
		where = append(where, "r.triage_status = ?")
		args = append(args, triageStatus)
	}
	args = append(args, clampLimit(limit))
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT `+reportCols+reportFrom+` WHERE `+strings.Join(where, " AND ")+` ORDER BY r.updated_at ASC, r.id ASC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []*Report
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.fillAttachments(ctx, all); err != nil {
		return nil, err
	}
	out := make([]Report, len(all))
	for i, r := range all {
		out[i] = *r
	}
	return out, nil
}

// UserReports lists a user's own reports (account page).
func (s *Store) UserReports(ctx context.Context, userID int64) ([]Report, error) {
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT `+reportCols+reportFrom+` WHERE r.user_id = ? AND r.anonymized = 0 AND r.visibility <> 'removed' ORDER BY r.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Report
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Store) fillAttachments(ctx context.Context, reports []*Report) error {
	if len(reports) == 0 {
		return nil
	}
	byID := map[int64]*Report{}
	ph := make([]string, len(reports))
	args := make([]any, len(reports))
	for i, r := range reports {
		byID[r.ID] = r
		ph[i] = "?"
		args[i] = r.ID
	}
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT id, report_id, user_id, filename, mime, size_bytes, sha256, storage_path, created_at FROM attachments WHERE deleted_at IS NULL AND report_id IN (`+strings.Join(ph, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a Attachment
		var created int64
		if err := rows.Scan(&a.ID, &a.ReportID, &a.UserID, &a.Filename, &a.MIME, &a.Size, &a.SHA256, &a.Path, &created); err != nil {
			return err
		}
		a.CreatedAt = unix(created)
		if r := byID[a.ReportID]; r != nil {
			r.Attachments = append(r.Attachments, a)
		}
	}
	return rows.Err()
}

// GetAttachment loads attachment metadata.
func (s *Store) GetAttachment(ctx context.Context, id int64) (*Attachment, error) {
	var a Attachment
	var created int64
	err := s.DB.Reader.QueryRowContext(ctx, `SELECT id, report_id, user_id, filename, mime, size_bytes, sha256, storage_path, created_at FROM attachments WHERE id = ? AND deleted_at IS NULL`, id).
		Scan(&a.ID, &a.ReportID, &a.UserID, &a.Filename, &a.MIME, &a.Size, &a.SHA256, &a.Path, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = unix(created)
	return &a, nil
}

// ExpireAttachments marks attachments older than cutoff (or belonging to
// removed reports) deleted and returns their storage paths for removal.
func (s *Store) ExpireAttachments(ctx context.Context, cutoff time.Time) (paths []string, err error) {
	err = s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		paths, err = deleteAttachments(ctx, tx, time.Now().Unix(), `created_at < ? OR report_id IN (SELECT id FROM reports WHERE visibility = 'removed')`, cutoff.Unix())
		return err
	})
	return paths, err
}

// --- comments and +1 ---------------------------------------------------------

// CommentInput is a validated comment.
type CommentInput struct {
	ReportID  int64
	UserID    int64 // 0 for bot
	Bot       bool
	Body      string
	Config    *ConfigInput
	IP        string
	UserAgent string
}

// AddComment stores a comment and bumps counters.
func (s *Store) AddComment(ctx context.Context, in CommentInput) (int64, error) {
	now := time.Now().Unix()
	var id int64
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var gameID int64
		var vis string
		if err := tx.QueryRowContext(ctx, `SELECT game_id, visibility FROM reports WHERE id = ?`, in.ReportID).Scan(&gameID, &vis); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if vis == "removed" {
			return ErrNotFound
		}
		var cfgID any
		if in.Config != nil {
			cid, err := insertConfig(ctx, tx, *in.Config, now)
			if err != nil {
				return err
			}
			cfgID = cid
		}
		kind := "user"
		var uid any = in.UserID
		if in.Bot {
			kind = "bot"
			uid = nil
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO comments (report_id, user_id, author_kind, body_md, config_id, created_at, ip, user_agent) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			in.ReportID, uid, kind, in.Body, cfgID, now, nullStr(in.IP), nullStr(in.UserAgent))
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		if err := recountComments(ctx, tx, in.ReportID, now); err != nil {
			return err
		}
		return recomputeGame(ctx, tx, gameID, now)
	})
	return id, err
}

// ListComments returns comments of a report, visible only unless includeHidden.
func (s *Store) ListComments(ctx context.Context, reportID int64, includeHidden bool) ([]Comment, error) {
	vis := " AND m.visibility = 'visible'"
	if includeHidden {
		vis = ""
	}
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT m.id, m.report_id, COALESCE(m.user_id, 0), COALESCE(u.display_name, 'Auto-Triage'), m.author_kind, m.body_md, m.visibility, m.created_at,
			c.id, c.utm_version_id, v.version, v.v_major, v.v_minor, v.v_patch, v.is_prerelease, c.guest_os, COALESCE(c.linux_distro, ''), COALESCE(c.linux_version, ''), COALESCE(c.other_os_name, ''), COALESCE(c.graphics_backend, ''), COALESCE(c.macos_version, ''), COALESCE(c.device_model, ''), COALESCE(c.chip, ''), COALESCE(c.ram_gb, 0)
		FROM comments m LEFT JOIN users u ON u.id = m.user_id
		LEFT JOIN configurations c ON c.id = m.config_id LEFT JOIN utm_versions v ON v.id = c.utm_version_id
		WHERE m.report_id = ?`+vis+` ORDER BY m.created_at ASC, m.id ASC`, reportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var m Comment
		var created int64
		var cid sql.NullInt64
		var v nullVersion
		var cfg Configuration
		var guest sql.NullString
		dest := append([]any{&m.ID, &m.ReportID, &m.UserID, &m.Author, &m.AuthorKind, &m.Body, &m.Visibility, &created, &cid}, v.dest()...)
		dest = append(dest, &guest, &cfg.LinuxDistro, &cfg.LinuxVersion, &cfg.OtherOSName, &cfg.GraphicsBackend, &cfg.MacOSVersion, &cfg.DeviceModel, &cfg.Chip, &cfg.RAMGB)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		m.CreatedAt = unix(created)
		if ver := v.get(); cid.Valid && ver != nil {
			cfg.ID = cid.Int64
			cfg.VersionID = ver.ID
			cfg.Version = *ver
			cfg.GuestOS = guest.String
			m.Config = &cfg
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ErrOwnReport is returned when a user tries to +1 their own report.
var ErrOwnReport = errors.New("you cannot +1 your own report")

// TogglePlusOne adds the user's +1 to a visible report, or removes it if
// present, and returns the new state and count.
func (s *Store) TogglePlusOne(ctx context.Context, reportID, userID int64) (on bool, count int, err error) {
	now := time.Now().Unix()
	err = s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var gameID, author int64
		var vis string
		if err := tx.QueryRowContext(ctx, `SELECT game_id, user_id, visibility FROM reports WHERE id = ?`, reportID).Scan(&gameID, &author, &vis); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if vis != "visible" {
			return ErrNotFound
		}
		if author == userID {
			return ErrOwnReport
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM plus_ones WHERE report_id = ? AND user_id = ?`, reportID, userID)
		if err != nil {
			return err
		}
		if removed, _ := res.RowsAffected(); removed == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO plus_ones (report_id, user_id, created_at) VALUES (?, ?, ?)`, reportID, userID, now); err != nil {
				return err
			}
			on = true
		}
		if err := recountPlusOnes(ctx, tx, reportID, now); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT plus_one_count FROM reports WHERE id = ?`, reportID).Scan(&count); err != nil {
			return err
		}
		return recomputeGame(ctx, tx, gameID, now)
	})
	return
}

// recountComments refreshes a report's denormalized visible-comment count.
func recountComments(ctx context.Context, tx *sql.Tx, reportID, now int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE reports SET comment_count = (SELECT COUNT(*) FROM comments WHERE report_id = ? AND visibility = 'visible'), updated_at = ? WHERE id = ?`, reportID, now, reportID)
	return err
}

// recountPlusOnes refreshes a report's denormalized +1 count.
func recountPlusOnes(ctx context.Context, tx *sql.Tx, reportID, now int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE reports SET plus_one_count = (SELECT COUNT(*) FROM plus_ones WHERE report_id = ?), updated_at = ? WHERE id = ?`, reportID, now, reportID)
	return err
}

// deleteAttachments marks the live attachments matching where deleted and
// returns their storage paths so the caller can remove the files.
func deleteAttachments(ctx context.Context, tx *sql.Tx, now int64, where string, args ...any) ([]string, error) {
	paths, err := queryStrings(ctx, tx, `SELECT storage_path FROM attachments WHERE deleted_at IS NULL AND (`+where+`)`, args...)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE attachments SET deleted_at = ? WHERE deleted_at IS NULL AND (`+where+`)`, append([]any{now}, args...)...)
	return paths, err
}

// UserPlusOnes lists the report ids a user has +1'd.
func (s *Store) UserPlusOnes(ctx context.Context, userID int64) ([]int64, error) {
	return queryInt64s(ctx, s.DB.Reader, `SELECT report_id FROM plus_ones WHERE user_id = ?`, userID)
}

// DeleteOwnReport soft-deletes a report by its author and returns the storage
// paths of its attachments so the caller can remove the files.
func (s *Store) DeleteOwnReport(ctx context.Context, reportID, userID int64) (paths []string, err error) {
	now := time.Now().Unix()
	err = s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var gameID, author int64
		if err := tx.QueryRowContext(ctx, `SELECT game_id, user_id FROM reports WHERE id = ? AND visibility <> 'removed'`, reportID).Scan(&gameID, &author); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if author != userID {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE reports SET visibility = 'removed', updated_at = ? WHERE id = ?`, now, reportID); err != nil {
			return err
		}
		removed, err := deleteAttachments(ctx, tx, now, `report_id = ?`, reportID)
		if err != nil {
			return err
		}
		paths = removed
		return recomputeGame(ctx, tx, gameID, now)
	})
	return paths, err
}

// DirectDeleteWindow is how long after creation an author may delete a report
// outright (provided nobody has commented on it).
const DirectDeleteWindow = 24 * time.Hour

// CanDeleteDirectly reports whether the author may delete without review.
func (r *Report) CanDeleteDirectly() bool {
	return r.CommentCount == 0 && time.Since(r.CreatedAt) < DirectDeleteWindow
}

// DeletionRequest is an author's request to remove a report that can no
// longer be deleted directly.
type DeletionRequest struct {
	ID        int64     `json:"id"`
	ReportID  int64     `json:"report_id"`
	GameName  string    `json:"game_name"`
	UserID    int64     `json:"user_id"`
	Reason    string    `json:"reason,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrDeletionRequested is returned when an open request already exists.
var ErrDeletionRequested = errors.New("a deletion request is already pending for this report")

// CreateDeletionRequest files a request for the author's report.
func (s *Store) CreateDeletionRequest(ctx context.Context, reportID, userID int64, reason string) error {
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var author int64
		if err := tx.QueryRowContext(ctx, `SELECT user_id FROM reports WHERE id = ? AND visibility <> 'removed'`, reportID).Scan(&author); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if author != userID {
			return ErrNotFound
		}
		var open int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM deletion_requests WHERE report_id = ? AND status = 'open'`, reportID).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			return ErrDeletionRequested
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO deletion_requests (report_id, user_id, reason, created_at) VALUES (?, ?, ?, ?)`, reportID, userID, nullStr(reason), time.Now().Unix())
		return err
	})
}

// HasOpenDeletionRequest reports whether a request is pending for the report.
func (s *Store) HasOpenDeletionRequest(ctx context.Context, reportID int64) (bool, error) {
	var n int
	err := s.DB.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM deletion_requests WHERE report_id = ? AND status = 'open'`, reportID).Scan(&n)
	return n > 0, err
}

// ListDeletionRequests returns requests, newest first, optionally filtered
// by status.
func (s *Store) ListDeletionRequests(ctx context.Context, st string, limit int) ([]DeletionRequest, error) {
	if st == "" {
		return s.deletionRequests(ctx, `1`, `d.id DESC`, clampLimit(limit))
	}
	return s.deletionRequests(ctx, `d.status = ?`, `d.id DESC`, clampLimit(limit), st)
}

// deletionRequests runs the deletion request query with a filter and order;
// limit < 0 means no limit.
func (s *Store) deletionRequests(ctx context.Context, where, order string, limit int, args ...any) ([]DeletionRequest, error) {
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT d.id, d.report_id, g.name, d.user_id, COALESCE(d.reason, ''), d.status, d.created_at
		FROM deletion_requests d JOIN reports r ON r.id = d.report_id JOIN igdb_games g ON g.id = r.game_id WHERE `+where+` ORDER BY `+order+` LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeletionRequest
	for rows.Next() {
		var d DeletionRequest
		var created int64
		if err := rows.Scan(&d.ID, &d.ReportID, &d.GameName, &d.UserID, &d.Reason, &d.Status, &created); err != nil {
			return nil, err
		}
		d.CreatedAt = unix(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

// clampLimit bounds a listing size from the internal API (default 100).
func clampLimit(limit int) int {
	if limit <= 0 || limit > 500 {
		return 100
	}
	return limit
}

// ResolveDeletionRequest marks a request done (and removes the report) or
// rejected. It returns the report's id and, for "done", the storage paths of
// the report's attachments so the caller can remove the files (the rows are
// marked deleted here, so the retention sweeper will not see them again).
func (s *Store) ResolveDeletionRequest(ctx context.Context, id int64, st string) (reportID int64, paths []string, err error) {
	if st != "done" && st != "rejected" && st != "open" {
		return 0, nil, errors.New("status must be done, rejected or open")
	}
	now := time.Now().Unix()
	err = s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT report_id FROM deletion_requests WHERE id = ?`, id).Scan(&reportID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE deletion_requests SET status = ? WHERE id = ?`, st, id); err != nil {
			return err
		}
		if st != "done" {
			return nil
		}
		var gameID int64
		if err := tx.QueryRowContext(ctx, `SELECT game_id FROM reports WHERE id = ?`, reportID).Scan(&gameID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE reports SET visibility = 'removed', updated_at = ? WHERE id = ?`, now, reportID); err != nil {
			return err
		}
		removed, err := deleteAttachments(ctx, tx, now, `report_id = ?`, reportID)
		if err != nil {
			return err
		}
		paths = removed
		return recomputeGame(ctx, tx, gameID, now)
	})
	return reportID, paths, err
}

// DeleteOwnComment soft-deletes a user's own comment.
func (s *Store) DeleteOwnComment(ctx context.Context, commentID, userID int64) (reportID int64, err error) {
	now := time.Now().Unix()
	err = s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var author sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT report_id, user_id FROM comments WHERE id = ? AND visibility <> 'removed'`, commentID).Scan(&reportID, &author); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if !author.Valid || author.Int64 != userID {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE comments SET visibility = 'removed' WHERE id = ?`, commentID); err != nil {
			return err
		}
		return recountComments(ctx, tx, reportID, now)
	})
	return reportID, err
}

// --- moderation --------------------------------------------------------------

// ValidVisibility reports whether v is a known visibility.
func ValidVisibility(v string) bool { return v == "visible" || v == "hidden" || v == "removed" }

// SetReportVisibility changes a report's visibility and recomputes its game.
func (s *Store) SetReportVisibility(ctx context.Context, reportID int64, vis string) error {
	if !ValidVisibility(vis) {
		return fmt.Errorf("invalid visibility %q", vis)
	}
	now := time.Now().Unix()
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var gameID int64
		if err := tx.QueryRowContext(ctx, `SELECT game_id FROM reports WHERE id = ?`, reportID).Scan(&gameID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE reports SET visibility = ?, updated_at = ? WHERE id = ?`, vis, now, reportID); err != nil {
			return err
		}
		return recomputeGame(ctx, tx, gameID, now)
	})
}

// SetCommentVisibility changes a comment's visibility and returns the id of
// the report it belongs to.
func (s *Store) SetCommentVisibility(ctx context.Context, commentID int64, vis string) (reportID int64, err error) {
	if !ValidVisibility(vis) {
		return 0, fmt.Errorf("invalid visibility %q", vis)
	}
	now := time.Now().Unix()
	err = s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT report_id FROM comments WHERE id = ?`, commentID).Scan(&reportID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE comments SET visibility = ? WHERE id = ?`, vis, commentID); err != nil {
			return err
		}
		return recountComments(ctx, tx, reportID, now)
	})
	return reportID, err
}

// --- spam log ------------------------------------------------------------------

// LogSpam records a rejected submission.
func (s *Store) LogSpam(ctx context.Context, userID int64, kind, payload, result string) error {
	_, err := s.DB.Writer.ExecContext(ctx, `INSERT INTO spam_log (user_id, kind, payload_json, akismet_result, created_at) VALUES (?, ?, ?, ?, ?)`, nullInt64(userID), kind, payload, result, time.Now().Unix())
	return err
}

func nullInt64(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// ListSpam returns recent spam log entries.
func (s *Store) ListSpam(ctx context.Context, limit int) ([]SpamEntry, error) {
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT id, COALESCE(user_id, 0), kind, payload_json, akismet_result, COALESCE(feedback, ''), created_at FROM spam_log ORDER BY id DESC LIMIT ?`, clampLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpamEntry
	for rows.Next() {
		var e SpamEntry
		var created int64
		if err := rows.Scan(&e.ID, &e.UserID, &e.Kind, &e.Payload, &e.AkismetResult, &e.Feedback, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = unix(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetSpam loads one spam log entry.
func (s *Store) GetSpam(ctx context.Context, id int64) (*SpamEntry, error) {
	var e SpamEntry
	var created int64
	err := s.DB.Reader.QueryRowContext(ctx, `SELECT id, COALESCE(user_id, 0), kind, payload_json, akismet_result, COALESCE(feedback, ''), created_at FROM spam_log WHERE id = ?`, id).Scan(&e.ID, &e.UserID, &e.Kind, &e.Payload, &e.AkismetResult, &e.Feedback, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.CreatedAt = unix(created)
	return &e, nil
}

// SetSpamFeedback records that an entry was reported back to Akismet.
func (s *Store) SetSpamFeedback(ctx context.Context, id int64, feedback string) error {
	_, err := s.DB.Writer.ExecContext(ctx, `UPDATE spam_log SET feedback = ? WHERE id = ?`, feedback, id)
	return err
}
