#!/bin/bash
# Installs or upgrades the UTM Database server on a Mac behind Cloudflare.
# Run as an administrator, from the repository checkout:
#
#   sudo ./deploy/macos/install.sh
#
# The service runs as a dedicated role account from a LaunchDaemon, so it
# is up from boot regardless of who is logged in. Its data, settings and
# keys under $PREFIX are readable by that account only. The internal API is
# a Unix socket that the account's group may use; the administrator who
# runs this script is added to the group (and so are the names in
# OPERATORS). Also installed machine-wide: the pf allowlist (the public
# port open to Cloudflare and loopback only), the daemon that enables pf at
# boot, the monthly allowlist refresh, and log rotation.
#
# Re-run after any change (new commit, edited .env, new certificate): it
# rebuilds only if a source changed, installs, and restarts the service.
# .env and the data directory are never overwritten.
#
# Variables (environment):
#   GAMESDB_USER   service account (default _gamesdb; role accounts start with _)
#   GAMESDB_GROUP  its group (default: same name)
#   OPERATORS      comma-separated users added to the group (default: the sudo caller)
#   PREFIX         install directory (default /usr/local/gamesdb)
#   PORT           public HTTPS port (default 443)
#   BINARY         skip the build and install this file
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "run with sudo" >&2; exit 1; }
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
SERVICE_USER=${GAMESDB_USER:-_gamesdb}
SERVICE_GROUP=${GAMESDB_GROUP:-$SERVICE_USER}
PREFIX=${PREFIX:-/usr/local/gamesdb}
PORT=${PORT:-443}
OPERATORS=${OPERATORS:-${SUDO_USER:-}}
SOCKET=$PREFIX/run/internal.sock
say() { printf '\n==> %s\n' "$*"; }

# --- service account and group ------------------------------------------------
free_id() { # free_id FROM TO: first id in the range unused by users and groups
  local used
  used=$( (dscl . -list /Users UniqueID; dscl . -list /Groups PrimaryGroupID) | awk '{print $2}')
  for id in $(seq "$1" "$2"); do
    echo "$used" | grep -qx "$id" || { echo "$id"; return; }
  done
  echo "no free id in $1-$2" >&2; exit 1
}
if ! dscl . -read "/Groups/$SERVICE_GROUP" >/dev/null 2>&1; then
  say "Creating group $SERVICE_GROUP"
  gid=$(free_id 450 499)
  dscl . -create "/Groups/$SERVICE_GROUP"
  dscl . -create "/Groups/$SERVICE_GROUP" PrimaryGroupID "$gid"
  dscl . -create "/Groups/$SERVICE_GROUP" RealName "UTM Database"
  dscl . -create "/Groups/$SERVICE_GROUP" Password '*'
fi
GID=$(dscl . -read "/Groups/$SERVICE_GROUP" PrimaryGroupID | awk '{print $2}')
if ! dscl . -read "/Users/$SERVICE_USER" >/dev/null 2>&1; then
  case $SERVICE_USER in _*) ;; *) echo "GAMESDB_USER must start with _ (a macOS role account)" >&2; exit 1 ;; esac
  say "Creating role account $SERVICE_USER"
  # macOS requires role accounts to have a UID in 450-499.
  sysadminctl -addUser "$SERVICE_USER" -fullName "UTM Database" -UID "$(free_id 450 499)" -GID "$GID" -roleAccount -home /var/empty -shell /usr/bin/false 2>&1 | grep -v "^$" | sed 's/^/  /' || true
  dscl . -read "/Users/$SERVICE_USER" UniqueID >/dev/null 2>&1 || { echo "could not create $SERVICE_USER" >&2; exit 1; }
fi
IFS=, read -ra ops <<<"$OPERATORS"
for op in "${ops[@]}"; do
  [ -n "$op" ] && [ "$op" != root ] || continue
  dseditgroup -o checkmember -m "$op" "$SERVICE_GROUP" >/dev/null 2>&1 || { say "Adding $op to group $SERVICE_GROUP"; dseditgroup -o edit -a "$op" -t user "$SERVICE_GROUP"; }
done

# --- build (as the caller, not root) -----------------------------------------------
BINARY=${BINARY:-}
if [ -z "$BINARY" ]; then
  say "Building"
  if [ -n "${SUDO_USER:-}" ]; then
    sudo -u "$SUDO_USER" -H env PATH="$PATH" make -C "$REPO" --no-print-directory build-mac
  else
    make -C "$REPO" --no-print-directory build-mac
  fi
  BINARY=$REPO/gamesdb-darwin-arm64
fi
[ -x "$BINARY" ] || { echo "no binary at $BINARY (is Go installed? brew install go)" >&2; exit 1; }

# --- layout -----------------------------------------------------------------------
say "Installing to $PREFIX (service account $SERVICE_USER, group $SERVICE_GROUP)"
install -d -o root -g wheel -m 755 "$PREFIX"
install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 700 "$PREFIX/data" "$PREFIX/tls"
install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 750 "$PREFIX/run" "$PREFIX/log"
[ -f "$PREFIX/log/gamesdb.log" ] || install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 640 /dev/null "$PREFIX/log/gamesdb.log"
install -o root -g wheel -m 755 "$BINARY" "$PREFIX/gamesdb.new"
mv -f "$PREFIX/gamesdb.new" "$PREFIX/gamesdb"
xattr -d com.apple.quarantine "$PREFIX/gamesdb" 2>/dev/null || true

mkdir -p /usr/local/sbin /etc/pf.anchors /etc/newsyslog.d /Library/LaunchDaemons   # a fresh Mac lacks /usr/local/sbin
say "Cloudflare IP ranges"
install -m 755 "$HERE/cloudflare-ips.sh" /usr/local/sbin/cloudflare-ips.sh
CIDRS=$(/usr/local/sbin/cloudflare-ips.sh | sed -n 's/^TRUSTED_PROXY_CIDRS=//p')
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
  cp "$REPO/.env.example" "$ENV_FILE"
  set_env BASE_URL https://db.getutm.app
  set_env LISTEN_ADDR ":$PORT"
  set_env TLS_CERT "$PREFIX/tls/origin.pem"
  set_env TLS_KEY "$PREFIX/tls/origin.key"
  set_env DATA_DIR "$PREFIX/data"
  set_env SECRET_KEY "$(openssl rand -hex 32)"
  NEW_ENV=1
else
  NEW_ENV=0
fi
set_env INTERNAL_LISTEN_ADDR "$SOCKET"
set_env TRUSTED_PROXY_CIDRS "$CIDRS"
chown "$SERVICE_USER:$SERVICE_GROUP" "$ENV_FILE" && chmod 600 "$ENV_FILE"
PORT=$(sed -n 's/^LISTEN_ADDR=.*:\([0-9]*\)$/\1/p' "$ENV_FILE")
[ -n "$PORT" ] || PORT=443
for f in "$PREFIX"/tls/*; do [ -f "$f" ] && chown "$SERVICE_USER:$SERVICE_GROUP" "$f" && chmod 600 "$f"; done

# --- machine-wide: pf, its daemons, log rotation ------------------------------------
say "pf: port $PORT reachable from Cloudflare and loopback only"
sed "s/__PORT__/$PORT/g" "$HERE/pf.anchor" > /etc/pf.anchors/gamesdb
chmod 644 /etc/pf.anchors/gamesdb
# Our own top-level ruleset includes Apple's and adds the anchor, so
# /etc/pf.conf itself is never edited and an OS update cannot undo this.
cat > /etc/pf.gamesdb.conf <<'PF'
include "/etc/pf.conf"
anchor "gamesdb"
load anchor "gamesdb" from "/etc/pf.anchors/gamesdb"
PF
chmod 644 /etc/pf.gamesdb.conf
pfctl -n -f /etc/pf.gamesdb.conf >/dev/null 2>&1 || { echo "pf ruleset does not parse; check: sudo pfctl -n -f /etc/pf.gamesdb.conf" >&2; exit 1; }
for d in app.getutm.pf app.getutm.cloudflare-ips; do
  install -m 644 "$HERE/$d.plist" "/Library/LaunchDaemons/$d.plist"
  launchctl bootout "system/$d" 2>/dev/null || true
  launchctl bootstrap system "/Library/LaunchDaemons/$d.plist"
done
pfctl -q -e 2>/dev/null || true   # non-zero when already enabled
# pfctl warns that -f discards rules system services add at runtime; a server has none.
pfctl -q -f /etc/pf.gamesdb.conf 2>&1 | grep -v -e 'Use of -f option' -e 'present in the main ruleset' -e 'See /etc/pf.conf' -e 'ALTQ' -e '^$' >&2 || true
sed -e "s|__PREFIX__|$PREFIX|g" -e "s|__USER__|$SERVICE_USER|g" -e "s|__GROUP__|$SERVICE_GROUP|g" "$HERE/newsyslog.conf" > /etc/newsyslog.d/gamesdb.conf

# --- the service -----------------------------------------------------------------------
PLIST=/Library/LaunchDaemons/app.getutm.db.plist
sed -e "s|__PREFIX__|$PREFIX|g" -e "s|__USER__|$SERVICE_USER|g" -e "s|__GROUP__|$SERVICE_GROUP|g" "$HERE/app.getutm.db.plist" > "$PLIST.new"
chmod 644 "$PLIST.new"
if [ ! -s "$PREFIX/tls/origin.pem" ] || [ ! -s "$PREFIX/tls/origin.key" ]; then
  mv -f "$PLIST.new" "$PLIST"
  cat <<MSG

Files installed, but the service was not started: the origin certificate is missing.
  1. Cloudflare dashboard -> SSL/TLS -> Origin Server -> Create Certificate (db.getutm.app)
  2. Save it as $PREFIX/tls/origin.pem and the key as $PREFIX/tls/origin.key (sudo; this script fixes ownership)
  3. Fill in $ENV_FILE (SMTP_*, IGDB_*, AKISMET_KEY)
  4. Run this script again
MSG
  exit 0
fi
# Start the daemon the first time; afterwards restart it so the new binary
# and .env are picked up, or reload it when its definition changed (launchd
# can refuse a bootstrap for a few seconds after a bootout, hence the retry).
if ! launchctl print system/app.getutm.db >/dev/null 2>&1; then
  action="Starting"
elif [ -f "$PLIST" ] && cmp -s "$PLIST.new" "$PLIST"; then
  action="Restarting"
else
  action="Reloading"
fi
mv -f "$PLIST.new" "$PLIST"
say "$action app.getutm.db"
case $action in
Restarting)
  launchctl kickstart -k system/app.getutm.db ;;
*)
  launchctl bootout system/app.getutm.db 2>/dev/null || true
  loaded=0
  for _ in $(seq 1 15); do
    if err=$(launchctl bootstrap system "$PLIST" 2>&1); then loaded=1; break; fi
    sleep 1
  done
  [ "$loaded" = 1 ] || { echo "could not load the daemon: $err" >&2; exit 1; } ;;
esac

say "Checking"
# -q (first argument) makes curl ignore ~/.curlrc.
for _ in $(seq 1 20); do
  if curl -q -sk --max-time 3 -o /dev/null -w '%{http_code}' "https://127.0.0.1:$PORT/healthz" 2>/dev/null | grep -q '^200$'; then
    echo "public:   https://127.0.0.1:$PORT answers (TLS)"
    break
  fi
  sleep 1
done
if ! curl -q -sk --max-time 3 -o /dev/null -w '%{http_code}' "https://127.0.0.1:$PORT/healthz" 2>/dev/null | grep -q '^200$'; then
  echo "no answer from https://127.0.0.1:$PORT/healthz" >&2
  launchctl print system/app.getutm.db 2>&1 | grep -E '^\s*(state|pid|last exit code) ' | sed 's/^\s*/  /' >&2 || true
  echo "last log lines ($PREFIX/log/gamesdb.log):" >&2
  tail -n 5 "$PREFIX/log/gamesdb.log" 2>/dev/null | sed 's/^/  /' >&2 || true
  exit 1
fi
# The internal API, reached the way operators reach it: as a group member.
probe_as=${ops[0]:-}
if [ -n "$probe_as" ] && sudo -u "$probe_as" curl -q -s --max-time 3 --unix-socket "$SOCKET" -o /dev/null -w '%{http_code}' http://localhost/internal/healthz 2>/dev/null | grep -q '^200$'; then
  echo "internal: $SOCKET answers for $probe_as"
else
  echo "internal: $SOCKET (members of $SERVICE_GROUP; a user added just now sees the group after logging in again)"
fi
[ "$NEW_ENV" = 1 ] && echo "Now fill in $ENV_FILE (SMTP_*, IGDB_*, AKISMET_KEY) and run this script again"
exit 0
