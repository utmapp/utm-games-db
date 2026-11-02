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
ssh -L 8081:127.0.0.1:8081 user@host  # when running the binary directly on a Linux host with a loopback INTERNAL_LISTEN_ADDR
```

Data (SQLite file, uploads, covers) lives in the `gamesdb-data` volume. Back it up by snapshotting the volume; the SQLite file is safe to copy while the server runs thanks to WAL mode (prefer `sqlite3 gamesdb.sqlite ".backup out.sqlite"`).

## macOS bare metal behind Cloudflare

The production setup for db.getutm.app: one static binary on a Mac, serving HTTPS on 443 with a Cloudflare Origin CA certificate, with pf admitting only Cloudflare's edge. The service runs as a dedicated role account (`_gamesdb`) from a LaunchDaemon, so it is up from boot whoever is logged in, and everything it owns under `/usr/local/gamesdb` is readable by that account alone. The internal API is a Unix socket that the account's group may use; the administrator who installs is added to the group. One script does all of it and is safe to re-run.

1. **Check out the repository on the Mac** and make sure Go is installed (`brew install go`); the installer builds from the checkout.

2. **Install**, as an administrator from the repository root:

   ```sh
   sudo ./deploy/macos/install.sh
   ```

   The first run creates the group and role account, builds the binary (`make build-mac`, which does nothing when no source changed), lays out `/usr/local/gamesdb/{data,tls,run,log}`, writes `.env` from `.env.example` with the macOS values filled in (port 443, TLS paths, data directory, the socket path, a fresh `SECRET_KEY`, Cloudflare's current ranges as `TRUSTED_PROXY_CIDRS`), configures pf and log rotation, and then stops because the certificate is not there yet. Variables: `GAMESDB_USER` (default `_gamesdb`), `GAMESDB_GROUP` (same name), `OPERATORS` (comma-separated users given access to the internal API; default: the caller), `PREFIX`, `PORT`, `BINARY`.

3. **Certificate and settings.** In the Cloudflare dashboard, SSL/TLS → Origin Server → Create Certificate for `db.getutm.app` (15 years). Save the certificate as `/usr/local/gamesdb/tls/origin.pem` and the key as `origin.key` (with sudo; the next run fixes ownership). Edit `/usr/local/gamesdb/.env` (sudo): the `SMTP_*` or direct-mail settings, `IGDB_CLIENT_ID`/`IGDB_CLIENT_SECRET`, `AKISMET_KEY`. Back up `SECRET_KEY` off the machine.

4. **Run the installer again.** It starts the LaunchDaemon and waits for `https://127.0.0.1:443/healthz` and for the internal socket.

   From then on, every change is the same step: pull or edit, then `sudo ./deploy/macos/install.sh`. It rebuilds only if a source changed, installs the binary, refreshes `TRUSTED_PROXY_CIDRS`, and restarts the running service (or reloads it when the daemon definition changed).

5. **Cloudflare.** Proxy the `db` DNS record (orange cloud) and set the SSL mode to **Full (strict)**. The origin listens on 443, which Cloudflare caches. Then the cache rule and optional purge hook from the Cloudflare zone section below.

What ends up where:

| Path | Owner, mode | Purpose |
|---|---|---|
| `/usr/local/gamesdb/gamesdb` | root, 755 | the binary (the service cannot overwrite itself) |
| `/usr/local/gamesdb/.env`, `data/`, `tls/` | `_gamesdb`, 600/700 | settings, SQLite + uploads + covers + DKIM key, certificate |
| `/usr/local/gamesdb/run/internal.sock` | `_gamesdb:_gamesdb`, 660 | the internal API; group members may use it, nobody else |
| `/usr/local/gamesdb/log/` | `_gamesdb:_gamesdb`, 750 | the log, readable by the group |
| `/Library/LaunchDaemons/app.getutm.db.plist` | root | runs `gamesdb serve` as `_gamesdb` from boot, restarts on failure |
| `/etc/pf.gamesdb.conf`, `/etc/pf.anchors/gamesdb`, `gamesdb.cloudflare` | root | a ruleset that includes Apple's `/etc/pf.conf` and allows port 443 from the Cloudflare table and loopback only, dropping the rest |
| `/Library/LaunchDaemons/app.getutm.pf.plist` | root | enables pf with that ruleset at boot |
| `/usr/local/sbin/cloudflare-ips.sh`, `/Library/LaunchDaemons/app.getutm.cloudflare-ips.plist` | root | refreshes the Cloudflare table on the first of each month |
| `/etc/newsyslog.d/gamesdb.conf` | root | weekly log rotation; it restarts the service (via `data/gamesdb.pid`) so the new file is picked up |

Day to day: `sudo ./deploy/macos/install.sh` applies any change and restarts the service; `tail -f /usr/local/gamesdb/log/gamesdb.log` follows it (group members can read it); `sudo pfctl -a gamesdb -s rules` shows the firewall rules. The internal API is the socket: on the box, `curl --unix-socket /usr/local/gamesdb/run/internal.sock http://localhost/internal/healthz`; from another machine, forward a local port to it over SSH as a group member, `ssh -L 8081:/usr/local/gamesdb/run/internal.sock administrator@host`, and use `http://127.0.0.1:8081` as before. A user added to the group sees it after logging in again. Back up with `GET /internal/backup` (next section) and keep `SECRET_KEY` with it. Nothing here lives in files macOS updates replace, so an update needs no re-setup. If the hosting provider offers its own firewall, add the same Cloudflare allowlist there too.

## Backups

Everything that cannot be regenerated lives in `/usr/local/gamesdb`: the settings file with `SECRET_KEY` (accounts are keyed by it), the TLS and DKIM keys, the SQLite database and the uploaded files. The IGDB catalog and the cover images are re-fetched by the server and are not worth backing up.

`GET /internal/backup` on the internal API streams all of it as one uncompressed tar: `env`, `tls/`, `dkim/`, `db/gamesdb.sqlite` (a consistent copy of every user and maintainer table, with the catalog tables left empty) and `uploads/`, described by a `manifest.json`. The database copy costs time proportional to the user data, not to the catalog, and the files are streamed without compression, so the endpoint stays cheap as the site grows. One backup runs at a time, and it may take as long as the data needs.

From a machine with the SSH tunnel to the internal socket (see the end of the previous section):

```sh
curl -sS -o "utmdb-backup-$(date +%Y%m%d-%H%M%S).tar" http://127.0.0.1:8081/internal/backup
```

Run that from cron on the backup machine and keep as many archives as you like; each is complete on its own. To restore, on the (new) Mac after a first `install.sh` run (the one that stops at the missing certificate is enough, since it creates the service account), as an administrator from the repository checkout:

```sh
sudo ./deploy/macos/restore.sh utmdb-backup-20261101-120000.tar
```

It stops the service if it runs, moves the current `/usr/local/gamesdb/data` aside as `data.pre-restore-<time>`, puts the archive's pieces in place owned by the service account, points the restored settings at this machine's paths, and runs `install.sh` to start the service. The server imports the catalog in the background and re-resolves covers and store links as it goes; the site is up immediately, with search filling in over a few minutes.

## Sending mail directly

Sign-in codes can be delivered by the server itself instead of through an SMTP relay. With `MAIL_MODE=direct` it looks up the recipient domain's mail servers, connects on port 25 with STARTTLS where offered, signs every message with DKIM, and retries temporary refusals (greylisting) every 90 seconds in the background for `MAIL_RETRY_FOR`, which should stay below `LOGIN_CODE_TTL`. For receivers to trust the mail, DNS has to let them verify the signature (DKIM), the sending address (SPF), and that both agree with the From domain (DMARC), and the sending address needs reverse DNS. The requirements on the host are outbound port 25 and a public IP whose reverse DNS the hosting provider will set.

Two names are involved:

- the **From domain**, taken from `MAIL_FROM`, which carries the SPF, DKIM and DMARC records;
- the **host name** in `MAIL_HELO`, which the server announces to receiving servers. It names the machine, not a service, so anything else on the box that needs reverse DNS shares it. It needs a DNS-only A record to the sending IP, and the IP's reverse DNS must give the same name.

Setup:

1. Set `MAIL_MODE=direct`, `MAIL_FROM`, `MAIL_HELO` and (optionally) `MAIL_DKIM_SELECTOR` in `.env`.

2. Run the setup command as the service user, with the public IP mail is sent from:

   ```sh
   gamesdb mail-setup --ip <sending IP>
   ```

   On the first run it creates the DKIM key at `DATA_DIR/dkim/<selector>.key`; the key is never overwritten, since a new one invalidates the published record, so back it up with `SECRET_KEY`. It then checks DNS and prints every record that is missing or wrong, with the exact value to publish: the A and SPF records for the host name, SPF for the From domain, the DKIM public key, and DMARC. The DKIM value is longer than 255 characters; DNS editors split it, so paste it whole.

3. Create the printed records (the A record with any CDN proxying off) and ask the hosting provider to set the reverse DNS of the sending IP to the host name. Providers verify that the name resolves to the IP first, so create the A record before asking.

4. Re-run the setup command until it reports that every record is in place; propagation can take a while.

5. Send a test message to a mailbox you can read, then restart the service:

   ```sh
   gamesdb mail-test you@example.com
   ```

   The message carries the fixed code 123456 in the same format as real sign-in mail, so it also shows whether the mail client offers the code for autofill. In Gmail, "Show original" should list SPF, DKIM and DMARC as PASS.

Nothing receives mail at the From address; replies and bounces are dropped by the receiving side.

## Cloudflare zone

Only CDN features are used. Suggested zone configuration for `db.getutm.app`:

- The `.json` twins of the pages and `/llms.txt` are covered by the same cache rule and the same purge hook; they send `max-age=60` so clients may reuse documents for a minute without asking.
- A cache rule that caches everything on the hostname and **respects origin TTL**: Caching → Cache Rules → Create rule; match *Hostname equals `db.getutm.app`*; Cache eligibility *Eligible for cache*; Edge TTL *Use cache-control header if present, bypass cache if not*; Browser TTL *Respect origin*. The app sets `s-maxage` on public pages and `private, no-store` on everything personal, so this one rule is enough; a second request for the front page shows `cf-cache-status: HIT`.
- **Required:** set `TRUSTED_PROXY_CIDRS` to the addresses the server actually receives connections from, so rate limits and Akismet see real client IPs from `CF-Connecting-IP`. If Cloudflare connects straight to the binary (the macOS setup above), that is Cloudflare's IP ranges (see `.env.example`; verify against <https://www.cloudflare.com/ips/>). If a reverse proxy or tunnel on the host connects to `127.0.0.1:8080`, the container sees the compose network's gateway instead (the server logs a warning naming it, e.g. `peer=172.18.0.1`): trust exactly that address (`172.18.0.1/32`, not the whole private range, which would let other containers forge the header), and make the proxy accept only Cloudflare's traffic or set `CF-Connecting-IP` itself. Without the right ranges every visitor shares one address and the sign-in rate limit locks everyone out together.
- Optional: set `CDN_PURGE_URL`/`CDN_PURGE_TOKEN` to the zone's purge-by-URL endpoint to invalidate pages immediately on writes. Without it, pages refresh when their TTL (5 min list, 1 h game/report pages) expires.
