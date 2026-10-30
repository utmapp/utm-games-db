package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/store"
)

// Anything that can cost the server resources is behind sign-in; the only
// exception is POST /login, which has its own defences.
func TestAnonymousGating(t *testing.T) {
	e := newEnv(t)
	anon := e.newClient()
	for _, p := range []string{"/reports/new", "/reports/new?game=1", "/account", "/games/1/donate", "/reports/1/edit"} {
		if resp, _ := anon.get(p); resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
			t.Errorf("GET %s anonymous = %d %s; want redirect to /login", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/games/search?q=portal", nil)
	req.Header.Set("X-Requested-With", "fetch")
	if resp := anon.do(req); resp.StatusCode != 401 {
		t.Errorf("search api anonymous = %d", resp.StatusCode)
	}
	for _, p := range []string{"/api/markdown/preview", "/reports/new", "/reports/1/comments", "/reports/1/plus-one", "/games/1/star", "/games/1/donate", "/account/export", "/account/delete", "/account/delete-attachments"} {
		if resp, _ := anon.postForm(p, url.Values{"x": {"y"}}, true); resp.StatusCode != 401 {
			t.Errorf("POST %s anonymous = %d; want 401", p, resp.StatusCode)
		}
	}
}

func TestLoginHardening(t *testing.T) {
	e := newEnv(t)
	c := e.newClient()
	// No form token: rejected before anything is spent.
	if resp, body := c.postForm("/login", url.Values{"email": {"x@example.com"}}, false); resp.StatusCode != 400 || !strings.Contains(body, "expired") {
		t.Fatalf("missing token: %d", resp.StatusCode)
	}
	// Wrong proof of work.
	v := c.loginForm()
	v.Set("email", "x@example.com")
	for i := 0; ; i++ { // a nonce that is certainly wrong for this token
		if n := "bad" + strconv.Itoa(i); !auth.VerifyProofOfWork(v.Get("form_token"), n, e.s.Cfg.LoginPoWBits) {
			v.Set("pow_nonce", n)
			break
		}
	}
	if resp, body := c.postForm("/login", v, false); resp.StatusCode != 400 || !strings.Contains(body, "could not be verified") {
		t.Fatalf("bad pow: %d %s", resp.StatusCode, body)
	}
	// A solved token works once; replaying it fails.
	v = c.loginForm()
	v.Set("email", "x@example.com")
	if resp, body := c.postForm("/login", v, false); resp.StatusCode != 200 || !strings.Contains(body, "Check your email") {
		t.Fatalf("first use: %d", resp.StatusCode)
	}
	if resp, _ := c.postForm("/login", v, false); resp.StatusCode != 400 {
		t.Fatalf("replayed token accepted: %d", resp.StatusCode)
	}
	// Honeypot and syntactically valid-but-unroutable addresses get the same
	// page as a real request, and nothing distinguishes an existing account.
	e.s.Login.Flush()
	sentBefore := len(e.mailer.codes)
	v = c.loginForm()
	v.Set("email", "x@example.com")
	v.Set("website", "bot")
	resp, body := c.postForm("/login", v, false)
	e.s.Login.Flush()
	if !strings.Contains(body, "Check your email") || len(e.mailer.codes) != sentBefore || !strings.Contains(resp.Header.Get("Set-Cookie"), auth.LoginCookie) {
		t.Fatal("honeypot should look like success (cookie included) and send nothing")
	}
	// Global budget exhausted: same page, nothing sent. (Spend the bucket
	// that the earlier successful request created.)
	for e.s.Limiter.Allow("login:global:minute", e.s.Cfg.LoginGlobalPerMinute, time.Minute) {
	}
	v = c.loginForm()
	v.Set("email", "y@example.com")
	resp, body = c.postForm("/login", v, false)
	e.s.Login.Flush()
	if !strings.Contains(body, "Check your email") || len(e.mailer.codes) != sentBefore || !strings.Contains(resp.Header.Get("Set-Cookie"), auth.LoginCookie) {
		t.Fatal("over budget should look like success (cookie included) and send nothing")
	}
	// The log mailer must never print the address.
	if strings.Contains(e.mailer.lastTo, "@") {
		t.Fatal("test mailer sanity")
	}
}

func TestAccountDataControls(t *testing.T) {
	e := newEnv(t)
	alice := e.newClient()
	alice.login("alice@example.com")
	bob := e.newClient()
	bob.login("bob@example.com")
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 64))
	resp, body := alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "playable", desc: "Runs well.", files: map[string][]byte{"shot.png": png}})
	if resp.StatusCode != 303 {
		t.Fatalf("report: %d %s", resp.StatusCode, body)
	}
	if resp, _ := bob.postForm("/reports/1/comments", url.Values{"body": {"Same here."}}, false); resp.StatusCode != 303 {
		t.Fatalf("comment: %d", resp.StatusCode)
	}
	bob.postForm("/reports/1/plus-one", nil, true)
	bob.postForm("/games/1/star", nil, true)
	// Bob's own report, which alice comments on and +1s.
	if resp, _ := bob.submitReport(reportOpts{gameID: 2, verID: e.verID, rating: "playable", desc: "Fine here."}); resp.StatusCode != 303 {
		t.Fatalf("bob report: %d", resp.StatusCode)
	}
	bobReport := int64(2)
	if resp, _ := alice.postForm(fmt.Sprintf("/reports/%d/comments", bobReport), url.Values{"body": {"Thanks for confirming."}}, false); resp.StatusCode != 303 {
		t.Fatalf("alice comment: %d", resp.StatusCode)
	}
	alice.postForm(fmt.Sprintf("/reports/%d/plus-one", bobReport), nil, true)
	g, _ := e.st.GetGame(context.Background(), 1)
	if g.ReportCount != 1 || g.StarCount != 2 {
		t.Fatalf("setup: %+v", g)
	}

	// A second, uncommented report on another game.
	if resp, _ := alice.submitReport(reportOpts{gameID: 3, verID: e.verID, rating: "gold", desc: ""}); resp.StatusCode != 303 {
		t.Fatalf("second report: %d", resp.StatusCode)
	}
	// Export streams a ZIP back.
	resp, body = alice.postForm("/account/export", nil, false)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("export = %d %v", resp.StatusCode, resp.Header)
	}
	zr, err := zip.NewReader(bytes.NewReader([]byte(body)), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		names[f.Name] = b
	}
	var ex store.UserExport
	if err := json.Unmarshal(names["data.json"], &ex); err != nil {
		t.Fatalf("data.json: %v", err)
	}
	if len(ex.Reports) != 2 || ex.Reports[0].GameName != "Portal 2" || len(ex.Reports[0].Comments) != 1 || len(ex.Attachments) != 1 || !ex.Attachments[0].Included || len(ex.Stars) != 2 || len(ex.Comments) != 1 || len(ex.PlusOnes) != 1 {
		t.Fatalf("export content: %+v", ex)
	}
	if got := names[ex.Attachments[0].ArchivePath]; !bytes.Equal(got, png) {
		t.Fatalf("attachment file missing from archive: %q", ex.Attachments[0].ArchivePath)
	}
	// Rate limited: a second export is refused.
	if resp, _ := alice.postForm("/account/export", nil, false); resp.StatusCode != 429 {
		t.Fatalf("second export = %d", resp.StatusCode)
	}

	// Delete all attachments: file gone, report remains.
	if resp, _ := alice.postForm("/account/delete-attachments", url.Values{"confirm": {"1"}}, false); resp.StatusCode != 303 {
		t.Fatalf("delete attachments = %d", resp.StatusCode)
	}
	var storagePath string
	e.d.Reader.QueryRow(`SELECT storage_path FROM attachments WHERE id = 1`).Scan(&storagePath)
	if _, err := os.Stat(e.s.Uploads.Root + "/" + storagePath); storagePath == "" || !os.IsNotExist(err) {
		t.Fatalf("attachment file still on disk: %q", storagePath)
	}
	if rep, err := e.st.GetReport(context.Background(), 1); err != nil || rep.Visibility != "visible" {
		t.Fatal("report should survive attachment deletion")
	}

	// Delete account: signed out. The report bob commented on survives as
	// "Deleted user" with bob's comment and +1; the uncommented report on
	// game 3 is gone; alice's comment and +1 on bob's report are gone (she
	// could have deleted those herself); alice's stars go, bob's stays.
	resp, body = alice.postForm("/account/delete", url.Values{"confirm": {"1"}}, false)
	if resp.StatusCode != 200 || !strings.Contains(body, "Account deleted") {
		t.Fatalf("delete account = %d", resp.StatusCode)
	}
	if resp, _ := alice.get("/account"); resp.StatusCode != 303 {
		t.Fatal("session should be gone")
	}
	g, _ = e.st.GetGame(context.Background(), 1)
	if g.ReportCount != 1 || g.StarCount != 1 || g.Status != "playable" {
		t.Fatalf("game 1 after account deletion: %+v", g)
	}
	if g3, _ := e.st.GetGame(context.Background(), 3); g3.ReportCount != 0 {
		t.Fatalf("uncommented report should be deleted: %+v", g3)
	}
	_, body = bob.get("/games/1-portal-2/reports/1")
	if !strings.Contains(body, "Deleted user") || !strings.Contains(body, "Same here.") || strings.Contains(body, `data-author="1"`) {
		t.Fatal("kept report should be anonymized with its comment intact")
	}
	var users, reports, comments, plus int
	e.d.Reader.QueryRow(`SELECT COUNT(*) FROM users WHERE display_name LIKE 'user-%'`).Scan(&users)
	e.d.Reader.QueryRow(`SELECT COUNT(*) FROM reports`).Scan(&reports)
	e.d.Reader.QueryRow(`SELECT COUNT(*) FROM comments`).Scan(&comments)
	e.d.Reader.QueryRow(`SELECT COUNT(*) FROM plus_ones`).Scan(&plus)
	if users != 1 || reports != 2 || comments != 1 || plus != 1 {
		t.Fatalf("rows left: users=%d reports=%d comments=%d plus=%d", users, reports, comments, plus)
	}
	if rep, _ := e.st.GetReport(context.Background(), bobReport); rep.CommentCount != 0 || rep.PlusOneCount != 0 || len(rep.Comments) != 0 {
		t.Fatalf("alice's comment/+1 on bob's report should be gone: %+v", rep)
	}
	// The surviving report stays traceable to the deleted account for
	// moderation, and a ban on it blocks the same address from returning.
	if rep, _ := e.st.GetReport(context.Background(), 1); rep.UserID != 1 || !rep.Anonymized || rep.Author != "Deleted user" {
		t.Fatalf("anonymized report should keep its user id: %+v", rep)
	}
	if du, err := e.st.GetUser(context.Background(), 1); err != nil || du.DeletedAt == nil || du.DisplayName != "deleted-1" {
		t.Fatalf("deleted user row should be kept: %+v %v", du, err)
	}
	if err := e.st.SetBanned(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	banned := e.newClient()
	v := banned.loginForm()
	v.Set("email", "alice@example.com")
	banned.postForm("/login", v, false)
	e.s.Login.Flush()
	if resp, body := banned.postForm("/login/code", url.Values{"code": {e.mailer.lastCode()}}, false); resp.StatusCode != 403 || !strings.Contains(body, "suspended") {
		t.Fatalf("banned deleted account should not sign in: %d", resp.StatusCode)
	}
	e.st.SetBanned(context.Background(), 1, false)
	if err := e.st.SetDisplayName(context.Background(), 2, "deleted user"); err == nil {
		t.Fatal("placeholder name should be reserved")
	}
	// Signing in again with the same address starts fresh: new name, the old
	// anonymized report is not listed as theirs and cannot be edited.
	alice2 := e.newClient()
	alice2.login("alice@example.com")
	if _, body := alice2.get("/account"); !strings.Contains(body, "user-") || strings.Contains(body, "Portal 2") {
		t.Fatal("re-registered account should have a fresh name and not list anonymized reports")
	}
	if resp, _ := alice2.get("/reports/1/edit"); resp.StatusCode != 404 {
		t.Fatalf("anonymized report should not be editable: %d", resp.StatusCode)
	}
}

// Content submissions by one user keep a minimum gap; toggles and search do not.
func TestContentInterval(t *testing.T) {
	e := newEnv(t)
	e.s.Cfg.RateContentInterval = 15 * time.Second
	alice := e.newClient()
	alice.login("alice@example.com")
	bob := e.newClient()
	bob.login("bob@example.com")
	if resp, _ := alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "playable", desc: "First."}); resp.StatusCode != 303 {
		t.Fatalf("first report: %d", resp.StatusCode)
	}
	// A comment right after the report is a second content submission.
	if resp, body := alice.postForm("/reports/1/comments", url.Values{"body": {"Too fast."}}, false); resp.StatusCode != 422 || !strings.Contains(body, "wait 15 seconds") {
		t.Fatalf("second submission within the gap = %d", resp.StatusCode)
	}
	// Other users are unaffected; stars, +1s and search are not throttled.
	if resp, _ := bob.postForm("/reports/1/comments", url.Values{"body": {"Fine for me."}}, false); resp.StatusCode != 303 {
		t.Fatalf("bob comment: %d", resp.StatusCode)
	}
	for i := 0; i < 3; i++ {
		if resp, _ := alice.postForm("/games/1/star", nil, true); resp.StatusCode != 200 {
			t.Fatalf("star toggle %d: %d", i, resp.StatusCode)
		}
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/games/search?q=portal", nil)
		req.Header.Set("X-Requested-With", "fetch")
		if resp := alice.do(req); resp.StatusCode != 200 {
			t.Fatalf("search %d: %d", i, resp.StatusCode)
		}
	}
}
