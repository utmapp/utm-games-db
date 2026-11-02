package internalapi

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/utmapp/utm-games-db/internal/store"
)

// --- work -------------------------------------------------------------------

// work is one call that returns everything a triage or moderation machine
// might act on: reports created or changed since a cursor, ascending, and
// the open queues in full. Everything is idempotent, so a machine that was
// off simply catches up on its first call.
func (s *Server) work(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var since time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			n, err2 := strconv.ParseInt(v, 10, 64)
			if err2 != nil {
				fail(w, http.StatusBadRequest, errors.New("since: RFC3339 or unix seconds"))
				return
			}
			t = time.Unix(n, 0)
		}
		since = t
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	ctx := r.Context()
	now := time.Now().UTC().Truncate(time.Second)
	reports, err := s.Store.ListReports(ctx, since, 0, "visible", "", limit)
	if err != nil {
		failFor(w, err)
		return
	}
	for i := range reports {
		if reports[i].Comments, err = s.Store.ListComments(ctx, reports[i].ID, true); err != nil {
			failFor(w, err)
			return
		}
	}
	donations, err := s.Store.ListTriageRequests(ctx, "open", 0)
	if err != nil {
		failFor(w, err)
		return
	}
	deletions, err := s.Store.ListDeletionRequests(ctx, "open", 0)
	if err != nil {
		failFor(w, err)
		return
	}
	flags, err := s.Store.ListFlags(ctx, 0)
	if err != nil {
		failFor(w, err)
		return
	}
	spam, err := s.Store.ListSpam(ctx, 0, true)
	if err != nil {
		failFor(w, err)
		return
	}
	untriaged, err := s.Store.CountUntriaged(ctx)
	if err != nil {
		failFor(w, err)
		return
	}
	// The cursor is inclusive (updated_at has second resolution, and a page
	// boundary can fall inside a second), so a report on the boundary may be
	// delivered twice; callers de-duplicate on id and updated_at. With a
	// full page the cursor is the last report; otherwise it is the current
	// second, which may still gain writes after this query ran.
	next := now
	if len(reports) > 0 && len(reports) >= limit {
		next = reports[len(reports)-1].UpdatedAt
	}
	ok(w, map[string]any{
		"now":               now,
		"next_since":        next,
		"reports":           orEmpty(reports),
		"reports_more":      len(reports) >= limit,
		"donation_requests": orEmpty(donations),
		"deletion_requests": orEmpty(deletions),
		"flags":             orEmpty(flags),
		"spam_pending":      orEmpty(spam),
		"counts": map[string]int{
			"untriaged_reports":      untriaged,
			"open_donation_requests": len(donations),
			"open_deletion_requests": len(deletions),
			"flagged_items":          len(flags),
			"spam_pending":           len(spam),
		},
	})
}

func orEmpty[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// --- backup ---------------------------------------------------------------------

var backupMu sync.Mutex

// backupManifest describes an archive to whoever restores it.
type backupManifest struct {
	CreatedAt      time.Time         `json:"created_at"`
	Format         int               `json:"format"`
	SchemaVersions []string          `json:"schema_versions"`
	Tables         map[string]int64  `json:"tables"`
	Excluded       []string          `json:"excluded"`
	Files          []backupFile      `json:"files"`
	Restore        map[string]string `json:"restore"` // archive path prefix -> place relative to the install
}

type backupFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// backup streams an uncompressed tar archive of everything needed to bring
// the site back on another machine: the settings file with its secrets, the
// TLS and DKIM keys, a database holding every user and maintainer table
// (not the IGDB mirror, which is re-imported), and the uploaded files. One
// backup runs at a time; it may take as long as the data needs.
//
// Layout: README.txt, manifest.json, env, tls/<files>, dkim/<file>,
// db/gamesdb.sqlite, uploads/<as stored>. deploy/macos/restore.sh puts the
// pieces back.
func (s *Server) backup(w http.ResponseWriter, r *http.Request) {
	if !backupMu.TryLock() {
		fail(w, http.StatusConflict, errors.New("a backup is already running"))
		return
	}
	defer backupMu.Unlock()
	ctx := r.Context()

	// The database copy first, into a temporary file: it is the one step
	// that can fail for a reason worth a clean error, and the tar needs its
	// size up front.
	tmp, err := os.CreateTemp("", "gamesdb-backup-*.sqlite")
	if err != nil {
		failFor(w, err)
		return
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	if err := s.Store.DB.CopyTables(ctx, tmpPath, store.BackupTables, store.BackupFixups); err != nil {
		failFor(w, err)
		return
	}
	counts, err := s.Store.TableCounts(ctx, store.BackupTables)
	if err != nil {
		failFor(w, err)
		return
	}
	versions, err := s.schemaVersions(ctx)
	if err != nil {
		failFor(w, err)
		return
	}

	// Small files are read now, so the manifest can list them with sizes.
	type blob struct {
		path string
		data []byte
	}
	var blobs []blob
	addFile := func(archivePath, src string) {
		if src == "" {
			return
		}
		data, err := os.ReadFile(src)
		if err != nil {
			s.Log.Warn("backup: skipping unreadable file", "path", src, "err", err)
			return
		}
		blobs = append(blobs, blob{archivePath, data})
	}
	addFile("env", s.envFile())
	if s.Cfg != nil {
		addFile("tls/"+filepath.Base(s.Cfg.TLSCert), s.Cfg.TLSCert)
		addFile("tls/"+filepath.Base(s.Cfg.TLSKey), s.Cfg.TLSKey)
		addFile("dkim/"+filepath.Base(s.Cfg.MailDKIMKey), s.Cfg.MailDKIMKey)
	}
	var uploads []string
	filepath.WalkDir(s.Uploads.Root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			uploads = append(uploads, path)
		}
		return nil
	})

	man := backupManifest{
		CreatedAt: time.Now().UTC(), Format: 1, SchemaVersions: versions, Tables: counts,
		Excluded: []string{"igdb_* (the IGDB mirror; re-imported by the next sync)", "login_codes (transient)", "covers/ (re-downloaded when details are resolved)"},
		Restore:  map[string]string{"env": ".env", "tls/": "tls/", "dkim/": "data/dkim/", "db/gamesdb.sqlite": "data/gamesdb.sqlite", "uploads/": "data/uploads/"},
	}
	dbInfo, err := os.Stat(tmpPath)
	if err != nil {
		failFor(w, err)
		return
	}
	for _, b := range blobs {
		man.Files = append(man.Files, backupFile{b.path, int64(len(b.data))})
	}
	man.Files = append(man.Files, backupFile{"db/gamesdb.sqlite", dbInfo.Size()})
	for _, p := range uploads {
		if st, err := os.Stat(p); err == nil {
			rel, _ := filepath.Rel(s.Uploads.Root, p)
			man.Files = append(man.Files, backupFile{"uploads/" + filepath.ToSlash(rel), st.Size()})
		}
	}
	manJSON, _ := json.MarshalIndent(man, "", "  ")

	// From here on the response is committed; failures can only be logged.
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(6 * time.Hour))
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "utmdb-backup-"+man.CreatedAt.Format("20060102-150405")+".tar"))
	w.Header().Set("Cache-Control", "no-store")
	tw := tar.NewWriter(w)
	write := func(name string, size int64, mode int64, body io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: size, Mode: mode, ModTime: man.CreatedAt, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := io.CopyN(tw, body, size)
		return err
	}
	readme := backupReadme()
	if err := write("README.txt", int64(len(readme)), 0o644, bytes.NewReader(readme)); err != nil {
		s.Log.Error("backup: write", "err", err)
		return
	}
	if err := write("manifest.json", int64(len(manJSON)), 0o644, bytes.NewReader(manJSON)); err != nil {
		s.Log.Error("backup: write", "err", err)
		return
	}
	for _, b := range blobs {
		if err := write(b.path, int64(len(b.data)), 0o600, bytes.NewReader(b.data)); err != nil {
			s.Log.Error("backup: write", "path", b.path, "err", err)
			return
		}
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		s.Log.Error("backup: open copy", "err", err)
		return
	}
	err = write("db/gamesdb.sqlite", dbInfo.Size(), 0o600, f)
	f.Close()
	if err != nil {
		s.Log.Error("backup: write database", "err", err)
		return
	}
	for _, p := range uploads {
		// Attachment files are never modified after upload, so the size from
		// the open descriptor is the size that is copied.
		f, err := os.Open(p)
		if err != nil {
			continue // deleted since the walk; the manifest entry is harmless
		}
		st, err := f.Stat()
		if err == nil {
			rel, _ := filepath.Rel(s.Uploads.Root, p)
			err = write("uploads/"+filepath.ToSlash(rel), st.Size(), 0o600, f)
		}
		f.Close()
		if err != nil {
			s.Log.Error("backup: write upload", "path", p, "err", err)
			return
		}
	}
	if err := tw.Close(); err != nil {
		s.Log.Error("backup: finish", "err", err)
		return
	}
	s.Log.Info("backup streamed", "files", len(man.Files), "database_bytes", dbInfo.Size())
}

// envFile is the settings file the server was started with (.env in the
// working directory), or EnvFile when set (tests).
func (s *Server) envFile() string {
	if s.EnvFile != "" {
		return s.EnvFile
	}
	if _, err := os.Stat(".env"); err == nil {
		return ".env"
	}
	return ""
}

func (s *Server) schemaVersions(ctx context.Context) ([]string, error) {
	rows, err := s.Store.DB.Reader.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func backupReadme() []byte {
	return []byte(`UTM Database backup

This archive holds everything needed to bring the site back on another
machine except the IGDB catalog, which the server re-imports on its own.

  env                 the settings file, including SECRET_KEY (accounts are
                      keyed by it; without it every account is orphaned)
  tls/                the origin certificate and key
  dkim/               the DKIM signing key (its public half is in DNS)
  db/gamesdb.sqlite   a database with every user and maintainer table; the
                      catalog tables are present but empty
  uploads/            attachment files, at the paths the database refers to
  manifest.json       what is here, with row counts and sizes

Restore with deploy/macos/restore.sh <this file> as the service user. It
stops the service, puts the pieces in place and starts it again; the
server then imports the catalog and re-resolves covers in the background.
`)
}
