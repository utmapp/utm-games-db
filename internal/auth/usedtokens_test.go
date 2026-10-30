package auth

import (
	"strconv"
	"testing"
	"time"
)

// The replay set refuses replays, stays cheap under a flood of fresh tokens
// and stops growing at its cap.
func TestUsedTokensBounded(t *testing.T) {
	u := NewUsedTokens(time.Hour)
	if !u.Consume("a") || u.Consume("a") {
		t.Fatal("first use must pass, replay must fail")
	}
	start := time.Now()
	for i := 0; i < maxUsedTokens; i++ {
		u.Consume("tok-" + strconv.Itoa(i))
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("filling the set took %v; sweeping must stay amortized", d)
	}
	if u.Consume("one-too-many") {
		t.Fatal("a full set must refuse new tokens")
	}
	if len(u.seen) > maxUsedTokens {
		t.Fatalf("set grew past its cap: %d", len(u.seen))
	}
}
