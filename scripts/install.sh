#!/usr/bin/env bash
#
# Bootstrap script for kubetunnel on macOS.
#
#   1. Checks dependencies (go, mkcert, kubectl).
#   2. Builds the daemon + CLI and installs them under /usr/local/bin.
#   3. Copies config.example.yaml to ~/.config/kubetunnel/config.yaml if missing.
#   4. Runs `mkcert -install` and generates certs for each hostname.
#   5. Runs `sudo tunnelctl install` to:
#        - add /etc/hosts entries
#        - install the LaunchDaemon plist
#        - bootstrap kubetunneld
#
# Re-running this script is idempotent.
set -euo pipefail

cd "$(dirname "$0")/.."

check() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing dependency: $1"
    echo "  install with: $2"
    exit 1
  fi
}

check go "brew install go"
check mkcert "brew install mkcert"
check kubectl "brew install kubectl"

echo "[1/5] Building binaries..."
go build -o ./bin/kubetunneld ./cmd/kubetunneld
go build -o ./bin/tunnelctl ./cmd/tunnelctl

echo "[2/5] Installing binaries to /usr/local/bin (requires sudo)..."
sudo install -m 0755 ./bin/kubetunneld /usr/local/bin/kubetunneld
sudo install -m 0755 ./bin/tunnelctl /usr/local/bin/tunnelctl

CFG_DIR="$HOME/.config/kubetunnel"
CFG_FILE="$CFG_DIR/config.yaml"
if [ ! -f "$CFG_FILE" ]; then
  echo "[3/5] Installing default config at $CFG_FILE"
  mkdir -p "$CFG_DIR"
  cp ./config.example.yaml "$CFG_FILE"
else
  echo "[3/5] Config already exists at $CFG_FILE (leaving it alone)"
fi

echo "[4/5] Installing local CA and generating certs..."
tunnelctl cert-install

echo "[5/5] Installing /etc/hosts entries and LaunchDaemon (requires sudo)..."
sudo tunnelctl install

echo
echo "✓ kubetunnel is installed and running."
echo "  tunnelctl status       — see tunnel state"
echo "  tunnelctl dashboard    — launch the TUI"
echo "  tunnelctl logs -f      — tail logs"
