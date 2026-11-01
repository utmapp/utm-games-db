package web

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// The JSON twins carry everything the pages show, nothing private, is
// cacheable, and is purged on the same writes as the pages.
func TestAgentView(t *testing.T) {
	e := newEnv(t)
	rec := &recordingPurger{}
	e.s.Purger = rec
	alice := e.newClient()
	alice.login("alice@example.com")
	bob := e.newClient()
	bob.login("bob@example.com")
	png := map[string][]byte{"shot.png": []byte("\x89PNG\r\n\x1a\nxxxx")}
	if resp, body := alice.submitReport(reportOpts{gameID: 1, verID: e.verID, rating: "in_game_issues", desc: "Textures flicker in the intro.", files: png}); resp.StatusCode != 303 {
		t.Fatalf("report: %d %s", resp.StatusCode, body)
	}
	for _, p := range []string{"/reports.json", "/games/1-portal-2.json", "/games/1-portal-2/reports/1.json", "/games.json"} {
		if !rec.has(p) {
			t.Errorf("report creation did not purge %s (got %v)", p, rec.urls)
		}
	}
	if resp, _ := bob.postForm("/reports/1/comments", url.Values{"body": {"Same here on an M2."}}, false); resp.StatusCode != 303 {
		t.Fatal("comment")
	}
	anon := e.newClient()

	// llms.txt: policy and endpoints, cacheable.
	resp, body := anon.get("/llms.txt")
	if resp.StatusCode != 200 || !strings.Contains(body, "read-only for agents") || !strings.Contains(body, "/reports.json") || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("llms.txt: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// Non-canonical addresses redirect to the canonical twin, like the pages.
	for from, to := range map[string]string{"/games/1.json": "/games/1-portal-2.json", "/reports/1.json": "/games/1-portal-2/reports/1.json", "/games/1-old-slug/reports/1.json": "/games/1-portal-2/reports/1.json"} {
		if resp, _ := anon.get(from); resp.StatusCode != 301 || resp.Header.Get("Location") != to {
			t.Errorf("%s -> %d %q; want 301 to %s", from, resp.StatusCode, resp.Header.Get("Location"), to)
		}
	}

	// A game document: report, comment, configuration, no attachment, cache headers.
	resp, body = anon.get("/games/1-portal-2.json")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "s-maxage=3600") || !strings.Contains(resp.Header.Get("Cache-Control"), "max-age=60") || resp.Header.Get("Set-Cookie") != "" || !strings.HasPrefix(resp.Header.Get("ETag"), `W/"`) {
		t.Errorf("game doc headers: %d %v", resp.StatusCode, resp.Header)
	}
	var game struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Reports []struct {
			ID            int64  `json:"id"`
			Author        string `json:"author"`
			AuthorID      int64  `json:"author_id"`
			Rating        string `json:"rating"`
			RatingLabel   string `json:"rating_label"`
			Description   string `json:"description_markdown"`
			Configuration struct {
				UTMVersion string `json:"utm_version"`
				GuestOS    string `json:"guest_os"`
				Chip       string `json:"chip"`
			} `json:"configuration"`
			Comments []struct {
				Author string `json:"author"`
				Kind   string `json:"author_kind"`
				Body   string `json:"body_markdown"`
			} `json:"comments"`
		} `json:"reports"`
	}
	if err := json.Unmarshal([]byte(body), &game); err != nil {
		t.Fatalf("game doc: %v\n%s", err, body)
	}
	if game.Name != "Portal 2" || len(game.Reports) != 1 || game.Reports[0].Rating != "in_game_issues" || game.Reports[0].RatingLabel == "" || game.Reports[0].Configuration.Chip != "M3" || len(game.Reports[0].Comments) != 1 || game.Reports[0].Comments[0].Body != "Same here on an M2." || game.Reports[0].Comments[0].Kind != "user" {
		t.Errorf("game doc content: %+v", game)
	}
	if strings.Contains(body, "attachment") || strings.Contains(body, "shot.png") || strings.Contains(body, "example.com") {
		t.Errorf("game doc leaks private data:\n%s", body)
	}

	// The polling feed lists the report, newest first, with its comment.
	resp, body = anon.get("/reports.json")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "s-maxage=300") {
		t.Errorf("recent headers: %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	var recent struct {
		Reports []struct {
			ID       int64  `json:"id"`
			GameName string `json:"game_name"`
			URL      string `json:"url"`
		} `json:"reports"`
	}
	if err := json.Unmarshal([]byte(body), &recent); err != nil || len(recent.Reports) != 1 || recent.Reports[0].GameName != "Portal 2" || !strings.HasPrefix(recent.Reports[0].URL, "http://games.test/games/1-portal-2/reports/1") {
		t.Errorf("recent feed: %v %+v", err, recent)
	}
	if resp, _ := anon.get("/games.json"); resp.StatusCode != 200 {
		t.Errorf("games list: %d", resp.StatusCode)
	}
	if _, b := anon.get("/versions.json"); !strings.Contains(b, `"version": "5.0.7"`) && !strings.Contains(b, `"version":`) {
		t.Errorf("versions: %s", b)
	}

	// Hidden reports vanish from every document; the document 404 is cacheable briefly.
	if err := e.st.SetReportVisibility(context.Background(), 1, "hidden"); err != nil {
		t.Fatal(err)
	}
	if resp, _ := anon.get("/games/1-portal-2/reports/1.json"); resp.StatusCode != 404 {
		t.Errorf("hidden report doc = %d", resp.StatusCode)
	}
	if _, b := anon.get("/reports.json"); strings.Contains(b, "Textures flicker") {
		t.Error("hidden report still in the feed")
	}
	e.st.SetReportVisibility(context.Background(), 1, "visible")

	// A deleted author is "Deleted user" with no id, as on the pages.
	if resp, _ := alice.postForm("/account/delete", url.Values{"confirm": {"1"}}, false); resp.StatusCode != 200 {
		t.Fatal("delete account")
	}
	_, body = anon.get("/games/1-portal-2/reports/1.json")
	if !strings.Contains(body, `"author": "Deleted user"`) || strings.Contains(body, `"author_id"`) {
		t.Errorf("anonymized report doc:\n%s", body)
	}

	// A report posted faster than a person could fill the form is refused.
	carol := e.newClient()
	carol.login("carol@example.com")
	e.s.Cfg.ContentFormMinAge = 10 * 1e9
	if resp, body := carol.submitReport(reportOpts{gameID: 2, verID: e.verID, rating: "playable", desc: "Fine."}); resp.StatusCode != 400 || !strings.Contains(body, "That was quick") {
		t.Errorf("instant submission accepted: %d", resp.StatusCode)
	}
	e.s.Cfg.ContentFormMinAge = 0
}
