// Command gamesdb runs the UTM games compatibility database.
//
//	gamesdb serve            start the public and internal HTTP servers
//	gamesdb sync [--full] [--max-pages N]
//	gamesdb seed [--force]   fill a development database with sample data
//	gamesdb migrate          apply database migrations and exit
//	gamesdb mail-setup [--ip ADDR] [--check]
//	                         create the DKIM key and print (or verify) the DNS records for direct mail
//	gamesdb mail-test ADDR   send a test sign-in code through the configured mailer
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/utmapp/utm-games-db/internal/antispam"
	"github.com/utmapp/utm-games-db/internal/auth"
	"github.com/utmapp/utm-games-db/internal/cdn"
	"github.com/utmapp/utm-games-db/internal/config"
	"github.com/utmapp/utm-games-db/internal/db"
	"github.com/utmapp/utm-games-db/internal/igdb"
	"github.com/utmapp/utm-games-db/internal/internalapi"
	"github.com/utmapp/utm-games-db/internal/store"
	"github.com/utmapp/utm-games-db/internal/upload"
	"github.com/utmapp/utm-games-db/internal/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	switch os.Args[1] {
	case "serve":
		err = serve(cfg, log)
	case "sync":
		err = sync(cfg, log, os.Args[2:])
	case "seed":
		err = seed(cfg, log, os.Args[2:])
	case "migrate":
		var d *db.DB
		d, err = openDB(cfg, log)
		if err == nil {
			d.Close()
			log.Info("migrations applied", "db", dbPath(cfg))
		}
	case "mail-setup":
		err = mailSetup(cfg, os.Args[2:])
	case "mail-test":
		err = mailTest(cfg, log, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error(os.Args[1]+" failed", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gamesdb serve | sync [--full] [--max-pages N] | seed [--force] | migrate | mail-setup [--ip ADDR] [--check] | mail-test ADDR")
}

func dbPath(cfg *config.Config) string { return filepath.Join(cfg.DataDir, "gamesdb.sqlite") }

func openDB(cfg *config.Config, log *slog.Logger) (*db.DB, error) {
	for _, sub := range []string{"", "uploads", "covers"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	if cfg.DevSecret {
		log.Warn("SECRET_KEY is not set: using the built-in development secret. Set one (openssl rand -hex 32) before any real sign-ins; changing it later orphans every account.")
	}
	return db.Open(dbPath(cfg))
}

func newSyncer(cfg *config.Config, d *db.DB, log *slog.Logger) *igdb.Syncer {
	client := igdb.NewClient(cfg.IGDBClientID, cfg.IGDBClientSecret, igdb.DBTokenStore{DB: d})
	platforms := make([]int64, len(cfg.IGDBPlatformIDs))
	for i, p := range cfg.IGDBPlatformIDs {
		platforms[i] = int64(p)
	}
	return &igdb.Syncer{DB: d, Client: client, Log: log, Platforms: platforms, CoverDir: filepath.Join(cfg.DataDir, "covers")}
}

func sync(cfg *config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	full := fs.Bool("full", false, "walk the whole catalog instead of changes since the last run")
	maxPages := fs.Int("max-pages", 0, "stop after N pages of 500 (testing)")
	fs.Parse(args)
	d, err := openDB(cfg, log)
	if err != nil {
		return err
	}
	defer d.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sum, err := newSyncer(cfg, d, log).Run(ctx, *full, *maxPages)
	if sum != nil {
		log.Info("sync summary", "pages", sum.Pages, "fetched", sum.Fetched, "inserted", sum.Inserted, "updated", sum.Updated, "unchanged", sum.Unchanged, "duration", sum.Duration)
	}
	return err
}

func serve(cfg *config.Config, log *slog.Logger) error {
	d, err := openDB(cfg, log)
	if err != nil {
		return err
	}
	defer d.Close()
	// The pid file lets log rotation (newsyslog on macOS) signal the process;
	// the supervisor restarts it and reopens the log.
	pidFile := filepath.Join(cfg.DataDir, "gamesdb.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		log.Warn("write pid file", "path", pidFile, "err", err)
	} else {
		defer os.Remove(pidFile)
	}

	st := store.New(d)
	keys := auth.NewKeys(cfg.SecretKey)
	au := &auth.Service{DB: d, Store: st, CodeTTL: cfg.LoginCodeTTL, SessionTTL: cfg.SessionTTL, Secure: cfg.CookieSecure, Keys: keys}
	mailer, err := newMailer(cfg, log, cfg.MailRetryFor)
	if err != nil {
		return err
	}
	if cfg.BaseURL.Scheme == "https" && len(cfg.TrustedProxyCIDRs) == 0 {
		log.Warn("TRUSTED_PROXY_CIDRS is empty: behind a CDN every visitor shares the proxy's IP, so per-IP rate limits (including sign-in) will lock everyone out together and Akismet sees the wrong address. Set it to the CDN's IP ranges.")
	}
	ak := &antispam.Akismet{Key: cfg.AkismetKey, Blog: cfg.BaseURL.String()}
	if !ak.Enabled() {
		log.Warn("AKISMET_KEY not set: submissions are not checked by Akismet")
	}
	lim := antispam.NewLimiter()
	defer lim.Close()
	sy := newSyncer(cfg, d, log)
	ups := upload.Store{Root: filepath.Join(cfg.DataDir, "uploads")}
	var purger cdn.Purger = cdn.NoopPurger{}
	if cfg.CDNPurgeURL != "" {
		purger = cdn.NewHTTPPurger(cfg.CDNPurgeURL, cfg.CDNPurgeToken, cfg.BaseURL.String(), log)
		log.Info("CDN purge hook enabled", "endpoint", cfg.CDNPurgeURL)
	}

	pub, err := web.New(cfg, st, au, mailer, ak, lim, sy, ups, purger, log)
	if err != nil {
		return err
	}
	defer pub.Close()
	sweep := func(ctx context.Context) (int, error) {
		paths, err := st.ExpireAttachments(ctx, time.Now().Add(-time.Duration(cfg.UploadRetentionDays)*24*time.Hour))
		if err != nil {
			return 0, err
		}
		n := 0
		for _, p := range paths {
			if err := ups.Remove(p); err != nil {
				log.Warn("remove expired attachment", "path", p, "err", err)
				continue
			}
			n++
		}
		return n, nil
	}
	internal := &internalapi.Server{Cfg: cfg, Store: st, Syncer: sy, Akismet: ak, Uploads: ups, Log: log, Sweep: sweep, Purger: purger}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Background jobs.
	go runPeriodic(ctx, log, "igdb sync", cfg.IGDBSyncInterval, cfg.IGDBSyncOnStart, func(ctx context.Context) error {
		if !sy.Client.Configured() {
			return errors.New("IGDB credentials not configured")
		}
		_, err := sy.Run(ctx, false, 0)
		if errors.Is(err, igdb.ErrAlreadyRunning) {
			return nil
		}
		return err
	})
	go runPeriodic(ctx, log, "resolve pending details", 10*time.Minute, true, func(ctx context.Context) error {
		if !sy.Client.Configured() {
			return nil
		}
		_, err := sy.ResolvePending(ctx)
		return err
	})
	go runPeriodic(ctx, log, "attachment sweep", 24*time.Hour, true, func(ctx context.Context) error {
		n, err := sweep(ctx)
		if n > 0 {
			log.Info("expired attachments removed", "count", n)
		}
		return err
	})
	go runPeriodic(ctx, log, "auth cleanup", 6*time.Hour, true, au.Cleanup)

	internalLn, err := net.Listen("tcp", cfg.InternalListenAddr)
	if err != nil {
		return fmt.Errorf("internal listener: %w", err)
	}
	publicLn, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("public listener: %w", err)
	}
	pubSrv := &http.Server{Handler: pub.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute, WriteTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	intSrv := &http.Server{Handler: internal.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Minute, WriteTimeout: 10 * time.Minute, IdleTimeout: 2 * time.Minute}

	errCh := make(chan error, 2)
	if cfg.TLSCert != "" {
		// Serve HTTPS directly (e.g. a Cloudflare Origin CA certificate on a
		// bare-metal host) so nothing has to sit in front of the binary.
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return fmt.Errorf("TLS_CERT/TLS_KEY: %w", err)
		}
		pubSrv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
		go func() { errCh <- pubSrv.ServeTLS(publicLn, "", "") }()
	} else {
		go func() { errCh <- pubSrv.Serve(publicLn) }()
	}
	go func() { errCh <- intSrv.Serve(internalLn) }()
	log.Info("serving", "public", publicLn.Addr().String(), "tls", cfg.TLSCert != "", "internal", internalLn.Addr().String(), "base_url", cfg.BaseURL.String(), "data_dir", cfg.DataDir)

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pubSrv.Shutdown(shutdownCtx)
	intSrv.Shutdown(shutdownCtx)
	return nil
}

// runPeriodic runs fn every interval (and immediately when runNow is set).
func runPeriodic(ctx context.Context, log *slog.Logger, name string, interval time.Duration, runNow bool, fn func(context.Context) error) {
	if interval <= 0 {
		return
	}
	run := func() {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			log.Error(name, "err", err)
		}
	}
	if runNow {
		run()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
