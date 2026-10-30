package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/db"
)

// Store wraps the database.
type Store struct {
	DB *db.DB
}

// New creates a Store.
func New(d *db.DB) *Store { return &Store{DB: d} }

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

const userCols = `id, display_name, is_banned, spam_strikes, created_at, COALESCE(last_login_at, 0), COALESCE(last_export_at, 0), COALESCE(deleted_at, 0)`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created, last, exported, deleted int64
	if err := row.Scan(&u.ID, &u.DisplayName, &u.Banned, &u.SpamStrikes, &created, &last, &exported, &deleted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt = unix(created)
	u.LastLoginAt = unix(last)
	if exported > 0 {
		t := unix(exported)
		u.LastExportAt = &t
	}
	if deleted > 0 {
		t := unix(deleted)
		u.DeletedAt = &t
	}
	return &u, nil
}

// GetUser loads a user by id.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.DB.Reader.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// GetOrCreateUser returns the user for an email identity (auth.Keys.HashEmail),
// creating one with a generated display name on first sign-in, and records
// the login time.
func (s *Store) GetOrCreateUser(ctx context.Context, emailHash string) (*User, error) {
	now := time.Now().Unix()
	var u *User
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email_hash = ?`, emailHash))
		if err == nil && u.DeletedAt != nil && !u.Banned {
			// A deleted account signing in again starts fresh (new name, no
			// link to anonymized posts in the UI) on the same identity, so a
			// ban on the old account still holds.
			for attempt := 0; attempt < 5; attempt++ {
				name := "user-" + randomToken(6)
				if _, err := tx.ExecContext(ctx, `UPDATE users SET deleted_at = NULL, display_name = ?, last_login_at = ? WHERE id = ?`, name, now, u.ID); err != nil {
					if db.IsUniqueViolation(err) {
						continue
					}
					return err
				}
				u, err = scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, u.ID))
				return err
			}
			return errors.New("could not allocate display name")
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, now, u.ID)
			return err
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		for attempt := 0; attempt < 5; attempt++ {
			name := "user-" + randomToken(6)
			res, err := tx.ExecContext(ctx, `INSERT INTO users (email_hash, display_name, created_at, last_login_at) VALUES (?, ?, ?, ?)`, emailHash, name, now, now)
			if err != nil {
				if db.IsUniqueViolation(err) {
					continue
				}
				return err
			}
			id, _ := res.LastInsertId()
			u, err = scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
			return err
		}
		return errors.New("could not allocate display name")
	})
	return u, err
}

var displayNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{1,30}[A-Za-z0-9._-]$`)

// ErrDisplayNameTaken is returned when the chosen name already exists.
var ErrDisplayNameTaken = errors.New("that name is already taken")

// ValidateDisplayName checks the format of a display name.
func ValidateDisplayName(name string) error {
	if !displayNameRe.MatchString(name) {
		return errors.New("display name must be 3–32 characters: letters, numbers, spaces, dots, dashes or underscores")
	}
	if strings.Contains(name, "@") {
		return errors.New("display name must not look like an email address")
	}
	if strings.EqualFold(strings.TrimSpace(name), DeletedUserName) || strings.HasPrefix(strings.ToLower(name), deletedNamePrefix) {
		return errors.New("that name is reserved")
	}
	return nil
}

// SetDisplayName changes the public name of a user.
func (s *Store) SetDisplayName(ctx context.Context, userID int64, name string) error {
	name = strings.TrimSpace(name)
	if err := ValidateDisplayName(name); err != nil {
		return err
	}
	_, err := s.DB.Writer.ExecContext(ctx, `UPDATE users SET display_name = ? WHERE id = ?`, name, userID)
	if db.IsUniqueViolation(err) {
		return ErrDisplayNameTaken
	}
	return err
}

// SetBanned bans or unbans a user. Banning also revokes their sessions.
func (s *Store) SetBanned(ctx context.Context, userID int64, banned bool) error {
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET is_banned = ? WHERE id = ?`, banned, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if banned {
			_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
		}
		return err
	})
}

// AddSpamStrike increments the per-user blatant-spam counter.
func (s *Store) AddSpamStrike(ctx context.Context, userID int64) error {
	_, err := s.DB.Writer.ExecContext(ctx, `UPDATE users SET spam_strikes = spam_strikes + 1 WHERE id = ?`, userID)
	return err
}

// ListUsers returns users for the internal API.
func (s *Store) ListUsers(ctx context.Context, limit int) ([]User, error) {
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY id DESC LIMIT ?`, clampLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("crypto/rand: %w", err))
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:n]
}
