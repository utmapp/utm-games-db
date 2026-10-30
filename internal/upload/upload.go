// Package upload validates and stores report attachments.
package upload

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mimepkg "mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// kinds maps each accepted extension to the stored MIME type and the
// content signature the file must carry. A file is accepted only when its
// name and its bytes agree, so a renamed executable or HTML page is refused.
var kinds = map[string]struct {
	mime  string
	match func(head []byte) bool
}{
	"png":   {"image/png", prefix("\x89PNG\r\n\x1a\n")},
	"jpg":   {"image/jpeg", prefix("\xff\xd8\xff")},
	"jpeg":  {"image/jpeg", prefix("\xff\xd8\xff")},
	"gif":   {"image/gif", func(b []byte) bool { return prefix("GIF87a")(b) || prefix("GIF89a")(b) }},
	"webp":  {"image/webp", func(b []byte) bool { return len(b) >= 12 && prefix("RIFF")(b) && string(b[8:12]) == "WEBP" }},
	"webm":  {"video/webm", prefix("\x1a\x45\xdf\xa3")},
	"mp4":   {"video/mp4", isoMedia},
	"mov":   {"video/quicktime", isoMedia},
	"zip":   {"application/zip", func(b []byte) bool { return prefix("PK\x03\x04")(b) || prefix("PK\x05\x06")(b) }},
	"plist": {"application/x-plist", func(b []byte) bool { return prefix("bplist00")(b) || looksText(b) }},
	"txt":   {"text/plain", looksText},
	"log":   {"text/plain", looksText},
	"json":  {"application/json", looksText},
}

func prefix(p string) func([]byte) bool {
	return func(b []byte) bool { return bytes.HasPrefix(b, []byte(p)) }
}

// isoMedia matches the ISO base media container (MP4 and QuickTime).
func isoMedia(b []byte) bool { return len(b) >= 12 && string(b[4:8]) == "ftyp" }

// Accepted lists the extensions users may upload, for display.
var Accepted = []string{"png", "jpg", "jpeg", "webp", "gif", "mp4", "mov", "webm", "txt", "log", "json", "plist", "zip"}

// AcceptedLabel is the human readable list.
func AcceptedLabel() string { return strings.Join(Accepted, ", ") }

var (
	// ErrType means the content did not match an accepted format.
	ErrType = errors.New("unsupported file type")
	// ErrEmpty means the file had no content.
	ErrEmpty = errors.New("empty file")
	// ErrTooLarge means the file exceeded the size limit.
	ErrTooLarge = errors.New("file too large")
)

// Sniff validates the first bytes of a file against its declared name and
// returns the MIME type to store. head should hold up to headSize bytes.
func Sniff(filename string, head []byte) (mime string, err error) {
	if len(head) == 0 {
		return "", ErrEmpty
	}
	k, ok := kinds[strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))]
	if !ok || !k.match(head) {
		return "", ErrType
	}
	return k.mime, nil
}

// headSize is how much of a file Sniff inspects.
const headSize = 1024

// looksText accepts UTF-8 without NUL bytes. Markup is not text here: an
// HTML or SVG page renamed to .txt is refused.
func looksText(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	if t := bytes.TrimSpace(b); len(t) > 0 && t[0] == '<' && !bytes.HasPrefix(t, []byte("<?xml")) {
		return false
	}
	// Allow a truncated multibyte rune at the end of the sample.
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
		if len(b) < 1000 {
			return utf8.Valid(b)
		}
	}
	return true
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SafeName sanitizes a user supplied file name for storage on disk.
func SafeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = unsafeName.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._")
	if name == "" {
		name = "file"
	}
	if len(name) > 120 {
		name = name[len(name)-120:]
	}
	return name
}

// Store writes files under a root directory.
type Store struct{ Root string }

// Saved describes a stored file.
type Saved struct {
	RelPath string
	Size    int64
	SHA256  string
	MIME    string
}

// Save validates r against filename and streams it to a new file under
// <root>/<subdir>, hashing it as it goes. The stored name is random plus the
// sanitized original, created exclusively so two uploads never share a file.
// A stream longer than maxBytes is rejected and nothing is left behind.
func (s Store) Save(subdir, filename string, r io.Reader, maxBytes int64) (*Saved, error) {
	head := make([]byte, headSize)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	head = head[:n]
	mime, err := Sniff(filename, head)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Root, subdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(io.MultiReader(bytes.NewReader(head), r), maxBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if size > maxBytes {
		return nil, fmt.Errorf("%w (%d bytes)", ErrTooLarge, maxBytes)
	}
	name := randomHex(8) + "_" + SafeName(filename)
	// Link fails if the name exists, unlike Rename, which would replace it.
	if err := os.Link(tmpName, filepath.Join(dir, name)); err != nil {
		return nil, err
	}
	return &Saved{RelPath: filepath.ToSlash(filepath.Join(subdir, name)), Size: size, SHA256: hex.EncodeToString(h.Sum(nil)), MIME: mime}, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (s Store) path(rel string) string {
	return filepath.Join(s.Root, filepath.Clean("/"+rel))
}

// Open returns the file for a stored relative path.
func (s Store) Open(rel string) (*os.File, error) {
	return os.Open(s.path(rel))
}

// Remove deletes a stored file (missing files are not an error).
func (s Store) Remove(rel string) error {
	if err := os.Remove(s.path(rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveAll deletes stored files, logging the ones that could not be removed.
func (s Store) RemoveAll(rels []string, log *slog.Logger) {
	for _, rel := range rels {
		if err := s.Remove(rel); err != nil {
			log.Warn("remove attachment file", "path", rel, "err", err)
		}
	}
}

// Serve sends a stored file as a download. The file is never rendered by
// the browser: it is an attachment, scripts are disabled by a sandboxing
// CSP, and text types are served as plain text.
func (s Store) Serve(w http.ResponseWriter, r *http.Request, rel, filename, mime string) {
	f, err := s.Open(rel)
	if err != nil {
		http.Error(w, "file missing", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "file missing", http.StatusNotFound)
		return
	}
	if strings.HasPrefix(mime, "text/") || mime == "application/json" || mime == "application/x-plist" {
		mime = "text/plain; charset=utf-8"
	}
	h := w.Header()
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Type", mime)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", ContentDisposition(filename))
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// ContentDisposition builds an attachment header for a user supplied file
// name, encoding it (RFC 6266) instead of trusting it to be header-safe.
func ContentDisposition(filename string) string {
	if v := mimepkg.FormatMediaType("attachment", map[string]string{"filename": filename}); v != "" {
		return v
	}
	return mimepkg.FormatMediaType("attachment", map[string]string{"filename": SafeName(filename)})
}
