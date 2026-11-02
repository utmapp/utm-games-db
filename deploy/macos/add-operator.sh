#!/bin/bash
# Gives a machine access to the internal API socket over SSH, and nothing
# else. Run as an administrator:
#
#   sudo ./deploy/macos/add-operator.sh "ssh-ed25519 AAAA... triage-1"
#
# Creates a hidden account (default _triage) with no password and no home
# whose only privilege is membership in the service group, which is what
# lets sshd open the socket on its behalf. Its shell is /bin/sh because
# macOS refuses SSH logins to accounts with /usr/bin/false; it is never
# reached interactively: the key is installed with forwarding-only options,
# and a sshd Match block refuses the account a terminal, SFTP, agent and
# X11 forwarding and replaces any command with sleep. What remains is
# forwarding connections, to Unix sockets the account may open (the
# internal API) and to TCP ports; it can read no files. (sshd cannot allow
# socket forwards while refusing TCP ones: one setting governs both.)
# The machine then connects with
#
#   ssh -N -L 8081:/usr/local/gamesdb/run/internal.sock _triage@host
#
# and uses http://127.0.0.1:8081. Run again with another key to add a
# second machine under the same account; keys accumulate in the key file.
#
# Variables: OPERATOR_USER (default _triage), GAMESDB_GROUP (default
# _gamesdb), PREFIX (default /usr/local/gamesdb).
# To remove a machine, delete its line from /etc/ssh/authorized_keys.d/<user>.
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "run with sudo" >&2; exit 1; }
KEY=${1:-}
case $KEY in
  ssh-ed25519\ *|ssh-rsa\ *|ecdsa-sha2-*\ *|sk-*\ *) ;;
  *) echo "usage: sudo $0 \"<public key line>\"" >&2; exit 1 ;;
esac
OPERATOR_USER=${OPERATOR_USER:-_triage}
SERVICE_GROUP=${GAMESDB_GROUP:-_gamesdb}
PREFIX=${PREFIX:-/usr/local/gamesdb}
KEYS_DIR=/etc/ssh/authorized_keys.d
KEYS_FILE=$KEYS_DIR/$OPERATOR_USER
CONF=/etc/ssh/sshd_config.d/020-gamesdb-$OPERATOR_USER.conf
say() { printf '\n==> %s\n' "$*"; }

GID=$(dscl . -read "/Groups/$SERVICE_GROUP" PrimaryGroupID 2>/dev/null | awk '{print $2}')
[ -n "$GID" ] || { echo "group $SERVICE_GROUP does not exist; run install.sh first" >&2; exit 1; }

if ! dscl . -read "/Users/$OPERATOR_USER" >/dev/null 2>&1; then
  say "Creating account $OPERATOR_USER (group $SERVICE_GROUP, no password, no home)"
  # Created with dscl rather than sysadminctl: a role account would get
  # /usr/bin/false, which PAM rejects for SSH. No AuthenticationAuthority
  # means no password can ever work; only the key below does.
  used=$( (dscl . -list /Users UniqueID; dscl . -list /Groups PrimaryGroupID) | awk '{print $2}')
  uid=; for id in $(seq 450 499); do echo "$used" | grep -qx "$id" || { uid=$id; break; }; done
  [ -n "$uid" ] || { echo "no free uid in 450-499" >&2; exit 1; }
  dscl . -create "/Users/$OPERATOR_USER"
  dscl . -create "/Users/$OPERATOR_USER" UniqueID "$uid"
  dscl . -create "/Users/$OPERATOR_USER" PrimaryGroupID "$GID"
  dscl . -create "/Users/$OPERATOR_USER" UserShell /bin/sh
  dscl . -create "/Users/$OPERATOR_USER" NFSHomeDirectory /var/empty
  dscl . -create "/Users/$OPERATOR_USER" RealName "UTM Database operator"
  dscl . -create "/Users/$OPERATOR_USER" IsHidden 1
  dscl . -create "/Users/$OPERATOR_USER" Password '*'
fi
gid_now=$(dscl . -read "/Users/$OPERATOR_USER" PrimaryGroupID | awk '{print $2}')
[ "$gid_now" = "$GID" ] || { echo "$OPERATOR_USER has primary group $gid_now, not $SERVICE_GROUP ($GID); delete the account (dscl . -delete /Users/$OPERATOR_USER) and run again" >&2; exit 1; }
shell_now=$(dscl . -read "/Users/$OPERATOR_USER" UserShell | awk '{print $2}')
if [ "$shell_now" = /usr/bin/false ]; then
  # macOS PAM denies SSH to accounts with this shell (role accounts have it
  # and refuse to change it).
  dscl . -change "/Users/$OPERATOR_USER" UserShell /usr/bin/false /bin/sh 2>/dev/null || { echo "$OPERATOR_USER is a role account; delete it (dseditgroup -o edit -d $OPERATOR_USER -t user com.apple.access_ssh; dscl . -delete /Users/$OPERATOR_USER) and run again" >&2; exit 1; }
fi

# Remote Login may be limited to a group; the account needs to be in it.
if dscl . -read /Groups/com.apple.access_ssh >/dev/null 2>&1; then
  dseditgroup -o checkmember -m "$OPERATOR_USER" com.apple.access_ssh >/dev/null 2>&1 || { say "Allowing $OPERATOR_USER to log in over SSH"; dseditgroup -o edit -a "$OPERATOR_USER" -t user com.apple.access_ssh; }
fi

say "Installing the key in $KEYS_FILE"
install -d -o root -g wheel -m 755 "$KEYS_DIR"
touch "$KEYS_FILE"; chown root:wheel "$KEYS_FILE"; chmod 644 "$KEYS_FILE"
LINE="restrict,port-forwarding $KEY"
grep -qxF "$LINE" "$KEYS_FILE" || echo "$LINE" >> "$KEYS_FILE"

say "sshd: forwarding to Unix sockets only for $OPERATOR_USER ($CONF)"
# A drop-in, sorted before Apple's 100-macos.conf; sshd takes the first
# value it sees per keyword, and a Match block applies only to this user.
cat > "$CONF" <<SSHD
# Installed by deploy/macos/add-operator.sh. $OPERATOR_USER may only forward
# connections (to the internal API socket, or TCP ports); no shell,
# terminal, SFTP, agent or X11 forwarding.
Match User $OPERATOR_USER
    AuthorizedKeysFile $KEYS_FILE
    AllowTcpForwarding local
    AllowStreamLocalForwarding local
    AllowAgentForwarding no
    X11Forwarding no
    PermitTTY no
    PermitUserRC no
    ForceCommand /bin/sleep 2147483647
SSHD
chmod 644 "$CONF"
sshd -t || { echo "sshd rejected the configuration; removing $CONF" >&2; rm -f "$CONF"; exit 1; }

say "Done"
echo "From the operator machine (new connections pick the configuration up at once):"
echo "  ssh -N -L 8081:$PREFIX/run/internal.sock $OPERATOR_USER@$(hostname -s 2>/dev/null || echo host)"
echo "  curl http://127.0.0.1:8081/internal/healthz"
