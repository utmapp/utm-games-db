# UTM Database

Community compatibility reports for running PC games in [UTM](https://getutm.app), served at **db.getutm.app**.

A single Go binary serves the public site and a loopback-only internal API, stores everything in one SQLite file, mirrors the IGDB PC game catalog, and keeps uploads and cover art on local disk. Cloudflare sits in front purely as a CDN.

## How it works

- **Catalog**: every IGDB game released on PC is mirrored locally (`igdb_games`, with FTS5 search). The first sync walks the whole catalog (~hundreds of requests at 4 req/s); later runs fetch only games whose `updated_at` moved past the stored watermark. Cover art and storefront links are resolved when a game first receives a report, and the cover is copied to `DATA_DIR/covers/`.
- **Active games**: only games with at least one report appear on the site and can be starred. Submitting a report auto-stars the game for the author.
- **Status**: among the visible reports on the newest UTM version that has reports, each report counts one vote plus one per +1; the rating with the most votes wins and ties go to the most recent report. Users cannot +1 their own reports.
- **UTM versions** are a curated list managed through the internal API; the report form offers a dropdown.
- **Reports** carry a rating, the UTM version and guest OS (fixed once submitted; edits can change everything else), optional issue details, a CrossOver/Wine answer and private attachments. Authors can delete a report outright while it has no comments and is under 24 h old; after that they file a deletion request for a maintainer. Own comments can always be deleted.
- **Auto-Triage**: games the project owns on a storefront (pushed by an external owned-games service) or that are free show an "Auto-Triage available" badge; others get a "Donate a copy" button (`/games/{id}/donate`) offering gift accounts for the storefronts the game is sold on, or a demo link. The agent reports progress with a per-report triage status and may comment; `/triage` explains the pipeline. Bot comments may contain Markdown links; user content never renders links.
- **Anti-spam** (synchronous): honeypot field, link/HTML rejection with an editable error, per-user and per-IP hourly rate limits plus a minimum gap between any two content submissions by one user (`RATE_CONTENT_INTERVAL`, default 15 s; search, previews, stars and +1s are exempt), then Akismet. Bot comments from the triage agent bypass it.
- **Attachments** are private (uploader, operators and the triage agent only): other users cannot see them or that a report has any. They are type-checked by content, limited per file/report publicly and per user privately, and swept after `ATTACHMENT_RETENTION_DAYS`.
- **Caching**: public pages are rendered identically for everyone with `Cache-Control: public, s-maxage=…` and strong ETags and never set cookies. Per-user state (sign-in, stars, +1s, edit links) is hydrated by `me.js` from `/api/me` (`no-store`). Authenticated pages and all POSTs are `private, no-store`.

- **Identity without addresses**: email addresses are never stored. An account is identified by an Argon2id digest of the normalized address with a pepper derived from `SECRET_KEY`. A database dump cannot be reversed into addresses (no rainbow tables without the pepper; with it, each guess costs a full Argon2 run). Addresses never reach logs, Akismet, the spam log or the internal API, and the log mailer withholds the recipient. Consequences: no notifications are possible, a new address is a new account, and **losing `SECRET_KEY` orphans every account**, so back it up outside the server.
- **Resource model**: everything an anonymous visitor can reach is static or served from SQLite and cacheable by the CDN; search, previews, reports, uploads and attachments require sign-in. The one anonymous endpoint that can spend resources is `POST /login`, defended in layers: honeypot, address syntax, a signed single-use form token with a 2 s–1 h age window, a proof-of-work puzzle bound to that token (`LOGIN_POW_BITS`, solved in the browser), per-IP and per-address limits, then a site-wide send budget. Only what passes is queued; `LOGIN_WORKERS` workers do the Argon2 hash, an MX lookup (cached) and the SMTP send. The response is always the "enter your code" page, with the same request cookie, so the endpoint cannot be used to find out which addresses exist. The code is bound to that cookie, so it is useless to anyone who only reads the mailbox; wrong guesses are counted and five void it. Scanners that fetch links cannot consume anything, and the mail ends with an `@host #code` line so Safari offers the code only on this site. Memory bound: workers × 64 MiB.
- **Visitor IP**: `CF-Connecting-IP` (or the first `X-Forwarded-For` entry) is used only when the peer is inside `TRUSTED_PROXY_CIDRS`; otherwise the socket address is used. Everything works with or without a CDN in front.
- **Your data**: the account page offers a ZIP export (account, reports, comments, +1s, stars, requests, attachment files), deletion of all attachments, and deletion of the whole account (everything the user could delete themselves is deleted; reports past the direct-deletion window are kept, flagged `anonymized` and shown as "Deleted user", with attachments removed; the user row stays with `deleted_at` set and its email identity, so the internal API can still trace those reports to the account and a ban on it blocks the same address from returning; affected games are recomputed). Exports are the only way to get attachments back and are deliberately throttled (`EXPORT_INTERVAL`, default one per day; attachments younger than `EXPORT_MIN_ATTACHMENT_AGE`, default 48 h, are withheld) so the site cannot double as a file-transfer service. Attachments are never exposed by URL.

## Running locally

Requires Go 1.27+.

```sh
cp .env.example .env            # fill in IGDB_CLIENT_ID / IGDB_CLIENT_SECRET; SECRET_KEY may stay empty locally
make build
MAIL_MODE=log DATA_DIR=./data BASE_URL=http://localhost:8080 ./gamesdb sync --full --max-pages 5   # small catalog slice
MAIL_MODE=log DATA_DIR=./data BASE_URL=http://localhost:8080 ./gamesdb seed                        # optional demo users, reports, statuses
MAIL_MODE=log DATA_DIR=./data BASE_URL=http://localhost:8080 ./gamesdb serve
```

With `MAIL_MODE=log`, sign-in codes are printed to stdout instead of being emailed (the recipient is withheld). Without `SECRET_KEY` the server uses a built-in development secret and warns; set a real one before any data you care about, since changing it later orphans every account. While editing templates, CSS or JS, start with `DEV_RELOAD=1` (or `make dev`) and the files are re-read from the source tree on every request, so a save plus a browser reload is enough; the production binary embeds them and needs a rebuild. Add at least one UTM version before submitting reports:

```sh
curl -X POST localhost:8081/internal/utm-versions -d '{"version":"4.6.5","released_at":"2025-06-01"}'
```

Then open <http://localhost:8080>, sign in, and submit a report. Run `go test ./...` for the test suite (includes an end-to-end test of the public site).

## Deployment

See [deploy/README.md](deploy/README.md): Docker Compose on a Linux VPS, the bare-metal macOS setup behind Cloudflare (launchd, TLS, pf allowlist), and the Cloudflare zone configuration.

## Internal API

Plain JSON over HTTP on the loopback listener, no authentication: the network boundary is the protection. Requests that carry browser headers (`Origin`, `Sec-Fetch-Site`) or a non-loopback `Host` are refused, so a web page open on the operator's machine cannot reach the API through the SSH tunnel. All routes are under `/internal/`.

| Method | Path | Purpose |
|---|---|---|
| GET/POST | `/internal/utm-versions` | List / add `{version, prerelease?, released_at?}` (version is `x.y.z`) |
| DELETE | `/internal/utm-versions/{id}` | Remove an unused version |
| GET | `/internal/reports?since=&game=&visibility=&limit=` | Reports (any visibility) with configuration and attachment metadata, oldest-updated first. `anonymized` marks reports whose author deleted their account; `user_id` still identifies that account |
| GET | `/internal/reports/{id}` | One report with all comments and the game |
| GET | `/internal/attachments/{id}` | Download an attachment |
| POST | `/internal/reports/{id}/comments` | Bot comment `{body_md}` (labelled "Auto-Triage"; Markdown links are rendered, unlike user comments) |
| POST | `/internal/reports/{id}/visibility`, `/internal/comments/{id}/visibility` | `{visibility: visible\|hidden\|removed}` |
| POST | `/internal/reports/{id}/triage` | `{status: pending\|in_progress\|needs_clarification\|fix_proposed\|wont_fix\|cannot_reproduce}` (empty clears); shown as a badge on the report. `GET /internal/reports?triage_status=none` lists untriaged reports |
| GET | `/internal/deletion-requests?status=open` | Authors' requests to remove reports that have comments or are older than 24 h; `POST …/{id}/status {status: done\|rejected}` ("done" removes the report) |
| GET | `/internal/triage/requests?status=open` | User gift/demo offers; `POST …/{id}/status {status}` |
| GET/PUT | `/internal/triage/owned` | Replace the owned-games list: `[{"storefront":"steam","storefront_id":"220","free":false}, {"igdb_game_id":1234}]` |
| PUT/DELETE | `/internal/triage/manual/{game_id}` | Force Auto-Triage availability on/off |
| GET | `/internal/users`; `POST /internal/users/{id}/ban|unban` | Moderation. Users carry no address, only id, display name and `deleted_at`; banning a deleted user blocks the same address from returning |
| GET | `/internal/spam-log`; `POST …/{id}/feedback {feedback: spam\|ham}` | Review rejections, train Akismet |
| POST | `/internal/sync {full?, max_pages?, wait?}`; `GET /internal/sync/status` | IGDB sync |
| POST | `/internal/games/{id}/refresh`, `/internal/games/{id}/recompute` | Re-resolve IGDB details / recompute status |
| POST | `/internal/maintenance/sweep-attachments` | Run the retention sweeper now |
| GET | `/internal/healthz` | Liveness |

Owned-games entries are matched to catalog games through the storefront ids parsed from IGDB website links (Steam app id, GOG/Epic slug, itch `user/game`). Entries that cannot be matched yet are kept and retried when a game becomes active.

## Layout

```
cmd/gamesdb          entrypoint: serve | sync | seed | migrate
internal/config      env configuration (.env is loaded if present)
internal/db          SQLite open (WAL, single writer pool) + embedded migrations
internal/igdb        IGDB client (rate limited), catalog sync, detail resolution
internal/store       typed queries and transactional domain operations
internal/status      version ordering and status rule (pure, tested)
internal/auth        email identities (Argon2id), sign-in codes, sessions, sign-in queue and proof of work
internal/mail        SMTP / log mailer
internal/antispam    link check, Akismet, rate limiter
internal/markdown    goldmark renderer: links disabled for users, allowed for the bot; no images or raw HTML
internal/upload      attachment validation, storage and download
internal/cdn         cache headers, ETag, optional purge hook
internal/web         public site: handlers, templates, static assets
internal/internalapi loopback JSON API (refuses browser requests)
deploy/              deployment notes, launchd/pf files for the macOS host, Docker files for a VPS
```

## License

Apache License 2.0; see [LICENSE](LICENSE).
