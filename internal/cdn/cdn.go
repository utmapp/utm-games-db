// Package cdn sets cache headers so public pages can be cached by the CDN in
// front of the server, and optionally asks that CDN to purge URLs on write.
package cdn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Public marks a response as shareable. max-age=0 keeps browsers revalidating
// (so a user who just posted sees fresh content) while s-maxage lets the CDN
// serve it for sMaxAge seconds.
func Public(w http.ResponseWriter, sMaxAge int) {
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=0, s-maxage=%d, stale-while-revalidate=86400", sMaxAge))
	w.Header().Set("Vary", "Accept-Encoding")
}

// Private marks a response as per-user and uncacheable.
func Private(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
}

// Immutable marks content-addressed static assets.
func Immutable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
}

// WriteHTML emits a buffered HTML body with a strong ETag, answering
// If-None-Match with 304 when it matches.
func WriteHTML(w http.ResponseWriter, r *http.Request, status int, body *bytes.Buffer) {
	sum := sha256.Sum256(body.Bytes())
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status == http.StatusOK && r.Method == http.MethodGet {
		for _, v := range strings.Split(r.Header.Get("If-None-Match"), ",") {
			if strings.TrimSpace(v) == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write(body.Bytes())
	}
}

// Purger invalidates URLs at the CDN. Implementations must be safe for
// concurrent use.
type Purger interface {
	Purge(urls ...string)
}

// NoopPurger does nothing (the default; the CDN TTL does the work).
type NoopPurger struct{}

// Purge implements Purger.
func (NoopPurger) Purge(...string) {}

// HTTPPurger POSTs {"files":[...]} to a configured endpoint with a bearer
// token, batching and de-duplicating URLs so bursts of writes become one
// request. The payload matches the common CDN purge-by-URL shape.
type HTTPPurger struct {
	Endpoint string
	Token    string
	BaseURL  string
	Log      *slog.Logger
	Client   *http.Client

	mu      sync.Mutex
	pending map[string]struct{}
	timer   *time.Timer
}

// NewHTTPPurger creates a purger that flushes 2 seconds after the first write.
func NewHTTPPurger(endpoint, token, baseURL string, log *slog.Logger) *HTTPPurger {
	return &HTTPPurger{Endpoint: endpoint, Token: token, BaseURL: strings.TrimRight(baseURL, "/"), Log: log, Client: &http.Client{Timeout: 15 * time.Second}, pending: map[string]struct{}{}}
}

// Purge implements Purger.
func (p *HTTPPurger) Purge(paths ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, path := range paths {
		p.pending[p.BaseURL+path] = struct{}{}
	}
	if p.timer == nil {
		p.timer = time.AfterFunc(2*time.Second, p.flush)
	}
}

func (p *HTTPPurger) flush() {
	p.mu.Lock()
	urls := make([]string, 0, len(p.pending))
	for u := range p.pending {
		urls = append(urls, u)
	}
	p.pending = map[string]struct{}{}
	p.timer = nil
	p.mu.Unlock()
	if len(urls) == 0 {
		return
	}
	for i := 0; i < len(urls); i += 30 {
		end := i + 30
		if end > len(urls) {
			end = len(urls)
		}
		body, _ := json.Marshal(map[string]any{"files": urls[i:end]})
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint, bytes.NewReader(body))
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if p.Token != "" {
			req.Header.Set("Authorization", "Bearer "+p.Token)
		}
		resp, err := p.Client.Do(req)
		if err != nil {
			p.Log.Warn("cdn purge failed", "err", err)
			cancel()
			continue
		}
		resp.Body.Close()
		cancel()
		if resp.StatusCode >= 300 {
			p.Log.Warn("cdn purge rejected", "status", resp.StatusCode)
		}
	}
}
