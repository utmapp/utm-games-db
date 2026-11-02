#!/bin/bash
# Restores a backup taken from GET /internal/backup. Run as an administrator
# from the repository checkout, after install.sh has created the service
# account (a first run that stops at the missing certificate is enough):
#
#   sudo ./deploy/macos/restore.sh utmdb-backup-20261101-120000.tar
#
# It stops the service, keeps the current data and settings aside, puts the
# archive's pieces in place owned by the service account (settings with
# SECRET_KEY, TLS and DKIM keys, the database without the catalog, the
# uploaded files) and runs install.sh, which starts the service; the server
# then imports the catalog and re-resolves covers on its own.
#
# Variables: GAMESDB_USER, GAMESDB_GROUP, PREFIX as for install.sh.
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "run with sudo" >&2; exit 1; }
ARCHIVE=${1:-}
[ -n "$ARCHIVE" ] && [ -f "$ARCHIVE" ] || { echo "usage: sudo $0 <backup.tar>" >&2; exit 1; }
HERE=$(cd "$(dirname "$0")" && pwd)
SERVICE_USER=${GAMESDB_USER:-_gamesdb}
SERVICE_GROUP=${GAMESDB_GROUP:-$SERVICE_USER}
PREFIX=${PREFIX:-/usr/local/gamesdb}
say() { printf '\n==> %s\n' "$*"; }
dscl . -read "/Users/$SERVICE_USER" >/dev/null 2>&1 || { echo "service account $SERVICE_USER does not exist; run install.sh first" >&2; exit 1; }

say "Checking $ARCHIVE"
tar -tf "$ARCHIVE" manifest.json db/gamesdb.sqlite >/dev/null || { echo "not a UTM Database backup (manifest.json or db/gamesdb.sqlite missing)" >&2; exit 1; }
tar -xOf "$ARCHIVE" manifest.json | sed -n 's/.*"created_at": "\([^"]*\)".*/backup taken \1/p'

if launchctl print system/app.getutm.db >/dev/null 2>&1; then
  say "Stopping the service"
  launchctl bootout system/app.getutm.db 2>/dev/null || true
fi

STAMP=$(date +%Y%m%d-%H%M%S)
if [ -e "$PREFIX/data" ]; then
  say "Keeping the current data as $PREFIX/data.pre-restore-$STAMP"
  mv "$PREFIX/data" "$PREFIX/data.pre-restore-$STAMP"
fi
[ -f "$PREFIX/.env" ] && cp -p "$PREFIX/.env" "$PREFIX/.env.pre-restore-$STAMP"
install -d -o root -g wheel -m 755 "$PREFIX"
install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 700 "$PREFIX/data" "$PREFIX/data/uploads" "$PREFIX/data/dkim" "$PREFIX/tls"
install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 750 "$PREFIX/run" "$PREFIX/log"

say "Extracting"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
tar -xf "$ARCHIVE" -C "$TMP"
install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 600 "$TMP/db/gamesdb.sqlite" "$PREFIX/data/gamesdb.sqlite"
[ -f "$TMP/env" ] && install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 600 "$TMP/env" "$PREFIX/.env"
for f in "$TMP"/tls/*; do [ -f "$f" ] && install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 600 "$f" "$PREFIX/tls/"; done
for f in "$TMP"/dkim/*; do [ -f "$f" ] && install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 600 "$f" "$PREFIX/data/dkim/"; done
if [ -d "$TMP/uploads" ]; then
  (cd "$TMP/uploads" && find . -type f -print0 | while IFS= read -r -d '' f; do
    install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 700 "$(dirname "$PREFIX/data/uploads/$f")"
    install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 600 "$f" "$PREFIX/data/uploads/$f"
  done)
fi
# The restored .env may name paths from the old machine; point it here.
CERT=$(basename "$(sed -n 's/^TLS_CERT=//p' "$PREFIX/.env")"); KEY=$(basename "$(sed -n 's/^TLS_KEY=//p' "$PREFIX/.env")")
sed -i '' -E "s|^DATA_DIR=.*|DATA_DIR=$PREFIX/data|; s|^TLS_CERT=.*|TLS_CERT=$PREFIX/tls/${CERT:-origin.pem}|; s|^TLS_KEY=.*|TLS_KEY=$PREFIX/tls/${KEY:-origin.key}|; /^MAIL_DKIM_KEY=/d; s|^INTERNAL_LISTEN_ADDR=.*|INTERNAL_LISTEN_ADDR=$PREFIX/run/internal.sock|" "$PREFIX/.env"
echo "database, settings and $(find "$PREFIX/data/uploads" -type f | wc -l | tr -d ' ') upload file(s) in place"

say "Starting the service"
"$HERE/install.sh"
echo
echo "The catalog is being imported in the background (a few minutes); covers and store links follow."
echo "The previous data is at $PREFIX/data.pre-restore-$STAMP; remove it once the site looks right."
