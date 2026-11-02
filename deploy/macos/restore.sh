#!/bin/bash
# Restores a backup taken from GET /internal/backup. Run as the service
# user, from the repository checkout:
#
#   ./deploy/macos/restore.sh utmdb-backup-20261101-120000.tar
#
# It stops the service, keeps the current data and settings aside, puts the
# archive's pieces in place (settings with SECRET_KEY, TLS and DKIM keys, the
# database without the catalog, the uploaded files) and runs install.sh,
# which starts the service; the server then imports the catalog and
# re-resolves covers on its own.
#
# Environment overrides: PREFIX (default ~/gamesdb).
set -euo pipefail

[ "$(id -u)" -ne 0 ] || { echo "run as the service user, not root" >&2; exit 1; }
ARCHIVE=${1:-}
[ -n "$ARCHIVE" ] && [ -f "$ARCHIVE" ] || { echo "usage: $0 <backup.tar>" >&2; exit 1; }
HERE=$(cd "$(dirname "$0")" && pwd)
PREFIX=${PREFIX:-$HOME/gamesdb}
say() { printf '\n==> %s\n' "$*"; }

say "Checking $ARCHIVE"
tar -tf "$ARCHIVE" manifest.json db/gamesdb.sqlite >/dev/null || { echo "not a UTM Database backup (manifest.json or db/gamesdb.sqlite missing)" >&2; exit 1; }
tar -xOf "$ARCHIVE" manifest.json | sed -n 's/.*"created_at": "\([^"]*\)".*/backup taken \1/p'

DOMAIN="gui/$(id -u)"
if launchctl print "$DOMAIN/app.getutm.db" >/dev/null 2>&1; then
  say "Stopping the service"
  launchctl bootout "$DOMAIN/app.getutm.db" 2>/dev/null || true
fi

STAMP=$(date +%Y%m%d-%H%M%S)
if [ -e "$PREFIX/data" ]; then
  say "Keeping the current data as $PREFIX/data.pre-restore-$STAMP"
  mv "$PREFIX/data" "$PREFIX/data.pre-restore-$STAMP"
fi
[ -f "$PREFIX/.env" ] && cp "$PREFIX/.env" "$PREFIX/.env.pre-restore-$STAMP"
mkdir -p "$PREFIX/data/uploads" "$PREFIX/data/dkim" "$PREFIX/tls" "$PREFIX/log"
chmod 700 "$PREFIX" "$PREFIX/data" "$PREFIX/tls"

say "Extracting"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
tar -xf "$ARCHIVE" -C "$TMP"
install -m 600 "$TMP/db/gamesdb.sqlite" "$PREFIX/data/gamesdb.sqlite"
[ -f "$TMP/env" ] && install -m 600 "$TMP/env" "$PREFIX/.env"
[ -d "$TMP/tls" ] && for f in "$TMP"/tls/*; do [ -f "$f" ] && install -m 600 "$f" "$PREFIX/tls/"; done
[ -d "$TMP/dkim" ] && for f in "$TMP"/dkim/*; do [ -f "$f" ] && install -m 600 "$f" "$PREFIX/data/dkim/"; done
if [ -d "$TMP/uploads" ]; then
  (cd "$TMP/uploads" && find . -type f -print0 | while IFS= read -r -d '' f; do install -D -m 600 "$f" "$PREFIX/data/uploads/$f" 2>/dev/null || { mkdir -p "$(dirname "$PREFIX/data/uploads/$f")"; install -m 600 "$f" "$PREFIX/data/uploads/$f"; }; done)
fi
# The restored .env may name paths from the old machine; point it here.
sed -i '' -E "s|^DATA_DIR=.*|DATA_DIR=$PREFIX/data|; s|^TLS_CERT=.*|TLS_CERT=$PREFIX/tls/$(basename "$(sed -n 's/^TLS_CERT=//p' "$PREFIX/.env")")|; s|^TLS_KEY=.*|TLS_KEY=$PREFIX/tls/$(basename "$(sed -n 's/^TLS_KEY=//p' "$PREFIX/.env")")|" "$PREFIX/.env" 2>/dev/null || true
sed -i '' -E "/^MAIL_DKIM_KEY=/d" "$PREFIX/.env" 2>/dev/null || true   # default path: DATA_DIR/dkim/<selector>.key
echo "database, settings and $(find "$PREFIX/data/uploads" -type f | wc -l | tr -d ' ') upload file(s) in place"

say "Starting the service"
"$HERE/install.sh"
echo
echo "The catalog is being imported in the background (a few minutes); covers and store links follow."
echo "The previous data is at $PREFIX/data.pre-restore-$STAMP; remove it once the site looks right."
