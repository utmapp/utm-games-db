package internalapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/utmapp/utm-games-db/internal/db"
	"github.com/utmapp/utm-games-db/internal/status"
	"github.com/utmapp/utm-games-db/internal/store"
)

func leaseFixture(t *testing.T) (http.Handler, *store.Store, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ctx := context.Background()
	if _, err := d.Writer.ExecContext(ctx, `INSERT INTO igdb_games (id, name, slug, raw_json, synced_at) VALUES (1, 'Portal 2', 'portal-2', '{}', 0)`); err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	v, err := st.CreateVersion(ctx, "5.0.8", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.GetOrCreateUser(ctx, "identity")
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.ConfigInput{VersionID: v.ID, GuestOS: "windows", GraphicsBackend: "unknown"}
	if _, err := st.CreateReport(ctx, store.ReportInput{GameID: 1, UserID: u.ID, Rating: status.Playable, Config: cfg}, nil,
		store.Quotas{DailyBytes: 1 << 30, DailyFiles: 10, TotalBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return s.Handler(), st, d
}

func call(h http.Handler, method, path, holder, body string) (int, map[string]any) {
	req := httptest.NewRequest(method, "http://127.0.0.1:8081"+path, strings.NewReader(body))
	if holder != "" {
		req.Header.Set(HolderHeader, holder)
	}
	resp := do(h, req)
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// expire moves a lease's expiry into the past, as if its holder stopped renewing.
func expire(t *testing.T, d *db.DB, report int) {
	t.Helper()
	if _, err := d.Writer.Exec(`UPDATE triage_leases SET expires_at = expires_at - 100000 WHERE report_id = ?`, report); err != nil {
		t.Fatal(err)
	}
}

func TestTriageLease(t *testing.T) {
	h, _, d := leaseFixture(t)

	if code, _ := call(h, "POST", "/internal/reports/1/lease", "", `{"holder":"mac-a","ttl_seconds":600}`); code != 200 {
		t.Fatalf("acquire = %d", code)
	}
	// another machine is refused and told who has it
	code, out := call(h, "POST", "/internal/reports/1/lease", "mac-b", `{"ttl_seconds":600}`)
	if code != 409 || out["lease"].(map[string]any)["holder"] != "mac-a" {
		t.Fatalf("second acquire = %d %v, want 409 naming mac-a", code, out)
	}
	// the holder renews
	if code, _ := call(h, "POST", "/internal/reports/1/lease", "mac-a", `{"ttl_seconds":600}`); code != 200 {
		t.Fatalf("renew = %d", code)
	}
	// triage writes from another machine fail; the holder's go through
	if code, _ := call(h, "POST", "/internal/reports/1/comments", "mac-b", `{"body_md":"dup"}`); code != 409 {
		t.Fatalf("comment by mac-b = %d, want 409", code)
	}
	if code, _ := call(h, "POST", "/internal/reports/1/triage", "", `{"status":"in_progress"}`); code != 409 {
		t.Fatalf("status without holder = %d, want 409", code)
	}
	if code, _ := call(h, "POST", "/internal/reports/1/triage", "mac-a", `{"status":"in_progress"}`); code != 200 {
		t.Fatalf("status by mac-a = %d", code)
	}
	// the feed shows the lease
	_, work := call(h, "GET", "/internal/work", "", "")
	if l := work["triage_leases"].(map[string]any)["1"].(map[string]any); l["holder"] != "mac-a" || l["active"] != true {
		t.Fatalf("feed lease = %v", l)
	}
	if n := len(work["stale_triage"].([]any)); n != 0 {
		t.Fatalf("stale_triage = %d, want 0", n)
	}
	// mac-a dies: its lease expires with the report in progress
	expire(t, d, 1)
	_, work = call(h, "GET", "/internal/work", "", "")
	stale := work["stale_triage"].([]any)
	if len(stale) != 1 || stale[0].(map[string]any)["holder"] != "mac-a" {
		t.Fatalf("stale_triage = %v, want report 1 from mac-a", stale)
	}
	// so mac-b takes over, and mac-a's late writes now fail
	if code, _ := call(h, "POST", "/internal/reports/1/lease", "mac-b", `{"ttl_seconds":600}`); code != 200 {
		t.Fatalf("takeover = %d", code)
	}
	if code, _ := call(h, "POST", "/internal/reports/1/comments", "mac-a", `{"body_md":"late"}`); code != 409 {
		t.Fatalf("late comment by mac-a = %d, want 409", code)
	}
	if code, _ := call(h, "DELETE", "/internal/reports/1/lease?holder=mac-a", "", ""); code != 409 {
		t.Fatalf("release by mac-a = %d, want 409", code)
	}
	// a final status ends the holder's lease, and the row keeps the last holder
	if code, _ := call(h, "POST", "/internal/reports/1/triage", "mac-b", `{"status":"needs_clarification"}`); code != 200 {
		t.Fatalf("final status = %d", code)
	}
	code, out = call(h, "GET", "/internal/reports/1/lease", "", "")
	if l := out["lease"].(map[string]any); code != 200 || l["holder"] != "mac-b" || l["active"] != false {
		t.Fatalf("after final status: %d %v", code, out)
	}
	// with no active lease anyone may write (a person working by hand)
	if code, _ := call(h, "POST", "/internal/reports/1/comments", "", `{"body_md":"by hand"}`); code != 201 {
		t.Fatalf("comment without lease = %d", code)
	}
	// but a machine without the lease may not claim to be working: its
	// death would never show in stale_triage
	if code, _ := call(h, "POST", "/internal/reports/1/triage", "mac-b", `{"status":"in_progress"}`); code != 409 {
		t.Fatalf("in progress by a machine without a lease = %d, want 409", code)
	}
	if code, out := call(h, "GET", "/internal/reports/1", "", ""); code != 200 || out["lease"].(map[string]any)["holder"] != "mac-b" {
		t.Fatalf("report after refused in_progress: %d %v", code, out)
	}
	// in progress by hand drops the inactive row: not mistaken for an abandoned machine
	if code, _ := call(h, "POST", "/internal/reports/1/triage", "", `{"status":"in_progress"}`); code != 200 {
		t.Fatalf("in progress by hand = %d", code)
	}
	_, work = call(h, "GET", "/internal/work", "", "")
	if n := len(work["stale_triage"].([]any)); n != 0 {
		t.Fatalf("stale_triage after manual in_progress = %d, want 0", n)
	}
	if code, out := call(h, "GET", "/internal/reports/1", "", ""); code != 200 || out["lease"] != nil {
		t.Fatalf("report after manual in_progress: %d %v", code, out)
	}
	// releasing what was never leased is nothing; a missing report is 404
	if code, _ := call(h, "DELETE", "/internal/reports/1/lease?holder=mac-a", "", ""); code != 200 {
		t.Fatalf("release with no lease = %d", code)
	}
	if code, _ := call(h, "DELETE", "/internal/reports/99/lease?holder=mac-a", "", ""); code != 404 {
		t.Fatalf("release on a missing report = %d, want 404", code)
	}
	if code, _ := call(h, "DELETE", "/internal/reports/1/lease?holder=%20%20", "", ""); code != 400 {
		t.Fatalf("release with a blank holder = %d, want 400", code)
	}
	if code, _ := call(h, "POST", "/internal/reports/99/lease", "mac-a", `{}`); code != 404 {
		t.Fatalf("lease on a missing report = %d, want 404", code)
	}
	// a holder is the same with or without surrounding whitespace
	if code, _ := call(h, "POST", "/internal/reports/1/lease", "", `{"holder":" mac-c ","ttl_seconds":600}`); code != 200 {
		t.Fatalf("acquire with padding = %d", code)
	}
	if code, _ := call(h, "DELETE", "/internal/reports/1/lease?holder=%20mac-c", "", ""); code != 200 {
		t.Fatalf("release with padding = %d", code)
	}
	// an empty page of the feed lists no leases at all
	_, work = call(h, "GET", "/internal/work?since=2200-01-01T00:00:00Z", "", "")
	if l, ok := work["triage_leases"].(map[string]any); !ok || len(l) != 0 {
		t.Fatalf("triage_leases on an empty page = %v, want {}", work["triage_leases"])
	}
	// a page the client asked to be larger than any served is still complete
	_, work = call(h, "GET", "/internal/work?limit=100000", "", "")
	if work["reports_more"] != false || len(work["reports"].([]any)) != 1 {
		t.Fatalf("oversized limit: reports_more=%v reports=%d", work["reports_more"], len(work["reports"].([]any)))
	}
}

// Duplicate moderation and gift work from a second machine fails with 409
// instead of acting twice.
func TestRepeatWritesConflict(t *testing.T) {
	h, st, d := leaseFixture(t)
	ctx := context.Background()
	u, _ := st.GetOrCreateUser(ctx, "identity")
	if err := st.CreateDeletionRequest(ctx, 1, u.ID, "please"); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(h, "POST", "/internal/deletion-requests/1/status", "", `{"status":"rejected"}`); code != 200 {
		t.Fatalf("first resolve = %d", code)
	}
	if code, _ := call(h, "POST", "/internal/deletion-requests/1/status", "", `{"status":"done"}`); code != 409 {
		t.Fatalf("second resolve = %d, want 409", code)
	}
	if code, _ := call(h, "POST", "/internal/deletion-requests/1/status", "", `{"status":"open"}`); code != 200 {
		t.Fatalf("reopen = %d", code)
	}
	req, err := st.CreateTriageRequest(ctx, 1, u.ID, "gift", "steam", "", "")
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/internal/triage/requests/%d/status", req)
	if code, _ := call(h, "POST", path, "", `{"status":"done"}`); code != 200 {
		t.Fatalf("first close = %d", code)
	}
	if code, _ := call(h, "POST", path, "", `{"status":"done"}`); code != 409 {
		t.Fatalf("second close = %d, want 409", code)
	}
	if code, _ := call(h, "POST", "/internal/flags/report/1/ignore", "", ``); code != 409 {
		t.Fatalf("ignore with no flags = %d, want 409", code)
	}
	// spam feedback is given once: the second machine's is refused, and
	// nothing but a pending entry can be claimed
	if err := st.LogSpam(ctx, u.ID, "comment", `{"fields":{"body":"buy now"}}`, "spam"); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(h, "POST", "/internal/spam-log/1/feedback", "", `{"feedback":"spam"}`); code != 200 {
		t.Fatalf("first feedback = %d", code)
	}
	if code, _ := call(h, "POST", "/internal/spam-log/1/feedback", "", `{"feedback":"ham"}`); code != 409 {
		t.Fatalf("second feedback = %d, want 409", code)
	}
	if code, _ := call(h, "POST", "/internal/spam-log/2/feedback", "", `{"feedback":"spam"}`); code != 404 {
		t.Fatalf("feedback on a missing entry = %d, want 404", code)
	}
	_ = d
}
