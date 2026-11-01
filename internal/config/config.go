// Package config loads the server configuration from environment variables
// (optionally seeded from a .env file in the working directory).
package config

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GiftAccount is a storefront account that users can gift games to so the
// Auto-Triage agent can test them.
type GiftAccount struct {
	Storefront string `json:"storefront"` // steam, gog, epic, itch, other
	Account    string `json:"account"`    // account name on the storefront (optional)
	URL        string `json:"url"`        // profile / wishlist URL
	Email      string `json:"email"`      // address gifts can be sent to (storefronts that gift by email)
	// Instructions overrides the built-in gifting steps shown for this
	// storefront on the triage request page (plain text).
	Instructions string `json:"instructions,omitempty"`
}

// Config holds every tunable of the server. Fields documented in .env.example.
type Config struct {
	BaseURL                  *url.URL
	ListenAddr               string
	TLSCert                  string // PEM certificate chain; with TLSKey, the public listener serves HTTPS
	TLSKey                   string
	InternalListenAddr       string
	InternalAllowNonLoopback bool
	DataDir                  string

	IGDBClientID     string
	IGDBClientSecret string
	IGDBSyncInterval time.Duration
	IGDBPlatformIDs  []int
	IGDBSyncOnStart  bool

	// SecretKey derives the email-identity pepper, sign-in form signing key
	// and rate-limit keys. Losing it orphans every account.
	SecretKey []byte
	// DevSecret is set when SecretKey fell back to the built-in dev value.
	DevSecret bool

	// Sign-in hardening.
	LoginPoWBits         int
	LoginFormMinAge      time.Duration
	LoginFormMaxAge      time.Duration
	LoginWorkers         int
	LoginQueueSize       int
	LoginGlobalPerMinute int
	LoginGlobalPerHour   int
	LoginMXCheck         bool

	// Account data export.
	ExportInterval         time.Duration
	ExportMinAttachmentAge time.Duration

	// DevReload serves templates and static files from the source tree on
	// every request instead of the embedded copies (development only).
	DevReload bool

	MailMode string // "smtp", "direct" or "log"
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	MailFrom string // From header in every mode; in direct mode its domain is the SPF/DKIM/DMARC domain
	// Direct delivery (MAIL_MODE=direct).
	MailHELO         string        // hostname announced to receiving servers; A record + PTR of the sending IP must match
	MailDKIMSelector string        // <selector>._domainkey.<from domain>
	MailDKIMKey      string        // PEM private key path
	MailRetryFor     time.Duration // how long temporary refusals are retried
	SMTPTLS          string        // "starttls", "tls", "none"

	AkismetKey        string
	AkismetFailClosed bool

	TrustedProxyCIDRs  []*net.IPNet
	TriageGiftAccounts []GiftAccount

	CDNPurgeURL   string
	CDNPurgeToken string

	// Public limits (shown on the form).
	UploadMaxFileBytes   int64
	UploadMaxFiles       int
	UploadMaxReportBytes int64
	UploadRetentionDays  int

	// Private quotas (never rendered).
	UploadQuotaDailyBytes int64
	UploadQuotaDailyFiles int
	UploadQuotaTotalBytes int64
	RateReportsPerHour    int
	RateCommentsPerHour   int
	RatePlusOnesPerHour   int
	RateSearchPerMinute   int
	RateLoginPerHourIP    int
	RateLoginPerHourEmail int
	RatePreviewPerMinute  int
	// RateContentInterval is the minimum gap between two content submissions
	// (reports, edits, comments, requests) by one user; 0 disables.
	RateContentInterval time.Duration

	SessionTTL   time.Duration
	LoginCodeTTL time.Duration
	CookieSecure bool
}

// Load reads .env (if present, without overriding existing variables) and then
// the environment.
func Load() (*Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return nil, err
	}
	var errs []error
	get := func(key, def string) string {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			return v
		}
		return def
	}
	getInt := func(key string, def int) int {
		v := get(key, "")
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return def
		}
		return n
	}
	getInt64 := func(key string, def int64) int64 {
		v := get(key, "")
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return def
		}
		return n
	}
	getBool := func(key string, def bool) bool {
		v := strings.ToLower(get(key, ""))
		switch v {
		case "":
			return def
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
		errs = append(errs, fmt.Errorf("%s: invalid boolean %q", key, v))
		return def
	}
	getDur := func(key string, def time.Duration) time.Duration {
		v := get(key, "")
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return def
		}
		return d
	}

	c := &Config{}
	rawBase := get("BASE_URL", "http://localhost:8080")
	u, err := url.Parse(rawBase)
	if err != nil || u.Scheme == "" || u.Host == "" {
		errs = append(errs, fmt.Errorf("BASE_URL: invalid %q", rawBase))
	} else {
		u.Path = strings.TrimRight(u.Path, "/")
		c.BaseURL = u
	}
	c.ListenAddr = get("LISTEN_ADDR", ":8080")
	c.TLSCert = get("TLS_CERT", "")
	c.TLSKey = get("TLS_KEY", "")
	if (c.TLSCert == "") != (c.TLSKey == "") {
		errs = append(errs, errors.New("TLS_CERT and TLS_KEY must be set together"))
	}
	c.InternalListenAddr = get("INTERNAL_LISTEN_ADDR", "127.0.0.1:8081")
	c.InternalAllowNonLoopback = getBool("INTERNAL_ALLOW_NONLOOPBACK", false)
	c.DataDir = get("DATA_DIR", "./data")
	c.DevReload = getBool("DEV_RELOAD", false)

	c.IGDBClientID = get("IGDB_CLIENT_ID", "")
	c.IGDBClientSecret = get("IGDB_CLIENT_SECRET", "")
	c.IGDBSyncInterval = getDur("IGDB_SYNC_INTERVAL", 24*time.Hour)
	c.IGDBSyncOnStart = getBool("IGDB_SYNC_ON_START", false)
	for _, p := range strings.Split(get("IGDB_PLATFORM_IDS", "6"), ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("IGDB_PLATFORM_IDS: %w", err))
			continue
		}
		c.IGDBPlatformIDs = append(c.IGDBPlatformIDs, n)
	}

	c.MailMode = strings.ToLower(get("MAIL_MODE", "log"))
	if c.MailMode != "smtp" && c.MailMode != "direct" && c.MailMode != "log" {
		errs = append(errs, fmt.Errorf("MAIL_MODE must be smtp, direct or log"))
	}
	c.MailFrom = get("MAIL_FROM", "UTM Games Database <no-reply@getutm.app>")
	if _, err := mail.ParseAddress(c.MailFrom); err != nil {
		errs = append(errs, fmt.Errorf("MAIL_FROM: %w", err))
	}
	c.SMTPHost = get("SMTP_HOST", "")
	c.SMTPPort = getInt("SMTP_PORT", 587)
	c.SMTPUser = get("SMTP_USER", "")
	c.SMTPPass = get("SMTP_PASS", "")
	c.SMTPTLS = strings.ToLower(get("SMTP_TLS", "starttls"))
	if c.MailMode == "smtp" && c.SMTPHost == "" {
		errs = append(errs, errors.New("SMTP_HOST is required when MAIL_MODE=smtp"))
	}
	c.MailHELO = get("MAIL_HELO", "")
	c.MailDKIMSelector = get("MAIL_DKIM_SELECTOR", "utmdb")
	c.MailDKIMKey = get("MAIL_DKIM_KEY", "")
	c.MailRetryFor = getDur("MAIL_RETRY_FOR", 8*time.Minute)
	if c.MailDKIMKey == "" {
		c.MailDKIMKey = filepath.Join(c.DataDir, "dkim", c.MailDKIMSelector+".key")
	}
	if c.MailMode == "direct" && c.MailHELO == "" {
		errs = append(errs, errors.New("MAIL_HELO is required when MAIL_MODE=direct (a DNS-only hostname of the sending address)"))
	}

	c.AkismetKey = get("AKISMET_KEY", "")
	c.AkismetFailClosed = getBool("AKISMET_FAIL_CLOSED", false)

	for _, cidr := range strings.Split(get("TRUSTED_PROXY_CIDRS", ""), ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			errs = append(errs, fmt.Errorf("TRUSTED_PROXY_CIDRS: %w", err))
			continue
		}
		c.TrustedProxyCIDRs = append(c.TrustedProxyCIDRs, n)
	}
	if raw := get("TRIAGE_GIFT_ACCOUNTS", ""); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.TriageGiftAccounts); err != nil {
			errs = append(errs, fmt.Errorf("TRIAGE_GIFT_ACCOUNTS: %w", err))
		}
	}
	c.CDNPurgeURL = get("CDN_PURGE_URL", "")
	c.CDNPurgeToken = get("CDN_PURGE_TOKEN", "")

	const mb = 1 << 20
	c.UploadMaxFileBytes = getInt64("UPLOAD_MAX_FILE_BYTES", 25*mb)
	c.UploadMaxFiles = getInt("UPLOAD_MAX_FILES", 5)
	c.UploadMaxReportBytes = getInt64("UPLOAD_MAX_REPORT_BYTES", 100*mb)
	c.UploadRetentionDays = getInt("ATTACHMENT_RETENTION_DAYS", 30)

	c.UploadQuotaDailyBytes = getInt64("UPLOAD_QUOTA_DAILY_BYTES", 200*mb)
	c.UploadQuotaDailyFiles = getInt("UPLOAD_QUOTA_DAILY_FILES", 20)
	c.UploadQuotaTotalBytes = getInt64("UPLOAD_QUOTA_TOTAL_BYTES", 2048*mb)
	c.RateReportsPerHour = getInt("RATE_REPORTS_PER_HOUR", 5)
	c.RateCommentsPerHour = getInt("RATE_COMMENTS_PER_HOUR", 30)
	c.RatePlusOnesPerHour = getInt("RATE_PLUSONES_PER_HOUR", 60)
	c.RateSearchPerMinute = getInt("RATE_SEARCH_PER_MINUTE", 60)
	c.RateLoginPerHourIP = getInt("RATE_LOGIN_PER_HOUR_IP", 10)
	c.RateLoginPerHourEmail = getInt("RATE_LOGIN_PER_HOUR_EMAIL", 3)
	c.RatePreviewPerMinute = getInt("RATE_PREVIEW_PER_MINUTE", 60)
	c.RateContentInterval = getDur("RATE_CONTENT_INTERVAL", 15*time.Second)

	c.SessionTTL = getDur("SESSION_TTL", 30*24*time.Hour)
	c.LoginCodeTTL = getDur("LOGIN_CODE_TTL", 10*time.Minute)
	c.CookieSecure = getBool("COOKIE_SECURE", c.BaseURL != nil && c.BaseURL.Scheme == "https")

	switch raw := strings.TrimSpace(get("SECRET_KEY", "")); {
	case raw == "" && c.BaseURL != nil && c.BaseURL.Scheme == "https":
		errs = append(errs, errors.New("SECRET_KEY is required (generate one with: openssl rand -hex 32)"))
	case raw == "":
		c.SecretKey = []byte("insecure-development-secret-key-do-not-use-in-production")
		c.DevSecret = true
	default:
		if b, err := hex.DecodeString(raw); err == nil && len(b) >= 16 {
			c.SecretKey = b
		} else if len(raw) >= 32 {
			c.SecretKey = []byte(raw)
		} else {
			errs = append(errs, errors.New("SECRET_KEY must be at least 32 hex characters (openssl rand -hex 32)"))
		}
	}
	c.LoginPoWBits = getInt("LOGIN_POW_BITS", 16)
	c.LoginFormMinAge = getDur("LOGIN_FORM_MIN_AGE", 2*time.Second)
	c.LoginFormMaxAge = getDur("LOGIN_FORM_MAX_AGE", time.Hour)
	c.LoginWorkers = getInt("LOGIN_WORKERS", 2)
	c.LoginQueueSize = getInt("LOGIN_QUEUE_SIZE", 256)
	c.LoginGlobalPerMinute = getInt("LOGIN_GLOBAL_PER_MINUTE", 30)
	c.LoginGlobalPerHour = getInt("LOGIN_GLOBAL_PER_HOUR", 300)
	c.LoginMXCheck = getBool("LOGIN_MX_CHECK", c.MailMode != "log")
	c.ExportInterval = getDur("EXPORT_INTERVAL", 24*time.Hour)
	c.ExportMinAttachmentAge = getDur("EXPORT_MIN_ATTACHMENT_AGE", 48*time.Hour)

	if !c.InternalAllowNonLoopback {
		host, _, err := net.SplitHostPort(c.InternalListenAddr)
		if err != nil {
			errs = append(errs, fmt.Errorf("INTERNAL_LISTEN_ADDR: %w", err))
		} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			errs = append(errs, fmt.Errorf("INTERNAL_LISTEN_ADDR %q must bind a loopback address (set INTERNAL_ALLOW_NONLOOPBACK=1 to override)", c.InternalListenAddr))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// IsTrustedProxy reports whether ip belongs to a configured proxy range.
func (c *Config) IsTrustedProxy(ip net.IP) bool {
	for _, n := range c.TrustedProxyCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// AbsURL joins a site-relative path onto BASE_URL.
func (c *Config) AbsURL(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.BaseURL.String() + path
}

// loadDotEnv reads KEY=VALUE lines. Existing environment variables win.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}
