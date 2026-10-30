package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/antispam"
	"github.com/utmapp/utm-games-db/internal/status"
	"github.com/utmapp/utm-games-db/internal/store"
)

// formValues mirrors submitted fields back into templates.
type formValues map[string]string

func valuesFrom(v url.Values, keys ...string) formValues {
	out := formValues{}
	for _, k := range keys {
		out[k] = strings.TrimSpace(v.Get(k))
	}
	return out
}

var configKeys = []string{"utm_version_id", "guest_os", "linux_distro", "linux_version", "other_os_name", "graphics_backend", "macos_version", "device_model", "chip", "ram_gb"}

func limitLen(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// parseConfig validates the shared configuration fields.
func parseConfig(v url.Values, errs map[string]string) store.ConfigInput {
	var c store.ConfigInput
	c.VersionID, _ = strconv.ParseInt(strings.TrimSpace(v.Get("utm_version_id")), 10, 64)
	if c.VersionID <= 0 {
		errs["utm_version_id"] = "Choose the UTM version you tested with."
	}
	c.GuestOS = v.Get("guest_os")
	switch c.GuestOS {
	case "windows":
	case "linux":
		c.LinuxDistro = limitLen(strings.TrimSpace(v.Get("linux_distro")), 80)
		c.LinuxVersion = limitLen(strings.TrimSpace(v.Get("linux_version")), 40)
		if c.LinuxDistro == "" {
			errs["linux_distro"] = "Tell us which distribution you used."
		}
	case "other":
		c.OtherOSName = limitLen(strings.TrimSpace(v.Get("other_os_name")), 80)
		if c.OtherOSName == "" {
			errs["other_os_name"] = "Name the operating system."
		}
	default:
		errs["guest_os"] = "Choose the guest operating system."
	}
	c.GraphicsBackend = v.Get("graphics_backend")
	if c.GraphicsBackend == "" {
		c.GraphicsBackend = "unknown"
	}
	if !slices.Contains(store.GraphicsBackends(c.GuestOS), c.GraphicsBackend) {
		errs["graphics_backend"] = "Pick a graphics backend available for this guest OS."
	}
	c.MacOSVersion = limitLen(strings.TrimSpace(v.Get("macos_version")), 40)
	c.DeviceModel = limitLen(strings.TrimSpace(v.Get("device_model")), 80)
	c.Chip = limitLen(strings.TrimSpace(v.Get("chip")), 40)
	if ram := strings.TrimSpace(v.Get("ram_gb")); ram != "" {
		n, err := strconv.Atoi(ram)
		if err != nil || n <= 0 || n > 4096 {
			errs["ram_gb"] = "Enter memory in whole gigabytes."
		} else {
			c.RAMGB = n
		}
	}
	for _, f := range []struct{ key, val string }{{"linux_distro", c.LinuxDistro}, {"linux_version", c.LinuxVersion}, {"other_os_name", c.OtherOSName}, {"macos_version", c.MacOSVersion}, {"device_model", c.DeviceModel}, {"chip", c.Chip}} {
		if len(antispam.FindLinks(f.val, 1)) > 0 {
			errs[f.key] = "Links and HTML are not allowed here."
		}
	}
	return c
}

var reportKeys = append([]string{"rating", "completion_confirmed", "issue_type", "description", "repro", "wine_same_issue"}, configKeys...)

// parseReport validates the questionnaire with its branching rules.
func parseReport(v url.Values, errs map[string]string) store.ReportInput {
	var in store.ReportInput
	in.Rating = status.Rating(v.Get("rating"))
	if !in.Rating.Valid() {
		errs["rating"] = "Choose a rating."
	}
	in.Config = parseConfig(v, errs)
	if in.Rating == status.Gold {
		if v.Get("completion_confirmed") != "1" {
			errs["completion_confirmed"] = "Please confirm you played the game to completion, or choose a different rating."
		}
		in.PlayedToCompletion = true
		return in
	}
	in.IssueType = v.Get("issue_type")
	switch in.IssueType {
	case "", "unknown":
		in.IssueType = "unknown"
		return in
	case "hang_crash", "graphics", "performance", "workaround", "other":
	default:
		errs["issue_type"] = "Choose the kind of issue."
		return in
	}
	in.Description = strings.TrimSpace(strings.ReplaceAll(v.Get("description"), "\r\n", "\n"))
	in.Repro = strings.TrimSpace(strings.ReplaceAll(v.Get("repro"), "\r\n", "\n"))
	needsDetails := in.IssueType == "workaround" || in.IssueType == "other"
	if needsDetails && len(in.Description) < 10 {
		errs["description"] = "Please add a few sentences of detail for this kind of issue."
	}
	if len(in.Description) > 20000 {
		errs["description"] = "Description is too long (20,000 characters max)."
	}
	if len(in.Repro) > 20000 {
		errs["repro"] = "Steps are too long (20,000 characters max)."
	}
	in.WineSameIssue = v.Get("wine_same_issue")
	switch in.WineSameIssue {
	case "yes", "no", "unknown":
	case "":
		in.WineSameIssue = "unknown"
	default:
		errs["wine_same_issue"] = "Choose an answer."
	}
	return in
}

// spamCheck runs the synchronous anti-spam pipeline over free-form fields.
// It returns a user-facing error and (for links) the offending snippets.
// kind is "report", "comment", "plus_one" or "triage_request".
func (s *Server) spamCheck(ctx context.Context, r *http.Request, user *store.User, kind string, rateKey string, perHour int, honeypot string, fields map[string]string, permalink string) (msg string, snippets []string) {
	if honeypot != "" {
		return "Your submission could not be accepted.", nil
	}
	for _, text := range fields {
		snippets = append(snippets, antispam.FindLinks(text, 5)...)
	}
	if len(snippets) > 0 {
		if len(snippets) > 5 {
			snippets = snippets[:5]
		}
		return "Links and HTML are not allowed in submissions. Remove the following and try again:", snippets
	}
	ip := s.clientIP(r)
	if !s.Limiter.Allow(fmt.Sprintf("%s:user:%d", rateKey, user.ID), perHour, time.Hour) || !s.Limiter.Allow(rateKey+":ip:"+ip, perHour*2, time.Hour) {
		return "You are submitting too quickly. Please wait a while and try again.", nil
	}
	// Anything that stores user-written content also keeps a minimum gap
	// between submissions, across all kinds, to slow down scripted posting.
	if iv := s.Cfg.RateContentInterval; iv > 0 && !s.Limiter.Allow(fmt.Sprintf("content:user:%d", user.ID), 1, iv) {
		return fmt.Sprintf("Please wait %d seconds between submissions.", int(iv.Seconds())), nil
	}
	content := antispam.JoinFields(fields)
	if !s.Akismet.Enabled() || content == "" {
		return "", nil
	}
	verdict, err := s.Akismet.Check(ctx, antispam.Comment{
		UserIP: ip, UserAgent: r.UserAgent(), Referrer: r.Referer(), Type: kindToAkismet(kind),
		Author: user.DisplayName, Content: content, Permalink: permalink,
	})
	if err != nil {
		s.Log.Warn("akismet check failed", "err", err)
		if s.Cfg.AkismetFailClosed {
			return "The spam check is temporarily unavailable. Please try again later.", nil
		}
		return "", nil
	}
	if verdict.Spam {
		payload, _ := json.Marshal(map[string]any{"fields": fields, "ip": ip, "ua": r.UserAgent(), "referrer": r.Referer(), "author": user.DisplayName, "permalink": permalink, "type": kindToAkismet(kind)})
		result := "spam"
		if verdict.Discard {
			result = "discard"
			if err := s.Store.AddSpamStrike(ctx, user.ID); err != nil {
				s.Log.Warn("spam strike", "err", err)
			}
		}
		if err := s.Store.LogSpam(ctx, user.ID, kind, string(payload), result); err != nil {
			s.Log.Warn("spam log", "err", err)
		}
		return "Your submission was flagged as spam. Please edit it and try again; if you think this is a mistake, keep the text short and factual.", nil
	}
	return "", nil
}

func kindToAkismet(kind string) string {
	switch kind {
	case "comment":
		return "comment"
	default:
		return "forum-post"
	}
}
