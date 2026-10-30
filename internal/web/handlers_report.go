package web

import (
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/cdn"
	"github.com/utmapp/utm-games-db/internal/status"
	"github.com/utmapp/utm-games-db/internal/store"
	"github.com/utmapp/utm-games-db/internal/upload"
)

type reportFormPage struct {
	Page
	Editing             bool
	LockedVersion       string // label of the UTM version, fixed when editing
	ReportID            int64
	CanDelete           bool
	DeletionPending     bool
	Action              string
	Query               string
	Hits                []store.CatalogHit
	Game                *store.GameDetail
	CoverURL            string
	Versions            []store.UTMVersion
	Ratings             []status.Rating
	Values              formValues
	Errors              map[string]string
	ExistingAttachments []store.Attachment
	Accept              string
	AcceptedLabel       string
	MaxFiles            int
	MaxFileBytes        int64
	MaxReportBytes      int64
}

func (s *Server) newReportFormPage(r *http.Request) (*reportFormPage, error) {
	versions, err := s.Store.ListVersions(r.Context())
	if err != nil {
		return nil, err
	}
	accept := make([]string, len(upload.Accepted))
	for i, e := range upload.Accepted {
		accept[i] = "." + e
	}
	return &reportFormPage{
		Page:           Page{Title: "Submit a report"},
		Action:         "/reports/new",
		Versions:       versions,
		Ratings:        status.AllRatings,
		Values:         formValues{},
		Errors:         map[string]string{},
		Accept:         strings.Join(accept, ","),
		AcceptedLabel:  upload.AcceptedLabel(),
		MaxFiles:       s.Cfg.UploadMaxFiles,
		MaxFileBytes:   s.Cfg.UploadMaxFileBytes,
		MaxReportBytes: s.Cfg.UploadMaxReportBytes,
	}, nil
}

// loadFormGame fills the selected game and its cover for the form.
func (s *Server) loadFormGame(r *http.Request, page *reportFormPage, gameID int64) error {
	g, err := s.Store.GetGame(r.Context(), gameID)
	if err != nil {
		return err
	}
	page.Game = g
	switch {
	case g.CoverBig != "":
		page.CoverURL = "/covers/" + g.CoverBig
	case g.CoverImageID != "":
		page.CoverURL = s.Syncer.Client.ImageURL(g.CoverImageID, "cover_big")
	case s.Limiter.Allow(fmt.Sprintf("search:user:%d", auth.UserFrom(r.Context()).ID), s.Cfg.RateSearchPerMinute, time.Minute):
		// Fetching a cover the mirror lacks costs an IGDB request, so it
		// shares the per-user search budget.
		imageID, err := s.Syncer.PeekCover(r.Context(), gameID)
		if err != nil {
			s.Log.Warn("peek cover", "game", gameID, "err", err)
		} else if imageID != "" {
			page.CoverURL = s.Syncer.Client.ImageURL(imageID, "cover_big")
		}
	}
	return nil
}

func (s *Server) handleReportNew(w http.ResponseWriter, r *http.Request) {
	page, err := s.newReportFormPage(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	q := r.URL.Query()
	if gid, _ := strconv.ParseInt(q.Get("game"), 10, 64); gid > 0 {
		if err := s.loadFormGame(r, page, gid); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				page.Error = "That game is not in the catalog."
			} else {
				s.serverError(w, r, err)
				return
			}
		}
		if len(page.Versions) == 0 {
			page.Error = "No UTM versions are configured yet, so reports cannot be submitted. Please check back later."
		}
	} else if query := strings.TrimSpace(q.Get("q")); query != "" {
		page.Query = limitLen(query, 100)
		if s.Limiter.Allow(fmt.Sprintf("search:user:%d", auth.UserFrom(r.Context()).ID), s.Cfg.RateSearchPerMinute, time.Minute) {
			page.Hits, err = s.Store.SearchCatalog(r.Context(), page.Query, 20)
			if err != nil {
				s.serverError(w, r, err)
				return
			}
		} else {
			page.Error = "Too many searches; slow down a little."
		}
	}
	s.private(w, r, "report_form", http.StatusOK, page)
}

// acceptsAttachments reports whether a report may carry attachments: only
// reports describing an issue do, so never Gold or "unknown issue" reports.
// The form shows the upload field under the same condition.
func acceptsAttachments(in store.ReportInput) bool {
	return in.Rating != status.Gold && in.IssueType != "unknown"
}

// parseUploads validates and stores attachments from a multipart form.
func (s *Server) parseUploads(r *http.Request, userID int64, errs map[string]string) ([]store.AttachmentInput, []string) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	files := r.MultipartForm.File["attachments"]
	var saved []store.AttachmentInput
	var paths []string
	if len(files) > s.Cfg.UploadMaxFiles {
		errs["attachments"] = fmt.Sprintf("At most %d files per report.", s.Cfg.UploadMaxFiles)
		return nil, nil
	}
	var total int64
	for _, fh := range files {
		if fh.Size == 0 {
			continue
		}
		if fh.Size > s.Cfg.UploadMaxFileBytes {
			errs["attachments"] = fmt.Sprintf("%s is larger than the %d MB limit.", limitLen(fh.Filename, 100), s.Cfg.UploadMaxFileBytes>>20)
			break
		}
		total += fh.Size
		if total > s.Cfg.UploadMaxReportBytes {
			errs["attachments"] = fmt.Sprintf("Attachments exceed %d MB in total.", s.Cfg.UploadMaxReportBytes>>20)
			break
		}
		in, path, err := s.saveUpload(fh, userID)
		if err != nil {
			switch {
			case errors.Is(err, upload.ErrEmpty):
				continue
			case errors.Is(err, upload.ErrType):
				errs["attachments"] = fmt.Sprintf("%s is not an accepted file type (%s), or its contents do not match its extension.", limitLen(fh.Filename, 100), upload.AcceptedLabel())
			case errors.Is(err, upload.ErrTooLarge):
				errs["attachments"] = fmt.Sprintf("%s is larger than the %d MB limit.", limitLen(fh.Filename, 100), s.Cfg.UploadMaxFileBytes>>20)
			default:
				s.Log.Error("save upload", "err", err)
				errs["attachments"] = "Could not store the attachment. Please try again."
			}
			break
		}
		saved = append(saved, *in)
		paths = append(paths, path)
	}
	if errs["attachments"] != "" {
		s.Uploads.RemoveAll(paths, s.Log)
		return nil, nil
	}
	return saved, paths
}

func (s *Server) saveUpload(fh *multipart.FileHeader, userID int64) (*store.AttachmentInput, string, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	saved, err := s.Uploads.Save(fmt.Sprintf("u%d", userID), fh.Filename, f, s.Cfg.UploadMaxFileBytes)
	if err != nil {
		return nil, "", err
	}
	return &store.AttachmentInput{Filename: limitLen(fh.Filename, 200), MIME: saved.MIME, Size: saved.Size, SHA256: saved.SHA256, Path: saved.RelPath}, saved.RelPath, nil
}

// readReportForm parses the multipart body with a hard size cap.
func (s *Server) readReportForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, s.Cfg.UploadMaxReportBytes+(2<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		return err
	}
	return nil
}

func (s *Server) handleReportCreate(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	page, err := s.newReportFormPage(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.readReportForm(w, r); err != nil {
		page.Error = "The upload is too large or the form could not be read. Keep attachments under the limits and try again."
		s.private(w, r, "report_form", http.StatusRequestEntityTooLarge, page)
		return
	}
	v := r.MultipartForm.Value
	form := url.Values(v)
	gameID, _ := strconv.ParseInt(form.Get("game_id"), 10, 64)
	if err := s.loadFormGame(r, page, gameID); err != nil {
		page.Error = "Choose a game first."
		s.private(w, r, "report_form", http.StatusBadRequest, page)
		return
	}
	page.Values = valuesFrom(form, reportKeys...)
	in := parseReport(form, page.Errors)
	in.GameID = gameID
	in.UserID = user.ID
	in.IP = s.clientIP(r)
	in.UserAgent = limitLen(r.UserAgent(), 300)
	if len(page.Errors) > 0 {
		page.Error = "Please fix the highlighted fields."
		s.private(w, r, "report_form", http.StatusBadRequest, page)
		return
	}
	msg, snippets := s.spamCheck(r.Context(), r, user, "report", "report", s.Cfg.RateReportsPerHour, form.Get("website"),
		map[string]string{"description": in.Description, "repro": in.Repro}, s.Cfg.AbsURL(page.Game.Path()))
	if msg != "" {
		page.Error, page.ErrorList = msg, snippets
		s.private(w, r, "report_form", http.StatusUnprocessableEntity, page)
		return
	}
	var files []store.AttachmentInput
	var paths []string
	if acceptsAttachments(in) {
		files, paths = s.parseUploads(r, user.ID, page.Errors)
		if len(page.Errors) > 0 {
			page.Error = "Please fix the highlighted fields."
			s.private(w, r, "report_form", http.StatusBadRequest, page)
			return
		}
	}
	id, err := s.Store.CreateReport(r.Context(), in, files, s.quotas())
	if err != nil {
		s.Uploads.RemoveAll(paths, s.Log)
		if errors.Is(err, store.ErrQuota) {
			page.Errors["attachments"] = err.Error()
			page.Error = "Please fix the highlighted fields."
			s.private(w, r, "report_form", http.StatusBadRequest, page)
			return
		}
		if errors.Is(err, store.ErrUnknownVersion) {
			page.Errors["utm_version_id"] = "Choose a valid UTM version."
			page.Error = "Please fix the highlighted fields."
			s.private(w, r, "report_form", http.StatusBadRequest, page)
			return
		}
		s.serverError(w, r, err)
		return
	}
	// Resolve cover art / storefront links when this report activated the
	// game; later reports reuse them. Best effort and bounded, so a slow IGDB
	// cannot hold the response: failures are retried by the background
	// resolver.
	if !page.Game.Active {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		if err := s.Syncer.ResolveDetails(ctx, gameID); err != nil {
			s.Log.Warn("resolve game details", "game", gameID, "err", err)
		}
		cancel()
	}
	s.Purger.Purge("/", page.Game.Path(), store.ReportPath(gameID, page.Game.Slug, id))
	s.Log.Info("report created", "report", id, "game", gameID, "user", user.ID)
	cdn.Private(w)
	http.Redirect(w, r, store.ReportPath(gameID, page.Game.Slug, id), http.StatusSeeOther)
}

func (s *Server) quotas() store.Quotas {
	return store.Quotas{DailyBytes: s.Cfg.UploadQuotaDailyBytes, DailyFiles: s.Cfg.UploadQuotaDailyFiles, TotalBytes: s.Cfg.UploadQuotaTotalBytes}
}

// loadOwnReport fetches a report the current user authored (else nil).
func (s *Server) loadOwnReport(w http.ResponseWriter, r *http.Request) *store.Report {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return nil
	}
	rep, err := s.Store.GetReport(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (rep.UserID != auth.UserFrom(r.Context()).ID || rep.Anonymized || rep.Visibility == "removed")) {
		s.notFound(w, r)
		return nil
	}
	if err != nil {
		s.serverError(w, r, err)
		return nil
	}
	return rep
}

func reportToValues(rep *store.Report) formValues {
	v := formValues{
		"rating":               string(rep.Rating),
		"issue_type":           rep.IssueType,
		"description":          rep.Description,
		"repro":                rep.Repro,
		"wine_same_issue":      rep.WineSameIssue,
		"utm_version_id":       strconv.FormatInt(rep.Config.VersionID, 10),
		"guest_os":             rep.Config.GuestOS,
		"linux_distro":         rep.Config.LinuxDistro,
		"linux_version":        rep.Config.LinuxVersion,
		"other_os_name":        rep.Config.OtherOSName,
		"graphics_backend":     rep.Config.GraphicsBackend,
		"macos_version":        rep.Config.MacOSVersion,
		"device_model":         rep.Config.DeviceModel,
		"chip":                 rep.Config.Chip,
		"completion_confirmed": "",
	}
	if rep.PlayedToCompletion {
		v["completion_confirmed"] = "1"
	}
	if rep.Config.RAMGB > 0 {
		v["ram_gb"] = strconv.Itoa(rep.Config.RAMGB)
	}
	return v
}

// editFormPage prepares the report form for editing rep.
func (s *Server) editFormPage(r *http.Request, rep *store.Report) (*reportFormPage, error) {
	page, err := s.newReportFormPage(r)
	if err != nil {
		return nil, err
	}
	page.Title = "Edit report"
	page.Editing = true
	page.ReportID = rep.ID
	page.CanDelete = rep.CanDeleteDirectly()
	if page.DeletionPending, err = s.Store.HasOpenDeletionRequest(r.Context(), rep.ID); err != nil {
		return nil, err
	}
	page.Action = fmt.Sprintf("/reports/%d/edit", rep.ID)
	page.ExistingAttachments = rep.Attachments
	page.LockedVersion = rep.Config.Version.Label()
	return page, s.loadFormGame(r, page, rep.GameID)
}

func (s *Server) handleReportEdit(w http.ResponseWriter, r *http.Request) {
	rep := s.loadOwnReport(w, r)
	if rep == nil {
		return
	}
	page, err := s.editFormPage(r, rep)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	page.Values = reportToValues(rep)
	s.private(w, r, "report_form", http.StatusOK, page)
}

func (s *Server) handleReportUpdate(w http.ResponseWriter, r *http.Request) {
	rep := s.loadOwnReport(w, r)
	if rep == nil {
		return
	}
	user := auth.UserFrom(r.Context())
	page, err := s.editFormPage(r, rep)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.readReportForm(w, r); err != nil {
		page.Values = reportToValues(rep)
		page.Error = "The upload is too large or the form could not be read."
		s.private(w, r, "report_form", http.StatusRequestEntityTooLarge, page)
		return
	}
	form := url.Values(r.MultipartForm.Value)
	// Rating, UTM version and guest OS are fixed once a report exists: they
	// decide which version group and status the report counts towards.
	form.Set("rating", string(rep.Rating))
	form.Set("utm_version_id", fmt.Sprint(rep.Config.VersionID))
	form.Set("guest_os", rep.Config.GuestOS)
	if rep.Rating == status.Gold {
		form.Set("completion_confirmed", "1")
	}
	page.Values = valuesFrom(form, reportKeys...)
	in := parseReport(form, page.Errors)
	in.GameID = rep.GameID
	in.UserID = user.ID
	in.IP = s.clientIP(r)
	in.UserAgent = limitLen(r.UserAgent(), 300)
	if len(page.Errors) > 0 {
		page.Error = "Please fix the highlighted fields."
		s.private(w, r, "report_form", http.StatusBadRequest, page)
		return
	}
	msg, snippets := s.spamCheck(r.Context(), r, user, "report", "report_edit", s.Cfg.RateReportsPerHour, form.Get("website"),
		map[string]string{"description": in.Description, "repro": in.Repro}, s.Cfg.AbsURL(rep.Path()))
	if msg != "" {
		page.Error, page.ErrorList = msg, snippets
		s.private(w, r, "report_form", http.StatusUnprocessableEntity, page)
		return
	}
	var remove []int64
	for _, v := range form["remove_attachment"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			remove = append(remove, id)
		}
	}
	var files []store.AttachmentInput
	var paths []string
	if acceptsAttachments(in) {
		files, paths = s.parseUploads(r, user.ID, page.Errors)
		if len(page.Errors) > 0 {
			page.Error = "Please fix the highlighted fields."
			s.private(w, r, "report_form", http.StatusBadRequest, page)
			return
		}
	}
	removed, err := s.Store.UpdateReport(r.Context(), rep.ID, in, files, remove, s.quotas())
	if err != nil {
		s.Uploads.RemoveAll(paths, s.Log)
		if errors.Is(err, store.ErrQuota) {
			page.Errors["attachments"] = err.Error()
			page.Error = "Please fix the highlighted fields."
			s.private(w, r, "report_form", http.StatusBadRequest, page)
			return
		}
		s.serverError(w, r, err)
		return
	}
	s.Uploads.RemoveAll(removed, s.Log)
	s.Purger.Purge("/", rep.GamePath(), rep.Path())
	cdn.Private(w)
	http.Redirect(w, r, rep.Path(), http.StatusSeeOther)
}

func (s *Server) handleReportDelete(w http.ResponseWriter, r *http.Request) {
	rep := s.loadOwnReport(w, r)
	if rep == nil {
		return
	}
	if err := r.ParseForm(); err != nil || r.PostFormValue("confirm") != "1" {
		cdn.Private(w)
		http.Redirect(w, r, fmt.Sprintf("/reports/%d/edit", rep.ID), http.StatusSeeOther)
		return
	}
	if !rep.CanDeleteDirectly() {
		s.errorPage(w, r, http.StatusForbidden, "Review needed", "This report can only be removed by request. Use \"Request deletion\" on the edit page.")
		return
	}
	paths, err := s.Store.DeleteOwnReport(r.Context(), rep.ID, auth.UserFrom(r.Context()).ID)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Uploads.RemoveAll(paths, s.Log)
	s.Purger.Purge("/", rep.GamePath(), rep.Path())
	s.Log.Info("report deleted by author", "report", rep.ID, "user", rep.UserID)
	cdn.Private(w)
	http.Redirect(w, r, "/account?deleted=1", http.StatusSeeOther)
}

func (s *Server) handleReportDeleteRequest(w http.ResponseWriter, r *http.Request) {
	rep := s.loadOwnReport(w, r)
	if rep == nil {
		return
	}
	u := auth.UserFrom(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil || r.PostFormValue("confirm") != "1" {
		cdn.Private(w)
		http.Redirect(w, r, fmt.Sprintf("/reports/%d/edit", rep.ID), http.StatusSeeOther)
		return
	}
	reason := limitLen(strings.TrimSpace(r.PostFormValue("reason")), 2000)
	msg, snippets := s.spamCheck(r.Context(), r, u, "deletion_request", "comment", s.Cfg.RateCommentsPerHour, r.PostFormValue("website"), map[string]string{"reason": reason}, s.Cfg.AbsURL(rep.Path()))
	if msg != "" {
		s.errorPage(w, r, http.StatusUnprocessableEntity, "Request not sent", msg+" "+strings.Join(snippets, ", "))
		return
	}
	err := s.Store.CreateDeletionRequest(r.Context(), rep.ID, u.ID, reason)
	if err != nil && !errors.Is(err, store.ErrDeletionRequested) {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("report deletion requested", "report", rep.ID, "user", u.ID)
	cdn.Private(w)
	http.Redirect(w, r, "/account?deletion_requested=1", http.StatusSeeOther)
}

func (s *Server) handleCommentDelete(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil || r.PostFormValue("confirm") != "1" {
		s.errorPage(w, r, http.StatusBadRequest, "Bad request", "Missing confirmation.")
		return
	}
	reportID, err := s.Store.DeleteOwnComment(r.Context(), id, u.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back := fmt.Sprintf("/reports/%d", reportID)
	if rep, err := s.Store.GetReport(r.Context(), reportID); err == nil {
		back = rep.Path()
		s.Purger.Purge("/", rep.GamePath(), back)
	}
	cdn.Private(w)
	http.Redirect(w, r, back+"#comments", http.StatusSeeOther)
}
