package mail

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// fakeMX is a minimal SMTP server: EHLO, MAIL, RCPT, DATA, QUIT. It can
// refuse the first N RCPTs temporarily, like a greylisting server.
type fakeMX struct {
	ln       net.Listener
	mu       sync.Mutex
	messages [][]byte
	greylist int
	helo     string
}

func newFakeMX(t *testing.T, greylist int) *fakeMX {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeMX{ln: ln, greylist: greylist}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeMX) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeMX) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	fmt.Fprintf(c, "220 fake ESMTP\r\n")
	var data bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			f.mu.Lock()
			f.helo = strings.TrimSpace(line[5:])
			f.mu.Unlock()
			fmt.Fprintf(c, "250-fake\r\n250 8BITMIME\r\n")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			fmt.Fprintf(c, "250 ok\r\n")
		case strings.HasPrefix(cmd, "RCPT TO"):
			f.mu.Lock()
			if f.greylist > 0 {
				f.greylist--
				f.mu.Unlock()
				fmt.Fprintf(c, "451 4.7.1 try again later\r\n")
				continue
			}
			f.mu.Unlock()
			fmt.Fprintf(c, "250 ok\r\n")
		case cmd == "DATA":
			fmt.Fprintf(c, "354 go\r\n")
			data.Reset()
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(strings.TrimPrefix(l, "."))
			}
			f.mu.Lock()
			f.messages = append(f.messages, append([]byte(nil), data.Bytes()...))
			f.mu.Unlock()
			fmt.Fprintf(c, "250 queued\r\n")
		case cmd == "QUIT":
			fmt.Fprintf(c, "221 bye\r\n")
			return
		default:
			fmt.Fprintf(c, "500 what\r\n")
		}
	}
}

func (f *fakeMX) last() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) == 0 {
		return nil
	}
	return f.messages[len(f.messages)-1]
}

func newMailer(t *testing.T, mx *fakeMX) (*DirectMailer, string) {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "dkim", "test.key")
	if created, err := CreateDKIMKey(keyPath); err != nil || !created {
		t.Fatalf("create key: %v %v", created, err)
	}
	if created, err := CreateDKIMKey(keyPath); err != nil || created {
		t.Fatal("second create must keep the existing key")
	}
	key, err := LoadDKIMKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := DKIMRecord(key)
	if err != nil {
		t.Fatal(err)
	}
	m := &DirectMailer{
		From:        "UTM Database <no-reply@db.example>",
		HELO:        "mail.example",
		Port:        mx.port(),
		DKIM:        DKIMSigner("db.example", "test", key),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialTimeout: 5 * time.Second,
		LookupMX:    func(_ context.Context, domain string) ([]string, error) { return []string{"127.0.0.1"}, nil },
	}
	return m, record
}

func TestDirectDeliveryIsSignedAndVerifiable(t *testing.T) {
	mx := newFakeMX(t, 0)
	m, record := newMailer(t, mx)
	body := "Your code is 123456 \u2014 enjoy\n"
	if err := m.Send(context.Background(), "alice@example.com", "Your code", body); err != nil {
		t.Fatal(err)
	}
	raw := mx.last()
	if raw == nil {
		t.Fatal("nothing delivered")
	}
	if mx.helo != "mail.example" {
		t.Errorf("EHLO %q", mx.helo)
	}
	for _, h := range []string{"From: ", "To: ", "Subject: Your code", "Date: ", "Message-ID: <", "@db.example>", "DKIM-Signature: "} {
		if !bytes.Contains(raw, []byte(h)) {
			t.Errorf("message lacks %q:\n%s", h, raw)
		}
	}
	// The body carries UTF-8 (the em dash in the sign-off), so it must be
	// transfer-encoded: a server without 8BITMIME must receive pure ASCII.
	if !bytes.Contains(raw, []byte("Content-Transfer-Encoding: quoted-printable")) {
		t.Errorf("body not quoted-printable encoded:\n%s", raw)
	}
	for i, b := range raw {
		if b >= 0x80 {
			t.Fatalf("non-ASCII byte at %d: %q", i, raw[max(0, i-20):i+1])
		}
	}
	results, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) {
			if name != "test._domainkey.db.example" {
				return nil, errors.New("unexpected lookup " + name)
			}
			return []string{record}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Err != nil || results[0].Domain != "db.example" {
		t.Fatalf("dkim verify: %+v", results)
	}
}

func TestDirectDeliveryRetriesTemporaryRefusals(t *testing.T) {
	mx := newFakeMX(t, 1)
	m, _ := newMailer(t, mx)
	// Without a retry window a temporary refusal is an error, with the
	// address redacted.
	err := m.Send(context.Background(), "alice@example.com", "x", "y")
	if err == nil || strings.Contains(err.Error(), "alice") || !strings.Contains(err.Error(), "451") {
		t.Fatalf("temporary refusal: %v", err)
	}
	mx2 := newFakeMX(t, 1)
	m2, _ := newMailer(t, mx2)
	m2.RetryFor = 10 * time.Second
	retryWait = 50 * time.Millisecond
	t.Cleanup(func() { retryWait = 90 * time.Second })
	if err := m2.Send(context.Background(), "alice@example.com", "x", "y"); err != nil {
		t.Fatalf("deferred delivery should not error: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for mx2.last() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if mx2.last() == nil {
		t.Fatal("message was not delivered on retry")
	}
}

func TestDirectDeliveryPermanentFailure(t *testing.T) {
	m := &DirectMailer{From: "a <a@db.example>", HELO: "mail.example", RetryFor: time.Hour,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		LookupMX: func(context.Context, string) ([]string, error) { return nil, ErrNoMailServer }}
	if err := m.Send(context.Background(), "nobody@nomail.example", "x", "y"); !errors.Is(err, ErrNoMailServer) {
		t.Fatalf("got %v", err)
	}
}

func TestRedactAddress(t *testing.T) {
	got := RedactAddress(errors.New("550 <alice@example.com> and alice unknown"), "alice@example.com")
	if strings.Contains(got, "alice") {
		t.Fatalf("not redacted: %q", got)
	}
}
