package internalapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

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
