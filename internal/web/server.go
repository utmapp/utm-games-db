// Package web serves the public site.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/utmapp/utm-games-db/internal/antispam"
	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/cdn"
	"github.com/utmapp/utm-games-db/internal/config"
	"github.com/utmapp/utm-games-db/internal/igdb"
	"github.com/utmapp/utm-games-db/internal/mail"
	"github.com/utmapp/utm-games-db/internal/markdown"
	"github.com/utmapp/utm-games-db/internal/status"
	"github.com/utmapp/utm-games-db/internal/store"
	"github.com/utmapp/utm-games-db/internal/upload"
	"os"
	"path/filepath"
	"runtime"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Server holds the dependencies of the public site.
type Server struct {
	Cfg     *config.Config
	Store   *store.Store
	Auth    *auth.Service
	Akismet *antispam.Akismet
	Limiter *antispam.Limiter
	Syncer  *igdb.Syncer
	Uploads upload.Store
	Purger  cdn.Purger
	Log     *slog.Logger

	Login      *auth.LoginQueue // the one pre-login path that spends resources
	usedTokens *auth.UsedTokens

	untrustedProxy sync.Once // warns once about forwarded IPs from an untrusted peer

	tmpl         map[string]*template.Template
	templates    fs.FS
	assetVersion string
	static       fs.FS
	perPage      int
	devReload    bool
}

// devSourceDir is the directory holding this package's templates/ and
// static/ folders, used by DEV_RELOAD. It is derived from the build path, so
// it only exists on a machine that has the source tree.
func devSourceDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("cannot locate source directory")
	}
	dir := filepath.Dir(file)
	if st, err := os.Stat(filepath.Join(dir, "templates")); err != nil || !st.IsDir() {
		return "", fmt.Errorf("DEV_RELOAD: no templates directory at %s", dir)
	}
	return dir, nil
}

// New wires the server and parses templates.
func New(cfg *config.Config, st *store.Store, au *auth.Service, mailer mail.Mailer, ak *antispam.Akismet, lim *antispam.Limiter, sy *igdb.Syncer, up upload.Store, purger cdn.Purger, log *slog.Logger) (*Server, error) {
	s := &Server{Cfg: cfg, Store: st, Auth: au, Akismet: ak, Limiter: lim, Syncer: sy, Uploads: up, Purger: purger, Log: log, perPage: 24}
	if cfg.DevReload {
		dir, err := devSourceDir()
		if err != nil {
			return nil, err
		}
		s.devReload = true
		s.templates = os.DirFS(filepath.Join(dir, "templates"))
		s.static = os.DirFS(filepath.Join(dir, "static"))
		s.assetVersion = "dev"
		log.Warn("DEV_RELOAD is on: templates and static files are read from disk on every request; never use this in production", "dir", dir)
	} else {
		sub, err := fs.Sub(staticFS, "static")
		if err != nil {
			return nil, err
		}
		s.static = sub
		s.assetVersion, err = hashFS(sub)
		if err != nil {
			return nil, err
		}
		s.templates = templateFS
	}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	s.usedTokens = auth.NewUsedTokens(cfg.LoginFormMaxAge)
	s.Login = auth.NewLoginQueue(cfg.LoginQueueSize, cfg.LoginWorkers)
	s.Login.Service = au
	s.Login.Mailer = mailer
	s.Login.Log = log
	s.Login.CodeTTL = cfg.LoginCodeTTL
	s.Login.MXCheck = cfg.LoginMXCheck
	s.Login.Start()
	return s, nil
}

// Close stops background work (the sign-in queue).
func (s *Server) Close() {
	if s.Login != nil {
		s.Login.Stop()
	}
}

func hashFS(fsys fs.FS) (string, error) {
	h := sha256.New()
	var names []string
	fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	sort.Strings(names)
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return "", err
		}
		h.Write([]byte(n))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:10], nil
}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"asset":       func(p string) string { return "/static/" + s.assetVersion + "/" + p },
		"reportPath":  store.ReportPath,
		"year":        func() int { return time.Now().Year() },
		"ratingLabel": func(r any) string { return toRating(r).Label() },
		"ratingColor": func(r any) string { return toRating(r).Color() },
		"ratingHelp": func(r any) string {
			switch toRating(r) {
			case status.DoesNotLaunch:
				return "The game never reaches a playable state."
			case status.InGameIssues:
				return "It runs, but with crashes, glitches or performance problems."
			case status.Playable:
				return "Runs well enough to enjoy, maybe with minor issues."
			case status.Gold:
				return "You played the whole game to completion without issues."
			default:
				return "You could not tell, or did not get far enough to judge."
			}
		},
		"issueLabel":  store.IssueLabel,
		"triageLabel": store.TriageLabel,
		"triageColor": store.TriageColor,
		"wineLabel":   store.WineLabel,
		"wineShort": func(v string) string {
			switch v {
			case "yes":
				return "Yes, same issue"
			case "no":
				return "No, works there"
			default:
				return "Not tested"
			}
		},
		"osLabel": func(v string) string {
			switch v {
			case "windows":
				return "Windows"
			case "linux":
				return "Linux"
			default:
				return "Other"
			}
		},
		"gfxLabel": func(v string) string {
			if l := store.GraphicsLabel(v); l != "" {
				return l
			}
			return "Unknown"
		},
		"storeIcon": func(v string) template.HTML {
			switch v {
			case "steam":
				return `<i class="fab fa-steam"></i>`
			case "itch":
				return `<i class="fab fa-itch-io"></i>`
			default:
				return `<i class="fas fa-store"></i>`
			}
		},
		"md":         markdown.Render,
		"codeLength": func() int { return auth.CodeLength },
		"mdBot":      markdown.RenderTrusted,
		"date":       func(t time.Time) string { return t.Format("Jan 2, 2006") },
		"isodate":    func(t time.Time) string { return t.Format(time.RFC3339) },
		"filesize": func(n int64) string {
			switch {
			case n >= 1<<30:
				return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
			case n >= 1<<20:
				return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
			case n >= 1<<10:
				return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
			}
			return fmt.Sprintf("%d B", n)
		},
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return one
			}
			return many
		},
		"join": strings.Join,
		"list": func(v ...string) []string { return v },
		"seq": func(a, b int) []int {
			var out []int
			for i := a; i <= b; i++ {
				out = append(out, i)
			}
			return out
		},
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"abs": func(a int) int {
			if a < 0 {
				return -a
			}
			return a
		},
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, errors.New("dict: odd number of arguments")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, errors.New("dict: keys must be strings")
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
		"pageURL": func(p *indexPage, n int) string { return p.pageURL(n) },
	}
}

func toRating(r any) status.Rating {
	switch v := r.(type) {
	case status.Rating:
		return v
	case string:
		return status.Rating(v)
	}
	return status.Unknown
}

var pageTemplates = []string{"index", "game", "report", "report_form", "auth", "account", "donate", "about", "triage", "privacy", "error"}

func (s *Server) parseTemplates() error {
	s.tmpl = map[string]*template.Template{}
	for _, p := range pageTemplates {
		t, err := s.parseTemplate(p)
		if err != nil {
			return err
		}
		s.tmpl[p] = t
	}
	return nil
}

// parseTemplate parses one page together with the layout. The embedded FS
// keeps the "templates/" prefix; the DEV_RELOAD directory FS does not.
func (s *Server) parseTemplate(name string) (*template.Template, error) {
	prefix := "templates/"
	if s.devReload {
		prefix = ""
	}
	t, err := template.New("layout").Funcs(s.funcs()).ParseFS(s.templates, prefix+"layout.html", prefix+name+".html")
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	return t, nil
}

// template returns the parsed page, re-reading it from disk under DEV_RELOAD.
func (s *Server) template(name string) (*template.Template, error) {
	if s.devReload {
		return s.parseTemplate(name)
	}
	if t := s.tmpl[name]; t != nil {
		return t, nil
	}
	return nil, fmt.Errorf("missing template %s", name)
}

// Page is the data every template receives (embedded by page structs).
type Page struct {
	Title       string
	Description string
	Canonical   string
	BodyClass   string
	NoIndex     bool
	User        *store.User // only on private pages
	Flash       string
	Error       string
	ErrorList   []string
}

func (p *Page) base() *Page { return p }

type pageData interface{ base() *Page }

// render executes a page template into a buffer and writes it with an ETag.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, code int, data pageData) {
	t, err := s.template(name)
	if err != nil {
		s.Log.Error("template", "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		s.Log.Error("render", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	cdn.WriteHTML(w, r, code, &buf)
}

// public renders a cacheable anonymous page.
func (s *Server) public(w http.ResponseWriter, r *http.Request, name string, sMaxAge int, data pageData) {
	data.base().User = nil
	cdn.Public(w, sMaxAge)
	s.render(w, r, name, http.StatusOK, data)
}

// private renders a per-user page.
func (s *Server) private(w http.ResponseWriter, r *http.Request, name string, code int, data pageData) {
	data.base().User = auth.UserFrom(r.Context())
	data.base().NoIndex = true
	cdn.Private(w)
	s.render(w, r, name, code, data)
}

type errorPage struct {
	Page
	Message string
}

// errorPage renders an error page. The CDN caches by URL alone, so only a
// 404 that cannot depend on who is asking is shared: an anonymous GET
// (sign-in-only routes redirect anonymous visitors before any handler runs).
func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, code int, title, msg string) {
	s.renderError(w, r, code, title, msg, code == http.StatusNotFound && r.Method == http.MethodGet && auth.UserFrom(r.Context()) == nil)
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, code int, title, msg string, shared bool) {
	cdn.Private(w)
	if shared {
		cdn.Public(w, 60)
	}
	s.render(w, r, "error", code, &errorPage{Page: Page{Title: title, NoIndex: true}, Message: msg})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.errorPage(w, r, http.StatusNotFound, "Not found", "That page does not exist.")
}

// privateNotFound is a 404 for a URL that exists for someone else (a hidden
// report for its author), so no visitor's answer may be cached for others.
func (s *Server) privateNotFound(w http.ResponseWriter, r *http.Request) {
	s.renderError(w, r, http.StatusNotFound, "Not found", "That page does not exist.", false)
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("request failed", "path", r.URL.Path, "err", err)
	s.errorPage(w, r, http.StatusInternalServerError, "Something went wrong", "Please try again in a moment.")
}

// writeJSON emits an uncacheable JSON response.
func writeJSON(w http.ResponseWriter, code int, v any) {
	cdn.Private(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// wantsJSON reports whether the client asked for JSON (fetch from me.js).
func wantsJSON(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "fetch" || strings.HasPrefix(r.Header.Get("Accept"), "application/json")
}

// clientIP trusts CF-Connecting-IP only from configured proxy ranges.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && s.Cfg.IsTrustedProxy(ip) {
		// The value keys rate limits and is stored with submissions, so
		// anything that is not an IP address is ignored.
		if v := net.ParseIP(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); v != nil {
			return v.String()
		}
		first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
		if v := net.ParseIP(strings.TrimSpace(first)); v != nil {
			return v.String()
		}
	} else if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) && (r.Header.Get("CF-Connecting-IP") != "" || r.Header.Get("X-Forwarded-For") != "") {
		// A local reverse proxy (or Docker's port forwarding) is in front but
		// not trusted: every visitor gets this one address for rate limits.
		s.untrustedProxy.Do(func() {
			if s.Log != nil {
				s.Log.Warn("a local proxy forwards client IPs but is not in TRUSTED_PROXY_CIDRS, so every visitor shares its address for rate limits and Akismet; add the address it connects from", "peer", host)
			}
		})
	}
	return host
}

// safeNext only allows site-relative redirect targets. Control characters
// are rejected outright: browsers strip tabs and newlines from URLs, so
// "/\t/evil.example" would otherwise become the off-site "//evil.example".
func safeNext(v string) string {
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.ContainsFunc(v, func(c rune) bool { return c == '\\' || c < 0x20 || c == 0x7f }) {
		return "/"
	}
	return path.Clean(v)
}
