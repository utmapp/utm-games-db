package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"time"

	"golang.org/x/crypto/argon2"
)

// Keys holds the purpose-specific keys derived from the single SECRET_KEY.
//
// Email addresses are never stored. A user's identity is the Argon2id digest
// of the normalized address with a secret pepper as the salt: a database
// dump alone cannot be matched against rainbow tables or leaked-address
// lists, and even with the pepper every guess costs a full Argon2 run.
type Keys struct {
	pepper   []byte // Argon2 salt for email identities
	limitKey []byte // HMAC key for cheap per-address rate-limit keys
	formKey  []byte // HMAC key for signed sign-in form tokens
}

// Argon2id parameters. 64 MiB and one pass take ~50 ms on a laptop core;
// the login queue bounds how many run at once (LOGIN_WORKERS).
const (
	argonTime      = 1
	argonMemoryKiB = 64 * 1024
	argonThreads   = 1
	argonKeyLen    = 32
)

// NewKeys derives the keys from secret.
func NewKeys(secret []byte) *Keys {
	derive := func(label string) []byte {
		m := hmac.New(sha256.New, secret)
		m.Write([]byte(label))
		return m.Sum(nil)
	}
	return &Keys{pepper: derive("utmdb email identity v1"), limitKey: derive("utmdb login limit v1"), formKey: derive("utmdb login form v1")}
}

// HashEmail returns the stored identity for a normalized address. It is
// deterministic (same address, same digest) and deliberately slow.
func (k *Keys) HashEmail(normalized string) string {
	return hex.EncodeToString(argon2.IDKey([]byte(normalized), k.pepper, argonTime, argonMemoryKiB, argonThreads, argonKeyLen))
}

// LimitKey is a cheap keyed digest used only as an in-memory rate-limit
// bucket name, so per-address limits apply before the slow hash runs.
func (k *Keys) LimitKey(normalized string) string {
	m := hmac.New(sha256.New, k.limitKey)
	m.Write([]byte(normalized))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// HashPrefix is a short, log-safe identifier for an identity.
func HashPrefix(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// --- sign-in form tokens -----------------------------------------------------

// FormToken issues a signed, timestamped, random token for the sign-in form.
// Format: base64url(ts || nonce || hmac).
func (k *Keys) FormToken(now time.Time) string {
	buf := make([]byte, 8+16, 8+16+32)
	binary.BigEndian.PutUint64(buf[:8], uint64(now.Unix()))
	if _, err := rand.Read(buf[8:24]); err != nil {
		panic(err)
	}
	m := hmac.New(sha256.New, k.formKey)
	m.Write(buf[:24])
	return base64.RawURLEncoding.EncodeToString(m.Sum(buf))
}

// ErrFormToken is returned for a missing, forged, too-fresh or stale token.
var ErrFormToken = errors.New("the sign-in form has expired; please reload the page and try again")

// VerifyFormToken checks the signature and that the token's age is within
// [minAge, maxAge]. It returns the issue time on success.
func (k *Keys) VerifyFormToken(tok string, now time.Time, minAge, maxAge time.Duration) (time.Time, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) != 8+16+32 {
		return time.Time{}, ErrFormToken
	}
	m := hmac.New(sha256.New, k.formKey)
	m.Write(raw[:24])
	if subtle.ConstantTimeCompare(m.Sum(nil), raw[24:]) != 1 {
		return time.Time{}, ErrFormToken
	}
	issued := time.Unix(int64(binary.BigEndian.Uint64(raw[:8])), 0)
	age := now.Sub(issued)
	if age < minAge || age > maxAge {
		return time.Time{}, ErrFormToken
	}
	return issued, nil
}

// --- proof of work -------------------------------------------------------------

// ErrProofOfWork is returned when the client did not solve the puzzle.
var ErrProofOfWork = errors.New("the sign-in form could not be verified; please reload the page and try again")

// VerifyProofOfWork checks that SHA-256(token + "." + nonce) starts with at
// least bits zero bits. bits <= 0 disables the check.
func VerifyProofOfWork(token, nonce string, bits int) bool {
	if bits <= 0 {
		return true
	}
	if len(nonce) == 0 || len(nonce) > 64 {
		return false
	}
	sum := sha256.Sum256([]byte(token + "." + nonce))
	return LeadingZeroBits(sum[:]) >= bits
}

// LeadingZeroBits counts leading zero bits of b.
func LeadingZeroBits(b []byte) int {
	n := 0
	for _, x := range b {
		if x == 0 {
			n += 8
			continue
		}
		for m := byte(0x80); m != 0 && x&m == 0; m >>= 1 {
			n++
		}
		break
	}
	return n
}
