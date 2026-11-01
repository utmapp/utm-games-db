package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "/",
		"/games/1-portal-2":     "/games/1-portal-2",
		"/a/../b":               "/b",
		"/reports/new?game=12":  "/reports/new?game=12",
		"//evil.example":        "/",
		"/\\evil.example":       "/",
		"https://evil.example/": "/",
		"/\t/evil.example":      "/", // browsers strip tabs: would become //evil.example
		"/\n/evil.example":      "/",
		"/\r/evil.example":      "/",
		"/\x00/evil.example":    "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// Large forms spill to temporary files; they must be removed after the
// request even though withUser hands handlers a copy of the request.
func TestMultipartTempFilesRemoved(t *testing.T) {
	spill := filepath.Join(t.TempDir(), "spill")
	if err := os.Mkdir(spill, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", spill)
	e := newEnv(t)
	alice := e.newClient()
	alice.login("alice@example.com")
	big := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("x"), 9<<20)...)
	resp, body := alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "in_game_issues", desc: "Crashes right after the intro.", files: map[string][]byte{"big.png": big}})
	if resp.StatusCode != 303 {
		t.Fatalf("report: %d %s", resp.StatusCode, body)
	}
	if left, _ := os.ReadDir(spill); len(left) != 0 {
		t.Fatalf("multipart temp files left behind: %d", len(left))
	}
}

// Hidden reports exist only for their author: commenting (including the
// error re-render) must not reveal them to anyone else.
func TestCommentOnHiddenReport(t *testing.T) {
	e := newEnv(t)
	alice := e.newClient()
	alice.login("alice@example.com")
	bob := e.newClient()
	bob.login("bob@example.com")
	if resp, _ := alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "in_game_issues", desc: "Secret words in a hidden report."}); resp.StatusCode != 303 {
		t.Fatal("report")
	}
	if err := e.st.SetReportVisibility(context.Background(), 1, "hidden"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"x", "A valid comment on a hidden report."} {
		resp, b := bob.postForm("/reports/1/comments", url.Values{"body": {body}}, false)
		if resp.StatusCode != 404 || strings.Contains(b, "Secret words") {
			t.Errorf("comment %q on hidden report by stranger = %d (leaked=%v)", body, resp.StatusCode, strings.Contains(b, "Secret words"))
		}
	}
	if resp, _ := alice.postForm("/reports/1/comments", url.Values{"body": {"Author note on my hidden report."}}, false); resp.StatusCode != 303 {
		t.Errorf("author comment on own hidden report = %d", resp.StatusCode)
	}
	// A +1 must not reveal or count on a hidden report either.
	if resp, _ := bob.postForm("/reports/1/plus-one", nil, true); resp.StatusCode != 404 {
		t.Errorf("+1 on hidden report by stranger = %d; want 404", resp.StatusCode)
	}
	var n int
	e.d.Reader.QueryRow(`SELECT plus_one_count FROM reports WHERE id = 1`).Scan(&n)
	if n != 0 {
		t.Errorf("hidden report gained a +1: count %d", n)
	}
}

// 404s that depend on who is asking must not be shared through the CDN, or
// one visitor's 404 is served to the owner.
func TestNotFoundCaching(t *testing.T) {
	e := newEnv(t)
	alice := e.newClient()
	alice.login("alice@example.com")
	bob := e.newClient()
	bob.login("bob@example.com")
	anon := e.newClient()
	if resp, _ := alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "playable", desc: "Runs well."}); resp.StatusCode != 303 {
		t.Fatal("report")
	}
	if resp, _ := bob.get("/reports/1/edit"); resp.StatusCode != 404 || strings.Contains(resp.Header.Get("Cache-Control"), "public") {
		t.Errorf("stranger's edit 404 = %d %q; want private", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	if err := e.st.SetReportVisibility(context.Background(), 1, "hidden"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/games/1-portal-2/reports/1", "/reports/1"} {
		if resp, _ := anon.get(p); resp.StatusCode != 404 || strings.Contains(resp.Header.Get("Cache-Control"), "public") {
			t.Errorf("hidden report %s for anonymous = %d %q; want a private 404", p, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
	}
	if resp, _ := alice.get("/games/1-portal-2/reports/1"); resp.StatusCode != 200 {
		t.Errorf("author view of hidden report = %d", resp.StatusCode)
	}
	if resp, _ := anon.get("/no-such-page"); resp.StatusCode != 404 || !strings.Contains(resp.Header.Get("Cache-Control"), "public") {
		t.Errorf("plain anonymous 404 should stay cacheable: %q", resp.Header.Get("Cache-Control"))
	}
}

// Nobody can pre-claim the placeholder name an account gets on deletion.
func TestDeletionPlaceholderReserved(t *testing.T) {
	e := newEnv(t)
	alice := e.newClient()
	alice.login("alice@example.com")
	bob := e.newClient()
	bob.login("bob@example.com")
	for _, name := range []string{"deleted-1", "Deleted-1"} {
		if resp, _ := bob.postForm("/account", url.Values{"display_name": {name}}, false); resp.StatusCode != 400 {
			t.Fatalf("placeholder name %q should be reserved: %d", name, resp.StatusCode)
		}
	}
	if resp, _ := alice.postForm("/account/delete", url.Values{"confirm": {"1"}}, false); resp.StatusCode != 200 {
		t.Fatalf("delete account = %d", resp.StatusCode)
	}
}

// A sign-in code only works in the browser that requested it, a bounded
// number of wrong guesses void it, and a right guess works exactly once.
func TestSignInCodeBinding(t *testing.T) {
	e := newEnv(t)
	request := func(c *client, email string) string {
		v := c.loginForm()
		v.Set("email", email)
		if resp, body := c.postForm("/login", v, false); resp.StatusCode != 200 || !strings.Contains(body, `autocomplete="one-time-code"`) {
			t.Fatalf("login form: %d", resp.StatusCode)
		}
		e.s.Login.Flush()
		return e.mailer.lastCode()
	}
	carol := e.newClient()
	code := request(carol, "carol@example.com")
	// Another browser (no cookie) cannot use the code, and the attempt does
	// not count against it.
	other := e.newClient()
	if resp, body := other.postForm("/login/code", url.Values{"code": {code}}, false); resp.StatusCode != 400 || !strings.Contains(body, `name="email"`) {
		t.Fatalf("code from another browser = %d; want 400 with the email form", resp.StatusCode)
	}
	// Wrong guesses are counted; the cap voids the code.
	wrong := "000000"
	if wrong == code {
		wrong = "000001"
	}
	for i := 0; i < 2; i++ {
		if resp, body := carol.postForm("/login/code", url.Values{"code": {wrong}}, false); resp.StatusCode != 400 || !strings.Contains(body, "not right") {
			t.Fatalf("wrong guess %d = %d", i, resp.StatusCode)
		}
	}
	if resp, _ := carol.postForm("/login/code", url.Values{"code": {" " + code[:3] + " " + code[3:] + " "}, "next": {"/account"}}, false); resp.StatusCode != 303 || resp.Header.Get("Location") != "/account" {
		t.Fatalf("right code (with spaces) = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := carol.get("/account"); resp.StatusCode != 200 {
		t.Fatalf("signed in but /account = %d", resp.StatusCode)
	}
	// The cookie is gone and the code is spent.
	if resp, _ := carol.postForm("/login/code", url.Values{"code": {code}}, false); resp.StatusCode != 400 {
		t.Fatalf("reused code = %d; want 400", resp.StatusCode)
	}

	dave := e.newClient()
	code = request(dave, "dave@example.com")
	for i := 0; i < 5; i++ {
		dave.postForm("/login/code", url.Values{"code": {wrong}}, false)
	}
	if resp, _ := dave.postForm("/login/code", url.Values{"code": {code}}, false); resp.StatusCode != 400 {
		t.Fatalf("code should be void after 5 wrong guesses: %d", resp.StatusCode)
	}
	// Requesting again issues a fresh code that works.
	code = request(dave, "dave@example.com")
	if resp, _ := dave.postForm("/login/code", url.Values{"code": {code}}, false); resp.StatusCode != 303 {
		t.Fatalf("fresh code = %d", resp.StatusCode)
	}

	var n int
	e.d.Reader.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n != 2 {
		t.Fatalf("users = %d; want carol and dave", n)
	}
}

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

func (p *recordingPurger) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.urls = nil
}

// Writes purge the canonical pages they change (not redirect URLs).
func TestPurgesCanonicalPages(t *testing.T) {
	e := newEnv(t)
	rec := &recordingPurger{}
	e.s.Purger = rec
	alice := e.newClient()
	alice.login("alice@example.com")
	bob := e.newClient()
	bob.login("bob@example.com")
	if resp, _ := alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "playable", desc: "Runs well."}); resp.StatusCode != 303 {
		t.Fatal("report")
	}
	bob.postForm("/reports/1/comments", url.Values{"body": {"Same here."}}, false)
	rec.reset()
	if resp, _ := bob.postForm("/games/1/star", nil, true); resp.StatusCode != 200 {
		t.Fatal("star")
	}
	if !rec.has("/games/1-portal-2") {
		t.Errorf("star should purge the canonical game page, got %v", rec.urls)
	}
	rec.reset()
	if resp, _ := bob.postForm("/account/delete", url.Values{"confirm": {"1"}}, false); resp.StatusCode != 200 {
		t.Fatal("delete account")
	}
	if !rec.has("/games/1-portal-2/reports/1") {
		t.Errorf("account deletion should purge the report page bob commented on, got %v", rec.urls)
	}
}

// Gold reports carry no attachments, whether files are sent with the report
// or added later through the edit form.
func TestGoldReportRejectsAttachments(t *testing.T) {
	e := newEnv(t)
	alice := e.newClient()
	alice.login("alice@example.com")
	png := map[string][]byte{"shot.png": []byte("\x89PNG\r\n\x1a\nxxxx")}
	if resp, body := alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "gold", files: png}); resp.StatusCode != 303 {
		t.Fatalf("gold report: %d %s", resp.StatusCode, body)
	}
	if resp, body := alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "gold", files: png, path: "/reports/1/edit"}); resp.StatusCode != 303 {
		t.Fatalf("gold edit: %d %s", resp.StatusCode, body)
	}
	var n int
	e.d.Reader.QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&n)
	if n != 0 {
		t.Fatalf("gold report has %d attachments", n)
	}
	if files, _ := filepath.Glob(filepath.Join(e.s.Uploads.Root, "u*", "*")); len(files) != 0 {
		t.Fatalf("files stored for a gold report: %v", files)
	}
}

// Over https every response tells browsers to keep using https.
func TestHSTSOnlyOverHTTPS(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	for _, hsts := range []bool{true, false} {
		rec := httptest.NewRecorder()
		withSecurityHeaders(ok, hsts).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if got := rec.Header().Get("Strict-Transport-Security"); (got != "") != hsts {
			t.Errorf("hsts=%v: header %q", hsts, got)
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Error("CSP missing")
		}
	}
}

// A Mac saved on the profile prefills the report form, is exposed to the
// page script through /api/me, and "Save to profile" on a report updates it.
func TestSavedHostConfig(t *testing.T) {
	e := newEnv(t)
	alice := e.newClient()
	alice.login("alice@example.com")
	if _, body := alice.get("/reports/new?game=1"); strings.Contains(body, `value="15.1"`) {
		t.Fatal("fresh profile should not prefill anything")
	}
	if resp, _ := alice.postForm("/account/host", url.Values{"macos_version": {"15.1"}, "device_model": {"Mac mini"}, "chip": {"M4"}, "ram_gb": {"24"}}, false); resp.StatusCode != 303 {
		t.Fatalf("save host = %d", resp.StatusCode)
	}
	if resp, _ := alice.postForm("/account/host", url.Values{"ram_gb": {"lots"}}, false); resp.StatusCode != 400 {
		t.Fatalf("bad memory should be rejected: %d", resp.StatusCode)
	}
	_, body := alice.get("/account")
	for _, want := range []string{`value="15.1"`, `value="Mac mini"`, `value="M4"`, `value="24"`} {
		if !strings.Contains(body, want) {
			t.Errorf("account page lacks %s", want)
		}
	}
	if _, body := alice.get("/reports/new?game=1"); !strings.Contains(body, `name="macos_version" value="15.1"`) || !strings.Contains(body, `name="ram_gb" value="24"`) || !strings.Contains(body, `name="save_host"`) {
		t.Error("report form should be prefilled from the profile and offer to save")
	}
	if _, body := alice.get("/api/me"); !strings.Contains(body, `"host":{"macos_version":"15.1","device_model":"Mac mini","chip":"M4","ram_gb":24}`) {
		t.Errorf("api/me lacks host: %s", body)
	}
	// The saved Mac is part of the account export.
	resp, body := alice.postForm("/account/export", nil, false)
	if resp.StatusCode != 200 {
		t.Fatalf("export = %d", resp.StatusCode)
	}
	var ex struct {
		User struct {
			Host struct {
				MacOSVersion string `json:"macos_version"`
			} `json:"host"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(zipText(t, body, "data.json")), &ex); err != nil || ex.User.Host.MacOSVersion != "15.1" {
		t.Errorf("export lacks the saved Mac: %v %+v", err, ex)
	}
	// Submitting a report with other values and the box ticked updates the profile.
	if resp, body := alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "playable", desc: "Runs well.", extra: map[string]string{"macos_version": "26.1", "chip": "M5", "save_host": "1"}}); resp.StatusCode != 303 {
		t.Fatalf("report: %d %s", resp.StatusCode, body)
	}
	u, _ := e.st.GetUser(context.Background(), 1)
	if u.Host.MacOSVersion != "26.1" || u.Host.Chip != "M5" || u.Host.DeviceModel != "MacBook Pro" {
		t.Fatalf("profile not updated from report: %+v", u.Host)
	}
	// Without the box the profile is left alone.
	alice.submitReport(reportOpts{gameID: 2, verID: e.verID, rating: "playable", desc: "Also fine.", extra: map[string]string{"macos_version": "14.0"}})
	if u, _ := e.st.GetUser(context.Background(), 1); u.Host.MacOSVersion != "26.1" {
		t.Fatalf("profile changed without consent: %+v", u.Host)
	}
}

// zipText returns one file of a ZIP archive held in body.
func zipText(t *testing.T, body, name string) string {
	t.Helper()
	zr, err := zip.NewReader(strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			return string(b)
		}
	}
	t.Fatalf("%s not in archive", name)
	return ""
}

// Reporting content: one click, not on one's own content, invisible to
// everyone, idempotent, gone with the content or when ignored.
func TestFlagContent(t *testing.T) {
	e := newEnv(t)
	alice := e.newClient()
	alice.login("alice@example.com")
	bob := e.newClient()
	bob.login("bob@example.com")
	if resp, _ := alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "playable", desc: "Runs well."}); resp.StatusCode != 303 {
		t.Fatal("report")
	}
	if resp, _ := bob.postForm("/reports/1/comments", url.Values{"body": {"Not for me, it crashes."}}, false); resp.StatusCode != 303 {
		t.Fatal("comment")
	}
	count := func() int {
		var n int
		e.d.Reader.QueryRow(`SELECT COUNT(*) FROM content_flags`).Scan(&n)
		return n
	}
	// Own content is refused; a stranger's is recorded once, however often.
	if resp, _ := alice.postForm("/reports/1/flag", nil, false); resp.StatusCode != 403 {
		t.Errorf("own report flagged: %d", resp.StatusCode)
	}
	if resp, _ := bob.postForm("/reports/1/flag", nil, false); resp.StatusCode != 303 || !strings.HasSuffix(resp.Header.Get("Location"), "/games/1-portal-2/reports/1?reported=1") {
		t.Fatalf("flag = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	bob.postForm("/reports/1/flag", nil, false)
	if resp, _ := alice.postForm("/comments/1/flag", nil, false); resp.StatusCode != 303 {
		t.Fatalf("flag comment = %d", resp.StatusCode)
	}
	if resp, _ := bob.postForm("/comments/1/flag", nil, false); resp.StatusCode != 403 {
		t.Errorf("own comment flagged: %d", resp.StatusCode)
	}
	if count() != 2 {
		t.Fatalf("flags = %d; want 2", count())
	}
	// Nothing public shows them: the page and its JSON twin are unchanged.
	anon := e.newClient()
	for _, p := range []string{"/games/1-portal-2/reports/1", "/games/1-portal-2/reports/1.json"} {
		if _, body := anon.get(p); strings.Contains(strings.ToLower(body), "flagged") || strings.Contains(body, "reported=1") {
			t.Errorf("%s hints at flags", p)
		}
	}
	if _, body := anon.get("/games/1-portal-2/reports/1?reported=1"); !strings.Contains(body, "Thanks. The maintainers will take a look.") {
		t.Error("thank-you missing after reporting")
	}
	// Deleting the comment deletes its flag; the report's stays.
	if resp, _ := bob.postForm("/comments/1/delete", url.Values{"confirm": {"1"}}, false); resp.StatusCode != 303 {
		t.Fatal("delete comment")
	}
	if count() != 1 {
		t.Fatalf("flags after comment deletion = %d; want 1", count())
	}
	// Ignoring deletes the flag and lets the reporter flag again.
	if n, err := e.st.IgnoreFlags(context.Background(), "report", 1); err != nil || n != 1 {
		t.Fatalf("ignore: %d %v", n, err)
	}
	if resp, _ := bob.postForm("/reports/1/flag", nil, false); resp.StatusCode != 303 || count() != 1 {
		t.Fatalf("re-flag after ignore: %d, flags %d", resp.StatusCode, count())
	}
	// Content of a deleted account can still be reported (a comment from
	// someone else keeps the report when its author leaves); removing the
	// report by moderation deletes its flags.
	carol := e.newClient()
	carol.login("carol@example.com")
	if resp, _ := carol.postForm("/reports/1/comments", url.Values{"body": {"Works on my Mac too."}}, false); resp.StatusCode != 303 {
		t.Fatal("carol comment")
	}
	if resp, _ := alice.postForm("/account/delete", url.Values{"confirm": {"1"}}, false); resp.StatusCode != 200 {
		t.Fatal("delete account")
	}
	if resp, _ := carol.postForm("/reports/1/flag", nil, false); resp.StatusCode != 303 || count() != 2 {
		t.Fatalf("flag anonymized report: %d, flags %d", resp.StatusCode, count())
	}
	if err := e.st.SetReportVisibility(context.Background(), 1, "removed"); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatalf("flags after removal = %d; want 0", count())
	}
}

// Every spelling of the same list redirects to one canonical URL, so the
// CDN caches one copy per list rather than one per form submission.
func TestListURLsAreCanonical(t *testing.T) {
	e := newEnv(t)
	anon := e.newClient()
	for from, to := range map[string]string{
		"/?q=&status=&sort=reports":    "/?sort=reports",
		"/?sort=reports&status=&q=":    "/?sort=reports",
		"/?sort=recent":                "/",
		"/?utm_source=twitter":         "/",
		"/?status=playable&q=+portal+": "/?q=portal&status=playable",
		"/?page=1":                     "/",
		"/?status=bogus":               "/",
	} {
		resp, _ := anon.get(from)
		if resp.StatusCode != 301 || resp.Header.Get("Location") != to || !strings.Contains(resp.Header.Get("Cache-Control"), "public") {
			t.Errorf("%s -> %d %q (%s); want 301 to %s, cacheable", from, resp.StatusCode, resp.Header.Get("Location"), resp.Header.Get("Cache-Control"), to)
		}
	}
	for _, p := range []string{"/", "/?sort=reports", "/?q=portal&status=playable", "/?page=2&sort=stars"} {
		if resp, _ := anon.get(p); resp.StatusCode != 200 {
			t.Errorf("canonical %s = %d", p, resp.StatusCode)
		}
	}
}
