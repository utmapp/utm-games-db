# Deploying UTM Database

## Docker Compose on a Linux VPS

```sh
cp .env.example .env   # production values; keep quotas here, not in git
docker compose up -d --build
```

Set `SECRET_KEY` (`openssl rand -hex 32`) and `TRUSTED_PROXY_CIDRS` in `.env`; the server refuses to start over https without the former. Back the key up outside the server. The container publishes the public site on `127.0.0.1:8080` (put your reverse proxy or Cloudflare origin in front) and keeps the internal API on `127.0.0.1:8081` *inside* the container. Reach it with:

```sh
docker compose exec gamesdb wget -qO- http://127.0.0.1:8081/internal/healthz
# or, from your machine, through SSH + docker port forwarding:
ssh -L 8081:127.0.0.1:8081 user@host  # when running the binary directly on the host
```

Data (SQLite file, uploads, covers) lives in the `gamesdb-data` volume. Back it up by snapshotting the volume; the SQLite file is safe to copy while the server runs thanks to WAL mode (prefer `sqlite3 gamesdb.sqlite ".backup out.sqlite"`).

## macOS bare metal behind Cloudflare

The production setup for db.getutm.app: one static binary on a Mac, serving HTTPS on 443 with a Cloudflare Origin CA certificate, with pf admitting only Cloudflare's edge. The machine auto-logs in to a standard user account without sudo rights; the service runs as that user from a LaunchAgent, and everything it owns lives under `~/gamesdb`. Two scripts in `deploy/macos/` split the work by privilege:

- `install-system.sh`, run once by an administrator with sudo: the pf allowlist, the daemons that enable pf at boot and refresh the allowlist monthly, and log rotation. Nothing else needs root.
- `install.sh`, run by the standard user without sudo: binary, `.env`, data, certificate and log directories, and the LaunchAgent. This is the one you re-run for upgrades.

1. **Build.** `make build-mac` on any machine produces `gamesdb-darwin-arm64`; copy it and the repository (or at least `deploy/macos/` and `.env.example`) to the Mac.

2. **Machine-wide, as an administrator**, naming the standard account. The script uses only the files beside it, so any checkout works, including one in the administrator's own home:

   ```sh
   sudo ./deploy/macos/install-system.sh <standard user>
   ```

   `PREFIX` and `PORT` can be set in the environment if the defaults (`~/gamesdb`, 443) change. It prints the resulting pf rules.

3. **User-local, as the standard user** (from the repository root):

   ```sh
   ./deploy/macos/install.sh
   ```

   The first run creates `~/gamesdb/{data,tls,log}`, installs the binary, writes `~/gamesdb/.env` from `.env.example` with the macOS values filled in (port 443, TLS paths, data directory, a fresh `SECRET_KEY`, Cloudflare's current ranges as `TRUSTED_PROXY_CIDRS`), and then stops because the certificate is not there yet. `PREFIX` and `BINARY` override `~/gamesdb` and the binary in the current directory.

4. **Certificate and settings.** In the Cloudflare dashboard, SSL/TLS → Origin Server → Create Certificate for `db.getutm.app` (15 years). Save the certificate as `~/gamesdb/tls/origin.pem` and the key as `~/gamesdb/tls/origin.key`. Edit `~/gamesdb/.env`: the `SMTP_*` settings of the transactional mail provider, `IGDB_CLIENT_ID`/`IGDB_CLIENT_SECRET`, `AKISMET_KEY`. Back up `SECRET_KEY` off the machine.

5. **Run `install.sh` again.** It loads the LaunchAgent and waits for `https://127.0.0.1:443/healthz`. Later runs (new binary, changed `.env`, new Cloudflare ranges) do the same and restart the service.

6. **Cloudflare.** Proxy the `db` DNS record (orange cloud) and set the SSL mode to **Full (strict)**. The origin listens on 443, which Cloudflare caches; macOS lets a normal user bind it. Then the cache rule and optional purge hook from the Cloudflare zone section below.

What ends up where:

| Path | Owner | Purpose |
|---|---|---|
| `~/gamesdb/gamesdb`, `.env`, `data/`, `tls/`, `log/` | service user, 700/600 | binary, settings, SQLite + uploads + covers, certificate, log |
| `~/Library/LaunchAgents/app.getutm.db.plist` | service user | runs `gamesdb serve` in the login session, restarts on failure (`install.sh`) |
| `/etc/pf.gamesdb.conf`, `/etc/pf.anchors/gamesdb`, `gamesdb.cloudflare` | root | a ruleset that includes Apple's `/etc/pf.conf` and allows port 443 from the Cloudflare table and loopback only, dropping the rest (`install-system.sh`) |
| `/Library/LaunchDaemons/app.getutm.pf.plist` | root | enables pf with that ruleset at boot |
| `/usr/local/sbin/cloudflare-ips.sh`, `/Library/LaunchDaemons/app.getutm.cloudflare-ips.plist` | root | refreshes the Cloudflare table on the first of each month (root-owned so the service user cannot widen it) |
| `/etc/newsyslog.d/gamesdb.conf` | root | weekly log rotation; it restarts the service (via `~/gamesdb/data/gamesdb.pid`) so the new file is picked up |

Day to day: `launchctl kickstart -k gui/$(id -u)/app.getutm.db` restarts the service, `tail -f ~/gamesdb/log/gamesdb.log` follows it, and `sudo pfctl -a gamesdb -s rules` shows the firewall rules (loopback stays open, so `curl -k https://127.0.0.1/healthz` works on the box). The internal API stays on loopback: `ssh -L 8081:127.0.0.1:8081 user@mac` and then `curl localhost:8081/internal/healthz`. Back up `~/gamesdb/data` (SQLite via `sqlite3 gamesdb.sqlite ".backup out.sqlite"`, plus `uploads/` and `covers/`) together with `SECRET_KEY`. Because the agent lives in the login session, the site is down while nobody is logged in; the auto-login makes that only the seconds after a reboot. Nothing here lives in files macOS updates replace, so an update needs no re-setup. If MacStadium's own firewall is available, add the same Cloudflare allowlist there too.

## Cloudflare zone

Only CDN features are used. Suggested zone configuration for `db.getutm.app`:

- A cache rule that caches everything on the hostname and **respects origin TTL**: Caching → Cache Rules → Create rule; match *Hostname equals `db.getutm.app`*; Cache eligibility *Eligible for cache*; Edge TTL *Use cache-control header if present, bypass cache if not*; Browser TTL *Respect origin*. The app sets `s-maxage` on public pages and `private, no-store` on everything personal, so this one rule is enough; a second request for the front page shows `cf-cache-status: HIT`.
- **Required:** set `TRUSTED_PROXY_CIDRS` to the addresses the server actually receives connections from, so rate limits and Akismet see real client IPs from `CF-Connecting-IP`. If Cloudflare connects straight to the binary (the macOS setup above), that is Cloudflare's IP ranges (see `.env.example`; verify against <https://www.cloudflare.com/ips/>). If a reverse proxy or tunnel on the host connects to `127.0.0.1:8080`, the container sees the compose network's gateway instead (the server logs a warning naming it, e.g. `peer=172.18.0.1`): trust exactly that address (`172.18.0.1/32`, not the whole private range, which would let other containers forge the header), and make the proxy accept only Cloudflare's traffic or set `CF-Connecting-IP` itself. Without the right ranges every visitor shares one address and the sign-in rate limit locks everyone out together.
- Optional: set `CDN_PURGE_URL`/`CDN_PURGE_TOKEN` to the zone's purge-by-URL endpoint to invalidate pages immediately on writes. Without it, pages refresh when their TTL (5 min list, 1 h game/report pages) expires.
