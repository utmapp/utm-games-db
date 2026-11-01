package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	netmail "net/mail"
	"os"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/config"
	"github.com/utmapp/utm-games-db/internal/mail"
)

// newMailer builds the mailer for MAIL_MODE. retryFor bounds background
// retries in direct mode; mail-test passes 0 so a refusal is reported.
func newMailer(cfg *config.Config, log *slog.Logger, retryFor time.Duration) (mail.Mailer, error) {
	switch cfg.MailMode {
	case "smtp":
		return mail.SMTPMailer{Host: cfg.SMTPHost, Port: cfg.SMTPPort, User: cfg.SMTPUser, Pass: cfg.SMTPPass, From: cfg.MailFrom, TLS: cfg.SMTPTLS}, nil
	case "direct":
		key, err := mail.LoadDKIMKey(cfg.MailDKIMKey)
		if err != nil {
			return nil, fmt.Errorf("DKIM key: %w (run: gamesdb mail-setup)", err)
		}
		addr, err := netmail.ParseAddress(cfg.MailFrom)
		if err != nil {
			return nil, err
		}
		_, domain, _ := strings.Cut(addr.Address, "@")
		return &mail.DirectMailer{From: cfg.MailFrom, HELO: cfg.MailHELO, DKIM: mail.DKIMSigner(domain, cfg.MailDKIMSelector, key), RetryFor: retryFor, Log: log}, nil
	default:
		log.Warn("MAIL_MODE=log: sign-in codes are printed to stdout instead of emailed")
		return mail.LogMailer{Log: log}, nil
	}
}

// mailSetup creates the DKIM key once, checks the DNS records direct
// delivery needs, and prints the ones that still have to be published.
func mailSetup(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("mail-setup", flag.ContinueOnError)
	ip := fs.String("ip", "", "public IP address mail is sent from")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ip == "" || net.ParseIP(*ip) == nil {
		return errors.New("usage: gamesdb mail-setup --ip <public IP mail is sent from>")
	}
	if cfg.MailHELO == "" {
		return errors.New("set MAIL_HELO to the machine's DNS-only host name (e.g. host1.example.com)")
	}
	addr, err := netmail.ParseAddress(cfg.MailFrom)
	if err != nil {
		return err
	}
	_, domain, _ := strings.Cut(addr.Address, "@")
	created, err := mail.CreateDKIMKey(cfg.MailDKIMKey)
	if err != nil {
		return err
	}
	key, err := mail.LoadDKIMKey(cfg.MailDKIMKey)
	if err != nil {
		return err
	}
	record, err := mail.DKIMRecord(key)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("Created DKIM key %s (back it up with SECRET_KEY; a new key invalidates the published record).\n", cfg.MailDKIMKey)
	} else {
		fmt.Printf("Using DKIM key %s.\n", cfg.MailDKIMKey)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Ask the zones' own name servers rather than the local resolver, whose
	// caches keep answering "no such record" for a while after a record is
	// created; this reports what is published, which is what matters.
	type rec struct {
		name, typ, value, note string
		lookup                 func() []string
	}
	txt := func(name string) func() []string {
		return func() []string { v, _ := authoritative(ctx, name).LookupTXT(ctx, name); return normalize(v) }
	}
	records := []rec{
		{cfg.MailHELO, "A", *ip, "DNS only, not proxied: receivers resolve it to check the sender",
			func() []string { v, _ := authoritative(ctx, cfg.MailHELO).LookupHost(ctx, cfg.MailHELO); return v }},
		{cfg.MailHELO, "TXT", "v=spf1 a -all", "SPF for the host name given in EHLO", txt(cfg.MailHELO)},
		{domain, "TXT", "v=spf1 ip4:" + *ip + " -all", "SPF for the From domain", txt(domain)},
		{cfg.MailDKIMSelector + "._domainkey." + domain, "TXT", record, "DKIM public key", txt(cfg.MailDKIMSelector + "._domainkey." + domain)},
		{"_dmarc." + domain, "TXT", "v=DMARC1; p=reject; adkim=s; aspf=s", "DMARC: reject mail not signed by and sent from us", txt("_dmarc." + domain)},
	}
	var missing []rec
	fmt.Println()
	for _, r := range records {
		have := r.lookup()
		want := r.value
		if r.typ == "TXT" {
			want = normalize([]string{want})[0]
		}
		if contains(have, want) {
			fmt.Printf("ok       %s %s\n", r.name, r.typ)
			continue
		}
		fmt.Printf("missing  %s %s\n", r.name, r.typ)
		missing = append(missing, r)
	}
	ptrs, _ := authoritative(ctx, reverseName(*ip)).LookupAddr(ctx, *ip)
	for i := range ptrs {
		ptrs[i] = strings.TrimSuffix(ptrs[i], ".")
	}
	ptrOK := contains(ptrs, cfg.MailHELO)
	if ptrOK {
		fmt.Printf("ok       PTR %s -> %s\n", *ip, cfg.MailHELO)
	} else {
		fmt.Printf("missing  PTR %s -> %s (currently: %s)\n", *ip, cfg.MailHELO, strings.Join(ptrs, " "))
	}
	if len(missing) == 0 && ptrOK {
		fmt.Println("\nAll records in place.")
		return nil
	}
	if len(missing) > 0 {
		fmt.Println("\nAdd these DNS records:")
		for _, r := range missing {
			fmt.Printf("\n  %s  %s\n  %s\n  (%s)\n", r.name, r.typ, r.value, r.note)
		}
	}
	if !ptrOK {
		fmt.Printf("\nAsk the hosting provider to set the reverse DNS (PTR) of %s to %s.\n", *ip, cfg.MailHELO)
	}
	fmt.Println("\nRe-run this command after adding them. Records are checked at the zone's own name servers; other resolvers see them once their caches expire.")
	os.Exit(1) // the report above says everything; no error line needed
	return nil
}

// authoritative returns a resolver that asks one of the name servers
// responsible for name, found by walking up its labels until a zone with NS
// records is reached. It falls back to the system resolver.
func authoritative(ctx context.Context, name string) *net.Resolver {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for i := 0; i < len(labels)-1; i++ {
		zone := strings.Join(labels[i:], ".")
		nss, err := net.DefaultResolver.LookupNS(ctx, zone)
		if err != nil || len(nss) == 0 {
			continue
		}
		addrs, err := net.DefaultResolver.LookupHost(ctx, strings.TrimSuffix(nss[0].Host, "."))
		if err != nil || len(addrs) == 0 {
			continue
		}
		server := net.JoinHostPort(addrs[0], "53")
		return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, server)
		}}
	}
	return net.DefaultResolver
}

// reverseName is the in-addr.arpa / ip6.arpa name of an address.
func reverseName(ip string) string {
	p := net.ParseIP(ip)
	if v4 := p.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa", v4[3], v4[2], v4[1], v4[0])
	}
	const hexdigits = "0123456789abcdef"
	var b strings.Builder
	for i := len(p) - 1; i >= 0; i-- {
		b.WriteByte(hexdigits[p[i]&0xf])
		b.WriteByte('.')
		b.WriteByte(hexdigits[p[i]>>4])
		b.WriteByte('.')
	}
	return b.String() + "ip6.arpa"
}

// mailTest sends a sign-in code mail (with the fixed code 123456) through
// the configured mailer, so delivery and the mail client's handling of the
// code can be checked end to end.
func mailTest(cfg *config.Config, log *slog.Logger, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: gamesdb mail-test ADDRESS")
	}
	to, err := auth.NormalizeEmail(args[0])
	if err != nil {
		return err
	}
	m, err := newMailer(cfg, log, 0)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := m.Send(ctx, to, "Your UTM Database sign-in code", auth.CodeBody("123456", cfg.LoginCodeTTL)); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "sent")
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// normalize removes the whitespace DNS providers may insert into long TXT values.
func normalize(txts []string) []string {
	out := make([]string, len(txts))
	for i, t := range txts {
		out[i] = strings.Join(strings.Fields(t), "")
	}
	return out
}
