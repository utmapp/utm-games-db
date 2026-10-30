package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/utmapp/utm-games-db/internal/db"
)

func TestDisplayNameCaseInsensitive(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s, ctx := New(d), context.Background()
	a, _ := s.GetOrCreateUser(ctx, "h1")
	b, _ := s.GetOrCreateUser(ctx, "h2")
	if err := s.SetDisplayName(ctx, a.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisplayName(ctx, b.ID, "Alice"); !errors.Is(err, ErrDisplayNameTaken) {
		t.Fatalf("got %v, want ErrDisplayNameTaken", err)
	}
}
