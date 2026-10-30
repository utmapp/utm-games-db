// Package igdb talks to the IGDB v4 API (via Twitch credentials) and keeps a
// local mirror of the PC game catalog.
package igdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	defaultAPIBase   = "https://api.igdb.com/v4"
	defaultTokenURL  = "https://id.twitch.tv/oauth2/token"
	defaultImageBase = "https://images.igdb.com/igdb/image/upload"
	pageSize         = 500
)

// TokenStore persists the OAuth token between runs (the DB sync_state table).
type TokenStore interface {
	GetToken(ctx context.Context) (token string, expiresAt time.Time, err error)
	SetToken(ctx context.Context, token string, expiresAt time.Time) error
}

// Client is a rate-limited IGDB API client.
type Client struct {
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
	APIBase      string
	TokenURL     string
	ImageBase    string
	Store        TokenStore

	limiter *rate.Limiter
	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewClient builds a client limited to 4 requests per second.
func NewClient(id, secret string, store TokenStore) *Client {
	return &Client{
		ClientID:     id,
		ClientSecret: secret,
		HTTP:         &http.Client{Timeout: 60 * time.Second},
		APIBase:      defaultAPIBase,
		TokenURL:     defaultTokenURL,
		ImageBase:    defaultImageBase,
		Store:        store,
		limiter:      rate.NewLimiter(rate.Limit(4), 4),
	}
}

// Configured reports whether credentials are present.
func (c *Client) Configured() bool { return c.ClientID != "" && c.ClientSecret != "" }

func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && time.Until(c.expires) > 48*time.Hour {
		return c.token, nil
	}
	if !force && c.Store != nil {
		tok, exp, err := c.Store.GetToken(ctx)
		if err == nil && tok != "" && time.Until(exp) > 48*time.Hour {
			c.token, c.expires = tok, exp
			return tok, nil
		}
	}
	if !c.Configured() {
		return "", errors.New("igdb: IGDB_CLIENT_ID / IGDB_CLIENT_SECRET not configured")
	}
	q := url.Values{"client_id": {c.ClientID}, "client_secret": {c.ClientSecret}, "grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("igdb: token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("igdb: token request failed: %d %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", fmt.Errorf("igdb: bad token response")
	}
	c.token = tr.AccessToken
	c.expires = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if c.Store != nil {
		if err := c.Store.SetToken(ctx, c.token, c.expires); err != nil {
			return "", err
		}
	}
	return c.token, nil
}

// Query posts an Apicalypse body to endpoint and decodes the JSON array into out.
func (c *Client) Query(ctx context.Context, endpoint, body string, out any) error {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}
		tok, err := c.accessToken(ctx, attempt > 0 && errors.Is(lastErr, errUnauthorized))
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBase+"/"+endpoint, bytes.NewBufferString(body))
		if err != nil {
			return err
		}
		req.Header.Set("Client-ID", c.ClientID)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			sleepCtx(ctx, backoff(attempt))
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			sleepCtx(ctx, backoff(attempt))
			continue
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("igdb: decode %s: %w", endpoint, err)
			}
			return nil
		case resp.StatusCode == http.StatusUnauthorized:
			lastErr = errUnauthorized
			continue
		case resp.StatusCode == http.StatusTooManyRequests:
			lastErr = fmt.Errorf("igdb: rate limited")
			wait := backoff(attempt)
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil {
					wait = time.Duration(secs) * time.Second
				}
			}
			sleepCtx(ctx, wait)
			continue
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("igdb: %s: status %d", endpoint, resp.StatusCode)
			sleepCtx(ctx, backoff(attempt))
			continue
		default:
			return fmt.Errorf("igdb: %s: status %d: %s", endpoint, resp.StatusCode, bytes.TrimSpace(data))
		}
	}
	return fmt.Errorf("igdb: giving up after retries: %w", lastErr)
}

var errUnauthorized = errors.New("igdb: unauthorized")

func backoff(attempt int) time.Duration {
	d := time.Second << uint(attempt)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// ImageURL builds an image URL for an IGDB image id and size (e.g. cover_big_2x).
func (c *Client) ImageURL(imageID, size string) string {
	return c.ImageBase + "/t_" + size + "/" + imageID + ".jpg"
}

// Download fetches an image into w (used for covers).
func (c *Client) Download(ctx context.Context, rawURL string, w io.Writer, maxBytes int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", rawURL, resp.StatusCode)
	}
	_, err = io.Copy(w, io.LimitReader(resp.Body, maxBytes))
	return err
}
