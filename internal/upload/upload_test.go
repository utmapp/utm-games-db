package upload

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSniff(t *testing.T) {
	ok := []struct {
		name string
		head []byte
		mime string
	}{
		{"a.png", []byte("\x89PNG\r\n\x1a\n...."), "image/png"},
		{"a.JPG", []byte("\xff\xd8\xff\xe0...."), "image/jpeg"},
		{"a.gif", []byte("GIF89a...."), "image/gif"},
		{"a.webp", append([]byte("RIFF\x00\x00\x00\x00WEBP"), []byte("VP8 ")...), "image/webp"},
		{"a.webm", []byte("\x1a\x45\xdf\xa3...."), "video/webm"},
		{"a.mp4", []byte("\x00\x00\x00\x18ftypisom...."), "video/mp4"},
		{"a.mov", []byte("\x00\x00\x00\x14ftypqt  ...."), "video/quicktime"},
		{"a.zip", []byte("PK\x03\x04...."), "application/zip"},
		{"a.log", []byte("2024-01-01 boot ok\nline 2\n"), "text/plain"},
		{"a.json", []byte(`{"a":1}`), "application/json"},
		{"a.plist", []byte("bplist00...."), "application/x-plist"},
		{"a.plist", []byte("<?xml version=\"1.0\"?>"), "application/x-plist"},
	}
	for _, c := range ok {
		m, err := Sniff(c.name, c.head)
		if err != nil || m != c.mime {
			t.Errorf("Sniff(%q) = %q, %v; want %q", c.name, m, err, c.mime)
		}
	}
	bad := []struct {
		name string
		head []byte
	}{
		{"a.exe", []byte("MZ\x90\x00....")},
		{"a.txt", []byte("bin\x00ary")},
		{"a.png", []byte("not a png")},
		{"a.docx", []byte("PK\x03\x04....")}, // zip container with a non-zip name
		{"a.html", []byte("<html>")},
		{"a.txt", []byte("<svg onload=alert(1)>")},
		{"a.png", []byte("GIF89a....")}, // valid image, wrong extension
		{"a.mov", []byte("\x89PNG\r\n\x1a\n")},
		{"a.txt", nil},
	}
	for _, c := range bad {
		if m, err := Sniff(c.name, c.head); err == nil {
			t.Errorf("Sniff(%q) = %q; should fail", c.name, m)
		}
	}
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":      "passwd",
		"C:\\Users\\x\\log.txt": "log.txt",
		"my screenshot (1).png": "my_screenshot_1_.png",
		"":                      "file",
		"...":                   "file",
	}
	for in, want := range cases {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestStoreSave(t *testing.T) {
	s := Store{Root: t.TempDir()}
	body := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("x"), 2000)...)
	saved, err := s.Save("7", "../shot.png", bytes.NewReader(body), 5000)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Size != 2008 || saved.MIME != "image/png" || !strings.HasPrefix(saved.RelPath, "7/") || !strings.HasSuffix(saved.RelPath, "_shot.png") || len(saved.SHA256) != 64 {
		t.Fatalf("%+v", saved)
	}
	again, err := s.Save("7", "shot.png", bytes.NewReader(body), 5000)
	if err != nil || again.RelPath == saved.RelPath {
		t.Fatalf("same name must get a distinct file: %+v %v", again, err)
	}
	if _, err := s.Save("7", "2.png", bytes.NewReader(body), 50); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize should fail: %v", err)
	}
	if _, err := s.Save("7", "page.txt", strings.NewReader("<html><script>x</script>"), 5000); !errors.Is(err, ErrType) {
		t.Fatalf("markup as text should fail: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root, "7"))
	if len(entries) != 2 {
		t.Fatalf("partial or temp file left behind: %d entries", len(entries))
	}
	if err := s.Remove(saved.RelPath); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(saved.RelPath); err != nil {
		t.Fatal("double remove should be ok")
	}
}

func TestContentDisposition(t *testing.T) {
	for in, want := range map[string]string{
		`a.png`:             `attachment; filename=a.png`,
		"x\"\r\nSet: y.txt": `attachment; filename`,
		"résumé.txt":        `attachment; filename*=utf-8''r%C3%A9sum%C3%A9.txt`,
	} {
		if got := ContentDisposition(in); !strings.HasPrefix(got, want) || strings.ContainsAny(got, "\r\n") {
			t.Errorf("ContentDisposition(%q) = %q; want prefix %q", in, got, want)
		}
	}
}
