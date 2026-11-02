package internalapi

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// IsSocketPath reports whether an INTERNAL_LISTEN_ADDR value names a Unix
// socket (an absolute path) rather than a TCP address.
func IsSocketPath(addr string) bool { return strings.HasPrefix(addr, "/") }

// Listen opens the internal API's listener. An absolute path is a Unix
// socket: a stale socket file is removed first, and the new one is made
// readable and writable by its owner and group only (0660), so who may use
// the API is whoever shares the service's group. Anything else is a TCP
// address.
func Listen(addr string) (net.Listener, error) {
	if !IsSocketPath(addr) {
		return net.Listen("tcp", addr)
	}
	if err := os.MkdirAll(filepath.Dir(addr), 0o750); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(addr); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", addr)
		}
		os.Remove(addr)
	}
	// The permission bits are applied after creation; the process umask may
	// make the socket briefly narrower, never wider.
	ln, err := net.Listen("unix", addr)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(addr, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
