#!/bin/sh
# Downloads Cloudflare's IP ranges, writes them to the pf table file and
# prints the matching TRUSTED_PROXY_CIDRS line. Installed to /usr/local/sbin by
# install-system.sh and run monthly as root; install.sh runs it as the service
# user with a temporary output path just to get the list.
#   cloudflare-ips.sh [output-file]   (default /etc/pf.anchors/gamesdb.cloudflare)
set -eu
OUT=${1:-/etc/pf.anchors/gamesdb.cloudflare}
TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT
curl -q -fsS https://www.cloudflare.com/ips-v4 > "$TMP"
echo >> "$TMP"
curl -q -fsS https://www.cloudflare.com/ips-v6 >> "$TMP"
# Refuse an empty or malformed download rather than lock everyone out.
grep -Ec '^[0-9a-f.:]+/[0-9]+$' "$TMP" | grep -q '^[1-9]' || { echo "unexpected response from cloudflare.com" >&2; exit 1; }
grep -E '^[0-9a-f.:]+/[0-9]+$' "$TMP" > "$OUT"
if [ "$(id -u)" -eq 0 ] && /sbin/pfctl -s info 2>/dev/null | grep -q 'Status: Enabled'; then
  /sbin/pfctl -a gamesdb -t cloudflare -T replace -f "$OUT" >/dev/null 2>&1 || true
fi
echo "TRUSTED_PROXY_CIDRS=$(paste -sd, "$OUT")"
