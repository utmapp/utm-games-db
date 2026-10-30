package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/config"
	"github.com/utmapp/utm-games-db/internal/store"
)

type donatePage struct {
	Page
	Game        *store.GameDetail
	GiftOptions []giftOption // configured accounts on storefronts this game is sold on
	Values      formValues
	Errors      map[string]string
	Done        bool
}

// giftOption pairs a configured gift account with the game's page on that
// storefront.
type giftOption struct {
	config.GiftAccount
	StoreURL     string
	StoreLabel   string
	Instructions string
}

// Built-in gifting steps per storefront; TRIAGE_GIFT_ACCOUNTS can override.
var giftInstructions = map[string]string{
	"steam": "Open the store page and choose “Buy as a gift”. The gift can be sent directly to the email address above. You may also add the profile above as friend and send the gift after the request is accepted.",
	"gog":   "Open the store page, add the game to your cart and choose the gift option at checkout. The gift can be sent directly to the email address above.",
	"epic":  "Epic only allows gifts between friends who have been connected for at least two days, and the sender needs two-factor authentication enabled. Send a friend request to the account above first; once accepted and the waiting period has passed, open the store page, choose “Buy as a gift” and pick the account as the recipient.",
	"itch":  "Open the store page, choose “Buy Now” and tick “Give this as a gift”. The gift can be sent directly to the email address above.",
}

// giftOptions keeps only the configured accounts whose storefront the game
// is actually listed on.
func giftOptionsFor(accounts []config.GiftAccount, g *store.GameDetail) []giftOption {
	var out []giftOption
	for _, a := range accounts {
		for _, w := range g.Websites {
			if w.Storefront != a.Storefront {
				continue
			}
			o := giftOption{GiftAccount: a, StoreURL: w.URL, StoreLabel: w.Label, Instructions: a.Instructions}
			if o.Instructions == "" {
				o.Instructions = giftInstructions[a.Storefront]
			}
			if o.Instructions == "" {
				o.Instructions = "Open the store page and buy the game as a gift for the account above."
			}
			out = append(out, o)
			break
		}
	}
	return out
}

func (s *Server) loadDonatePage(w http.ResponseWriter, r *http.Request) *donatePage {
	id, ok := parseGameRef(r.PathValue("id"))
	if !ok {
		s.notFound(w, r)
		return nil
	}
	g, err := s.Store.GetGame(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !g.Active) {
		s.notFound(w, r)
		return nil
	}
	if err != nil {
		s.serverError(w, r, err)
		return nil
	}
	return &donatePage{Page: Page{Title: "Donate a copy"}, Game: g, GiftOptions: giftOptionsFor(s.Cfg.TriageGiftAccounts, g), Values: formValues{}, Errors: map[string]string{}}
}

func (s *Server) handleDonateForm(w http.ResponseWriter, r *http.Request) {
	page := s.loadDonatePage(w, r)
	if page == nil {
		return
	}
	if page.Game.TriageAvailable {
		page.Flash = "Auto-Triage is already available for this game."
	}
	s.private(w, r, "donate", http.StatusOK, page)
}

func (s *Server) handleDonateSubmit(w http.ResponseWriter, r *http.Request) {
	page := s.loadDonatePage(w, r)
	if page == nil {
		return
	}
	u := auth.UserFrom(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		page.Error = "Could not read the form."
		s.private(w, r, "donate", http.StatusBadRequest, page)
		return
	}
	page.Values = valuesFrom(r.PostForm, "kind", "storefront", "demo_url", "notes")
	kind := page.Values["kind"]
	storefront, demo := "", ""
	notes := limitLen(strings.TrimSpace(r.PostFormValue("notes")), 2000)
	switch kind {
	case "gift":
		storefront = page.Values["storefront"]
		if !slices.ContainsFunc(page.GiftOptions, func(o giftOption) bool { return o.Storefront == storefront }) {
			if len(page.GiftOptions) == 0 {
				page.Error = "This game is not sold on a storefront we can receive gifts on. Please provide a demo link instead."
			} else {
				page.Error = "Choose which account you gifted the game to."
			}
		}
	case "demo":
		demo = strings.TrimSpace(page.Values["demo_url"])
		pu, err := url.Parse(demo)
		if demo == "" || err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" || len(demo) > 500 {
			page.Errors["demo_url"] = "Enter a valid http(s) link to the demo."
		}
	default:
		page.Error = "Choose how we can get the game."
	}
	if len(page.Errors) > 0 && page.Error == "" {
		page.Error = "Please fix the highlighted fields."
	}
	if page.Error != "" {
		s.private(w, r, "donate", http.StatusBadRequest, page)
		return
	}
	// Notes must not carry links; the demo URL is the one allowed link.
	msg, snippets := s.spamCheck(r.Context(), r, u, "triage_request", "triage", s.Cfg.RateCommentsPerHour, r.PostFormValue("website"), map[string]string{"notes": notes}, s.Cfg.AbsURL(page.Game.Path()))
	if msg != "" {
		page.Error, page.ErrorList = msg, snippets
		s.private(w, r, "donate", http.StatusUnprocessableEntity, page)
		return
	}
	if _, err := s.Store.CreateTriageRequest(r.Context(), page.Game.ID, u.ID, kind, storefront, demo, notes); err != nil {
		s.serverError(w, r, err)
		return
	}
	page.Done = true
	s.private(w, r, "donate", http.StatusOK, page)
}
