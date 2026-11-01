package auth

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHashEmailAndLimitKey(t *testing.T) {
	k := NewKeys([]byte("secret-one"))
	k2 := NewKeys([]byte("secret-two"))
	a := k.HashEmail("alice@example.com")
	if a != k.HashEmail("alice@example.com") || len(a) != 64 {
		t.Fatalf("hash not deterministic or wrong length: %q", a)
	}
	if a == k2.HashEmail("alice@example.com") {
		t.Fatal("different secrets must give different identities")
	}
	if a == k.HashEmail("bob@example.com") {
		t.Fatal("different addresses must differ")
	}
	if k.LimitKey("alice@example.com") == k.LimitKey("bob@example.com") || len(k.LimitKey("x@y")) != 32 {
		t.Fatal("limit key")
	}
	if strings.Contains(a, "alice") {
		t.Fatal("identity leaks the address")
	}
}

func TestFormToken(t *testing.T) {
	k := NewKeys([]byte("s"))
	now := time.Now()
	tok := k.FormToken(now)
	if _, err := k.VerifyFormToken(tok, now.Add(5*time.Second), 2*time.Second, time.Hour); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if _, err := k.VerifyFormToken(tok, now.Add(time.Second), 2*time.Second, time.Hour); err == nil {
		t.Fatal("too-fresh token accepted")
	}
	if _, err := k.VerifyFormToken(tok, now.Add(2*time.Hour), 0, time.Hour); err == nil {
		t.Fatal("stale token accepted")
	}
	if _, err := NewKeys([]byte("other")).VerifyFormToken(tok, now.Add(5*time.Second), 0, time.Hour); err == nil {
		t.Fatal("forged token accepted")
	}
	if _, err := k.VerifyFormToken(tok[:len(tok)-2]+"zz", now.Add(5*time.Second), 0, time.Hour); err == nil {
		t.Fatal("tampered token accepted")
	}
	if _, err := k.VerifyFormToken("", now, 0, time.Hour); err == nil {
		t.Fatal("empty token accepted")
	}
}

func TestProofOfWork(t *testing.T) {
	if LeadingZeroBits([]byte{0, 0x0f}) != 12 || LeadingZeroBits([]byte{0x80}) != 0 || LeadingZeroBits([]byte{0, 0}) != 16 {
		t.Fatal("LeadingZeroBits")
	}
	tok := "token"
	n := solveProofOfWork(tok, 10)
	if !VerifyProofOfWork(tok, n, 10) {
		t.Fatal("solution rejected")
	}
	if VerifyProofOfWork(tok, n+"1", 10) && VerifyProofOfWork(tok, n+"2", 10) {
		t.Fatal("suspicious: unrelated nonces accepted")
	}
	if VerifyProofOfWork("other-token", n, 10) && VerifyProofOfWork("another", n, 10) {
		t.Fatal("solution should be bound to the token")
	}
	if !VerifyProofOfWork(tok, "", 0) {
		t.Fatal("bits=0 must disable the check")
	}
	if VerifyProofOfWork(tok, "", 1) || VerifyProofOfWork(tok, strings.Repeat("9", 65), 1) {
		t.Fatal("empty or oversized nonce accepted")
	}
}

func TestUsedTokens(t *testing.T) {
	u := NewUsedTokens(time.Hour)
	if !u.Consume("a") || u.Consume("a") || !u.Consume("b") {
		t.Fatal("single use")
	}
}

func TestLoginQueueMXAndRedaction(t *testing.T) {
	q := NewLoginQueue(4, 1)
	q.Log = slog.Default()
	q.MXCheck = true
	q.LookupMX = func(_ context.Context, domain string) (bool, error) {
		switch domain {
		case "good.example":
			return true, nil
		case "flaky.example":
			return false, errors.New("timeout")
		}
		return false, nil
	}
	ctx := context.Background()
	if ok, err := q.deliverable(ctx, "a@good.example"); !ok || err != nil {
		t.Fatal("good domain")
	}
	if ok, _ := q.deliverable(ctx, "a@nomail.example"); ok {
		t.Fatal("domain without mail servers accepted")
	}
	if ok, err := q.deliverable(ctx, "a@flaky.example"); !ok || err == nil {
		t.Fatal("resolver error must fail open and surface the error")
	}
	calls := 0
	q.LookupMX = func(_ context.Context, _ string) (bool, error) { calls++; return true, nil }
	q.deliverable(ctx, "x@cached.example")
	q.deliverable(ctx, "y@cached.example")
	if calls != 1 {
		t.Fatalf("mx result not cached: %d lookups", calls)
	}
	// Queue capacity: a full queue drops without blocking.
	q2 := NewLoginQueue(1, 1)
	q2.Log = slog.Default()
	if !q2.Enqueue(LoginRequest{Email: "a@b"}) || q2.Enqueue(LoginRequest{Email: "c@d"}) {
		t.Fatal("second enqueue should be dropped while no worker runs")
	}
}

// solveProofOfWork finds a nonce for token by brute force.
func solveProofOfWork(token string, bits int) string {
	for i := 0; ; i++ {
		n := strconv.Itoa(i)
		if VerifyProofOfWork(token, n, bits) {
			return n
		}
	}
}
