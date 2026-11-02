package internalapi

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/utmapp/utm-games-db/internal/db"
	"github.com/utmapp/utm-games-db/internal/status"
	"github.com/utmapp/utm-games-db/internal/store"
	"github.com/utmapp/utm-games-db/internal/upload"
)

type recordingPurger struct {
	mu   sync.Mutex
	urls []string
}

func (p *recordingPurger) Purge(urls ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.urls = append(p.urls, urls...)
}

func (p *recordingPurger) has(u string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.urls, u)
}

func post(h http.Handler, path, body string) *http.Response {
	return do(h, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8081"+path, strings.NewReader(body)))
}

func do(h http.Handler, req *http.Request) *http.Response {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

// Browsers are refused, including through DNS rebinding (a foreign Host),
// while curl-style requests to localhost go through.
func TestRejectsBrowsers(t *testing.T) {
	h := (&Server{}).Handler()
	req := func(host string, hdr ...string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/internal/healthz", nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		return r
	}
	for _, r := range []*http.Request{
		req("evil.example:8081"),
		req("127.0.0.1:8081", "Origin", "https://evil.example"),
		req("localhost:8081", "Sec-Fetch-Site", "cross-site"),
	} {
		if resp := do(h, r); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %v: got %d, want 403", r.Host, r.Header, resp.StatusCode)
		}
	}
	for _, host := range []string{"127.0.0.1:8081", "localhost:8081", "[::1]:8081"} {
		if resp := do(h, req(host)); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: got %d, want 200", host, resp.StatusCode)
		}
	}
}

// Moderation writes invalidate the cached public pages, and approving a
// deletion request removes the report's attachment files.
func TestModerationPurgesAndRemovesFiles(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ctx := context.Background()
	if _, err := d.Writer.ExecContext(ctx, `INSERT INTO igdb_games (id, name, slug, raw_json, synced_at) VALUES (1, 'Portal 2', 'portal-2', '{}', 0)`); err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	v, err := st.CreateVersion(ctx, "5.0.7", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.GetOrCreateUser(ctx, "identity")
	if err != nil {
		t.Fatal(err)
	}
	ups := upload.Store{Root: filepath.Join(dir, "uploads")}
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 64))
	saved, err := ups.Save("u1", "shot.png", bytes.NewReader(png), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.ConfigInput{VersionID: v.ID, GuestOS: "windows", GraphicsBackend: "unknown"}
	rid, err := st.CreateReport(ctx, store.ReportInput{GameID: 1, UserID: u.ID, Rating: status.Playable, Config: cfg},
		[]store.AttachmentInput{{Filename: "shot.png", MIME: saved.MIME, Size: saved.Size, SHA256: saved.SHA256, Path: saved.RelPath}},
		store.Quotas{DailyBytes: 1 << 30, DailyFiles: 10, TotalBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	purger := &recordingPurger{}
	s := &Server{Store: st, Uploads: ups, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Purger: purger}
	h := s.Handler()

	if resp := post(h, "/internal/reports/1/visibility", `{"visibility":"hidden"}`); resp.StatusCode != 200 {
		t.Fatalf("visibility = %d", resp.StatusCode)
	}
	for _, p := range []string{"/", "/games/1-portal-2", "/games/1-portal-2/reports/1"} {
		if !purger.has(p) {
			t.Errorf("visibility change did not purge %s (got %v)", p, purger.urls)
		}
	}

	if err := st.CreateDeletionRequest(ctx, rid, u.ID, "please"); err != nil {
		t.Fatal(err)
	}
	if resp := post(h, "/internal/deletion-requests/1/status", `{"status":"done"}`); resp.StatusCode != 200 {
		t.Fatalf("resolve = %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(ups.Root, saved.RelPath)); !os.IsNotExist(err) {
		t.Fatalf("attachment file should be removed when the deletion request is done (stat err=%v)", err)
	}
}

// Flags are listed with context and can be ignored.
func TestFlagsListAndIgnore(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ctx := context.Background()
	d.Writer.ExecContext(ctx, `INSERT INTO igdb_games (id, name, slug, raw_json, synced_at) VALUES (1, 'Portal 2', 'portal-2', '{}', 0)`)
	st := store.New(d)
	v, _ := st.CreateVersion(ctx, "5.0.7", false, nil)
	author, _ := st.GetOrCreateUser(ctx, "author")
	reporter, _ := st.GetOrCreateUser(ctx, "reporter")
	rid, err := st.CreateReport(ctx, store.ReportInput{GameID: 1, UserID: author.ID, Rating: status.Playable, Description: "Spam spam spam", Config: store.ConfigInput{VersionID: v.ID, GuestOS: "windows", GraphicsBackend: "unknown"}}, nil, store.Quotas{DailyBytes: 1, DailyFiles: 1, TotalBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Flag(ctx, store.FlagReport, rid, reporter.ID); err != nil {
		t.Fatal(err)
	}
	h := (&Server{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	resp := do(h, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8081/internal/flags", nil))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"flags": 1`) || !strings.Contains(string(body), "Spam spam spam") || !strings.Contains(string(body), "/games/1-portal-2/reports/1") {
		t.Fatalf("list flags: %d %s", resp.StatusCode, body)
	}
	if resp := post(h, "/internal/flags/report/1/ignore", ""); resp.StatusCode != 200 {
		t.Fatalf("ignore = %d", resp.StatusCode)
	}
	resp = do(h, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8081/internal/flags", nil))
	body, _ = io.ReadAll(resp.Body)
	if strings.Contains(string(body), "Spam spam spam") {
		t.Fatalf("flag still listed after ignore: %s", body)
	}
}

func get(h http.Handler, path string) (int, string) {
	resp := do(h, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8081"+path, nil))
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// Owned entries are added and removed one machine at a time without
// touching what other triage machines reported, and availability follows.
func TestOwnedUpsertAndDelete(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO igdb_games (id, name, slug, raw_json, synced_at) VALUES (1, 'Portal 2', 'portal-2', '{}', 0)`,
		`INSERT INTO igdb_websites (id, game_id, url, storefront, storefront_id) VALUES (1, 1, 'https://store.steampowered.com/app/620', 'steam', '620')`,
		`INSERT INTO games (igdb_game_id, activated_at, report_count, updated_at) VALUES (1, 0, 1, 0)`,
	} {
		if _, err := d.Writer.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	st := store.New(d)
	h := (&Server{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	available := func() bool {
		var on bool
		if err := d.Reader.QueryRowContext(ctx, `SELECT triage_available FROM games WHERE igdb_game_id = 1`).Scan(&on); err != nil {
			t.Fatal(err)
		}
		return on
	}

	if resp := post(h, "/internal/triage/owned", `[{"storefront":"gog","storefront_id":"other-machine"}]`); resp.StatusCode != 200 {
		t.Fatalf("first machine's add = %d", resp.StatusCode)
	}
	if resp := post(h, "/internal/triage/owned", `[{"storefront":"Steam","storefront_id":"620"}]`); resp.StatusCode != 200 {
		t.Fatalf("upsert = %d", resp.StatusCode)
	}
	if code, body := get(h, "/internal/triage/owned"); code != 200 || !strings.Contains(body, `"other-machine"`) || !strings.Contains(body, `"620"`) {
		t.Fatalf("upsert must keep the other entries: %d %s", code, body)
	}
	if !available() {
		t.Fatal("game should be triage-available after the upsert")
	}
	del := func(path string) int {
		return do(h, httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:8081"+path, nil)).StatusCode
	}
	if code := del("/internal/triage/owned/steam/620"); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if available() {
		t.Fatal("game should not be triage-available after its entry is deleted")
	}
	if code := del("/internal/triage/owned/steam/620"); code != 404 {
		t.Fatalf("second delete = %d, want 404", code)
	}
	if _, body := get(h, "/internal/triage/owned"); !strings.Contains(body, `"other-machine"`) {
		t.Fatalf("delete removed another entry: %s", body)
	}
}

// The spam log can be limited to entries nobody has reviewed, and a user's
// page counts their content and flags.
func TestSpamPendingAndUserActivity(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ctx := context.Background()
	d.Writer.ExecContext(ctx, `INSERT INTO igdb_games (id, name, slug, raw_json, synced_at) VALUES (1, 'Portal 2', 'portal-2', '{}', 0)`)
	st := store.New(d)
	v, _ := st.CreateVersion(ctx, "5.0.7", false, nil)
	author, _ := st.GetOrCreateUser(ctx, "author")
	reporter, _ := st.GetOrCreateUser(ctx, "reporter")
	rid, err := st.CreateReport(ctx, store.ReportInput{GameID: 1, UserID: author.ID, Rating: status.Playable, Description: "buy cheap", Config: store.ConfigInput{VersionID: v.ID, GuestOS: "windows", GraphicsBackend: "unknown"}}, nil, store.Quotas{DailyBytes: 1, DailyFiles: 1, TotalBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Flag(ctx, store.FlagReport, rid, reporter.ID); err != nil {
		t.Fatal(err)
	}
	st.LogSpam(ctx, author.ID, "report", `{"fields":{"description":"first"}}`, "spam")
	st.LogSpam(ctx, author.ID, "comment", `{"fields":{"body":"second"}}`, "discard")
	st.SetSpamFeedback(ctx, 1, "spam")
	h := (&Server{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()

	if _, body := get(h, "/internal/spam-log?pending=1"); strings.Contains(body, "first") || !strings.Contains(body, "second") {
		t.Fatalf("pending spam log: %s", body)
	}
	if _, body := get(h, "/internal/spam-log"); !strings.Contains(body, "first") || !strings.Contains(body, "second") {
		t.Fatalf("full spam log: %s", body)
	}
	code, body := get(h, "/internal/users/"+strconv.FormatInt(author.ID, 10))
	for _, want := range []string{`"reports": 1`, `"reports_visible": 1`, `"flagged_items": 1`, `"flags_received": 1`, `"spam_log_entries": 2`, `"flags_made": 0`} {
		if code != 200 || !strings.Contains(body, want) {
			t.Fatalf("user activity lacks %s: %d %s", want, code, body)
		}
	}
	if code, _ := get(h, "/internal/users/999"); code != 404 {
		t.Fatalf("missing user = %d, want 404", code)
	}
}

// Created responses are JSON too: a header set after WriteHeader is lost.
func TestCreatedIsJSON(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	h := (&Server{Store: store.New(d), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	resp := post(h, "/internal/utm-versions", `{"version":"5.0.8"}`)
	if resp.StatusCode != http.StatusCreated || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("create version: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// /internal/work hands a machine the changed reports since a cursor and the
// open queues; /internal/backup streams the user data without the catalog.
func TestWorkAndBackup(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO igdb_games (id, name, slug, raw_json, synced_at) VALUES (1, 'Portal 2', 'portal-2', '{}', 0), (2, 'Half-Life 2', 'half-life-2', '{}', 0)`,
		`INSERT INTO igdb_covers (id, game_id, image_id, width, height, local_big, local_small, fetched_at) VALUES (1, 1, 'co1abc', 1, 1, 'co1abc_big.jpg', 'co1abc_small.jpg', 0)`,
	} {
		if _, err := d.Writer.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	st := store.New(d)
	v, _ := st.CreateVersion(ctx, "5.0.7", false, nil)
	author, _ := st.GetOrCreateUser(ctx, "author")
	other, _ := st.GetOrCreateUser(ctx, "other")
	ups := upload.Store{Root: filepath.Join(dir, "uploads")}
	saved, err := ups.Save("u1", "shot.png", bytes.NewReader([]byte("\x89PNG\r\n\x1a\n"+strings.Repeat("x", 64))), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.ConfigInput{VersionID: v.ID, GuestOS: "windows", GraphicsBackend: "unknown"}
	quotas := store.Quotas{DailyBytes: 1 << 30, DailyFiles: 10, TotalBytes: 1 << 30}
	r1, err := st.CreateReport(ctx, store.ReportInput{GameID: 1, UserID: author.ID, Rating: status.Playable, Description: "Runs well.", Config: cfg},
		[]store.AttachmentInput{{Filename: "shot.png", MIME: saved.MIME, Size: saved.Size, SHA256: saved.SHA256, Path: saved.RelPath}}, quotas)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTriageRequest(ctx, 1, other.ID, "demo", "", "https://example.com/demo", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Flag(ctx, store.FlagReport, r1, other.ID); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "env")
	os.WriteFile(envFile, []byte("SECRET_KEY=abc\n"), 0o600)
	s := &Server{Store: st, Uploads: ups, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), EnvFile: envFile}
	h := s.Handler()

	// work: everything on the first call, nothing new on the second.
	code, body := get(h, "/internal/work")
	var work struct {
		Now       time.Time `json:"now"`
		NextSince time.Time `json:"next_since"`
		Reports   []struct {
			ID       int64 `json:"id"`
			Comments []any `json:"comments"`
		} `json:"reports"`
		More      bool  `json:"reports_more"`
		Donations []any `json:"donation_requests"`
		Deletions []any `json:"deletion_requests"`
		Flags     []any `json:"flags"`
		Spam      []any `json:"spam_pending"`
		Counts    struct {
			Untriaged int `json:"untriaged_reports"`
			Flagged   int `json:"flagged_items"`
		} `json:"counts"`
	}
	if err := json.Unmarshal([]byte(body), &work); err != nil || code != 200 {
		t.Fatalf("work: %d %v %s", code, err, body)
	}
	if len(work.Reports) != 1 || work.Reports[0].ID != r1 || len(work.Donations) != 1 || len(work.Flags) != 1 || len(work.Deletions) != 0 || len(work.Spam) != 0 || work.Counts.Untriaged != 1 || work.Counts.Flagged != 1 || work.More {
		t.Fatalf("work content: %+v", work)
	}
	// The cursor is inclusive at second resolution, so the next poll (a
	// second later) with the cursor plus a second sees no reports but still
	// the open queues.
	time.Sleep(1100 * time.Millisecond)
	cursor := work.NextSince.Add(time.Second).Format(time.RFC3339)
	code, body = get(h, "/internal/work?since="+cursor)
	var again struct {
		Reports []any `json:"reports"`
		Flags   []any `json:"flags"`
	}
	json.Unmarshal([]byte(body), &again)
	if code != 200 || len(again.Reports) != 0 || len(again.Flags) != 1 {
		t.Fatalf("second work call: %d reports=%d flags=%d", code, len(again.Reports), len(again.Flags))
	}
	// A new comment makes the report show up again from the cursor.
	if _, err := st.AddComment(ctx, store.CommentInput{ReportID: r1, UserID: other.ID, Body: "Same here."}); err != nil {
		t.Fatal(err)
	}
	code, body = get(h, "/internal/work?since="+cursor)
	json.Unmarshal([]byte(body), &again)
	if code != 200 || len(again.Reports) != 1 {
		t.Fatalf("work after comment: %d reports=%d", code, len(again.Reports))
	}

	// backup: a tar with manifest, env, the database and the upload, and
	// nothing from the catalog.
	resp := do(h, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8081/internal/backup", nil))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-tar" {
		t.Fatalf("backup: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	tr := tar.NewReader(resp.Body)
	got := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[hdr.Name] = b
	}
	for _, name := range []string{"README.txt", "manifest.json", "env", "db/gamesdb.sqlite", "uploads/" + saved.RelPath} {
		if _, found := got[name]; !found {
			t.Errorf("backup lacks %s (has %v)", name, keys(got))
		}
	}
	if string(got["env"]) != "SECRET_KEY=abc\n" {
		t.Errorf("env content: %q", got["env"])
	}
	var man struct {
		Tables   map[string]int64 `json:"tables"`
		Files    []struct{ Path string }
		Excluded []string
	}
	if err := json.Unmarshal(got["manifest.json"], &man); err != nil || man.Tables["reports"] != 1 || man.Tables["users"] != 2 || len(man.Excluded) == 0 {
		t.Errorf("manifest: %v %+v", err, man)
	}
	// The database copy opens, holds the user rows and no catalog rows,
	// and will re-resolve details after a restore.
	copyPath := filepath.Join(dir, "restored.sqlite")
	os.WriteFile(copyPath, got["db/gamesdb.sqlite"], 0o600)
	rd, err := db.Open(copyPath)
	if err != nil {
		t.Fatalf("restored database does not open: %v", err)
	}
	defer rd.Close()
	var reports, users, catalog, covers, pendingDetails int
	rd.Reader.QueryRow(`SELECT COUNT(*) FROM reports`).Scan(&reports)
	rd.Reader.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)
	rd.Reader.QueryRow(`SELECT COUNT(*) FROM igdb_games`).Scan(&catalog)
	rd.Reader.QueryRow(`SELECT COUNT(*) FROM igdb_covers`).Scan(&covers)
	rd.Reader.QueryRow(`SELECT COUNT(*) FROM games WHERE details_fetched_at IS NULL`).Scan(&pendingDetails)
	if reports != 1 || users != 2 || catalog != 0 || covers != 0 || pendingDetails != 1 {
		t.Fatalf("restored copy: reports=%d users=%d catalog=%d covers=%d pending=%d", reports, users, catalog, covers, pendingDetails)
	}
	// A second backup is possible right away (the lock is released).
	if resp := do(h, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8081/internal/backup", nil)); resp.StatusCode != 200 {
		t.Fatalf("second backup: %d", resp.StatusCode)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
