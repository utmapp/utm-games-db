package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"math/rand"
	"path/filepath"
	"time"

	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/config"
	"github.com/utmapp/utm-games-db/internal/status"
	"github.com/utmapp/utm-games-db/internal/store"
	"github.com/utmapp/utm-games-db/internal/upload"
)

// seed fills a development database with plausible data: UTM versions,
// users, reports across versions, comments, +1s, stars, attachments and
// Auto-Triage data. It refuses to run on a database that already has reports
// unless --force is given. Game details (covers, store links) are fetched
// from IGDB, so credentials must be configured and the catalog synced.
func seed(cfg *config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	force := fs.Bool("force", false, "seed even if reports already exist")
	fs.Parse(args)

	d, err := openDB(cfg, log)
	if err != nil {
		return err
	}
	defer d.Close()
	ctx := context.Background()
	st := store.New(d)
	keys := auth.NewKeys(cfg.SecretKey)
	sy := newSyncer(cfg, d, log)

	var existing int
	if err := d.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM reports`).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 && !*force {
		return fmt.Errorf("database already has %d reports; pass --force to seed anyway", existing)
	}
	var catalog int
	d.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM igdb_games`).Scan(&catalog)
	if catalog == 0 {
		return errors.New("catalog is empty; run `gamesdb sync` first")
	}
	rng := rand.New(rand.NewSource(42))

	// --- versions -----------------------------------------------------------
	type verSpec struct {
		v   string
		pre bool
		age int // days ago
	}
	specs := []verSpec{{"4.5.4", false, 400}, {"4.6.0", false, 200}, {"4.6.5", false, 60}, {"5.0.0", true, 7}}
	versions := make([]store.UTMVersion, 0, len(specs))
	for _, sp := range specs {
		rel := time.Now().AddDate(0, 0, -sp.age)
		v, err := st.CreateVersion(ctx, sp.v, sp.pre, &rel)
		if err != nil {
			if !errors.Is(err, store.ErrVersionExists) {
				return err
			}
			all, _ := st.ListVersions(ctx)
			for _, x := range all {
				if x.Version == sp.v {
					v = &x
				}
			}
		}
		versions = append(versions, *v)
	}
	log.Info("versions ready", "count", len(versions))

	// --- users ----------------------------------------------------------------
	names := []string{"pixelpilot", "vm_wanderer", "metalhead", "dxmt_fan", "retro_rob", "linux_lara", "m3max_mike", "mini_mia", "shader_sam", "glitchhunter", "speedrun_sue", "casual_cal"}
	users := make([]*store.User, 0, len(names))
	for _, n := range names {
		u, err := st.GetOrCreateUser(ctx, keys.HashEmail(n+"@example.com"))
		if err != nil {
			return err
		}
		if u.DisplayName != n {
			if err := st.SetDisplayName(ctx, u.ID, n); err != nil && !errors.Is(err, store.ErrDisplayNameTaken) {
				return err
			}
			u.DisplayName = n
		}
		users = append(users, u)
	}

	// --- games ------------------------------------------------------------------
	titles := []string{
		"Half-Life 2", "Portal 2", "Stardew Valley", "Hades", "Celeste", "Hollow Knight", "The Witcher 3: Wild Hunt",
		"Cyberpunk 2077", "Elden Ring", "Terraria", "Factorio", "Doom", "Baldur's Gate 3", "Undertale", "Minecraft",
		"Slay the Spire", "Dead Cells", "Cuphead", "Disco Elysium", "Subnautica", "RimWorld", "Outer Wilds", "Valheim",
		"Hotline Miami", "Grand Theft Auto V", "The Elder Scrolls V: Skyrim", "Deus Ex", "Thief II: The Metal Age",
		"StarCraft II: Wings of Liberty", "Civilization VI", "Dark Souls III", "Counter-Strike 2", "Left 4 Dead 2",
	}
	var games []store.GameDetail
	for _, t := range titles {
		var id int64
		// Prefer main games (game_type 0) with a cover; earliest id wins.
		err := d.Reader.QueryRowContext(ctx, `SELECT id FROM igdb_games WHERE name = ? COLLATE NOCASE ORDER BY (COALESCE(game_type_id, 0) = 0) DESC, (cover_id IS NOT NULL) DESC, id LIMIT 1`, t).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			log.Warn("title not in catalog, skipped", "title", t)
			continue
		}
		if err != nil {
			return err
		}
		g, err := st.GetGame(ctx, id)
		if err != nil {
			return err
		}
		games = append(games, *g)
	}
	if len(games) == 0 {
		return errors.New("none of the seed titles are in the catalog; run a full sync first")
	}
	log.Info("seed games selected", "count", len(games))

	// --- content pools ---------------------------------------------------------------
	type issue struct {
		kind, desc, repro string
	}
	issues := []issue{
		{"graphics", "Textures on characters flicker black every few seconds, especially in cutscenes. Lowering shadow quality does **not** help.\n\nSwitching the backend to D3DMetal makes the flicker go away but halves the frame rate.", "1. Start a new game\n2. Skip the intro\n3. Look at any character up close"},
		{"hang_crash", "The game hangs on the loading screen after the publisher logos. The VM stays responsive (I can still open Task Manager) but the game window never updates.\n\nEvent Viewer shows an `APPCRASH` in `d3d11.dll`.", "1. Launch from Steam\n2. Wait on the loading screen for ~30 s"},
		{"performance", "Runs, but at 12–18 fps on the lowest settings. CPU usage inside the guest sits at 100% on all cores while the host is mostly idle.\n\nDisabling vsync in the game settings gained maybe 3 fps.", ""},
		{"hang_crash", "Crashes to desktop roughly 10 minutes into any session. No error dialog; the process just disappears.\n\n```\nfaulting module: ntdll.dll\nexception code: 0xc0000005\n```", "1. Play for ~10 minutes\n2. Crash"},
		{"graphics", "The screen is entirely black after the main menu but audio keeps playing. Alt-tabbing out and back in sometimes restores the picture.", "1. Start any level\n2. Observe black screen"},
		{"other", "Controller input is not recognised even though the guest sees the gamepad in *Devices and Printers*. Keyboard and mouse work fine.", "1. Plug in an Xbox controller\n2. Open the input settings"},
		{"performance", "Frame pacing is uneven: average fps is fine (~55) but there is a hitch every second or so that makes movement feel stuttery.", ""},
		{"graphics", "Water and glass render as flat magenta surfaces. Everything else looks right.", "1. Walk to any body of water"},
		{"workaround", "The launcher refuses to start because it thinks the GPU is unsupported. Adding `-force-d3d11` to the launch options skips the check and the game then runs fine.", "1. Launch without the option and watch it exit\n2. Add `-force-d3d11` in Steam launch options"},
	}
	playable := []string{
		"Runs great at 1080p medium. Occasional half-second stall when a new area loads, otherwise smooth.",
		"Completely playable. I finished the first act without noticing anything wrong except slightly long load times.",
		"Playable at 30 fps locked. Needed to disable motion blur, which was rendering as a smear.",
		"Works out of the box with DXMT. No crashes in about six hours.",
	}
	commentsPool := []string{
		"Same here. Setting the texture quality to *low* made the flicker much rarer for me.",
		"Can confirm on the same build. Did you try the DXMT setting under Display? It fixed the black screen on my machine.",
		"Not reproducible on my side, but my VM has 8 GB of RAM instead of 4. Could be memory pressure.",
		"Works for me after updating the SPICE guest tools inside the VM.",
		"This started happening after the latest UTM update; it was fine on 4.6.0.",
		"Thanks, the workaround with the `-dx11` launch option worked for me too.",
	}
	botComments := []string{
		"Auto-Triage picked this up. I reproduced the flicker on UTM 4.6.5 (108) with DXMT on an M2 host. Could you confirm whether the game is running in fullscreen or windowed mode when it happens?",
		"Auto-Triage: I could not reproduce the hang after three launches on the newest build. If it still happens for you, please attach the `UTM.log` from the VM's folder.",
		"Auto-Triage: reproduced, and a fix for the shader cache invalidation is in progress. Will report back here.",
	}
	distros := [][2]string{{"Ubuntu", "24.04"}, {"Fedora", "40"}, {"Arch Linux", "rolling"}, {"Debian", "12"}}
	devices := [][2]string{{"MacBook Pro 14-inch 2023", "M3 Pro"}, {"MacBook Air 2022", "M2"}, {"Mac mini 2023", "M2 Pro"}, {"Mac Studio 2023", "M2 Max"}, {"MacBook Pro 16-inch 2024", "M4 Max"}, {"iMac 24-inch 2024", "M4"}}
	macs := []string{"15.1", "15.0", "14.6.1", "15.2", "26.0"}

	randomConfig := func(v store.UTMVersion) store.ConfigInput {
		c := store.ConfigInput{VersionID: v.ID}
		dev := devices[rng.Intn(len(devices))]
		if rng.Intn(4) > 0 {
			c.MacOSVersion = macs[rng.Intn(len(macs))]
			c.DeviceModel = dev[0]
			c.Chip = dev[1]
			c.RAMGB = []int{8, 16, 24, 32, 64, 128}[rng.Intn(6)]
		}
		switch r := rng.Intn(10); {
		case r < 7:
			c.GuestOS = "windows"
			c.GraphicsBackend = []string{"dxmt", "dxmt", "d3dmetal", "unknown"}[rng.Intn(4)]
		case r < 9:
			c.GuestOS = "linux"
			dist := distros[rng.Intn(len(distros))]
			c.LinuxDistro, c.LinuxVersion = dist[0], dist[1]
		default:
			c.GuestOS = "other"
			c.OtherOSName = "FreeBSD 14"
		}
		return c
	}
	quotas := store.Quotas{DailyBytes: 1 << 40, DailyFiles: 1 << 20, TotalBytes: 1 << 40}
	ups := upload.Store{Root: filepath.Join(cfg.DataDir, "uploads")}

	var reportIDs []int64
	var reportVersionAge = map[int64]int{}
	for gi, g := range games {
		// Older games skew toward playable; a few are gold; a couple are broken.
		n := 1 + rng.Intn(5)
		if gi%7 == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			v := versions[rng.Intn(len(versions))]
			if i == 0 && gi%3 == 0 {
				v = versions[len(versions)-2] // make sure recent versions are represented
			}
			u := users[rng.Intn(len(users))]
			in := store.ReportInput{GameID: g.ID, UserID: u.ID, Config: randomConfig(v), IP: "203.0.113.7", UserAgent: "seed"}
			var files []store.AttachmentInput
			switch r := rng.Intn(10); {
			case gi%5 == 1 && i == 0:
				in.Rating = status.Gold
				in.PlayedToCompletion = true
			case r < 4:
				in.Rating = status.Playable
				in.IssueType = "unknown"
				if rng.Intn(2) == 0 {
					in.IssueType = "other"
					in.Description = playable[rng.Intn(len(playable))]
					in.WineSameIssue = "unknown"
				}
			case r < 8:
				in.Rating = status.InGameIssues
				is := issues[rng.Intn(len(issues))]
				in.IssueType, in.Description, in.Repro = is.kind, is.desc, is.repro
				in.WineSameIssue = []string{"unknown", "no", "yes"}[rng.Intn(3)]
				if rng.Intn(3) == 0 {
					f, err := fakeAttachment(ups, u.ID, rng)
					if err != nil {
						return err
					}
					files = append(files, f...)
				}
			case r < 9:
				in.Rating = status.DoesNotLaunch
				is := issues[1]
				in.IssueType, in.Description, in.Repro = is.kind, is.desc, is.repro
				in.WineSameIssue = "no"
			default:
				in.Rating = status.Unknown
				in.IssueType = "unknown"
			}
			id, err := st.CreateReport(ctx, in, files, quotas)
			if err != nil {
				return fmt.Errorf("report for %s: %w", g.Name, err)
			}
			reportIDs = append(reportIDs, id)
			// Backdate: a few days after the version shipped.
			age := 0
			for _, sp := range specs {
				if sp.v == v.Version {
					age = sp.age
				}
			}
			days := age - rng.Intn(max(age/2, 1)) - 1
			if days < 0 {
				days = 0
			}
			reportVersionAge[id] = days
			ts := time.Now().AddDate(0, 0, -days).Add(-time.Duration(rng.Intn(86400)) * time.Second).Unix()
			if _, err := d.Writer.ExecContext(ctx, `UPDATE reports SET created_at = ?, updated_at = ? WHERE id = ?`, ts, ts, id); err != nil {
				return err
			}
			if _, err := d.Writer.ExecContext(ctx, `UPDATE configurations SET created_at = ? WHERE id = (SELECT config_id FROM reports WHERE id = ?)`, ts, id); err != nil {
				return err
			}
		}
		// Extra stars from random users.
		for _, u := range users {
			if rng.Intn(3) == 0 {
				st.ToggleStar(ctx, u.ID, g.ID)
			}
		}
		if err := sy.ResolveDetails(ctx, g.ID); err != nil {
			log.Warn("details", "game", g.Name, "err", err)
		}
		if err := st.RecomputeGame(ctx, g.ID); err != nil {
			return err
		}
		log.Info("seeded game", "name", g.Name, "reports", n)
	}

	// --- comments and +1s ----------------------------------------------------------------
	for _, rid := range reportIDs {
		rep, err := st.GetReport(ctx, rid)
		if err != nil {
			return err
		}
		for _, u := range users {
			if u.ID == rep.UserID || rng.Intn(4) != 0 {
				continue
			}
			if _, _, err := st.TogglePlusOne(ctx, rid, u.ID); err != nil { // a new report, so this adds
				return err
			}
		}
		nc := rng.Intn(3)
		if rep.Rating == status.Gold {
			nc = 0
		}
		for i := 0; i < nc; i++ {
			u := users[rng.Intn(len(users))]
			in := store.CommentInput{ReportID: rid, UserID: u.ID, Body: commentsPool[rng.Intn(len(commentsPool))], IP: "203.0.113.9", UserAgent: "seed"}
			if rng.Intn(2) == 0 {
				c := randomConfig(versions[rng.Intn(len(versions))])
				in.Config = &c
			}
			cid, err := st.AddComment(ctx, in)
			if err != nil {
				return err
			}
			ts := time.Now().AddDate(0, 0, -max(reportVersionAge[rid]-1-rng.Intn(3), 0)).Unix()
			d.Writer.ExecContext(ctx, `UPDATE comments SET created_at = ? WHERE id = ?`, ts, cid)
		}
		if rep.Rating == status.InGameIssues && rng.Intn(3) == 0 {
			if _, err := st.AddComment(ctx, store.CommentInput{ReportID: rid, Bot: true, Body: botComments[rng.Intn(len(botComments))]}); err != nil {
				return err
			}
			if err := st.SetTriageStatus(ctx, rid, store.TriageStatuses[rng.Intn(len(store.TriageStatuses))]); err != nil {
				return err
			}
		}
	}

	// --- Auto-Triage ---------------------------------------------------------------------------
	var owned []store.OwnedEntry
	rows, err := d.Reader.QueryContext(ctx, `SELECT game_id, storefront, storefront_id FROM igdb_websites WHERE storefront IS NOT NULL AND game_id IN (SELECT igdb_game_id FROM games) ORDER BY game_id`)
	if err != nil {
		return err
	}
	seen := map[int64]bool{}
	for rows.Next() {
		var gid int64
		var sf, sid string
		if err := rows.Scan(&gid, &sf, &sid); err != nil {
			rows.Close()
			return err
		}
		if seen[gid] || rng.Intn(5) >= 2 { // own ~40% of the seeded games
			continue
		}
		seen[gid] = true
		owned = append(owned, store.OwnedEntry{Storefront: sf, StorefrontID: sid, Free: len(owned)%4 == 3})
	}
	rows.Close()
	owned = append(owned, store.OwnedEntry{Storefront: "steam", StorefrontID: "999999999"}) // unmatched entry
	matched, err := st.ReplaceOwned(ctx, owned)
	if err != nil {
		return err
	}
	log.Info("owned games pushed", "entries", len(owned), "matched", matched)
	for _, g := range games {
		if !seen[g.ID] {
			if _, err := st.CreateTriageRequest(ctx, g.ID, users[rng.Intn(len(users))].ID, "demo", "", "https://example.com/demos/"+g.Slug+".zip", "Demo on the official site, no login needed."); err != nil {
				return err
			}
			break
		}
	}
	if len(games) > 1 {
		st.CreateTriageRequest(ctx, games[1].ID, users[0].ID, "gift", "steam", "", "Gifted from my Steam account, should show up under pending gifts.")
	}

	var totalReports, totalGames int
	d.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM reports`).Scan(&totalReports)
	d.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM games`).Scan(&totalGames)
	log.Info("seed complete", "games", totalGames, "reports", totalReports, "users", len(users))
	fmt.Printf("\nSeeded %d active games and %d reports. Sign in with any of these addresses (codes print to the log):\n", totalGames, totalReports)
	for _, n := range names[:4] {
		fmt.Printf("  %s@example.com\n", n)
	}
	return nil
}

// fakeAttachment writes a small PNG and a log file and returns their records.
func fakeAttachment(ups upload.Store, userID int64, rng *rand.Rand) ([]store.AttachmentInput, error) {
	img := image.NewRGBA(image.Rect(0, 0, 320, 180))
	base := color.RGBA{uint8(rng.Intn(200)), uint8(rng.Intn(200)), uint8(rng.Intn(200)), 255}
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			c := base
			c.R = uint8(int(c.R) + x/4)
			c.G = uint8(int(c.G) + y/2)
			if (x/20+y/20)%2 == 0 {
				c.B += 40
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	var out []store.AttachmentInput
	saved, err := ups.Save(fmt.Sprintf("u%d", userID), "screenshot.png", &buf, 10<<20)
	if err != nil {
		return nil, err
	}
	out = append(out, store.AttachmentInput{Filename: "screenshot.png", MIME: saved.MIME, Size: saved.Size, SHA256: saved.SHA256, Path: saved.RelPath})
	logText := []byte("2026-10-29 21:04:11 qemu: spice server started\n2026-10-29 21:04:12 dxmt: device created (M3 Pro)\n2026-10-29 21:05:40 dxmt: shader cache miss x412\n2026-10-29 21:05:41 guest: APPCRASH d3d11.dll\n")
	saved, err = ups.Save(fmt.Sprintf("u%d", userID), "UTM.log", bytes.NewReader(logText), 10<<20)
	if err != nil {
		return nil, err
	}
	out = append(out, store.AttachmentInput{Filename: "UTM.log", MIME: saved.MIME, Size: saved.Size, SHA256: saved.SHA256, Path: saved.RelPath})
	return out, nil
}
