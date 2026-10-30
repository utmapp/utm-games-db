#!/bin/bash
# User-local part of the macOS deployment; run as the service user, no sudo:
#
#   ./deploy/macos/install.sh            # from the repository root
#
# Installs the binary, .env, data, certificate and log directories under
# $PREFIX (default ~/gamesdb) and a LaunchAgent that runs the server in this
# user's login session. The machine-wide part (firewall, log rotation) is
# install-system.sh, run once by an administrator.
#
# Re-running is safe: it upgrades the binary, refreshes TRUSTED_PROXY_CIDRS
# and restarts the service. .env and the data directory are never overwritten.
#
# Environment overrides: PREFIX, BINARY (default ./gamesdb-darwin-arm64 or ./gamesdb).
set -euo pipefail

[ "$(id -u)" -ne 0 ] || { echo "run as the service user, not root" >&2; exit 1; }
HERE=$(cd "$(dirname "$0")" && pwd)
PREFIX=${PREFIX:-$HOME/gamesdb}
BINARY=${BINARY:-}
if [ -z "$BINARY" ]; then
  for b in gamesdb-darwin-arm64 gamesdb; do [ -x "$b" ] && BINARY=$b && break; done
fi
[ -n "$BINARY" ] && [ -x "$BINARY" ] || { echo "no built binary found; run: make build-mac" >&2; exit 1; }
say() { printf '\n==> %s\n' "$*"; }

say "Installing to $PREFIX"
mkdir -p "$PREFIX/data" "$PREFIX/tls" "$PREFIX/log"
chmod 700 "$PREFIX" "$PREFIX/data" "$PREFIX/tls"
install -m 755 "$BINARY" "$PREFIX/gamesdb.new"
mv -f "$PREFIX/gamesdb.new" "$PREFIX/gamesdb"
xattr -d com.apple.quarantine "$PREFIX/gamesdb" 2>/dev/null || true   # in case it arrived via a browser download

say "Fetching Cloudflare IP ranges for TRUSTED_PROXY_CIDRS"
TMP=$(mktemp); trap 'rm -f "$TMP"' EXIT
CIDRS=$("$HERE/cloudflare-ips.sh" "$TMP" | sed -n 's/^TRUSTED_PROXY_CIDRS=//p')
[ -n "$CIDRS" ] || { echo "could not fetch Cloudflare ranges" >&2; exit 1; }

ENV_FILE=$PREFIX/.env
set_env() { # set_env KEY VALUE: replace (or add) the KEY= line, uncommenting it if needed
  if grep -qE "^#?$1=" "$ENV_FILE"; then
    sed -i '' -E "s|^#?$1=.*|$1=$2|" "$ENV_FILE"
  else
    printf '%s=%s\n' "$1" "$2" >> "$ENV_FILE"
  fi
}
if [ ! -f "$ENV_FILE" ]; then
  say "Creating $ENV_FILE from .env.example"
  [ -f "$HERE/../../.env.example" ] || { echo ".env.example not found; run from the repository root" >&2; exit 1; }
  cp "$HERE/../../.env.example" "$ENV_FILE"
  set_env BASE_URL https://db.getutm.app
  set_env LISTEN_ADDR :443
  set_env TLS_CERT "$PREFIX/tls/origin.pem"
  set_env TLS_KEY "$PREFIX/tls/origin.key"
  set_env DATA_DIR "$PREFIX/data"
  set_env SECRET_KEY "$(openssl rand -hex 32)"
  NEW_ENV=1
else
  NEW_ENV=0
fi
set_env TRUSTED_PROXY_CIDRS "$CIDRS"
chmod 600 "$ENV_FILE"
PORT=$(sed -n 's/^LISTEN_ADDR=.*:\([0-9]*\)$/\1/p' "$ENV_FILE")
[ -n "$PORT" ] || PORT=443

say "Installing LaunchAgent app.getutm.db"
AGENTS=$HOME/Library/LaunchAgents
mkdir -p "$AGENTS"
sed "s|__PREFIX__|$PREFIX|g" "$HERE/app.getutm.db.plist" > "$AGENTS/app.getutm.db.plist"
if [ ! -s "$PREFIX/tls/origin.pem" ] || [ ! -s "$PREFIX/tls/origin.key" ]; then
  cat <<MSG

Files installed, but the service was not started: the origin certificate is missing.
  1. Cloudflare dashboard -> SSL/TLS -> Origin Server -> Create Certificate (db.getutm.app)
  2. Save it as $PREFIX/tls/origin.pem and the key as $PREFIX/tls/origin.key
  3. Fill in $ENV_FILE (SMTP_*, IGDB_*, AKISMET_KEY)
  4. Run this script again
MSG
  exit 0
fi
chmod 600 "$PREFIX"/tls/*
DOMAIN="gui/$(id -u)"
# Reload so plist changes take effect. launchd can refuse a bootstrap for a
# few seconds after a bootout while the old job is torn down, so retry.
launchctl bootout "$DOMAIN/app.getutm.db" 2>/dev/null || true
loaded=0
for _ in $(seq 1 15); do
  if err=$(launchctl bootstrap "$DOMAIN" "$AGENTS/app.getutm.db.plist" 2>&1); then loaded=1; break; fi
  sleep 1
done
if [ "$loaded" != 1 ]; then
  echo "could not load the agent: $err" >&2
  echo "if you are not in a login session, it starts at the next login; otherwise run: launchctl bootstrap $DOMAIN $AGENTS/app.getutm.db.plist" >&2
  exit 1
fi

say "Checking"
# -q (first argument) makes curl ignore ~/.curlrc.
for _ in $(seq 1 20); do
  if curl -q -sk --max-time 3 -o /dev/null -w '%{http_code}' "https://127.0.0.1:$PORT/healthz" 2>/dev/null | grep -q '^200$'; then
    echo "service answers on https://127.0.0.1:$PORT (TLS)"
    [ "$NEW_ENV" = 1 ] && echo "Now fill in $ENV_FILE (SMTP_*, IGDB_*, AKISMET_KEY) and run: launchctl kickstart -k $DOMAIN/app.getutm.db"
    exit 0
  fi
  sleep 1
done
echo "no answer from https://127.0.0.1:$PORT/healthz" >&2
launchctl print "$DOMAIN/app.getutm.db" 2>&1 | grep -E '^\s*(state|pid|last exit code) ' | sed 's/^\s*/  /' >&2 || true
echo "last log lines ($PREFIX/log/gamesdb.log):" >&2
tail -n 5 "$PREFIX/log/gamesdb.log" 2>/dev/null | sed 's/^/  /' >&2 || true
exit 1
