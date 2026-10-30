package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/utmapp/utm-games-db/internal/antispam"
	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/cdn"
	"github.com/utmapp/utm-games-db/internal/config"
	"github.com/utmapp/utm-games-db/internal/db"
	"github.com/utmapp/utm-games-db/internal/igdb"
	"github.com/utmapp/utm-games-db/internal/store"
	"github.com/utmapp/utm-games-db/internal/upload"
)

type capturingMailer struct {
	codes  []string
	lastTo string
}

var codeRe = regexp.MustCompile(`(?m)^@[^ ]+ #(\d{6})$`)

func (m *capturingMailer) Send(_ context.Context, to, subject, body string) error {
	mt := codeRe.FindStringSubmatch(body)
	if mt == nil {
		panic("sign-in mail without a domain-bound code line: " + body)
	}
	m.codes = append(m.codes, mt[1])
	m.lastTo = "[withheld]" // the real log mailer never prints the recipient
	return nil
}

type env struct {
	t      *testing.T
	srv    *httptest.Server
	s      *Server
	st     *store.Store
	d      *db.DB
	mailer *capturingMailer
	verID  int64
	verID2 int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	// Fake IGDB: empty details, token endpoint.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"tok","expires_in":5000000}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/websites") && strings.Contains(string(body), "game = 2;") {
			// Half-Life 2 has a Steam page so owned-games matching can find it.
			fmt.Fprint(w, `[{"id":100,"game":2,"type":13,"url":"https://store.steampowered.com/app/220/HalfLife_2/","trusted":true}]`)
			return
		}
		fmt.Fprint(w, "[]")
	}))
	t.Cleanup(fake.Close)

	os.Setenv("BASE_URL", "http://games.test")
	os.Setenv("DATA_DIR", dir)
	os.Setenv("MAIL_MODE", "log")
	os.Setenv("TRIAGE_GIFT_ACCOUNTS", `[{"storefront":"steam","label":"Steam: utm-triage","email":"steam-gifts@example.com","url":"https://steamcommunity.com/id/utm-triage"},{"storefront":"gog","label":"GOG: utm-triage","url":"https://www.gog.com/u/utm-triage"}]`)
	os.Setenv("IGDB_CLIENT_ID", "cid")
	os.Setenv("IGDB_CLIENT_SECRET", "sec")
	os.Setenv("UPLOAD_QUOTA_DAILY_FILES", "3")
	os.Setenv("TRUSTED_PROXY_CIDRS", "127.0.0.0/8,::1/128")
	os.Setenv("SECRET_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	os.Setenv("LOGIN_FORM_MIN_AGE", "0s")
	os.Setenv("LOGIN_POW_BITS", "8")
	os.Setenv("LOGIN_MX_CHECK", "false")
	os.Setenv("EXPORT_MIN_ATTACHMENT_AGE", "0s")
	os.Setenv("RATE_CONTENT_INTERVAL", "0s")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	au := &auth.Service{DB: d, Store: st, CodeTTL: cfg.LoginCodeTTL, SessionTTL: cfg.SessionTTL, Secure: false, Keys: auth.NewKeys(cfg.SecretKey)}
	client := igdb.NewClient("cid", "sec", igdb.DBTokenStore{DB: d})
	client.APIBase = fake.URL + "/v4"
	client.TokenURL = fake.URL + "/oauth2/token"
	sy := &igdb.Syncer{DB: d, Client: client, Log: slog.Default(), Platforms: []int64{6}, CoverDir: filepath.Join(dir, "covers")}
	mailer := &capturingMailer{}
	lim := antispam.NewLimiter()
	t.Cleanup(lim.Close)
	s, err := New(cfg, st, au, mailer, &antispam.Akismet{}, lim, sy, upload.Store{Root: filepath.Join(dir, "uploads")}, cdn.NoopPurger{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(s.Close)

	ctx := context.Background()
	for _, g := range []struct {
		id   int64
		name string
		slug string
	}{{1, "Portal 2", "portal-2"}, {2, "Half-Life 2", "half-life-2"}, {3, "Stardew Valley", "stardew-valley"}} {
		if _, err := d.Writer.ExecContext(ctx, `INSERT INTO igdb_games (id, name, slug, summary, url, first_release_date, raw_json, synced_at) VALUES (?, ?, ?, 'A game.', 'https://igdb.com/x', 1300000000, '{}', 0)`, g.id, g.name, g.slug); err != nil {
			t.Fatal(err)
		}
	}
	v1, err := st.CreateVersion(ctx, "5.0.7", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := st.CreateVersion(ctx, "5.0.8", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, srv: srv, s: s, st: st, d: d, mailer: mailer, verID: v1.ID, verID2: v2.ID}
}

// client is a browser-like client with its own cookie jar.
type client struct {
	e     *env
	hc    *http.Client
	ip    string // distinct per client so per-IP limits do not collide
	email string
}

var clientSeq int

func (e *env) newClient() *client {
	jar, _ := cookiejar.New(nil)
	clientSeq++
	return &client{e: e, ip: fmt.Sprintf("198.51.100.%d", clientSeq), hc: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *client) do(req *http.Request) *http.Response {
	req.Header.Set("Origin", "http://games.test")
	req.Header.Set("CF-Connecting-IP", c.ip)
	resp, err := c.hc.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	return resp
}

func (c *client) get(path string) (*http.Response, string) {
	req, _ := http.NewRequest(http.MethodGet, c.e.srv.URL+path, nil)
	resp := c.do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func (c *client) postForm(path string, v url.Values, json bool) (*http.Response, string) {
	req, _ := http.NewRequest(http.MethodPost, c.e.srv.URL+path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if json {
		req.Header.Set("X-Requested-With", "fetch")
		req.Header.Set("Accept", "application/json")
	}
	resp := c.do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

var formTokenRe = regexp.MustCompile(`name="form_token" value="([^"]+)"`)

// loginForm fetches the sign-in page and solves its puzzle like login.js does.
func (c *client) loginForm() url.Values {
	_, body := c.get("/login")
	m := formTokenRe.FindStringSubmatch(body)
	if m == nil {
		c.e.t.Fatal("no form token on /login")
	}
	return url.Values{"form_token": {m[1]}, "pow_nonce": {solvePoW(m[1], c.e.s.Cfg.LoginPoWBits)}}
}

func (c *client) login(email string) {
	c.email = email
	v := c.loginForm()
	v.Set("email", email)
	resp, _ := c.postForm("/login", v, false)
	if resp.StatusCode != 200 {
		c.e.t.Fatalf("login status %d", resp.StatusCode)
	}
	c.e.s.Login.Flush()
	// The emailed code is typed into the same browser that asked for it.
	resp, body := c.postForm("/login/code", url.Values{"code": {c.e.mailer.lastCode()}, "next": {v.Get("next")}}, false)
	if resp.StatusCode != 303 {
		c.e.t.Fatalf("code status %d %s", resp.StatusCode, body)
	}
	if len(c.hc.Jar.Cookies(mustURL(c.e.srv.URL))) == 0 {
		c.e.t.Fatal("no session cookie set")
	}
}

func (m *capturingMailer) lastCode() string { return m.codes[len(m.codes)-1] }

func mustURL(s string) *url.URL {
	u, _ := url.Parse(s)
	return u
}

type reportOpts struct {
	gameID  int64
	verID   int64
	rating  string
	desc    string
	files   map[string][]byte
	issue   string
	guestOS string
	path    string // defaults to /reports/new
}

func (c *client) submitReport(o reportOpts) (*http.Response, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fields := map[string]string{
		"game_id": fmt.Sprint(o.gameID), "rating": o.rating, "utm_version_id": fmt.Sprint(o.verID),
		"guest_os": "windows", "graphics_backend": "dxmt", "macos_version": "15.1", "device_model": "MacBook Pro", "chip": "M3", "ram_gb": "32",
		"issue_type": "hang_crash", "description": o.desc, "repro": "1. Launch\n2. Wait", "wine_same_issue": "unknown",
	}
	if o.issue != "" {
		fields["issue_type"] = o.issue
	}
	if o.guestOS != "" {
		fields["guest_os"] = o.guestOS
		fields["linux_distro"] = "Ubuntu"
	}
	if o.rating == "gold" {
		fields["completion_confirmed"] = "1"
	}
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for name, content := range o.files {
		fw, _ := mw.CreateFormFile("attachments", name)
		fw.Write(content)
	}
	mw.Close()
	path := o.path
	if path == "" {
		path = "/reports/new"
	}
	req, _ := http.NewRequest(http.MethodPost, c.e.srv.URL+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp := c.do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

var png = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 200))

func TestEndToEnd(t *testing.T) {
	e := newEnv(t)
	anon := e.newClient()

	// Public list renders, is cacheable and sets no cookie.
	resp, body := anon.get("/")
	if resp.StatusCode != 200 || !strings.Contains(body, "Game compatibility") {
		t.Fatalf("index: %d %s", resp.StatusCode, body[:200])
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "public") || !strings.Contains(cc, "s-maxage") {
		t.Errorf("index Cache-Control = %q", cc)
	}
	if resp.Header.Get("Set-Cookie") != "" {
		t.Error("index must not set cookies")
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("missing ETag")
	}
	// Inactive catalog games are not public.
	if resp, _ := anon.get("/games/1-portal-2"); resp.StatusCode != 404 {
		t.Errorf("inactive game status = %d", resp.StatusCode)
	}
	// Internal routes are absent from the public mux.
	if resp, _ := anon.get("/internal/healthz"); resp.StatusCode != 404 {
		t.Errorf("internal route on public mux = %d", resp.StatusCode)
	}
	// Anonymous report form redirects to login.
	if resp, _ := anon.get("/reports/new"); resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Errorf("anon /reports/new = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Cross-site POST is rejected.
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/login", strings.NewReader("email=a@b.c"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.test")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 403 {
		t.Errorf("cross-site POST not rejected: %v", r)
	}

	alice := e.newClient()
	alice.login("Alice@Example.com")
	resp, body = alice.get("/reports/new?game=1")
	if resp.StatusCode != 200 || !strings.Contains(body, "Portal 2") || !strings.Contains(body, "5.0.8") {
		t.Fatalf("report form: %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Error("report form must be private")
	}

	// Links are rejected with an editable error.
	resp, body = alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "in_game_issues", desc: "Crashes, see https://evil.example/spam for details"})
	if resp.StatusCode != 422 || !strings.Contains(body, "Links and HTML are not allowed") || !strings.Contains(body, "evil.example") {
		t.Fatalf("link block: %d", resp.StatusCode)
	}
	var n int
	e.d.Reader.QueryRow(`SELECT COUNT(*) FROM reports`).Scan(&n)
	if n != 0 {
		t.Fatal("rejected report was saved")
	}
	// Unknown version id is rejected.
	resp, _ = alice.submitReport(reportOpts{gameID: 1, verID: 999, rating: "in_game_issues", desc: "The game crashes right after the intro movie."})
	if resp.StatusCode != 400 {
		t.Fatalf("unknown version: %d", resp.StatusCode)
	}
	// Bad attachment type is rejected.
	resp, body = alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "in_game_issues", desc: "The game crashes right after the intro movie.", files: map[string][]byte{"evil.exe": []byte("MZ\x90\x00....")}})
	if resp.StatusCode != 400 || !strings.Contains(body, "not an accepted file type") {
		t.Fatalf("bad attachment: %d", resp.StatusCode)
	}

	// A valid report activates the game, auto-stars, stores the attachment.
	resp, body = alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "in_game_issues", desc: "The game crashes right after the intro movie.", files: map[string][]byte{"shot.png": png, "log.txt": []byte("boot ok\n")}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/games/1-portal-2/reports/1" {
		t.Fatalf("create report: %d %s %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	g, err := e.st.GetGame(context.Background(), 1)
	if err != nil || !g.Active || g.ReportCount != 1 || g.StarCount != 1 || g.Status != "in_game_issues" {
		t.Fatalf("game after report: %+v err=%v", g, err)
	}

	// Game page is public and cacheable even with a session cookie present.
	resp, body = alice.get("/games/1-portal-2")
	if resp.StatusCode != 200 || !strings.Contains(body, "intro movie") || !strings.Contains(body, "In-game issues") {
		t.Fatalf("game page: %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Cache-Control"), "public") || resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("game page caching headers: %v", resp.Header)
	}
	if strings.Contains(body, "Alice@") || strings.Contains(body, "alice@example.com") {
		t.Error("email leaked into public page")
	}
	// Bare id and wrong slug redirect to the canonical /games/{id}-{slug}.
	for _, p := range []string{"/games/1", "/games/1-wrong"} {
		if resp, _ := alice.get(p); resp.StatusCode != 301 || resp.Header.Get("Location") != "/games/1-portal-2" {
			t.Errorf("game redirect %s: %d %s", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// Reports live under their game; short and un-slugged forms redirect.
	for _, p := range []string{"/reports/1", "/games/1/reports/1", "/games/1-old/reports/1"} {
		if resp, _ := anon.get(p); resp.StatusCode != 301 || resp.Header.Get("Location") != "/games/1-portal-2/reports/1" {
			t.Errorf("report redirect %s: %d %s", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// Markdown rendered, html escaped.
	resp, body = anon.get("/games/1-portal-2/reports/1")
	if resp.StatusCode != 200 || !strings.Contains(body, "<li>Launch</li>") || strings.Contains(body, "1. Launch") {
		t.Fatalf("report page: %d markdown not rendered", resp.StatusCode)
	}
	// /api/me reflects stars and is private.
	resp, body = alice.get("/api/me")
	var me struct {
		User *struct {
			DisplayName string `json:"display_name"`
		} `json:"user"`
		Stars []int64 `json:"stars"`
	}
	json.Unmarshal([]byte(body), &me)
	if me.User == nil || len(me.Stars) != 1 || me.Stars[0] != 1 || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("api/me: %s %v", body, resp.Header)
	}

	// Attachments: the owner sees them listed on the edit page but cannot
	// download them by URL (the throttled export is the only way out); public
	// pages do not reveal that a report has any.
	bob := e.newClient()
	bob.login("bob@example.com")
	for _, p := range []string{"/games/1-portal-2", "/games/1-portal-2/reports/1"} {
		_, b := bob.get(p)
		if strings.Contains(b, "shot.png") || strings.Contains(b, "log.txt") || strings.Contains(b, "/attachments/") || strings.Contains(b, "paperclip") {
			t.Errorf("attachment presence leaked on %s", p)
		}
	}
	if _, b := alice.get("/reports/1/edit"); !strings.Contains(b, "shot.png") || strings.Contains(b, "/attachments/") {
		t.Errorf("owner edit page should list attachments without download links")
	}
	for _, c := range []*client{alice, bob} {
		if resp, _ := c.get("/attachments/1"); resp.StatusCode != 404 {
			t.Errorf("attachment download by URL = %d; want 404", resp.StatusCode)
		}
	}

	// Star toggle: bob can star the now-active game; nobody can star game 3.
	resp, body = bob.postForm("/games/1/star", nil, true)
	if resp.StatusCode != 200 || !strings.Contains(body, `"on":true`) || !strings.Contains(body, `"count":2`) {
		t.Fatalf("star: %d %s", resp.StatusCode, body)
	}
	resp, body = bob.postForm("/games/3/star", nil, true)
	if resp.StatusCode != 409 {
		t.Fatalf("star inactive: %d %s", resp.StatusCode, body)
	}

	// +1: a single toggle, never on your own report.
	resp, body = alice.postForm("/reports/1/plus-one", nil, true)
	if resp.StatusCode != 403 || !strings.Contains(body, "own report") {
		t.Fatalf("self plus one: %d %s", resp.StatusCode, body)
	}
	resp, body = bob.postForm("/reports/1/plus-one", nil, true)
	if resp.StatusCode != 200 || !strings.Contains(body, `"count":1`) || !strings.Contains(body, `"on":true`) {
		t.Fatalf("plus one: %d %s", resp.StatusCode, body)
	}
	resp, body = bob.postForm("/reports/1/plus-one", nil, true)
	if resp.StatusCode != 200 || !strings.Contains(body, `"count":0`) || !strings.Contains(body, `"on":false`) {
		t.Fatalf("plus one toggle off: %d %s", resp.StatusCode, body)
	}
	bob.postForm("/reports/1/plus-one", nil, true)
	resp, body = bob.postForm("/reports/1/comments", url.Values{"body": {"Same here with **DXMT**"}, "include_config": {"1"}, "utm_version_id": {fmt.Sprint(e.verID)}, "guest_os": {"windows"}, "graphics_backend": {"dxmt"}}, false)
	if resp.StatusCode != 303 {
		t.Fatalf("comment: %d %s", resp.StatusCode, body)
	}
	_, body = anon.get("/games/1-portal-2/reports/1")
	if !strings.Contains(body, "<strong>DXMT</strong>") || !strings.Contains(body, "1 comment") {
		t.Fatalf("comment not rendered")
	}
	resp, body = bob.postForm("/reports/1/comments", url.Values{"body": {"<script>alert(1)</script> visit www.spam.example"}}, false)
	if resp.StatusCode != 422 || !strings.Contains(body, "Links and HTML are not allowed") {
		t.Fatalf("comment link block: %d", resp.StatusCode)
	}

	// Triage status set by the bot/maintainer shows as a badge.
	if err := e.st.SetTriageStatus(context.Background(), 1, "needs_clarification"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetTriageStatus(context.Background(), 1, "bogus"); err == nil {
		t.Fatal("invalid triage status accepted")
	}
	_, body = anon.get("/games/1-portal-2/reports/1")
	if !strings.Contains(body, "Needs clarification") {
		t.Fatal("triage badge missing")
	}
	if reps, _ := e.st.ListReports(context.Background(), time.Time{}, 0, "", "needs_clarification", 10); len(reps) != 1 {
		t.Fatalf("triage filter: %d", len(reps))
	}

	// Status rule: a single report on a newer version overrides.
	resp, _ = bob.submitReport(reportOpts{gameID: 1, verID: e.verID2, rating: "playable", desc: "Runs fine on the newer build, minor stutter only."})
	if resp.StatusCode != 303 {
		t.Fatalf("second report: %d", resp.StatusCode)
	}

	// Editing keeps the rating, UTM version and guest OS even if the form
	// tries to change them; other fields update.
	if _, b := alice.get("/reports/1/edit"); !strings.Contains(b, `type="hidden" name="rating" value="in_game_issues"`) || strings.Contains(b, `type="radio" name="rating"`) || strings.Contains(b, `<select class="input" id="utm_version_id"`) {
		t.Fatal("edit form should lock rating and version")
	}
	resp, _ = alice.submitReport(reportOpts{path: "/reports/1/edit", gameID: 1, verID: e.verID2, rating: "gold", guestOS: "linux", desc: "Edited: crashes right after the intro movie."})
	if resp.StatusCode != 303 {
		t.Fatalf("edit: %d", resp.StatusCode)
	}
	if rep, _ := e.st.GetReport(context.Background(), 1); rep.Rating != "in_game_issues" || rep.Config.VersionID != e.verID || rep.Config.GuestOS != "windows" || !strings.HasPrefix(rep.Description, "Edited:") {
		t.Fatalf("locked fields changed on edit: %+v", rep)
	}

	// Bot comments may carry links; user comments never render them.
	if _, err := e.st.AddComment(context.Background(), store.CommentInput{ReportID: 2, Bot: true, Body: "Fix in [PR 42](https://github.com/utmapp/UTM/pull/42)."}); err != nil {
		t.Fatal(err)
	}
	var bobID int64
	if err := e.st.DB.Reader.QueryRow(`SELECT id FROM users WHERE email_hash = ?`, e.s.Auth.Keys.HashEmail("bob@example.com")).Scan(&bobID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.AddComment(context.Background(), store.CommentInput{ReportID: 2, UserID: bobID, Body: "Also see [this](https://evil.example)."}); err != nil {
		t.Fatal(err)
	}
	_, body = anon.get("/games/1-portal-2/reports/2")
	if !strings.Contains(body, `href="https://github.com/utmapp/UTM/pull/42"`) || strings.Contains(body, `href="https://evil.example"`) {
		t.Fatal("bot link should render as an anchor, user link should not")
	}

	g, _ = e.st.GetGame(context.Background(), 1)
	if g.Status != "playable" || g.StatusVersion == nil || g.StatusVersion.ID != e.verID2 || g.ReportCount != 2 {
		t.Fatalf("status after newer report: %+v", g)
	}
	// +1s count towards the majority: alice's +1 on bob's report keeps it
	// playable; two more reports saying otherwise are outweighed once the
	// playable report has enough +1s.
	frank := e.newClient()
	frank.login("frank@example.com")
	grace := e.newClient()
	grace.login("grace@example.com")
	frank.submitReport(reportOpts{gameID: 1, verID: e.verID2, rating: "does_not_launch", desc: "Black screen on launch."})
	grace.submitReport(reportOpts{gameID: 1, verID: e.verID2, rating: "does_not_launch", desc: "Black screen for me too."})
	if g, _ = e.st.GetGame(context.Background(), 1); g.Status != "does_not_launch" {
		t.Fatalf("two reports should outvote one: %+v", g)
	}
	alice.postForm("/reports/2/plus-one", nil, true)
	erin := e.newClient()
	erin.login("erin@example.com")
	erin.postForm("/reports/2/plus-one", nil, true)
	if g, _ = e.st.GetGame(context.Background(), 1); g.Status != "playable" {
		t.Fatalf("report with two +1s (weight 3) should outvote two reports: %+v", g)
	}
	erin.postForm("/reports/2/plus-one", nil, true) // toggle off -> weight 2, tie -> most recent report (grace's)
	if g, _ = e.st.GetGame(context.Background(), 1); g.Status != "does_not_launch" {
		t.Fatalf("tie should go to the most recent report: %+v", g)
	}
	erin.postForm("/reports/2/plus-one", nil, true) // back on so later assertions see a stable state
	for _, c := range []*client{frank, grace} {
		var rid int64
		e.d.Reader.QueryRow(`SELECT r.id FROM reports r JOIN users u ON u.id = r.user_id WHERE r.game_id = 1 AND u.email_hash = ?`, e.s.Auth.Keys.HashEmail(c.email)).Scan(&rid)
		c.postForm(fmt.Sprintf("/reports/%d/delete", rid), url.Values{"confirm": {"1"}}, false)
	}
	if g, _ = e.st.GetGame(context.Background(), 1); g.Status != "playable" || g.ReportCount != 2 {
		t.Fatalf("state after cleanup: %+v", g)
	}
	// Older version group is collapsed, newest open.
	_, body = anon.get("/games/1-portal-2")
	if !strings.Contains(body, `<details class="version-group" open>`) || strings.Count(body, `<details class="version-group">`) != 1 {
		t.Fatalf("version groups not collapsed as expected")
	}

	// Gold star skips the questionnaire and needs the confirmation.
	resp, body = bob.submitReport(reportOpts{gameID: 3, verID: e.verID2, rating: "gold", desc: ""})
	if resp.StatusCode != 303 {
		t.Fatalf("gold report: %d %s", resp.StatusCode, body)
	}
	// Details are optional for most issue types but required for "other".
	resp, body = alice.submitReport(reportOpts{gameID: 3, verID: e.verID2, rating: "in_game_issues", issue: "other", desc: ""})
	if resp.StatusCode != 400 || !strings.Contains(body, "few sentences") {
		t.Fatalf("other without details: %d", resp.StatusCode)
	}
	resp, _ = alice.submitReport(reportOpts{gameID: 3, verID: e.verID2, rating: "in_game_issues", issue: "performance", desc: ""})
	if resp.StatusCode != 303 {
		t.Fatalf("performance without details: %d", resp.StatusCode)
	}
	// Linux requires the distro; issue=unknown needs no description.
	resp, _ = alice.submitReport(reportOpts{gameID: 3, verID: e.verID2, rating: "unknown", issue: "unknown", desc: "", guestOS: "linux"})
	if resp.StatusCode != 303 {
		t.Fatalf("unknown-issue report: %d", resp.StatusCode)
	}

	// Rate limit: 5 reports/hour per user.
	carol := e.newClient()
	carol.login("carol@example.com")
	for i := 0; i < 6; i++ {
		resp, body = carol.submitReport(reportOpts{gameID: 2, verID: e.verID2, rating: "playable", desc: "Repeated submission number for the rate limit test."})
		if i == 0 && resp.StatusCode != 303 {
			t.Fatalf("rate limit: first report status %d", resp.StatusCode)
		}
	}
	if resp.StatusCode != 422 || !strings.Contains(body, "too quickly") {
		t.Fatalf("rate limit not enforced: %d", resp.StatusCode)
	}

	// Upload quota (3 files/day).
	dave := e.newClient()
	dave.login("dave@example.com")
	resp, body = dave.submitReport(reportOpts{gameID: 2, verID: e.verID2, rating: "in_game_issues", desc: "Testing the per-user quota on uploads here.", files: map[string][]byte{"a.png": png, "b.png": png, "c.png": png, "d.png": png}})
	if resp.StatusCode != 400 || !strings.Contains(body, "upload limit") {
		t.Fatalf("quota: %d", resp.StatusCode)
	}
	files, _ := filepath.Glob(filepath.Join(e.s.Uploads.Root, "u*", "*"))
	if len(files) != 2 {
		t.Fatalf("rejected uploads left on disk: %v", files)
	}

	// Account page and display name.
	resp, body = alice.get("/account")
	if resp.StatusCode != 200 || strings.Contains(body, "alice@example.com") || !strings.Contains(body, "Portal 2") {
		t.Fatalf("account: %d", resp.StatusCode)
	}
	resp, _ = alice.postForm("/account", url.Values{"display_name": {"Alice W"}}, false)
	if resp.StatusCode != 303 {
		t.Fatalf("rename: %d", resp.StatusCode)
	}
	_, body = anon.get("/games/1-portal-2/reports/1")
	if !strings.Contains(body, "Alice W") {
		t.Fatal("renamed author not shown")
	}
	// Logout clears the session.
	alice.postForm("/logout", nil, false)
	if resp, _ := alice.get("/account"); resp.StatusCode != 303 {
		t.Errorf("after logout account = %d", resp.StatusCode)
	}

	// Triage: owned list pushed before activation lights the badge afterwards.
	matched, err := e.st.ReplaceOwned(context.Background(), []store.OwnedEntry{{Storefront: "steam", StorefrontID: "220"}, {Storefront: "steam", StorefrontID: "999999"}})
	if err != nil || matched != 1 {
		t.Fatalf("ReplaceOwned matched=%d err=%v", matched, err)
	}
	g, _ = e.st.GetGame(context.Background(), 2)
	if !g.Active || !g.TriageAvailable {
		t.Fatalf("triage availability after activation: %+v", g)
	}
	_, body = anon.get("/games/2-half-life-2")
	if !strings.Contains(body, "Auto-Triage available") || strings.Contains(body, "Donate a copy") {
		t.Fatal("triage badge missing or donate button shown for an available game")
	}
	_, body = anon.get("/games/3-stardew-valley")
	if !strings.Contains(body, "Auto-Triage unavailable") || !strings.Contains(body, "/games/3/donate") || !strings.Contains(body, "Donate a copy") {
		t.Fatal("donate link missing")
	}
	// Gift accounts are offered only for storefronts the game is sold on.
	_, body = bob.get("/games/2/donate")
	if !strings.Contains(body, "Steam: utm-triage") || !strings.Contains(body, `data-store="steam"`) || !strings.Contains(body, "store.steampowered.com/app/220") || !strings.Contains(body, "Gift email: <strong>steam-gifts@example.com") || strings.Contains(body, "GOG: utm-triage") {
		t.Error("triage form should offer the Steam account only")
	}
	_, body = bob.get("/games/3/donate")
	if strings.Contains(body, "I can gift") || strings.Contains(body, "Steam: utm-triage") || !strings.Contains(body, `value="demo" checked`) {
		t.Error("triage form without matching storefront should offer only a demo")
	}
	if resp, _ := bob.postForm("/games/3/donate", url.Values{"kind": {"gift"}, "storefront": {"steam"}}, false); resp.StatusCode != 400 {
		t.Errorf("gift on unsupported storefront = %d", resp.StatusCode)
	}
	resp, body = bob.postForm("/games/3/donate", url.Values{"kind": {"demo"}, "demo_url": {"https://example.com/demo.zip"}, "notes": {"Demo on the official site"}}, false)
	if resp.StatusCode != 200 || !strings.Contains(body, "Thanks") {
		t.Fatalf("triage request: %d", resp.StatusCode)
	}
	reqs, _ := e.st.ListTriageRequests(context.Background(), "open", 10)
	if len(reqs) != 1 || reqs[0].DemoURL != "https://example.com/demo.zip" {
		t.Fatalf("triage requests: %+v", reqs)
	}

	// Moderation: hiding a report recomputes status and removes it from the page.
	if err := e.st.SetReportVisibility(context.Background(), 2, "hidden"); err != nil {
		t.Fatal(err)
	}
	g, _ = e.st.GetGame(context.Background(), 1)
	if g.Status != "in_game_issues" || g.ReportCount != 1 {
		t.Fatalf("status after hiding: %+v", g)
	}
	if resp, _ := anon.get("/games/1-portal-2/reports/2"); resp.StatusCode != 404 {
		t.Errorf("hidden report public status = %d", resp.StatusCode)
	}
	if resp, _ := bob.get("/games/1-portal-2/reports/2"); resp.StatusCode != 200 {
		t.Errorf("hidden report author status = %d", resp.StatusCode)
	}

	// Authors can delete their own reports; strangers cannot.
	resp, _ = bob.postForm("/reports/1/delete", url.Values{"confirm": {"1"}}, false)
	if resp.StatusCode != 404 {
		t.Fatalf("stranger delete = %d", resp.StatusCode)
	}
	alice.login("alice@example.com")
	resp, _ = alice.postForm("/reports/1/delete", nil, false) // no confirmation → back to edit page
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/reports/1/edit" {
		t.Fatalf("unconfirmed delete = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Report 1 has a comment, so direct deletion is refused and a request is filed instead.
	resp, _ = alice.postForm("/reports/1/delete", url.Values{"confirm": {"1"}}, false)
	if resp.StatusCode != 403 {
		t.Fatalf("delete with comments = %d", resp.StatusCode)
	}
	_, body = alice.get("/reports/1/edit")
	if !strings.Contains(body, "Request deletion") || strings.Contains(body, "Yes, delete my report") {
		t.Fatal("edit page should offer a deletion request, not direct delete")
	}
	resp, _ = alice.postForm("/reports/1/delete-request", url.Values{"confirm": {"1"}, "reason": {"Posted on the wrong game"}}, false)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/account?deletion_requested=1" {
		t.Fatalf("delete request = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	_, body = alice.get("/reports/1/edit")
	if !strings.Contains(body, "pending review") {
		t.Fatal("pending request not shown")
	}
	dreqs, _ := e.st.ListDeletionRequests(context.Background(), "open", 10)
	if len(dreqs) != 1 || dreqs[0].ReportID != 1 || dreqs[0].Reason != "Posted on the wrong game" {
		t.Fatalf("deletion requests: %+v", dreqs)
	}
	// Bob deletes his own comment; the report then qualifies for direct deletion
	// (still within 24 h), which exercises the direct path.
	var commentID int64
	e.d.Reader.QueryRow(`SELECT id FROM comments WHERE report_id = 1 AND user_id = ?`, 2).Scan(&commentID)
	resp, _ = alice.postForm(fmt.Sprintf("/comments/%d/delete", commentID), url.Values{"confirm": {"1"}}, false)
	if resp.StatusCode != 404 {
		t.Fatalf("delete someone else's comment = %d", resp.StatusCode)
	}
	resp, _ = bob.postForm(fmt.Sprintf("/comments/%d/delete", commentID), url.Values{"confirm": {"1"}}, false)
	if resp.StatusCode != 303 {
		t.Fatalf("delete own comment = %d", resp.StatusCode)
	}
	if rep, _ := e.st.GetReport(context.Background(), 1); rep.CommentCount != 0 || len(rep.Comments) != 0 {
		t.Fatalf("comment not removed: %+v", rep.CommentCount)
	}
	resp, _ = alice.postForm("/reports/1/delete", url.Values{"confirm": {"1"}}, false)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/account?deleted=1" {
		t.Fatalf("delete = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := anon.get("/games/1-portal-2/reports/1"); resp.StatusCode != 404 {
		t.Errorf("deleted report public status = %d", resp.StatusCode)
	}
	g, _ = e.st.GetGame(context.Background(), 1)
	if g.ReportCount != 0 || g.Active {
		t.Fatalf("game after deleting its only visible report: %+v", g)
	}
	if resp, _ := anon.get("/games/1-portal-2"); resp.StatusCode != 404 {
		t.Errorf("game with no reports should be 404, got %d", resp.StatusCode)
	}
	_, body = anon.get("/")
	if strings.Contains(body, "Portal 2") {
		t.Error("game with no reports still listed")
	}
	if err := e.st.SetReportVisibility(context.Background(), 2, "visible"); err != nil {
		t.Fatal(err)
	}

	// Retention sweep removes old files.
	time.Sleep(10 * time.Millisecond)
	paths, err := e.st.ExpireAttachments(context.Background(), time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	_ = paths
	// Pager renders once there is more than one page.
	e.s.perPage = 2
	resp, body = anon.get("/?sort=name")
	if resp.StatusCode != 200 || !strings.Contains(body, `class="pager"`) || !strings.Contains(body, `href="/?page=2&amp;sort=name"`) {
		t.Fatalf("pager missing: %d", resp.StatusCode)
	}
	if resp, body := anon.get("/?page=2&sort=name"); resp.StatusCode != 200 || !strings.Contains(body, `<span class="current">2</span>`) {
		t.Fatalf("page 2: %d", resp.StatusCode)
	}
	// Static assets are immutable when versioned.
	resp, _ = anon.get("/static/" + e.s.assetVersion + "/css/main.css")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("static: %d %s", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	// About page renders.
	if resp, b := anon.get("/triage"); resp.StatusCode != 200 || !strings.Contains(b, "Fix proposed") || !strings.Contains(resp.Header.Get("Cache-Control"), "public") {
		t.Errorf("triage doc page: %d", resp.StatusCode)
	}
	if resp, b := anon.get("/privacy"); resp.StatusCode != 200 || !strings.Contains(b, "Attachments") {
		t.Errorf("privacy page: %d", resp.StatusCode)
	}
	if resp, _ := anon.get("/about"); resp.StatusCode != 200 {
		t.Errorf("about: %d", resp.StatusCode)
	}
}

// solvePoW finds a proof-of-work nonce for a sign-in form token.
func solvePoW(token string, bits int) string {
	for i := 0; ; i++ {
		if n := strconv.Itoa(i); auth.VerifyProofOfWork(token, n, bits) {
			return n
		}
	}
}
