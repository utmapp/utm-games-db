package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	netmail "net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	gomail "github.com/wneessen/go-mail"
)

// DirectMailer delivers mail itself: it looks up the recipient domain's MX
// hosts and hands the DKIM-signed message to the first one that accepts it,
// over STARTTLS when the host offers it. Temporary refusals (greylisting is
// the usual one for an address a receiver has not seen before) are retried
// in the background for RetryFor.
type DirectMailer struct {
	From     string            // "Name <local@domain>"; the DKIM domain must match
	HELO     string            // hostname announced in EHLO; its A record and the sending IP's PTR must agree
	Port     int               // 25 unless a test says otherwise
	DKIM     *dkim.SignOptions // nil sends unsigned (tests only)
	RetryFor time.Duration     // keep below the sign-in code TTL; 0 = fail on the first temporary refusal (mail-test)
	Log      *slog.Logger
	// LookupMX returns the hosts to try in preference order; nil uses DNS.
	LookupMX func(ctx context.Context, domain string) ([]string, error)

	DialTimeout time.Duration
	retries     chan struct{} // bounds concurrent background retries
	once        sync.Once
}

// ErrNoMailServer means the domain publishes no way to receive mail.
var ErrNoMailServer = errors.New("domain does not accept mail")

type tempError struct{ err error }

func (e tempError) Error() string { return e.err.Error() }
func (e tempError) Unwrap() error { return e.err }

func (m *DirectMailer) init() {
	m.once.Do(func() {
		m.retries = make(chan struct{}, 16)
		if m.Port == 0 {
			m.Port = 25
		}
		if m.DialTimeout == 0 {
			m.DialTimeout = 15 * time.Second
		}
		if m.Log == nil {
			m.Log = slog.Default()
		}
	})
}

// Send builds, signs and delivers the message. A temporary refusal from every
// mail server is retried in the background and reported as success; a
// permanent one is returned. Errors never contain the address.
func (m *DirectMailer) Send(ctx context.Context, to, subject, body string) error {
	m.init()
	from, err := netmail.ParseAddress(m.From)
	if err != nil {
		return fmt.Errorf("mail: from: %w", err)
	}
	id := randomID()
	raw, err := m.build(from, to, subject, body, id)
	if err != nil {
		return err
	}
	err = m.deliver(ctx, from.Address, to, raw)
	var temp tempError
	if errors.As(err, &temp) && m.RetryFor > 0 {
		m.Log.Info("mail deferred; will retry", "mail", id, "err", RedactAddress(err, to))
		go m.retry(from.Address, to, raw, id, time.Now().Add(m.RetryFor))
		return nil
	}
	if err != nil {
		if errors.Is(err, ErrNoMailServer) {
			return fmt.Errorf("mail: %w", ErrNoMailServer)
		}
		return fmt.Errorf("mail: %s", RedactAddress(err, to))
	}
	m.Log.Info("mail delivered", "mail", id)
	return nil
}

// build renders the message with the headers receivers expect and signs it.
func (m *DirectMailer) build(from *netmail.Address, to, subject, body, id string) ([]byte, error) {
	_, domain, _ := strings.Cut(from.Address, "@")
	msg := gomail.NewMsg()
	if err := msg.From(m.From); err != nil {
		return nil, fmt.Errorf("mail: from: %w", err)
	}
	if err := msg.To(to); err != nil {
		return nil, fmt.Errorf("mail: to: %w", err)
	}
	msg.Subject(subject)
	msg.SetDate()
	msg.SetMessageIDWithValue(id + "@" + domain)
	msg.SetBodyString(gomail.TypeTextPlain, body)
	var unsigned bytes.Buffer
	if _, err := msg.WriteTo(&unsigned); err != nil {
		return nil, fmt.Errorf("mail: render: %w", err)
	}
	if m.DKIM == nil {
		return unsigned.Bytes(), nil
	}
	var signed bytes.Buffer
	if err := dkim.Sign(&signed, &unsigned, m.DKIM); err != nil {
		return nil, fmt.Errorf("mail: dkim: %w", err)
	}
	return signed.Bytes(), nil
}

// deliver tries each mail server of the recipient's domain in order. It
// returns a tempError when every server refused temporarily or could not be
// reached, so the caller can retry later.
func (m *DirectMailer) deliver(ctx context.Context, from, to string, raw []byte) error {
	_, domain, ok := strings.Cut(to, "@")
	if !ok {
		return errors.New("invalid recipient")
	}
	hosts, err := m.mxHosts(ctx, domain)
	if err != nil {
		return err
	}
	var last error
	for _, host := range hosts {
		err := m.deliverTo(ctx, host, from, to, raw)
		if err == nil {
			return nil
		}
		var temp tempError
		if !errors.As(err, &temp) {
			return err // permanent: no point asking the next host
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	if last == nil {
		last = tempError{errors.New("no mail server reachable")}
	}
	return last
}

func (m *DirectMailer) mxHosts(ctx context.Context, domain string) ([]string, error) {
	if m.LookupMX != nil {
		return m.LookupMX(ctx, domain)
	}
	return LookupMXHosts(ctx, domain)
}

// LookupMXHosts returns a domain's mail servers by preference, falling back
// to the domain itself when it has no MX records (RFC 5321). A null MX means
// the domain refuses mail.
func LookupMXHosts(ctx context.Context, domain string) ([]string, error) {
	mx, err := net.DefaultResolver.LookupMX(ctx, domain)
	var dnsErr *net.DNSError
	if err != nil && (!errors.As(err, &dnsErr) || !dnsErr.IsNotFound) {
		return nil, tempError{fmt.Errorf("mx lookup: %w", err)}
	}
	if len(mx) == 1 && (mx[0].Host == "." || mx[0].Host == "") {
		return nil, ErrNoMailServer
	}
	hosts := make([]string, 0, len(mx))
	for _, r := range mx {
		if h := strings.TrimSuffix(r.Host, "."); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		hosts = []string{domain}
	}
	return hosts, nil
}

// deliverTo runs one SMTP session with host.
func (m *DirectMailer) deliverTo(ctx context.Context, host, from, to string, raw []byte) error {
	dialer := net.Dialer{Timeout: m.DialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(m.Port)))
	if err != nil {
		return tempError{fmt.Errorf("%s: %w", host, err)}
	}
	defer conn.Close()
	deadline := time.Now().Add(2 * time.Minute)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return classify(host, err)
	}
	defer c.Close()
	if err := c.Hello(m.HELO); err != nil {
		return classify(host, err)
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		// Opportunistic TLS: encrypt whenever the server offers it. The
		// certificate is not verified, since nothing in SMTP tells us what
		// name it should carry; this matches what mail servers do by default.
		if err := c.StartTLS(&tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}); err != nil {
			return tempError{fmt.Errorf("%s: starttls: %w", host, err)}
		}
	}
	if err := c.Mail(from); err != nil {
		return classify(host, err)
	}
	if err := c.Rcpt(to); err != nil {
		return classify(host, err)
	}
	w, err := c.Data()
	if err != nil {
		return classify(host, err)
	}
	if _, err := w.Write(raw); err != nil {
		return tempError{fmt.Errorf("%s: %w", host, err)}
	}
	if err := w.Close(); err != nil {
		return classify(host, err)
	}
	c.Quit()
	return nil
}

// classify turns an SMTP reply into a temporary (4xx, or no reply) or
// permanent (5xx) failure.
func classify(host string, err error) error {
	var te *textproto.Error
	if errors.As(err, &te) && te.Code >= 500 {
		return fmt.Errorf("%s: %w", host, err)
	}
	return tempError{fmt.Errorf("%s: %w", host, err)}
}

// retry re-attempts delivery every retryWait until the deadline: greylisting
// servers accept a sender once it has come back after a few minutes, and a
// sign-in code is only useful inside its own validity window, so the gaps
// stay short rather than growing. The address lives only in this
// goroutine's memory meanwhile.
func (m *DirectMailer) retry(from, to string, raw []byte, id string, deadline time.Time) {
	select {
	case m.retries <- struct{}{}:
		defer func() { <-m.retries }()
	default:
		m.Log.Warn("mail retry queue full; giving up", "mail", id)
		return
	}
	for {
		wait := retryWait
		if time.Now().Add(wait).After(deadline) {
			m.Log.Warn("mail not delivered before its deadline", "mail", id)
			return
		}
		time.Sleep(wait)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err := m.deliver(ctx, from, to, raw)
		cancel()
		if err == nil {
			m.Log.Info("mail delivered on retry", "mail", id)
			return
		}
		var temp tempError
		if !errors.As(err, &temp) {
			m.Log.Warn("mail rejected", "mail", id, "err", RedactAddress(err, to))
			return
		}
	}
}

// retryWait is the gap between delivery attempts; tests shorten it.
var retryWait = 90 * time.Second

func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// RedactAddress removes the address from an error message before it is
// logged; mail servers routinely echo the recipient in their replies.
func RedactAddress(err error, email string) string {
	if err == nil {
		return ""
	}
	s := strings.ReplaceAll(err.Error(), email, "[address]")
	if local, _, ok := strings.Cut(email, "@"); ok && len(local) >= 3 {
		s = strings.ReplaceAll(s, local, "[address]")
	}
	return s
}
