package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/db"
	"github.com/utmapp/utm-games-db/internal/status"
)

const versionCols = `id, version, v_major, v_minor, v_patch, is_prerelease, released_at`

func scanVersion(row interface{ Scan(...any) error }) (*UTMVersion, error) {
	var v UTMVersion
	var released sql.NullInt64
	if err := row.Scan(&v.ID, &v.Version, &v.Major, &v.Minor, &v.Patch, &v.IsPrerelease, &released); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if released.Valid {
		t := unix(released.Int64)
		v.ReleasedAt = &t
	}
	return &v, nil
}

// ListVersions returns all versions, newest first.
func (s *Store) ListVersions(ctx context.Context) ([]UTMVersion, error) {
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT `+versionCols+` FROM utm_versions ORDER BY v_major DESC, v_minor DESC, v_patch DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UTMVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// GetVersion loads one version.
func (s *Store) GetVersion(ctx context.Context, id int64) (*UTMVersion, error) {
	return scanVersion(s.DB.Reader.QueryRowContext(ctx, `SELECT `+versionCols+` FROM utm_versions WHERE id = ?`, id))
}

// CreateVersion adds a release to the curated list.
func (s *Store) CreateVersion(ctx context.Context, version string, prerelease bool, releasedAt *time.Time) (*UTMVersion, error) {
	version = strings.TrimSpace(version)
	major, minor, patch, err := status.ParseVersion(version)
	if err != nil {
		return nil, err
	}
	var rel any
	if releasedAt != nil {
		rel = releasedAt.Unix()
	}
	res, err := s.DB.Writer.ExecContext(ctx, `INSERT INTO utm_versions (version, v_major, v_minor, v_patch, is_prerelease, released_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		version, major, minor, patch, prerelease, rel, time.Now().Unix())
	if err != nil {
		if db.IsUniqueViolation(err) {
			return nil, ErrVersionExists
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetVersion(ctx, id)
}

// ErrVersionExists is returned when adding a version that is already listed.
var ErrVersionExists = errors.New("that version already exists")

// ErrVersionInUse is returned when deleting a referenced version.
var ErrVersionInUse = errors.New("version is referenced by existing reports")

// DeleteVersion removes an unused version.
func (s *Store) DeleteVersion(ctx context.Context, id int64) error {
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM configurations WHERE utm_version_id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrVersionInUse
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM utm_versions WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}
