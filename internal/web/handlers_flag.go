package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/cdn"
	"github.com/utmapp/utm-games-db/internal/store"
)

// handleFlag records that the signed-in user reported a report or comment
// to the maintainers. One click, no reason; repeated clicks change nothing;
// nothing public shows it. Rate limited like +1s.
func (s *Server) handleFlag(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFrom(r.Context())
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			s.notFound(w, r)
			return
		}
		if !s.Limiter.Allow(fmt.Sprintf("flag:user:%d", u.ID), s.Cfg.RatePlusOnesPerHour, time.Hour) {
			s.errorPage(w, r, http.StatusTooManyRequests, "Not possible", "Too many requests; slow down.")
			return
		}
		err = s.Store.Flag(r.Context(), kind, id, u.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.notFound(w, r)
			return
		case errors.Is(err, store.ErrOwnContent):
			s.errorPage(w, r, http.StatusForbidden, "Not possible", "You cannot report your own content.")
			return
		case err != nil:
			s.serverError(w, r, err)
			return
		}
		// Back to the report page with a thank-you; the flag itself leaves no trace there.
		reportID := id
		if kind == store.FlagComment {
			if rid, err := s.Store.CommentReport(r.Context(), id); err == nil {
				reportID = rid
			}
		}
		back := fmt.Sprintf("/reports/%d", reportID)
		if path, _, err := s.Store.ReportPaths(r.Context(), reportID); err == nil {
			back = path
		}
		cdn.Private(w)
		http.Redirect(w, r, back+"?reported=1", http.StatusSeeOther)
	}
}
