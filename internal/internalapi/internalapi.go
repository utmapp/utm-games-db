// Package internalapi is the loopback-only JSON API used by operators (via an
// SSH tunnel and curl) and by the Auto-Triage agent. It has no
// authentication of its own: isolation is the network boundary, and
// browsers are refused (see rejectBrowsers) so a web page the operator has
// open cannot reach it through the tunnel.
package internalapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/antispam"
	"github.com/utmapp/utm-games-db/internal/cdn"
	"github.com/utmapp/utm-games-db/internal/config"
	"github.com/utmapp/utm-games-db/internal/igdb"
	"github.com/utmapp/utm-games-db/internal/store"
	"github.com/utmapp/utm-games-db/internal/upload"
)

// Server serves the internal API.
type Server struct {
	Cfg     *config.Config
	Store   *store.Store
	Syncer  *igdb.Syncer
	Akismet *antispam.Akismet
	Uploads upload.Store
	Log     *slog.Logger
	// Sweep runs the attachment retention sweeper (wired by main).
	Sweep func(ctx context.Context) (int, error)
	// Purger invalidates the public pages a moderation write changes (nil:
	// rely on the CDN TTL).
	Purger cdn.Purger
}

// purgeReport invalidates the list, the game page and the report page that
// show a report.
func (s *Server) purgeReport(ctx context.Context, reportID int64) {
	if s.Purger == nil {
		return
	}
	if rep, err := s.Store.GetReport(ctx, reportID); err == nil {
		s.Purger.Purge(store.PagesForReport(rep.GameID, rep.GameSlug, rep.ID)...)
	}
}

// purgeGame invalidates the list and a game's page.
func (s *Server) purgeGame(ctx context.Context, gameID int64) {
	if s.Purger == nil {
		return
	}
	if g, err := s.Store.GetGame(ctx, gameID); err == nil {
		s.Purger.Purge(store.PagesForGame(g.ID, g.Slug)...)
	}
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/healthz", func(w http.ResponseWriter, r *http.Request) { ok(w, map[string]string{"status": "ok"}) })

	// Versions
	mux.HandleFunc("GET /internal/utm-versions", s.listVersions)
	mux.HandleFunc("POST /internal/utm-versions", s.createVersion)
	mux.HandleFunc("DELETE /internal/utm-versions/{id}", s.deleteVersion)

	// Reports / triage agent
	mux.HandleFunc("GET /internal/reports", s.listReports)
	mux.HandleFunc("GET /internal/reports/{id}", s.getReport)
	mux.HandleFunc("GET /internal/attachments/{id}", s.getAttachment)
	mux.HandleFunc("POST /internal/reports/{id}/comments", s.botComment)
	mux.HandleFunc("POST /internal/reports/{id}/visibility", s.reportVisibility)
	mux.HandleFunc("POST /internal/reports/{id}/triage", s.reportTriage)
	mux.HandleFunc("POST /internal/comments/{id}/visibility", s.commentVisibility)
	mux.HandleFunc("GET /internal/deletion-requests", s.listDeletionRequests)
	mux.HandleFunc("POST /internal/deletion-requests/{id}/status", s.deletionRequestStatus)
	mux.HandleFunc("GET /internal/triage/requests", s.listTriageRequests)
	mux.HandleFunc("POST /internal/triage/requests/{id}/status", s.triageRequestStatus)
	mux.HandleFunc("GET /internal/triage/owned", s.listOwned)
	mux.HandleFunc("PUT /internal/triage/owned", s.replaceOwned)
	mux.HandleFunc("PUT /internal/triage/manual/{id}", s.manualAvailability(true))
	mux.HandleFunc("DELETE /internal/triage/manual/{id}", s.manualAvailability(false))

	// Users / moderation
	mux.HandleFunc("GET /internal/users", s.listUsers)
	mux.HandleFunc("POST /internal/users/{id}/ban", s.ban(true))
	mux.HandleFunc("POST /internal/users/{id}/unban", s.ban(false))
	mux.HandleFunc("GET /internal/spam-log", s.listSpam)
	mux.HandleFunc("POST /internal/spam-log/{id}/feedback", s.spamFeedback)

	// Maintenance
	mux.HandleFunc("POST /internal/sync", s.sync)
	mux.HandleFunc("GET /internal/sync/status", s.syncStatus)
	mux.HandleFunc("POST /internal/games/{id}/refresh", s.refreshGame)
	mux.HandleFunc("POST /internal/games/{id}/recompute", s.recomputeGame)
	mux.HandleFunc("POST /internal/maintenance/sweep-attachments", s.sweep)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if err := s.rejectBrowsers(r); err != nil {
			fail(w, http.StatusForbidden, err)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// rejectBrowsers refuses requests a browser could make on a web page's
// behalf. Browsers mark every request with Sec-Fetch-Site (or Origin on
// cross-site writes); curl and the triage agent send neither. The Host check
// stops DNS rebinding, where a hostile name resolves to 127.0.0.1 and turns
// a page into a same-origin client of the tunnelled port.
func (s *Server) rejectBrowsers(r *http.Request) error {
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		return errors.New("browser requests are not accepted")
	}
	if s.Cfg != nil && s.Cfg.InternalAllowNonLoopback {
		return nil
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || strings.EqualFold(host, "localhost") {
		return nil
	}
	return errors.New("host must be localhost or a loopback address")
}

func ok(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func failFor(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrVersionInUse), errors.Is(err, store.ErrGameInactive):
		fail(w, http.StatusConflict, err)
	default:
		fail(w, http.StatusInternalServerError, err)
	}
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// --- versions ---------------------------------------------------------------

func (s *Server) listVersions(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.ListVersions(r.Context())
	if err != nil {
		failFor(w, err)
		return
	}
	if v == nil {
		v = []store.UTMVersion{}
	}
	ok(w, v)
}

func (s *Server) createVersion(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version    string `json:"version"`
		Prerelease bool   `json:"prerelease"`
		ReleasedAt string `json:"released_at"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	var rel *time.Time
	if in.ReleasedAt != "" {
		t, err := time.Parse("2006-01-02", in.ReleasedAt)
		if err != nil {
			fail(w, http.StatusBadRequest, fmt.Errorf("released_at: use YYYY-MM-DD"))
			return
		}
		rel = &t
	}
	v, err := s.Store.CreateVersion(r.Context(), in.Version, in.Prerelease, rel)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.Log.Info("utm version added", "version", v.Version)
	w.WriteHeader(http.StatusCreated)
	ok(w, v)
}

func (s *Server) deleteVersion(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.DeleteVersion(r.Context(), id); err != nil {
		failFor(w, err)
		return
	}
	ok(w, map[string]any{"deleted": id})
}

// --- reports ----------------------------------------------------------------

func (s *Server) listReports(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var since time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			if n, err2 := strconv.ParseInt(v, 10, 64); err2 == nil {
				t = time.Unix(n, 0)
			} else {
				fail(w, http.StatusBadRequest, fmt.Errorf("since: RFC3339 or unix seconds"))
				return
			}
		}
		since = t
	}
	gameID, _ := strconv.ParseInt(q.Get("game"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	reps, err := s.Store.ListReports(r.Context(), since, gameID, q.Get("visibility"), q.Get("triage_status"), limit)
	if err != nil {
		failFor(w, err)
		return
	}
	if reps == nil {
		reps = []store.Report{}
	}
	ok(w, map[string]any{"reports": reps})
}

func (s *Server) getReport(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	rep, err := s.Store.GetReport(r.Context(), id)
	if err != nil {
		failFor(w, err)
		return
	}
	rep.Comments, err = s.Store.ListComments(r.Context(), id, true)
	if err != nil {
		failFor(w, err)
		return
	}
	g, err := s.Store.GetGame(r.Context(), rep.GameID)
	if err != nil {
		failFor(w, err)
		return
	}
	ok(w, map[string]any{"report": rep, "game": g})
}

func (s *Server) getAttachment(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	a, err := s.Store.GetAttachment(r.Context(), id)
	if err != nil {
		failFor(w, err)
		return
	}
	s.Uploads.Serve(w, r, a.Path, a.Filename, a.MIME)
}

func (s *Server) botComment(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	var in struct {
		Body string `json:"body_md"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	in.Body = strings.TrimSpace(in.Body)
	if in.Body == "" || len(in.Body) > 20000 {
		fail(w, http.StatusBadRequest, errors.New("body_md must be 1–20000 characters"))
		return
	}
	cid, err := s.Store.AddComment(r.Context(), store.CommentInput{ReportID: id, Bot: true, Body: in.Body})
	if err != nil {
		failFor(w, err)
		return
	}
	s.purgeReport(r.Context(), id)
	w.WriteHeader(http.StatusCreated)
	ok(w, map[string]any{"comment_id": cid})
}

func (s *Server) reportVisibility(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	var in struct {
		Visibility string `json:"visibility"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if !store.ValidVisibility(in.Visibility) {
		fail(w, http.StatusBadRequest, errors.New("visibility must be visible, hidden or removed"))
		return
	}
	if err := s.Store.SetReportVisibility(r.Context(), id, in.Visibility); err != nil {
		failFor(w, err)
		return
	}
	s.purgeReport(r.Context(), id)
	s.Log.Info("report visibility changed", "report", id, "visibility", in.Visibility)
	ok(w, map[string]any{"report": id, "visibility": in.Visibility})
}

// reportTriage sets {status: pending|in_progress|needs_clarification|fix_proposed|wont_fix|cannot_reproduce|""}.
func (s *Server) reportTriage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.SetTriageStatus(r.Context(), id, in.Status); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			failFor(w, err)
		} else {
			fail(w, http.StatusBadRequest, fmt.Errorf("%w (valid: %s, or empty to clear)", err, strings.Join(store.TriageStatuses, ", ")))
		}
		return
	}
	s.purgeReport(r.Context(), id)
	s.Log.Info("report triage status set", "report", id, "status", in.Status)
	ok(w, map[string]any{"report": id, "triage_status": in.Status})
}

func (s *Server) commentVisibility(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	var in struct {
		Visibility string `json:"visibility"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if !store.ValidVisibility(in.Visibility) {
		fail(w, http.StatusBadRequest, errors.New("visibility must be visible, hidden or removed"))
		return
	}
	reportID, err := s.Store.SetCommentVisibility(r.Context(), id, in.Visibility)
	if err != nil {
		failFor(w, err)
		return
	}
	s.purgeReport(r.Context(), reportID)
	ok(w, map[string]any{"comment": id, "visibility": in.Visibility})
}

func (s *Server) listDeletionRequests(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	st := r.URL.Query().Get("status")
	if st == "" && !r.URL.Query().Has("status") {
		st = "open"
	}
	reqs, err := s.Store.ListDeletionRequests(r.Context(), st, limit)
	if err != nil {
		failFor(w, err)
		return
	}
	if reqs == nil {
		reqs = []store.DeletionRequest{}
	}
	ok(w, map[string]any{"requests": reqs})
}

// deletionRequestStatus sets {status: done|rejected|open}; "done" removes the report.
func (s *Server) deletionRequestStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	reportID, paths, err := s.Store.ResolveDeletionRequest(r.Context(), id, in.Status)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			failFor(w, err)
		} else {
			fail(w, http.StatusBadRequest, err)
		}
		return
	}
	s.Uploads.RemoveAll(paths, s.Log)
	if in.Status == "done" {
		s.purgeReport(r.Context(), reportID)
	}
	s.Log.Info("deletion request resolved", "request", id, "status", in.Status)
	ok(w, map[string]any{"request": id, "status": in.Status})
}

// --- triage -------------------------------------------------------------------

func (s *Server) listTriageRequests(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	reqs, err := s.Store.ListTriageRequests(r.Context(), r.URL.Query().Get("status"), limit)
	if err != nil {
		failFor(w, err)
		return
	}
	if reqs == nil {
		reqs = []store.TriageRequest{}
	}
	ok(w, map[string]any{"requests": reqs})
}

func (s *Server) triageRequestStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.SetTriageRequestStatus(r.Context(), id, in.Status); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			failFor(w, err)
		} else {
			fail(w, http.StatusBadRequest, err)
		}
		return
	}
	ok(w, map[string]any{"request": id, "status": in.Status})
}

func (s *Server) listOwned(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListOwned(r.Context())
	if err != nil {
		failFor(w, err)
		return
	}
	if list == nil {
		list = []store.OwnedEntry{}
	}
	ok(w, map[string]any{"owned": list})
}

func (s *Server) replaceOwned(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	var entries []store.OwnedEntry
	if err := json.Unmarshal(body, &entries); err != nil || entries == nil {
		fail(w, http.StatusBadRequest, errors.New("expected a JSON array of {storefront, storefront_id, free} or {igdb_game_id, free}; send [] to clear the list explicitly"))
		return
	}
	matched, err := s.Store.ReplaceOwned(r.Context(), entries)
	if err != nil {
		failFor(w, err)
		return
	}
	if s.Purger != nil {
		s.Purger.Purge("/", store.JSONGamesPath) // game pages pick up badge changes within their TTL
	}
	s.Log.Info("owned games list replaced", "entries", len(entries), "matched", matched)
	ok(w, map[string]any{"received": len(entries), "matched": matched})
}

func (s *Server) manualAvailability(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		if err := s.Store.SetManualAvailability(r.Context(), id, on); err != nil {
			failFor(w, err)
			return
		}
		s.purgeGame(r.Context(), id)
		ok(w, map[string]any{"game": id, "manual_triage_available": on})
	}
}

// --- users / moderation --------------------------------------------------------

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	users, err := s.Store.ListUsers(r.Context(), limit)
	if err != nil {
		failFor(w, err)
		return
	}
	if users == nil {
		users = []store.User{}
	}
	ok(w, map[string]any{"users": users})
}

func (s *Server) ban(banned bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		if err := s.Store.SetBanned(r.Context(), id, banned); err != nil {
			failFor(w, err)
			return
		}
		s.Log.Info("user ban state changed", "user", id, "banned", banned)
		ok(w, map[string]any{"user": id, "banned": banned})
	}
}

func (s *Server) listSpam(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.Store.ListSpam(r.Context(), limit)
	if err != nil {
		failFor(w, err)
		return
	}
	if entries == nil {
		entries = []store.SpamEntry{}
	}
	ok(w, map[string]any{"entries": entries})
}

func (s *Server) spamFeedback(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	var in struct {
		Feedback string `json:"feedback"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if in.Feedback != "spam" && in.Feedback != "ham" {
		fail(w, http.StatusBadRequest, errors.New("feedback must be spam or ham"))
		return
	}
	entry, err := s.Store.GetSpam(r.Context(), id)
	if err != nil {
		failFor(w, err)
		return
	}
	if s.Akismet.Enabled() {
		var payload struct {
			Fields    map[string]string `json:"fields"`
			IP        string            `json:"ip"`
			UA        string            `json:"ua"`
			Referrer  string            `json:"referrer"`
			Author    string            `json:"author"`
			Permalink string            `json:"permalink"`
			Type      string            `json:"type"`
		}
		if err := json.Unmarshal([]byte(entry.Payload), &payload); err != nil {
			fail(w, http.StatusInternalServerError, fmt.Errorf("spam log payload: %w", err))
			return
		}
		c := antispam.Comment{UserIP: payload.IP, UserAgent: payload.UA, Referrer: payload.Referrer, Type: payload.Type, Author: payload.Author, Content: antispam.JoinFields(payload.Fields), Permalink: payload.Permalink}
		if in.Feedback == "ham" {
			err = s.Akismet.SubmitHam(r.Context(), c)
		} else {
			err = s.Akismet.SubmitSpam(r.Context(), c)
		}
		if err != nil {
			fail(w, http.StatusBadGateway, err)
			return
		}
	}
	if err := s.Store.SetSpamFeedback(r.Context(), id, in.Feedback); err != nil {
		failFor(w, err)
		return
	}
	ok(w, map[string]any{"entry": id, "feedback": in.Feedback, "sent_to_akismet": s.Akismet.Enabled()})
}

// --- maintenance -----------------------------------------------------------------

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Full     bool `json:"full"`
		MaxPages int  `json:"max_pages"`
		Wait     bool `json:"wait"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if in.Wait {
		sum, err := s.Syncer.Run(r.Context(), in.Full, in.MaxPages)
		if err != nil {
			if errors.Is(err, igdb.ErrAlreadyRunning) {
				fail(w, http.StatusConflict, err)
				return
			}
			fail(w, http.StatusBadGateway, err)
			return
		}
		ok(w, sum)
		return
	}
	go func() {
		if _, err := s.Syncer.Run(context.Background(), in.Full, in.MaxPages); err != nil && !errors.Is(err, igdb.ErrAlreadyRunning) {
			s.Log.Error("igdb sync", "err", err)
		}
	}()
	w.WriteHeader(http.StatusAccepted)
	ok(w, map[string]string{"status": "started"})
}

func (s *Server) syncStatus(w http.ResponseWriter, r *http.Request) {
	ok(w, s.Syncer.Status(r.Context()))
}

func (s *Server) refreshGame(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Syncer.ResolveDetails(r.Context(), id); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	s.purgeGame(r.Context(), id)
	g, err := s.Store.GetGame(r.Context(), id)
	if err != nil {
		failFor(w, err)
		return
	}
	ok(w, g)
}

func (s *Server) recomputeGame(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.RecomputeGame(r.Context(), id); err != nil {
		failFor(w, err)
		return
	}
	s.purgeGame(r.Context(), id)
	g, err := s.Store.GetGame(r.Context(), id)
	if err != nil {
		failFor(w, err)
		return
	}
	ok(w, g)
}

func (s *Server) sweep(w http.ResponseWriter, r *http.Request) {
	if s.Sweep == nil {
		fail(w, http.StatusNotImplemented, errors.New("sweeper not configured"))
		return
	}
	n, err := s.Sweep(r.Context())
	if err != nil {
		failFor(w, err)
		return
	}
	ok(w, map[string]any{"removed": n})
}
