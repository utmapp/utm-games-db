package web

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/cdn"
	"github.com/utmapp/utm-games-db/internal/status"
	"github.com/utmapp/utm-games-db/internal/store"
)

const (
	cacheList = 300  // seconds the CDN may serve the list
	cacheGame = 3600 // seconds for game and report pages
)

type indexPage struct {
	Page
	Query   string
	Status  string
	Sort    string
	PageNum int `json:"-"`
	Pages   int
	Games   []store.GameCard
	Stats   store.Stats
	Ratings []status.Rating
}

// Page number accessor used by templates (field named PageNum to avoid
// clashing with the embedded Page struct).
func (p *indexPage) pageURL(n int) string {
	if q := listQuery(p.Query, p.Status, p.Sort, n); q != "" {
		return "/?" + q
	}
	return "/"
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pg, _ := strconv.Atoi(q.Get("page"))
	if pg < 1 {
		pg = 1
	}
	sort := q.Get("sort")
	switch sort {
	case "stars", "reports", "name", "recent":
	default:
		sort = "recent"
	}
	st := q.Get("status")
	if !status.Rating(st).Valid() {
		st = ""
	}
	query := strings.TrimSpace(q.Get("q"))
	if len(query) > 100 {
		query = query[:100]
	}
	// One URL per distinct list: empty, unknown and default parameters are
	// dropped and the rest put in a fixed order, so the CDN holds one copy
	// of each list instead of one per spelling of the same request.
	if canonical := listQuery(query, st, sort, pg); canonical != r.URL.RawQuery {
		cdn.Public(w, cacheList)
		target := "/"
		if canonical != "" {
			target += "?" + canonical
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return
	}
	perPage := s.perPage
	games, total, err := s.Store.ListGames(r.Context(), store.ListParams{Query: query, Status: st, Sort: sort, Page: pg, PerPage: perPage})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	stats, err := s.Store.Stats(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pages := (total + perPage - 1) / perPage
	data := &indexPage{
		Page:  Page{Description: "Community compatibility reports for running PC games in UTM.", Canonical: s.Cfg.AbsURL("/")},
		Query: query, Status: st, Sort: sort, PageNum: pg, Pages: pages, Games: games, Stats: stats, Ratings: status.AllRatings,
	}
	if query != "" || st != "" {
		data.NoIndex = true
	}
	s.public(w, r, "index", cacheList, data)
}

type gamePage struct {
	Page
	Game   *store.GameDetail
	Groups []store.VersionGroup
}

// parseGameRef accepts "123" or "123-some-slug" and returns the id.
func parseGameRef(ref string) (int64, bool) {
	digits, _, _ := strings.Cut(ref, "-")
	id, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func (s *Server) handleGame(w http.ResponseWriter, r *http.Request) {
	id, ok := parseGameRef(r.PathValue("game"))
	if !ok {
		s.notFound(w, r)
		return
	}
	g, err := s.Store.GetGame(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !g.Active) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if r.URL.Path != g.Path() {
		cdn.Public(w, cacheGame)
		http.Redirect(w, r, g.Path(), http.StatusMovedPermanently)
		return
	}
	groups, err := s.Store.ListGameReports(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	desc := fmt.Sprintf("%s in UTM: %s. %d reports, %d stars.", g.Name, g.Status.Label(), g.ReportCount, g.StarCount)
	s.public(w, r, "game", cacheGame, &gamePage{Page: Page{Title: g.Name, Description: desc, Canonical: s.Cfg.AbsURL(g.Path()), AltJSON: s.Cfg.AbsURL(store.JSONGamePath(g.ID, g.Slug))}, Game: g, Groups: groups})
}

type reportPage struct {
	Page
	Report   *store.Report
	CanEdit  bool
	Versions []store.UTMVersion
	Values   map[string]string
	Errors   map[string]string
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.notFound(w, r)
		return
	}
	rep, err := s.Store.GetReport(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rep.Attachments = nil // private: never rendered on public pages
	user := auth.UserFrom(r.Context())
	versions, err := s.Store.ListVersions(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data := &reportPage{
		Page:     Page{Title: rep.GameName + " report", Description: rep.GameName + " in UTM: " + rep.Rating.Label(), Canonical: s.Cfg.AbsURL(rep.Path()), AltJSON: s.Cfg.AbsURL(store.JSONReportPath(rep.GameID, rep.GameSlug, rep.ID))},
		Report:   rep,
		Versions: versions,
		Values:   map[string]string{},
		Errors:   map[string]string{},
	}
	if r.URL.Query().Get("reported") == "1" {
		// Shown to whoever lands on this variant of the URL; it says nothing
		// about the content, so the page stays cacheable.
		data.Flash = "Thanks. The maintainers will take a look."
	}
	// Hidden/removed reports are only visible to their author; for anyone
	// else they do not exist, not even as a redirect to their game.
	if rep.Visibility != "visible" && (user == nil || user.ID != rep.UserID) {
		s.privateNotFound(w, r)
		return
	}
	if r.URL.Path != rep.Path() {
		// /reports/{id}, /games/{id}/reports/{id} and stale slugs all redirect
		// to the canonical /games/{id}-{slug}/reports/{id}.
		if rep.Visibility == "visible" {
			cdn.Public(w, cacheGame)
		} else {
			cdn.Private(w)
		}
		http.Redirect(w, r, rep.Path(), http.StatusMovedPermanently)
		return
	}
	if rep.Visibility != "visible" {
		data.CanEdit = true
		s.private(w, r, "report", http.StatusOK, data)
		return
	}
	// The public page is anonymous and cacheable; the edit link is added by
	// the client only when the viewer is the author (see me.js hydration of
	// the account state). Authors can always reach /reports/{id}/edit.
	s.public(w, r, "report", cacheGame, data)
}

type aboutPage struct{ Page }

func (s *Server) handlePrivacy(w http.ResponseWriter, r *http.Request) {
	s.public(w, r, "privacy", 86400, &aboutPage{Page: Page{Title: "Privacy", Canonical: s.Cfg.AbsURL("/privacy")}})
}

// handleTriageDoc explains the Auto-Triage pipeline.
func (s *Server) handleTriageDoc(w http.ResponseWriter, r *http.Request) {
	s.public(w, r, "triage", 86400, &aboutPage{Page: Page{Title: "How Auto-Triage works", Description: "How UTM's Auto-Triage agent reproduces, fixes and validates reported game issues.", Canonical: s.Cfg.AbsURL("/triage")}})
}

func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	s.public(w, r, "about", 86400, &aboutPage{Page: Page{Title: "About", Canonical: s.Cfg.AbsURL("/about")}})
}

var coverFileRe = regexp.MustCompile(`^[A-Za-z0-9_-]+_(big|small)\.jpg$`)

func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !coverFileRe.MatchString(name) {
		s.notFound(w, r)
		return
	}
	p := filepath.Join(s.Cfg.DataDir, "covers", name)
	f, err := os.Open(p)
	if err != nil {
		s.notFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		s.notFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	f, err := s.static.Open(p)
	if err != nil {
		s.notFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		s.notFound(w, r)
		return
	}
	switch {
	case s.devReload:
		cdn.Private(w)
	case r.PathValue("version") == s.assetVersion:
		cdn.Immutable(w)
	default:
		cdn.Public(w, 300)
	}
	rs, ok := f.(interface {
		fs.File
		Seek(int64, int) (int64, error)
	})
	if !ok {
		s.notFound(w, r)
		return
	}
	http.ServeContent(w, r, p, st.ModTime(), rs)
}

// listQuery is the canonical query string of a list page: q, status, sort
// and page, in that order, each only when it carries information.
func listQuery(query, st, sort string, page int) string {
	v := url.Values{}
	if query != "" {
		v.Set("q", query)
	}
	if st != "" {
		v.Set("status", st)
	}
	if sort != "" && sort != "recent" {
		v.Set("sort", sort)
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	// url.Values.Encode sorts keys alphabetically (page, q, sort, status),
	// which is as good a fixed order as any.
	return v.Encode()
}
