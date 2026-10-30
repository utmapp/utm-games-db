#!/bin/bash
# Machine-wide part of the macOS deployment; run once by an administrator:
#
#   sudo ./deploy/macos/install-system.sh <service user>      # from any checkout
#
# Installs only what cannot live in the service user's home: the pf allowlist
# (port open to Cloudflare and loopback only), the daemon that enables pf at
# boot, the monthly allowlist refresh, and log rotation. Everything else is
# done by install.sh, run by the service user without sudo. Safe to re-run.
#
# Environment overrides: PREFIX (default <user home>/gamesdb), PORT (443).
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "run with sudo" >&2; exit 1; }
USER_NAME=${1:-}
[ -n "$USER_NAME" ] || { echo "usage: sudo $0 <service user>" >&2; exit 1; }
HOME_DIR=$(dscl . -read "/Users/$USER_NAME" NFSHomeDirectory 2>/dev/null | awk '{print $2}')
[ -d "$HOME_DIR" ] || { echo "no such user: $USER_NAME" >&2; exit 1; }
HERE=$(cd "$(dirname "$0")" && pwd)
PREFIX=${PREFIX:-$HOME_DIR/gamesdb}
PORT=${PORT:-443}
say() { printf '\n==> %s\n' "$*"; }

mkdir -p /usr/local/sbin /etc/pf.anchors /etc/newsyslog.d /Library/LaunchDaemons   # a fresh Mac lacks /usr/local/sbin

say "Cloudflare IP ranges"
install -m 755 "$HERE/cloudflare-ips.sh" /usr/local/sbin/cloudflare-ips.sh
/usr/local/sbin/cloudflare-ips.sh >/dev/null

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
pfctl -a gamesdb -s rules 2>/dev/null | sed 's/^/pf: /'

say "Log rotation"
sed -e "s|__PREFIX__|$PREFIX|g" -e "s|__USER__|$USER_NAME|g" "$HERE/newsyslog.conf" > /etc/newsyslog.d/gamesdb.conf

say "Done. Now, as $USER_NAME (no sudo): ./deploy/macos/install.sh"
