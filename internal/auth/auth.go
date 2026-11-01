// Package auth implements emailed one-time-code sign-in and cookie sessions.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/db"
	"github.com/utmapp/utm-games-db/internal/store"
)

// SessionCookie is the cookie name.
const SessionCookie = "utmg_session"

// Service issues sign-in codes and sessions.
type Service struct {
	DB         *db.DB
	Store      *store.Store
	CodeTTL    time.Duration
	SessionTTL time.Duration
	Secure     bool
	Keys       *Keys
}

// ErrInvalidEmail is returned for unparsable addresses.
var ErrInvalidEmail = errors.New("please enter a valid email address")

// NormalizeEmail validates and lower-cases an address.
func NormalizeEmail(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 254 {
		return "", ErrInvalidEmail
	}
	addr, err := mail.ParseAddress(raw)
	if err != nil || addr.Address != raw || !strings.Contains(raw, "@") {
		return "", ErrInvalidEmail
	}
	return strings.ToLower(addr.Address), nil
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// CodeLength is the number of digits in a sign-in code.
const CodeLength = 6

// MaxCodeAttempts is how many wrong guesses void a code. With six digits
// that bounds a guess at 5 in a million per request.
const MaxCodeAttempts = 5

// LoginCookie carries the secret that ties a sign-in request to the browser
// that made it. A code is useless without it, which is what makes an
// emailed code safe to type anywhere the page is open.
const LoginCookie = "utmg_login"

// NewRequestSecret returns the secret stored in the sign-in cookie.
func NewRequestSecret() string { return newToken() }

func newCode() string {
	// Uniform digits from crypto/rand; a 6-digit string, leading zeros kept.
	b := make([]byte, CodeLength)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	digits := make([]byte, CodeLength)
	for i, v := range b {
		digits[i] = '0' + v%10 // 256 mod 10 bias is 2.4%, irrelevant at this size
	}
	return string(digits)
}

func codeHash(secret, code string) []byte {
	return hash(secret + ":" + code)
}

// CreateCode stores a hashed sign-in code for an email identity, bound to the
// request secret, and returns the code to email. The address itself is never
// written.
func (s *Service) CreateCode(ctx context.Context, secret, emailHash, ip string) (string, error) {
	code := newCode()
	now := time.Now()
	_, err := s.DB.Writer.ExecContext(ctx, `INSERT INTO login_codes (request_hash, code_hash, email_hash, ip, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		hash(secret), codeHash(secret, code), emailHash, ip, now.Unix(), now.Add(s.CodeTTL).Unix())
	if err != nil {
		return "", err
	}
	return code, nil
}

// ErrCodeInvalid is returned for wrong, expired, used or voided codes, and
// when no code was requested from this browser.
var ErrCodeInvalid = errors.New("that code is not valid; check the email or request a new one")

// ErrBanned is returned when a banned user tries to sign in.
var ErrBanned = errors.New("this account has been suspended")

// VerifyCode checks a typed code against the request secret from the cookie.
// Every attempt is counted before the comparison, so a code is voided after
// MaxCodeAttempts wrong guesses. On success the code is consumed and a
// session is created; the user plus the session token are returned.
func (s *Service) VerifyCode(ctx context.Context, secret, code string) (*store.User, string, error) {
	if secret == "" || len(code) != CodeLength {
		return nil, "", ErrCodeInvalid
	}
	var emailHash string
	now := time.Now()
	// The verdict is carried out of the transaction separately: returning
	// ErrCodeInvalid from the callback would roll back the attempt count.
	verdict := ErrCodeInvalid
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var id, expires int64
		var attempts int
		var stored []byte
		var used sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT id, code_hash, email_hash, expires_at, attempts, used_at FROM login_codes WHERE request_hash = ?`, hash(secret)).Scan(&id, &stored, &emailHash, &expires, &attempts, &used)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if used.Valid || now.Unix() > expires || attempts >= MaxCodeAttempts {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE login_codes SET attempts = attempts + 1 WHERE id = ?`, id); err != nil {
			return err
		}
		if subtle.ConstantTimeCompare(stored, codeHash(secret, code)) != 1 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE login_codes SET used_at = ? WHERE id = ?`, now.Unix(), id); err != nil {
			return err
		}
		verdict = nil
		return nil
	})
	if err == nil {
		err = verdict
	}
	if err != nil {
		return nil, "", err
	}
	u, err := s.Store.GetOrCreateUser(ctx, emailHash)
	if err != nil {
		return nil, "", err
	}
	if u.Banned {
		return nil, "", ErrBanned
	}
	sess := newToken()
	_, err = s.DB.Writer.ExecContext(ctx, `INSERT INTO sessions (user_id, token_hash, created_at, expires_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
		u.ID, hash(sess), now.Unix(), now.Add(s.SessionTTL).Unix(), now.Unix())
	if err != nil {
		return nil, "", err
	}
	return u, sess, nil
}

// UserFromToken resolves a session token to its user. Banned users and
// expired sessions yield nil.
func (s *Service) UserFromToken(ctx context.Context, token string) (*store.User, error) {
	if token == "" {
		return nil, nil
	}
	var userID, expires, lastSeen int64
	err := s.DB.Reader.QueryRowContext(ctx, `SELECT user_id, expires_at, last_seen_at FROM sessions WHERE token_hash = ?`, hash(token)).Scan(&userID, &expires, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if now > expires {
		return nil, nil
	}
	u, err := s.Store.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if u.Banned {
		return nil, nil
	}
	if now-lastSeen > 3600 {
		// Best effort; a failed touch must not break the request.
		s.DB.Writer.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, now, hash(token))
	}
	return u, nil
}

// Logout deletes the session.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	_, err := s.DB.Writer.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash(token))
	return err
}

// Cleanup removes expired codes and sessions.
func (s *Service) Cleanup(ctx context.Context) error {
	now := time.Now().Unix()
	if _, err := s.DB.Writer.ExecContext(ctx, `DELETE FROM login_codes WHERE expires_at < ?`, now-86400); err != nil {
		return err
	}
	_, err := s.DB.Writer.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now)
	return err
}

// SetCookie writes the session cookie.
func (s *Service) SetCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.SessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie removes the session cookie.
func (s *Service) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode})
}

// SetLoginCookie binds the browser to a pending sign-in request. The cookie
// is scoped to /login so it accompanies only the request and code forms.
func (s *Service) SetLoginCookie(w http.ResponseWriter, secret string) {
	http.SetCookie(w, &http.Cookie{
		Name:     LoginCookie,
		Value:    secret,
		Path:     "/login",
		MaxAge:   int(s.CodeTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearLoginCookie removes the sign-in request cookie.
func (s *Service) ClearLoginCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: LoginCookie, Value: "", Path: "/login", MaxAge: -1, HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode})
}

// LoginSecretFromRequest reads the sign-in request cookie.
func LoginSecretFromRequest(r *http.Request) string {
	c, err := r.Cookie(LoginCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// TokenFromRequest reads the session cookie.
func TokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

type ctxKey struct{}

// WithUser stores the user in the context.
func WithUser(ctx context.Context, u *store.User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFrom returns the signed-in user or nil.
func UserFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(ctxKey{}).(*store.User)
	return u
}

// CodeBody renders the sign-in email.
func CodeBody(code string, ttl time.Duration) string {
	return fmt.Sprintf(`Your UTM Database sign-in code is %s

Enter it on the sign-in page you requested it from. The code expires in %d minutes and works once. If you did not request it, you can ignore this email.
`, code, int(ttl.Minutes()))
}
