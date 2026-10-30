package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/cdn"
	"github.com/utmapp/utm-games-db/internal/store"
)

type authPage struct {
	Page
	Mode       string // login | code | error
	Email      string
	Next       string
	TTLMinutes int
	FormToken  string
	PoWBits    int
	MinAgeMs   int64
}

func (s *Server) loginPage(mode, next string) *authPage {
	return &authPage{Page: Page{Title: "Sign in"}, Mode: mode, Next: next,
		FormToken: s.Auth.Keys.FormToken(time.Now()), PoWBits: s.Cfg.LoginPoWBits, MinAgeMs: s.Cfg.LoginFormMinAge.Milliseconds()}
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if auth.UserFrom(r.Context()) != nil {
		cdn.Private(w)
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.private(w, r, "auth", http.StatusOK, s.loginPage("login", safeNext(r.URL.Query().Get("next"))))
}

// handleLoginSubmit is the only anonymous endpoint that can cost the server
// anything, so it is defended in layers. Everything here is cheap and
// synchronous: honeypot, address syntax, a signed single-use form token
// with an age window, the proof of work bound to that token, per-IP and
// per-address limits, then the global send budget. What passes is queued;
// the expensive work (Argon2, DNS, SMTP) happens in bounded workers, and the
// response is the same "enter the code" page, with the same cookie, whether
// or not anything will be sent, so the endpoint cannot be used to find out
// which addresses exist.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "Bad request", "Could not read the form.")
		return
	}
	next := safeNext(r.PostFormValue("next"))
	page := s.loginPage("login", next)
	page.Email = strings.TrimSpace(r.PostFormValue("email"))
	// The secret ties the code to this browser; the cookie is set on every
	// success-looking answer so its presence reveals nothing about what
	// happened.
	secret := auth.NewRequestSecret()
	sent := func() {
		s.Auth.SetLoginCookie(w, secret)
		s.codePage(w, r, http.StatusOK, page)
	}
	fail := func(code int, msg string) {
		page.Error = msg
		s.private(w, r, "auth", code, page)
	}
	if r.PostFormValue("website") != "" { // honeypot
		sent()
		return
	}
	email, err := auth.NormalizeEmail(page.Email)
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now()
	tok := r.PostFormValue("form_token")
	if _, err := s.Auth.Keys.VerifyFormToken(tok, now, s.Cfg.LoginFormMinAge, s.Cfg.LoginFormMaxAge); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if !auth.VerifyProofOfWork(tok, r.PostFormValue("pow_nonce"), s.Cfg.LoginPoWBits) {
		fail(http.StatusBadRequest, auth.ErrProofOfWork.Error())
		return
	}
	if !s.usedTokens.Consume(tok) {
		fail(http.StatusBadRequest, auth.ErrFormToken.Error())
		return
	}
	ip := s.clientIP(r)
	if !s.Limiter.Allow("login:ip:"+ip, s.Cfg.RateLoginPerHourIP, time.Hour) ||
		!s.Limiter.Allow("login:addr:"+s.Auth.Keys.LimitKey(email), s.Cfg.RateLoginPerHourEmail, time.Hour) {
		fail(http.StatusTooManyRequests, "Too many sign-in requests. Please wait a while and try again.")
		return
	}
	// Site-wide budget: beyond it nothing is sent, but the answer does not
	// change, so a flood learns nothing and costs only this check.
	if !s.Limiter.Allow("login:global:minute", s.Cfg.LoginGlobalPerMinute, time.Minute) ||
		!s.Limiter.Allow("login:global:hour", s.Cfg.LoginGlobalPerHour, time.Hour) {
		s.Log.Warn("sign-in budget exceeded; request dropped")
		sent()
		return
	}
	s.Login.Enqueue(auth.LoginRequest{Email: email, IP: ip, Secret: secret})
	sent()
}

// codePage renders the "enter the code from your email" step.
func (s *Server) codePage(w http.ResponseWriter, r *http.Request, code int, page *authPage) {
	page.Mode = "code"
	page.TTLMinutes = int(s.Cfg.LoginCodeTTL.Minutes())
	s.private(w, r, "auth", code, page)
}

// handleLoginCode checks the typed code against the request bound to this
// browser's cookie. Verification is cheap (a hash compare); the attempt cap
// on the code itself bounds guessing, and a per-IP limit keeps a guessing
// script from filling the log.
func (s *Server) handleLoginCode(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "Bad request", "Could not read the form.")
		return
	}
	next := safeNext(r.PostFormValue("next"))
	page := s.loginPage("code", next)
	page.Email = strings.TrimSpace(r.PostFormValue("email"))
	secret := auth.LoginSecretFromRequest(r)
	if secret == "" {
		// No request cookie (never set here, or expired with the code): back
		// to the email form rather than a code field nothing can satisfy.
		page.Mode = "login"
		page.Error = "This browser has no pending sign-in. Enter your email to request a code."
		s.private(w, r, "auth", http.StatusBadRequest, page)
		return
	}
	if !s.Limiter.Allow("login:code:"+s.clientIP(r), 30, time.Minute) {
		page.Error = "Too many attempts. Please wait a minute and try again."
		s.codePage(w, r, http.StatusTooManyRequests, page)
		return
	}
	code := strings.Join(strings.Fields(r.PostFormValue("code")), "")
	u, sess, err := s.Auth.VerifyCode(r.Context(), secret, code)
	switch {
	case errors.Is(err, auth.ErrCodeInvalid):
		page.Error = "That code is not right, or it has expired. Check the email and try again, or request a new code."
		s.codePage(w, r, http.StatusBadRequest, page)
		return
	case errors.Is(err, auth.ErrBanned):
		s.Auth.ClearLoginCookie(w)
		s.private(w, r, "auth", http.StatusForbidden, &authPage{Page: Page{Title: "Sign in", Error: "This account has been suspended."}, Mode: "error"})
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.Auth.ClearLoginCookie(w)
	s.Auth.SetCookie(w, sess)
	cdn.Private(w)
	s.Log.Info("user signed in", "user", u.ID)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.Logout(r.Context(), auth.TokenFromRequest(r)); err != nil {
		s.Log.Warn("logout", "err", err)
	}
	s.Auth.ClearCookie(w)
	cdn.Private(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type accountPage struct {
	Page
	Stars           []store.GameCard
	Reports         []store.Report
	Attachments     int
	ExportNext      *time.Time // when the next export is allowed, if limited
	ExportMinAgeHrs int
}

func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, code int, errMsg string) {
	u := auth.UserFrom(r.Context())
	stars, err := s.Store.UserStarredGames(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	reports, err := s.Store.UserReports(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	atts, err := s.Store.UserAttachments(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	flash := ""
	switch {
	case r.URL.Query().Get("deleted") == "1":
		flash = "Your report has been deleted."
	case r.URL.Query().Get("deletion_requested") == "1":
		flash = "Your deletion request has been sent. A maintainer will review it."
	case r.URL.Query().Get("attachments_deleted") == "1":
		flash = "All your attachments have been deleted."
	}
	page := &accountPage{Page: Page{Title: "Account", Error: errMsg, Flash: flash}, Stars: stars, Reports: reports, Attachments: len(atts), ExportMinAgeHrs: int(s.Cfg.ExportMinAttachmentAge.Hours())}
	if u.LastExportAt != nil {
		if next := u.LastExportAt.Add(s.Cfg.ExportInterval); time.Now().Before(next) {
			page.ExportNext = &next
		}
	}
	s.private(w, r, "account", code, page)
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	s.renderAccount(w, r, http.StatusOK, "")
}

func (s *Server) handleAccountSave(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderAccount(w, r, http.StatusBadRequest, "Could not read the form.")
		return
	}
	if err := s.Store.SetDisplayName(r.Context(), u.ID, r.PostFormValue("display_name")); err != nil {
		s.renderAccount(w, r, http.StatusBadRequest, err.Error())
		return
	}
	// Display names appear on cached pages; let the CDN refresh them lazily
	// (TTL) rather than purging every page the user ever touched.
	cdn.Private(w)
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}
