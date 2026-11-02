package internalapi

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// The internal API serves over a Unix socket that only its owner and group
// may use, and a stale socket file is replaced on start.
func TestListenUnixSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "run", "internal.sock")
	os.MkdirAll(filepath.Dir(sock), 0o750)
	os.WriteFile(filepath.Join(dir, "x"), nil, 0o600)
	// A leftover socket from a previous run must not block the new one.
	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()

	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	st, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode = %v; want a socket with 0660", st.Mode())
	}
	srv := &http.Server{Handler: (&Server{}).Handler()}
	go srv.Serve(ln)
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := client.Get("http://localhost/internal/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz over the socket = %d", resp.StatusCode)
	}
	if _, err := Listen(filepath.Join(dir, "x")); err == nil {
		t.Fatal("a regular file in the socket's place must be refused, not deleted")
	}
}
