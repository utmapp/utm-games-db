package web

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/utmapp/utm-games-db/internal/config"
)

// The visitor IP comes from CF-Connecting-IP only when the peer is a trusted
// proxy; a direct connection (no CDN) uses the socket address, and a spoofed
// header from an untrusted peer is ignored.
func TestClientIP(t *testing.T) {
	_, cf, _ := net.ParseCIDR("203.0.113.0/24")
	s := &Server{Cfg: &config.Config{TrustedProxyCIDRs: []*net.IPNet{cf}}}
	req := func(remote, cfIP, xff string) *http.Request {
		r, _ := http.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if cfIP != "" {
			r.Header.Set("CF-Connecting-IP", cfIP)
		}
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	cases := []struct{ remote, cf, xff, want string }{
		{"203.0.113.9:443", "198.51.100.7", "", "198.51.100.7"},           // via CDN
		{"203.0.113.9:443", "", "198.51.100.8, 10.0.0.1", "198.51.100.8"}, // generic proxy header
		{"192.0.2.5:5555", "198.51.100.7", "", "192.0.2.5"},               // direct: header spoofed, ignored
		{"192.0.2.5:5555", "", "", "192.0.2.5"},                           // direct
		{"[2001:db8::1]:443", "198.51.100.7", "", "2001:db8::1"},          // direct over IPv6
	}
	for _, c := range cases {
		if got := s.clientIP(req(c.remote, c.cf, c.xff)); got != c.want {
			t.Errorf("clientIP(%s, cf=%q, xff=%q) = %q, want %q", c.remote, c.cf, c.xff, got, c.want)
		}
	}
}

// A local proxy that forwards client IPs but is not trusted (e.g. the Docker
// gateway behind a host reverse proxy) makes every visitor share one address;
// the server says so, once.
func TestUntrustedLocalProxyWarns(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{Cfg: &config.Config{}, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	for i := 0; i < 3; i++ {
		r, _ := http.NewRequest("GET", "/", nil)
		r.RemoteAddr = "172.18.0.1:40000"
		r.Header.Set("CF-Connecting-IP", "198.51.100.7")
		if got := s.clientIP(r); got != "172.18.0.1" {
			t.Fatalf("clientIP = %q", got)
		}
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 || !strings.Contains(buf.String(), "TRUSTED_PROXY_CIDRS") {
		t.Fatalf("want exactly one warning, got %d: %s", n, buf.String())
	}
}
