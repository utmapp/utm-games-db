package igdb

// Game is the subset of /v4/games we mirror. Field order matters: the JSON
// encoding of this struct is stored as raw_json and compared on resync.
type Game struct {
	ID               int64   `json:"id"`
	Checksum         string  `json:"checksum,omitempty"`
	Name             string  `json:"name"`
	Slug             string  `json:"slug"`
	Summary          string  `json:"summary,omitempty"`
	URL              string  `json:"url,omitempty"`
	FirstReleaseDate int64   `json:"first_release_date,omitempty"`
	GameType         int64   `json:"game_type,omitempty"`
	Cover            int64   `json:"cover,omitempty"`
	CreatedAt        int64   `json:"created_at,omitempty"`
	UpdatedAt        int64   `json:"updated_at,omitempty"`
	Platforms        []int64 `json:"platforms,omitempty"`
	Genres           []int64 `json:"genres,omitempty"`
	GameEngines      []int64 `json:"game_engines,omitempty"`
	Websites         []int64 `json:"websites,omitempty"`
}

// GameFields is the Apicalypse field list matching Game.
const GameFields = "checksum,cover,created_at,first_release_date,game_engines,game_type,genres,name,platforms,slug,summary,updated_at,url,websites"

// Cover is /v4/covers.
type Cover struct {
	ID      int64  `json:"id"`
	Game    int64  `json:"game"`
	ImageID string `json:"image_id"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

// Website is /v4/websites.
type Website struct {
	ID      int64  `json:"id"`
	Game    int64  `json:"game"`
	Type    int64  `json:"type"`
	URL     string `json:"url"`
	Trusted bool   `json:"trusted"`
}

// Named is a generic id/name/slug lookup row (genres, game_engines).
type Named struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

// Typed is a generic id/type lookup row (game_types).
type Typed struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// IGDB website type ids of the storefronts the app recognises.
const (
	WebsiteSteam = 13
	WebsiteItch  = 15
	WebsiteEpic  = 16
	WebsiteGOG   = 17
)

// StorefrontLabel is the display name of a storefront key.
func StorefrontLabel(key string) string {
	switch key {
	case "steam":
		return "Steam"
	case "epic":
		return "Epic Games Store"
	case "gog":
		return "GOG"
	case "itch":
		return "itch.io"
	default:
		return key
	}
}
