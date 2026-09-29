#!/usr/bin/env bash
# Install a published ClinePassProxy release on the VPS.
#
# Release artifacts are built by GitHub Actions only. This script downloads the
# published binary, verifies its checksum, swaps it in and restarts the systemd
# unit. It never compiles anything, so a VPS deployment cannot drift from the
# released, reviewed artifacts.
#
# Usage:
#   scripts/deploy-vps.sh v0.1.3
#   scripts/deploy-vps.sh            # newest published release
set -euo pipefail

REPO="${CLINEPASSPROXY_REPO:-nonlog/ClinePassProxy}"
INSTALL_DIR="${CLINEPASSPROXY_INSTALL_DIR:-/opt/clinepassproxy}"
BIN_PATH="${INSTALL_DIR}/bin/clinepassproxy"
SERVICE="${CLINEPASSPROXY_SERVICE:-clinepassproxy}"
HEALTH_URL="${CLINEPASSPROXY_HEALTH_URL:-http://127.0.0.1:8788/health}"
READY_URL="${CLINEPASSPROXY_READY_URL:-http://127.0.0.1:8788/ready}"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

TAG="${1:-}"
if [[ -n "$TAG" ]]; then
  BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"
else
  BASE_URL="https://github.com/${REPO}/releases/latest/download"
fi
ASSET="clinepassproxy-linux-${ARCH}"

if ! systemctl cat "$SERVICE" >/dev/null 2>&1; then
  echo "systemd unit ${SERVICE} is not installed; install it before deploying" >&2
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "downloading ${ASSET} from ${BASE_URL}"
curl -fsSL -o "${WORK}/${ASSET}" "${BASE_URL}/${ASSET}"
curl -fsSL -o "${WORK}/${ASSET}.sha256" "${BASE_URL}/${ASSET}.sha256"
(cd "$WORK" && sha256sum -c "${ASSET}.sha256")

install -d -m 0700 "${INSTALL_DIR}/bin"
install -m 0755 "${WORK}/${ASSET}" "${BIN_PATH}.new"
INSTALLED_VERSION="$("${BIN_PATH}.new" -version 2>&1)"
mv "${BIN_PATH}.new" "$BIN_PATH"
echo "installed: ${INSTALLED_VERSION}"

systemctl restart "$SERVICE"

for _ in $(seq 1 30); do
  if curl -fsS "$HEALTH_URL" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

echo -n "health: "
curl -fsS "$HEALTH_URL" || { echo "FAILED"; journalctl -u "$SERVICE" -n 30 --no-pager; exit 1; }
echo
echo -n "ready:  "
curl -fsS "$READY_URL" || echo "(not ready yet; check credentials and the gateway key)"
echo
