#!/usr/bin/env bash
#
# Install a sudoers.d snippet that whitelists (without password) the exact
# commands scripts/update.sh needs. Safe because:
#   - It only covers 3 fixed paths with fixed arguments.
#   - `install` is constrained to a single destination.
#   - `launchctl kickstart -k` only restarts the kubetunnel unit.
#
# Run once after installing kubetunnel. Re-running is idempotent.
#
set -euo pipefail

USER_NAME="${SUDO_USER:-$USER}"
SUDOERS_FILE="/etc/sudoers.d/kubetunnel-dev"

TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT

cat > "$TMP" <<EOF
# Allow $USER_NAME to run kubetunnel dev update commands without a password.
# Installed by scripts/install-sudoers.sh.
$USER_NAME ALL=(root) NOPASSWD: /usr/bin/install -m 0755 * /usr/local/bin/kubetunneld
$USER_NAME ALL=(root) NOPASSWD: /usr/bin/install -m 0755 * /usr/local/bin/tunnelctl
$USER_NAME ALL=(root) NOPASSWD: /bin/launchctl kickstart -k system/dev.kubetunnel
$USER_NAME ALL=(root) NOPASSWD: /bin/launchctl bootout system/dev.kubetunnel
$USER_NAME ALL=(root) NOPASSWD: /bin/launchctl bootstrap system /Library/LaunchDaemons/dev.kubetunnel.plist
EOF

# Validate before installing — visudo -cf checks syntax.
if ! sudo visudo -cf "$TMP" >/dev/null; then
  echo "sudoers snippet failed validation; aborting."
  exit 1
fi

sudo install -m 0440 -o root -g wheel "$TMP" "$SUDOERS_FILE"
echo "✓ Installed $SUDOERS_FILE"
echo
echo "You can now run: ./scripts/update.sh"
echo "To remove: sudo rm $SUDOERS_FILE"
