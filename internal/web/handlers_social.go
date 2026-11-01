package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/cdn"
	"github.com/utmapp/utm-games-db/internal/markdown"
	"github.com/utmapp/utm-games-db/internal/store"
)

// handleMe returns the per-user state that me.js hydrates onto cached pages.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "Cookie")
	u := auth.UserFrom(r.Context())
	if u == nil {
		writeJSON(w, http.StatusOK, map[string]any{"user": nil})
		return
	}
	stars, err := s.Store.UserStars(r.Context(), u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
		return
	}
	plus, err := s.Store.UserPlusOnes(r.Context(), u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":      map[string]any{"id": u.ID, "display_name": u.DisplayName},
		"host":      u.Host,
		"stars":     stars,
		"plus_ones": plus,
	})
}

func (s *Server) handleStar(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	id, ok := parseGameRef(r.PathValue("id"))
	if !ok {
		s.notFound(w, r)
		return
	}
	if !s.Limiter.Allow(fmt.Sprintf("star:user:%d", u.ID), 120, time.Hour) {
		s.respondToggle(w, r, false, 0, "Too many requests; slow down.", http.StatusTooManyRequests, store.GamePath(id, ""))
		return
	}
	on, count, err := s.Store.ToggleStar(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrGameInactive) {
		s.respondToggle(w, r, false, 0, "Only games with at least one report can be starred. Submit a report first.", http.StatusConflict, fmt.Sprintf("/reports/new?game=%d", id))
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if g, err := s.Store.GetGame(r.Context(), id); err == nil {
		s.Purger.Purge("/", g.Path()) // the canonical page; /games/{id} only redirects
	}
	s.respondToggle(w, r, on, count, "", http.StatusOK, store.GamePath(id, ""))
}

// respondToggle answers a star/+1 action as JSON for fetch or a redirect.
func (s *Server) respondToggle(w http.ResponseWriter, r *http.Request, on bool, count int, errMsg string, code int, back string) {
	if wantsJSON(r) {
		if errMsg != "" {
			writeJSON(w, code, map[string]any{"error": errMsg})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"on": on, "count": count})
		return
	}
	cdn.Private(w)
	if errMsg != "" {
		s.errorPage(w, r, code, "Not possible", errMsg)
		return
	}
	if ref := r.Referer(); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Host == r.Host && strings.HasPrefix(u.Path, "/") {
			back = u.RequestURI()
		}
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) handlePlusOne(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if !s.Limiter.Allow(fmt.Sprintf("plusone:user:%d", u.ID), s.Cfg.RatePlusOnesPerHour, time.Hour) {
		s.respondToggle(w, r, false, 0, "Too many requests; slow down.", http.StatusTooManyRequests, fmt.Sprintf("/reports/%d", id))
		return
	}
	on, count, err := s.Store.TogglePlusOne(r.Context(), id, u.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.respondToggle(w, r, false, 0, "This report is not available.", http.StatusNotFound, "/")
		return
	case errors.Is(err, store.ErrOwnReport):
		s.respondToggle(w, r, false, 0, "You can't +1 your own report.", http.StatusForbidden, fmt.Sprintf("/reports/%d", id))
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	back, gamePath, err := s.Store.ReportPaths(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Purger.Purge("/", back, gamePath)
	s.respondToggle(w, r, on, count, "", http.StatusOK, back)
}

// renderReportWithForm re-renders the report page with a form error. rep must
// already be checked as viewable by the current user.
func (s *Server) renderReportWithForm(w http.ResponseWriter, r *http.Request, rep *store.Report, msg string, snippets []string, values formValues, errs map[string]string) {
	versions, err := s.Store.ListVersions(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	u := auth.UserFrom(r.Context())
	s.private(w, r, "report", http.StatusUnprocessableEntity, &reportPage{
		Page:     Page{Title: rep.GameName + " report", Error: msg, ErrorList: snippets},
		Report:   rep,
		CanEdit:  u != nil && u.ID == rep.UserID,
		Versions: versions,
		Values:   values,
		Errors:   errs,
	})
}

func (s *Server) handleCommentCreate(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	// Hidden and removed reports exist only for their author (as on the
	// report page); everyone else gets a 404 rather than the report.
	rep, err := s.Store.GetReport(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && rep.Visibility != "visible" && rep.UserID != u.ID) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rep.Attachments = nil // private: never rendered
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "Bad request", "Could not read the form.")
		return
	}
	values := valuesFrom(r.PostForm, append([]string{"body", "include_config"}, configKeys...)...)
	errs := map[string]string{}
	body := strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("body"), "\r\n", "\n"))
	if len(body) < 2 {
		errs["body"] = "Write a comment first."
	}
	if len(body) > 10000 {
		errs["body"] = "Comments are limited to 10,000 characters."
	}
	in := store.CommentInput{ReportID: id, UserID: u.ID, Body: body, IP: s.clientIP(r), UserAgent: limitLen(r.UserAgent(), 300)}
	if r.PostFormValue("include_config") == "1" {
		c := parseConfig(r.PostForm, errs)
		in.Config = &c
	}
	if len(errs) > 0 {
		s.renderReportWithForm(w, r, rep, "Please fix the highlighted fields.", nil, values, errs)
		return
	}
	back := rep.Path()
	msg, snippets := s.spamCheck(r.Context(), r, u, "comment", "comment", s.Cfg.RateCommentsPerHour, r.PostFormValue("website"), map[string]string{"body": body}, s.Cfg.AbsURL(back))
	if msg != "" {
		s.renderReportWithForm(w, r, rep, msg, snippets, values, errs)
		return
	}
	cid, err := s.Store.AddComment(r.Context(), in)
	if err == nil && in.Config != nil {
		s.saveHostIfAsked(r, u.ID, *in.Config)
	}
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		if errors.Is(err, store.ErrUnknownVersion) {
			errs["utm_version_id"] = "Choose a valid UTM version."
			s.renderReportWithForm(w, r, rep, "Please fix the highlighted fields.", nil, values, errs)
			return
		}
		s.serverError(w, r, err)
		return
	}
	s.Purger.Purge("/", rep.GamePath(), back)
	cdn.Private(w)
	http.Redirect(w, r, fmt.Sprintf("%s#comment-%d", back, cid), http.StatusSeeOther)
}

func (s *Server) handleSearchAPI(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.Limiter.Allow(fmt.Sprintf("search:user:%d", u.ID), s.Cfg.RateSearchPerMinute, time.Minute) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "slow down"})
		return
	}
	q := limitLen(strings.TrimSpace(r.URL.Query().Get("q")), 100)
	hits, err := s.Store.SearchCatalog(r.Context(), q, 15)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed"})
		return
	}
	if hits == nil {
		hits = []store.CatalogHit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.Limiter.Allow(fmt.Sprintf("preview:user:%d", u.ID), s.Cfg.RatePreviewPerMinute, time.Minute) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	text := limitLen(r.PostFormValue("text"), 20000)
	cdn.Private(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(markdown.Render(text)))
}
