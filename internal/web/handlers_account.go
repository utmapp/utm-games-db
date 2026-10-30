package web

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/cdn"
	"github.com/utmapp/utm-games-db/internal/store"
)

// exportSlots bounds concurrent export streams site-wide.
var exportSlots = make(chan struct{}, 2)

// handleAccountExport streams a ZIP with data.json and the user's attachment
// files. It is the only way to get an attachment back out of the site.
//
// Anti-abuse: POST with confirmation from a signed-in session only, no
// stored file and no URL that could be shared, `no-store`, one export per
// EXPORT_INTERVAL per user, and only attachments older than
// EXPORT_MIN_ATTACHMENT_AGE are included, so "upload now, export now" does
// not work as a file-transfer channel.
func (s *Server) handleAccountExport(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	// Take a slot first so a busy server does not burn the user's daily claim.
	select {
	case exportSlots <- struct{}{}:
		defer func() { <-exportSlots }()
	case <-time.After(5 * time.Second):
		s.renderAccount(w, r, http.StatusServiceUnavailable, "Exports are busy right now. Please try again in a few minutes.")
		return
	}
	if err := s.Store.ClaimExport(r.Context(), u.ID, s.Cfg.ExportInterval); err != nil {
		var tooSoon store.ErrExportTooSoon
		if errors.As(err, &tooSoon) {
			s.renderAccount(w, r, http.StatusTooManyRequests, fmt.Sprintf("You exported your data recently. The next export is available %s.", tooSoon.Next.UTC().Format("2 Jan 2006 15:04 MST")))
			return
		}
		s.serverError(w, r, err)
		return
	}
	cutoff := time.Now().Add(-s.Cfg.ExportMinAttachmentAge)
	ex, err := s.Store.ExportUserData(r.Context(), u.ID, cutoff)
	if err != nil {
		s.Store.ReleaseExport(r.Context(), u.ID) // nothing was delivered; let them retry
		s.serverError(w, r, err)
		return
	}
	// Large archives can take a while; lift the server's write deadline for
	// this response only.
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Minute))
	cdn.Private(w)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "utm-database-export-"+time.Now().UTC().Format("20060102")+".zip"))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	zw := zip.NewWriter(w)
	defer zw.Close()
	readme, _ := zw.Create("README.txt")
	fmt.Fprintf(readme, "UTM Database export for %s (%s)\n\n"+
		"data.json holds your account, reports, comments, +1s, stars, donation and deletion requests, and attachment metadata.\n"+
		"attachments/ holds the files you uploaded. Files uploaded less than %s before this export are listed in data.json but not included; export again later to receive them.\n",
		u.DisplayName, ex.ExportedAt.UTC().Format(time.RFC3339), s.Cfg.ExportMinAttachmentAge)
	dj, _ := zw.Create("data.json")
	enc := json.NewEncoder(dj)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ex); err != nil {
		s.Log.Error("export json", "err", err)
		return
	}
	for _, a := range ex.Attachments {
		if !a.Included {
			continue
		}
		f, err := s.Uploads.Open(a.Path)
		if err != nil {
			s.Log.Warn("export: attachment file missing", "attachment", a.ID)
			continue
		}
		hdr := &zip.FileHeader{Name: a.ArchivePath, Method: zip.Deflate, Modified: a.CreatedAt}
		out, err := zw.CreateHeader(hdr)
		if err == nil {
			_, err = io.Copy(out, f)
		}
		f.Close()
		if err != nil {
			s.Log.Warn("export: write attachment", "attachment", a.ID, "err", err)
			return
		}
	}
	s.Log.Info("account exported", "user", u.ID, "attachments", len(ex.Attachments))
}

// handleAccountDeleteAttachments removes every file the user uploaded.
func (s *Server) handleAccountDeleteAttachments(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if r.PostFormValue("confirm") != "1" {
		s.renderAccount(w, r, http.StatusBadRequest, "Please confirm before deleting your attachments.")
		return
	}
	paths, err := s.Store.DeleteAllUserAttachments(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Uploads.RemoveAll(paths, s.Log)
	s.Log.Info("user deleted all attachments", "user", u.ID, "files", len(paths))
	cdn.Private(w)
	http.Redirect(w, r, "/account?attachments_deleted=1", http.StatusSeeOther)
}

// handleAccountDelete removes the account and all of its data.
func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if r.PostFormValue("confirm") != "1" {
		s.renderAccount(w, r, http.StatusBadRequest, "Please confirm before deleting your account.")
		return
	}
	paths, pages, err := s.Store.DeleteUser(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Uploads.RemoveAll(paths, s.Log)
	s.Purger.Purge(append(pages, "/")...)
	s.Auth.ClearCookie(w)
	s.Log.Info("account deleted", "user", u.ID, "pages", len(pages), "files", len(paths))
	s.errorPage(w, r, http.StatusOK, "Account deleted", "Your account has been deleted. Thank you for contributing.")
}
