package igdb

import (
	"regexp"
	"strings"
)

var (
	steamRe = regexp.MustCompile(`(?i)store\.steampowered\.com/app/(\d+)`)
	gogRe   = regexp.MustCompile(`(?i)gog\.com/(?:[a-z]{2}/)?game/([A-Za-z0-9_-]+)`)
	epicRe  = regexp.MustCompile(`(?i)store\.epicgames\.com/(?:[a-z]{2}(?:-[A-Za-z]{2})?/)?p/([A-Za-z0-9_-]+)`)
	itchRe  = regexp.MustCompile(`(?i)https?://([a-z0-9-]+)\.itch\.io/([a-z0-9-]+)`)
)

// ParseStorefront extracts (storefront, id) from a store page URL. Steam ids
// are numeric app ids; GOG and Epic use the product slug; itch uses user/game.
func ParseStorefront(typeID int64, rawURL string) (store, id string, ok bool) {
	switch typeID {
	case WebsiteSteam:
		if m := steamRe.FindStringSubmatch(rawURL); m != nil {
			return "steam", m[1], true
		}
	case WebsiteGOG:
		if m := gogRe.FindStringSubmatch(rawURL); m != nil {
			return "gog", strings.ToLower(m[1]), true
		}
	case WebsiteEpic:
		if m := epicRe.FindStringSubmatch(rawURL); m != nil {
			return "epic", strings.ToLower(m[1]), true
		}
	case WebsiteItch:
		if m := itchRe.FindStringSubmatch(rawURL); m != nil {
			return "itch", strings.ToLower(m[1] + "/" + m[2]), true
		}
	}
	return "", "", false
}
