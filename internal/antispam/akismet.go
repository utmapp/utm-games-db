package antispam

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Akismet is a minimal client for the Akismet REST API.
type Akismet struct {
	Key     string
	Blog    string // site URL
	Client  *http.Client
	BaseURL string // overridable for tests
}

// Comment is the content submitted for checking.
type Comment struct {
	UserIP    string
	UserAgent string
	Referrer  string
	Type      string // "comment", "forum-post", ...
	Author    string
	Content   string
	Permalink string
}

// Verdict is Akismet's answer.
type Verdict struct {
	Spam    bool
	Discard bool // X-akismet-pro-tip: discard (blatant spam)
}

// ErrNotConfigured is returned when no key is set.
var ErrNotConfigured = errors.New("akismet: not configured")

// JoinFields concatenates the non-empty free-text fields of a submission in
// key order, the content Akismet checks and later learns from.
func JoinFields(fields map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		if fields[k] != "" {
			b.WriteString(fields[k])
			b.WriteString("\n\n")
		}
	}
	return b.String()
}

// Enabled reports whether a key is configured.
func (a *Akismet) Enabled() bool { return a != nil && a.Key != "" }

func (a *Akismet) base() string {
	if a.BaseURL != "" {
		return a.BaseURL
	}
	return "https://rest.akismet.com"
}

func (a *Akismet) form(c Comment) url.Values {
	v := url.Values{}
	v.Set("api_key", a.Key)
	v.Set("blog", a.Blog)
	v.Set("user_ip", c.UserIP)
	v.Set("user_agent", c.UserAgent)
	if c.Referrer != "" {
		v.Set("referrer", c.Referrer)
	}
	v.Set("comment_type", c.Type)
	v.Set("comment_author", c.Author)
	v.Set("comment_content", c.Content)
	if c.Permalink != "" {
		v.Set("permalink", c.Permalink)
	}
	v.Set("blog_charset", "UTF-8")
	return v
}

func (a *Akismet) post(ctx context.Context, path string, form url.Values) (string, http.Header, error) {
	if !a.Enabled() {
		return "", nil, ErrNotConfigured
	}
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+path, strings.NewReader(form.Encode()))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "utm-games-db/1.0 | Akismet/1.1")
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", resp.Header, fmt.Errorf("akismet: status %d", resp.StatusCode)
	}
	return strings.TrimSpace(string(body)), resp.Header, nil
}

// Check asks Akismet whether the comment is spam.
func (a *Akismet) Check(ctx context.Context, c Comment) (Verdict, error) {
	body, hdr, err := a.post(ctx, "/1.1/comment-check", a.form(c))
	if err != nil {
		return Verdict{}, err
	}
	switch body {
	case "true":
		return Verdict{Spam: true, Discard: strings.EqualFold(hdr.Get("X-akismet-pro-tip"), "discard")}, nil
	case "false":
		return Verdict{}, nil
	}
	if help := hdr.Get("X-akismet-debug-help"); help != "" {
		return Verdict{}, fmt.Errorf("akismet: %s", help)
	}
	return Verdict{}, fmt.Errorf("akismet: unexpected response %q", body)
}

// SubmitSpam reports a missed spam to Akismet.
func (a *Akismet) SubmitSpam(ctx context.Context, c Comment) error {
	_, _, err := a.post(ctx, "/1.1/submit-spam", a.form(c))
	return err
}

// SubmitHam reports a false positive to Akismet.
func (a *Akismet) SubmitHam(ctx context.Context, c Comment) error {
	_, _, err := a.post(ctx, "/1.1/submit-ham", a.form(c))
	return err
}
