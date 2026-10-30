// Package mail sends transactional email over SMTP, or logs it in development.
package mail

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	gomail "github.com/wneessen/go-mail"
)

// Mailer sends a plain-text message.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// LogMailer prints messages to the log (MAIL_MODE=log).
type LogMailer struct{ Log *slog.Logger }

// Send logs the message instead of sending it.
func (m LogMailer) Send(_ context.Context, to, subject, body string) error {
	// The recipient is deliberately not printed: addresses must not reach logs.
	m.Log.Info("mail (not sent, MAIL_MODE=log)", "subject", subject)
	fmt.Printf("\n----- MAIL (recipient withheld): %s -----\n%s\n----- END MAIL -----\n\n", subject, body)
	return nil
}

// SMTPMailer sends through an SMTP relay.
type SMTPMailer struct {
	Host string
	Port int
	User string
	Pass string
	From string
	TLS  string // starttls | tls | none
}

// Send delivers the message.
func (m SMTPMailer) Send(ctx context.Context, to, subject, body string) error {
	msg := gomail.NewMsg()
	if err := msg.From(m.From); err != nil {
		return fmt.Errorf("mail: from: %w", err)
	}
	if err := msg.To(to); err != nil {
		return fmt.Errorf("mail: to: %w", err)
	}
	msg.Subject(subject)
	msg.SetBodyString(gomail.TypeTextPlain, body)

	opts := []gomail.Option{gomail.WithPort(m.Port)}
	switch strings.ToLower(m.TLS) {
	case "tls":
		opts = append(opts, gomail.WithSSL())
	case "none":
		opts = append(opts, gomail.WithTLSPolicy(gomail.NoTLS))
	default:
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	}
	if m.User != "" {
		opts = append(opts, gomail.WithSMTPAuth(gomail.SMTPAuthAutoDiscover), gomail.WithUsername(m.User), gomail.WithPassword(m.Pass))
	}
	c, err := gomail.NewClient(m.Host, opts...)
	if err != nil {
		return fmt.Errorf("mail: client: %w", err)
	}
	if err := c.DialAndSendWithContext(ctx, msg); err != nil {
		return fmt.Errorf("mail: send: %w", err)
	}
	return nil
}
