package web

import (
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"

	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/cdn"
)

// Handler builds the public router with middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public, cacheable
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /games/{game}", s.handleGame)                // /games/{id}-{slug}; /games/{id} redirects
	mux.HandleFunc("GET /games/{game}/reports/{id}", s.handleReport) // canonical report permalink
	mux.HandleFunc("GET /reports/{id}", s.handleReport)              // short form, redirects to canonical
	mux.HandleFunc("GET /about", s.handleAbout)
	mux.HandleFunc("GET /triage", s.handleTriageDoc)
	mux.HandleFunc("GET /privacy", s.handlePrivacy)
	mux.HandleFunc("GET /covers/{file}", s.handleCover)
	mux.HandleFunc("GET /static/{version}/{path...}", s.handleStatic)
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		cdn.Public(w, 86400)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("User-agent: *\nDisallow: /login\nDisallow: /account\nDisallow: /reports/new\nDisallow: /api/\n"))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		cdn.Private(w)
		w.Write([]byte("ok"))
	})

	// Auth
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /login/code", s.handleLoginCode)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /account", s.requireUser(s.handleAccount))
	mux.HandleFunc("POST /account", s.requireUser(s.handleAccountSave))
	mux.HandleFunc("POST /account/export", s.requireUser(s.handleAccountExport))
	mux.HandleFunc("POST /account/delete-attachments", s.requireUser(s.handleAccountDeleteAttachments))
	mux.HandleFunc("POST /account/delete", s.requireUser(s.handleAccountDelete))

	// Reports
	mux.HandleFunc("GET /reports/new", s.requireUser(s.handleReportNew))
	mux.HandleFunc("POST /reports/new", s.requireUser(s.handleReportCreate))
	mux.HandleFunc("GET /reports/{id}/edit", s.requireUser(s.handleReportEdit))
	mux.HandleFunc("POST /reports/{id}/edit", s.requireUser(s.handleReportUpdate))
	mux.HandleFunc("POST /reports/{id}/delete", s.requireUser(s.handleReportDelete))
	mux.HandleFunc("POST /reports/{id}/delete-request", s.requireUser(s.handleReportDeleteRequest))
	mux.HandleFunc("POST /comments/{id}/delete", s.requireUser(s.handleCommentDelete))
	mux.HandleFunc("POST /reports/{id}/comments", s.requireUser(s.handleCommentCreate))
	mux.HandleFunc("POST /reports/{id}/plus-one", s.requireUser(s.handlePlusOne))
	mux.HandleFunc("POST /games/{id}/star", s.requireUser(s.handleStar))
	mux.HandleFunc("GET /games/{id}/donate", s.requireUser(s.handleDonateForm))
	mux.HandleFunc("POST /games/{id}/donate", s.requireUser(s.handleDonateSubmit))
	// Attachments have no public URL on purpose: the throttled account export
	// is the only way to get them back (see handleAccountExport).

	// JSON
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/games/search", s.requireUser(s.handleSearchAPI))
	mux.HandleFunc("POST /api/markdown/preview", s.requireUser(s.handlePreview))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { s.notFound(w, r) })

	var h http.Handler = mux
	h = s.withUser(h)
	h = s.withOriginCheck(h)
	h = withSecurityHeaders(h)
	h = s.withRecover(h)
	return h
}

// withUser loads the session user into the context. It never sets cookies,
// so cached public responses stay identical for everyone.
func (s *Server) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := auth.TokenFromRequest(r)
		if tok != "" {
			u, err := s.Auth.UserFromToken(r.Context(), tok)
			if err != nil {
				s.Log.Warn("session lookup", "err", err)
			}
			if u != nil {
				withUser := r.WithContext(auth.WithUser(r.Context(), u))
				// net/http removes multipart temp files only for the request
				// it created, so forms parsed on this copy are cleaned up here.
				defer func() {
					if withUser.MultipartForm != nil && withUser.MultipartForm != r.MultipartForm {
						withUser.MultipartForm.RemoveAll()
					}
				}()
				next.ServeHTTP(w, withUser)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireUser redirects anonymous users to the login page (or 401 for fetch).
func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if auth.UserFrom(r.Context()) == nil {
			if wantsJSON(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
				return
			}
			cdn.Private(w)
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// withOriginCheck rejects cross-site POSTs (CSRF) using Origin/Referer and
// Sec-Fetch-Site; the session cookie is SameSite=Lax as a second layer.
func (s *Server) withOriginCheck(next http.Handler) http.Handler {
	host := s.Cfg.BaseURL.Host
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete {
			ok := false
			if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
				ok = sfs == "same-origin" || sfs == "none"
			}
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err == nil && (strings.EqualFold(u.Host, host) || strings.EqualFold(u.Host, r.Host)) {
					ok = true
				} else {
					ok = false
				}
			} else if ref := r.Header.Get("Referer"); ref != "" && !ok {
				if u, err := url.Parse(ref); err == nil && (strings.EqualFold(u.Host, host) || strings.EqualFold(u.Host, r.Host)) {
					ok = true
				}
			}
			if !ok {
				http.Error(w, "cross-site request rejected", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func withSecurityHeaders(next http.Handler) http.Handler {
	csp := strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com https://cdnjs.cloudflare.com",
		"font-src https://fonts.gstatic.com https://cdnjs.cloudflare.com",
		"img-src 'self' https://images.igdb.com https://getutm.app data:",
		"connect-src 'self'",
		"frame-ancestors 'none'",
		"form-action 'self'",
		"base-uri 'self'",
	}, "; ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("panic", "path", r.URL.Path, "err", rec, "stack", string(debug.Stack()))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
