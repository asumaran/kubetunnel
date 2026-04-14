#!/usr/bin/env bash
#
# Dev update loop: rebuild both binaries, install them under /usr/local/bin,
# and kickstart the LaunchDaemon.
#
# Relies on the sudoers snippet installed by scripts/install-sudoers.sh so no
# password prompt is needed. If you skipped that step, sudo will prompt once.
#
set -euo pipefail

cd "$(dirname "$0")/.."

echo "[1/3] Building..."
go build -o ./bin/kubetunneld ./cmd/kubetunneld
go build -o ./bin/tunnelctl ./cmd/tunnelctl

echo "[2/3] Installing binaries..."
sudo -n install -m 0755 ./bin/kubetunneld /usr/local/bin/kubetunneld 2>/dev/null \
  || sudo install -m 0755 ./bin/kubetunneld /usr/local/bin/kubetunneld
sudo -n install -m 0755 ./bin/tunnelctl /usr/local/bin/tunnelctl 2>/dev/null \
  || sudo install -m 0755 ./bin/tunnelctl /usr/local/bin/tunnelctl

echo "[3/3] Restarting daemon..."
if sudo launchctl print system/dev.kubetunnel >/dev/null 2>&1; then
  sudo -n launchctl kickstart -k system/dev.kubetunnel 2>/dev/null \
    || sudo launchctl kickstart -k system/dev.kubetunnel
else
  echo "    daemon not bootstrapped yet — running 'tunnelctl install'..."
  sudo -n /usr/local/bin/tunnelctl install 2>/dev/null \
    || sudo /usr/local/bin/tunnelctl install
fi

# Wait for the daemon's control socket to come back before returning so the
# next `tunnelctl ...` invocation doesn't race the restart.
echo -n "    waiting for daemon..."
SOCKET=/var/run/kubetunnel.sock
for i in $(seq 1 30); do
  if [ -S "$SOCKET" ] && /usr/local/bin/tunnelctl status >/dev/null 2>&1; then
    echo " ready"
    break
  fi
  sleep 0.2
  echo -n "."
done

echo
echo "✓ Updated. Run: tunnelctl status"
