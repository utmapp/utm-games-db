// Package store contains the typed queries and transactional domain
// operations shared by the public site and the internal API.
package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/status"
)

// User is a signed-in account.
type User struct {
	ID           int64      `json:"id"`
	DisplayName  string     `json:"display_name"`
	Banned       bool       `json:"banned"`
	SpamStrikes  int        `json:"spam_strikes"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  time.Time  `json:"last_login_at"`
	LastExportAt *time.Time `json:"last_export_at,omitempty"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"` // account deleted; row kept for moderation
}

// UTMVersion is one entry of the curated release list.
type UTMVersion struct {
	ID           int64      `json:"id"`
	Version      string     `json:"version"`
	Major        int        `json:"-"`
	Minor        int        `json:"-"`
	Patch        int        `json:"-"`
	IsPrerelease bool       `json:"prerelease"`
	ReleasedAt   *time.Time `json:"released_at,omitempty"`
}

// Label renders "4.6.5" (plus " beta" for prereleases).
func (v UTMVersion) Label() string {
	if v.ID == 0 {
		return "unknown version"
	}
	if v.IsPrerelease {
		return v.Version + " beta"
	}
	return v.Version
}

// nullVersion scans the utm_versions columns (id, version, v_major,
// v_minor, v_patch, is_prerelease) of an outer join, which may all be NULL.
type nullVersion struct {
	id, major, minor, patch sql.NullInt64
	version                 sql.NullString
	pre                     sql.NullBool
}

// dest returns the scan destinations in column order.
func (n *nullVersion) dest() []any {
	return []any{&n.id, &n.version, &n.major, &n.minor, &n.patch, &n.pre}
}

// get returns the version, or nil when the join matched nothing.
func (n *nullVersion) get() *UTMVersion {
	if !n.id.Valid {
		return nil
	}
	return &UTMVersion{ID: n.id.Int64, Version: n.version.String, Major: int(n.major.Int64), Minor: int(n.minor.Int64), Patch: int(n.patch.Int64), IsPrerelease: n.pre.Bool}
}

// Ver converts to the status package's comparable form.
func (v UTMVersion) Ver() status.Version {
	return status.Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch}
}

// Website is a storefront link.
type Website struct {
	Storefront   string `json:"storefront"`
	StorefrontID string `json:"storefront_id,omitempty"`
	URL          string `json:"url"`
	Label        string `json:"label"`
}

// GameCard is a row on the list page.
type GameCard struct {
	ID              int64         `json:"id"`
	Name            string        `json:"name"`
	Slug            string        `json:"slug"`
	Year            int           `json:"year,omitempty"`
	Status          status.Rating `json:"status"`
	StatusVersion   *UTMVersion   `json:"status_version,omitempty"`
	ReportCount     int           `json:"report_count"`
	StarCount       int           `json:"star_count"`
	TriageAvailable bool          `json:"triage_available"`
	CoverSmall      string        `json:"cover_small,omitempty"` // local file name
	UpdatedAt       time.Time     `json:"updated_at"`
}

// GamePath builds the canonical game page path, /games/{id}-{slug}. The
// slug is decorative: /games/{id} redirects to the canonical form.
func GamePath(id int64, slug string) string {
	if slug == "" {
		return fmt.Sprintf("/games/%d", id)
	}
	return fmt.Sprintf("/games/%d-%s", id, slug)
}

// ReportPath builds the canonical report path under its game.
func ReportPath(gameID int64, slug string, reportID int64) string {
	return fmt.Sprintf("%s/reports/%d", GamePath(gameID, slug), reportID)
}

// Path is the canonical page path.
func (g GameCard) Path() string { return GamePath(g.ID, g.Slug) }

// GameDetail is everything the game page needs besides reports.
type GameDetail struct {
	GameCard
	Summary      string    `json:"summary"`
	IGDBURL      string    `json:"igdb_url"`
	GameType     string    `json:"game_type,omitempty"`
	Genres       []string  `json:"genres,omitempty"`
	Engines      []string  `json:"engines,omitempty"`
	Websites     []Website `json:"websites"`
	CoverBig     string    `json:"cover_big,omitempty"`
	CoverImageID string    `json:"cover_image_id,omitempty"`
	ActivatedAt  time.Time `json:"activated_at"`
	TriageSource string    `json:"triage_source,omitempty"`
	Active       bool      `json:"active"`
}

// CatalogHit is a search result in the report form.
type CatalogHit struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Year     int    `json:"year,omitempty"`
	GameType string `json:"game_type,omitempty"`
	Active   bool   `json:"active"`
}

// Configuration is the environment a report/comment/+1 was made on.
type Configuration struct {
	ID              int64      `json:"id"`
	VersionID       int64      `json:"version_id"`
	Version         UTMVersion `json:"version"`
	GuestOS         string     `json:"guest_os"` // windows | linux | other
	LinuxDistro     string     `json:"linux_distro,omitempty"`
	LinuxVersion    string     `json:"linux_version,omitempty"`
	OtherOSName     string     `json:"other_os_name,omitempty"`
	GraphicsBackend string     `json:"graphics_backend,omitempty"` // unknown | dxmt | d3dmetal
	MacOSVersion    string     `json:"macos_version,omitempty"`
	DeviceModel     string     `json:"device_model,omitempty"`
	Chip            string     `json:"chip,omitempty"`
	RAMGB           int        `json:"ram_gb,omitempty"`
}

// GuestOSLabel is the display name of the guest OS.
func (c Configuration) GuestOSLabel() string {
	switch c.GuestOS {
	case "windows":
		return "Windows"
	case "linux":
		s := "Linux"
		if c.LinuxDistro != "" {
			s += " · " + c.LinuxDistro
			if c.LinuxVersion != "" {
				s += " " + c.LinuxVersion
			}
		}
		return s
	case "other":
		if c.OtherOSName != "" {
			return c.OtherOSName
		}
		return "Other OS"
	}
	return c.GuestOS
}

// GraphicsBackends lists the backend keys valid for a guest OS. Windows
// guests only get the Direct3D translation layers; Linux and other guests
// also get the OpenGL/Vulkan backends.
func GraphicsBackends(guestOS string) []string {
	base := []string{"unknown", "dxmt", "d3dmetal"}
	if guestOS == "windows" {
		return base
	}
	return append(base, "moltenvk", "kosmickrisp", "angle_metal", "angle_opengl", "core_gl")
}

// GraphicsLabel is the display name of a graphics backend key.
func GraphicsLabel(key string) string {
	switch key {
	case "unknown":
		return "Unknown"
	case "dxmt":
		return "DXMT"
	case "d3dmetal":
		return "D3DMetal"
	case "moltenvk":
		return "MoltenVK"
	case "kosmickrisp":
		return "KosmicKrisp"
	case "angle_metal":
		return "ANGLE (Metal)"
	case "angle_opengl":
		return "ANGLE (OpenGL)"
	case "core_gl":
		return "Core GL"
	}
	return ""
}

// GraphicsLabel is the display name of the configuration's graphics backend.
func (c Configuration) GraphicsLabel() string {
	if c.GraphicsBackend == "unknown" {
		return "Unknown backend"
	}
	return GraphicsLabel(c.GraphicsBackend)
}

// HostLabel summarises the host hardware fields that were given.
func (c Configuration) HostLabel() string {
	var parts []string
	if c.MacOSVersion != "" {
		parts = append(parts, "macOS "+c.MacOSVersion)
	}
	if c.DeviceModel != "" {
		parts = append(parts, c.DeviceModel)
	}
	if c.Chip != "" {
		parts = append(parts, c.Chip)
	}
	if c.RAMGB > 0 {
		parts = append(parts, fmt.Sprintf("%d GB", c.RAMGB))
	}
	return strings.Join(parts, " · ")
}

// Attachment is an uploaded file (metadata only; the file itself is private).
type Attachment struct {
	ID        int64     `json:"id"`
	ReportID  int64     `json:"report_id"`
	UserID    int64     `json:"user_id"`
	Filename  string    `json:"filename"`
	MIME      string    `json:"mime"`
	Size      int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256"`
	Path      string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// Comment is a reply on a report.
type Comment struct {
	ID         int64          `json:"id"`
	ReportID   int64          `json:"report_id"`
	UserID     int64          `json:"user_id,omitempty"`
	Author     string         `json:"author"`
	AuthorKind string         `json:"author_kind"` // user | bot
	Body       string         `json:"body_md"`
	Config     *Configuration `json:"configuration,omitempty"`
	Visibility string         `json:"visibility"`
	CreatedAt  time.Time      `json:"created_at"`
}

// Report is a compatibility report with everything needed to render it.
type Report struct {
	ID                 int64         `json:"id"`
	GameID             int64         `json:"game_id"`
	GameName           string        `json:"game_name"`
	GameSlug           string        `json:"game_slug"`
	UserID             int64         `json:"user_id"`
	Author             string        `json:"author"`
	Anonymized         bool          `json:"anonymized,omitempty"` // author deleted their account; shown as "Deleted user"
	Rating             status.Rating `json:"rating"`
	PlayedToCompletion bool          `json:"played_to_completion"`
	Config             Configuration `json:"configuration"`
	IssueType          string        `json:"issue_type,omitempty"`
	Description        string        `json:"description_md"`
	Repro              string        `json:"repro_md"`
	WineSameIssue      string        `json:"wine_same_issue,omitempty"`
	Visibility         string        `json:"visibility"`
	TriageStatus       string        `json:"triage_status,omitempty"`
	PlusOneCount       int           `json:"plus_one_count"`
	CommentCount       int           `json:"comment_count"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
	EditedAt           *time.Time    `json:"edited_at,omitempty"`
	Attachments        []Attachment  `json:"attachments,omitempty"`
	Comments           []Comment     `json:"comments,omitempty"`
}

// Path is the canonical report page path (under its game).
func (r Report) Path() string { return ReportPath(r.GameID, r.GameSlug, r.ID) }

// GamePath is the parent game page path.
func (r Report) GamePath() string { return GamePath(r.GameID, r.GameSlug) }

// IssueLabel is the display name of the issue type.
func IssueLabel(t string) string {
	switch t {
	case "hang_crash":
		return "Hang / crash"
	case "graphics":
		return "Incorrect graphics"
	case "performance":
		return "Performance"
	case "workaround":
		return "Workaround needed"
	case "other":
		return "Other"
	case "unknown":
		return "Unknown"
	}
	return ""
}

// WineLabel is the display text for the CrossOver/Wine question.
func WineLabel(v string) string {
	switch v {
	case "yes":
		return "Yes, same issue under CrossOver/Wine"
	case "no":
		return "No, works under CrossOver/Wine"
	case "unknown":
		return "Not tested under CrossOver/Wine"
	}
	return ""
}

// TriageStatuses lists the valid triage states in display order.
var TriageStatuses = []string{"pending", "in_progress", "needs_clarification", "fix_proposed", "wont_fix", "cannot_reproduce"}

// ValidTriageStatus reports whether v is a known triage status.
func ValidTriageStatus(v string) bool { return slices.Contains(TriageStatuses, v) }

// TriageLabel is the display name of a triage status.
func TriageLabel(v string) string {
	switch v {
	case "pending":
		return "Pending"
	case "in_progress":
		return "In progress"
	case "needs_clarification":
		return "Needs clarification"
	case "fix_proposed":
		return "Fix proposed"
	case "wont_fix":
		return "Not to fix"
	case "cannot_reproduce":
		return "Cannot reproduce"
	}
	return ""
}

// TriageColor is the CSS color class suffix for a triage status.
func TriageColor(v string) string {
	switch v {
	case "in_progress":
		return "blue"
	case "needs_clarification":
		return "yellow"
	case "fix_proposed":
		return "green"
	case "wont_fix":
		return "red"
	}
	return "gray"
}

// VersionGroup is the set of reports made on one UTM version.
type VersionGroup struct {
	Version UTMVersion
	Reports []Report
	Newest  bool
}

// TriageRequest is a user's request to make a game available for Auto-Triage.
type TriageRequest struct {
	ID         int64     `json:"id"`
	GameID     int64     `json:"game_id"`
	GameName   string    `json:"game_name"`
	UserID     int64     `json:"user_id"`
	Kind       string    `json:"kind"` // gift | demo
	Storefront string    `json:"storefront,omitempty"`
	DemoURL    string    `json:"demo_url,omitempty"`
	Notes      string    `json:"notes,omitempty"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

// SpamEntry is a rejected submission kept for review.
type SpamEntry struct {
	ID            int64     `json:"id"`
	UserID        int64     `json:"user_id,omitempty"`
	Kind          string    `json:"kind"`
	Payload       string    `json:"payload"`
	AkismetResult string    `json:"akismet_result"`
	Feedback      string    `json:"feedback,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// OwnedEntry is one row pushed by the owned-games service.
type OwnedEntry struct {
	Storefront   string `json:"storefront"`
	StorefrontID string `json:"storefront_id"`
	Free         bool   `json:"free"`
	IGDBGameID   int64  `json:"igdb_game_id,omitempty"`
}

func unix(t int64) time.Time {
	if t == 0 {
		return time.Time{}
	}
	return time.Unix(t, 0).UTC()
}

func unixPtr(t *int64) *time.Time {
	if t == nil || *t == 0 {
		return nil
	}
	u := time.Unix(*t, 0).UTC()
	return &u
}
