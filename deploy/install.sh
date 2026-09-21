#!/usr/bin/env bash
# Install (or reinstall/upgrade) PanDrive on a Linux server from GitHub Releases.
#
# One-liner from the README:
#   curl -fsSL https://raw.githubusercontent.com/jhopan/pandrive/master/deploy/install.sh | bash
#
# Modes:
#   install.sh                 install the LATEST release
#   install.sh v0.24.1         install a SPECIFIC version (tag without or with the leading v)
#   install.sh --system        also create & start the systemd service (default when root)
#   install.sh --no-system     skip the systemd service entirely
#
# Safety: the binary is checksum-verified against the release's SHA256SUMS before it is placed;
# an existing install is upgraded in place, keeping the previous binary as .prev and the database.
set -euo pipefail

REPO="${GITHUB_REPO:-jhopan/pandrive}"
INSTALL_DIR="${INSTALL_DIR:-/opt/9drive}"
SERVICE="${SERVICE:-9drive}"

for tool in curl sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required command: ${tool}" >&2; exit 1; }
done

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
ASSET="pandrive-linux-${ARCH}"

API="https://api.github.com/repos/${REPO}"
AUTH=()
[ -n "${GITHUB_TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
json_field() { sed -nE "s/.*\"$2\": *\"([^\"]+)\".*/\1/p" <<<"$1" | head -1; }

# Resolve the requested version: no arg = latest; arg accepts v1.0.0 or 1.0.0.
if [ $# -gt 0 ] && [ "$1" != "--system" ] && [ "$1" != "--no-system" ]; then
  TAG="$1"; case "$TAG" in v*) ;; *) TAG="v${TAG}";; esac
else
  PAYLOAD="$(curl -fsSL --retry 3 --retry-delay 2 "${AUTH[@]}" -H 'Accept: application/vnd.github+json' "${API}/releases/latest")" \
    || { echo "could not reach ${API}" >&2; exit 1; }
  TAG="$(json_field "$PAYLOAD" tag_name)"
  [ -n "$TAG" ] || { echo "could not resolve the latest release for ${REPO}" >&2; exit 1; }
fi
WANT_SYSTEM=auto
for arg in "$@"; do
  case "$arg" in
    --system) WANT_SYSTEM=yes ;;
    --no-system) WANT_SYSTEM=no ;;
  esac
done

BASE="https://github.com/${REPO}/releases/download/${TAG}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> Downloading ${TAG} (${ASSET})"
curl -fsSL --retry 3 --retry-delay 2 -o "${TMP}/${ASSET}" "${BASE}/${ASSET}" \
  || { echo "release ${TAG} has no ${ASSET} asset" >&2; exit 1; }
curl -fsSL --retry 3 --retry-delay 2 -o "${TMP}/SHA256SUMS" "${BASE}/SHA256SUMS" \
  || { echo "release ${TAG} does not publish SHA256SUMS — refusing to install unverified" >&2; exit 1; }
( cd "$TMP" && grep " ${ASSET}\$" SHA256SUMS | sha256sum -c - ) \
  || { echo "checksum verification failed — nothing changed" >&2; exit 1; }
echo "==> Checksum OK"

mkdir -p "$INSTALL_DIR"
# Keep the previous binary for a manual rollback; database & .env are untouched.
[ -f "${INSTALL_DIR}/${ASSET}" ] && cp -a "${INSTALL_DIR}/${ASSET}" "${INSTALL_DIR}/${ASSET}.prev"
install -m 755 "${TMP}/${ASSET}" "${INSTALL_DIR}/${ASSET}"
printf '%s\n' "$TAG" > "${INSTALL_DIR}/.installed-version"

# Seed .env secrets on a FRESH install only; never overwrite an existing one.
if [ ! -f "${INSTALL_DIR}/.env" ]; then
  JWT="$(head -c 32 /dev/urandom | sha256sum | cut -d' ' -f1)"
  KEY="$(head -c 32 /dev/urandom | sha256sum | cut -d' ' -f1)"
  cat > "${INSTALL_DIR}/.env" <<EOF
APP_PORT=4000
APP_BIND=127.0.0.1
DATABASE_URL=data/9drive.db
JWT_ACCESS_SECRET=${JWT}
TOKEN_ENCRYPTION_KEY=${KEY}
EOF
  chmod 600 "${INSTALL_DIR}/.env"
  echo "==> Wrote fresh ${INSTALL_DIR}/.env (random secrets)"
fi

# systemd service (default when running as root; --no-system to skip; --system to force)
if [ "$WANT_SYSTEM" != "no" ] && { [ "$WANT_SYSTEM" = "yes" ] || [ "$(id -u)" = "0" ]; }; then
  if command -v systemctl >/dev/null 2>&1; then
    cat > "/etc/systemd/system/${SERVICE}.service" <<EOF
[Unit]
Description=PanDrive gateway
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory=${INSTALL_DIR}
ExecStart=${INSTALL_DIR}/${ASSET}
Restart=always
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=${INSTALL_DIR}

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable --now "${SERVICE}" >/dev/null 2>&1 || systemctl restart "${SERVICE}"
    sleep 3
    if curl -fsS -m 5 http://127.0.0.1:4000/health 2>/dev/null | grep -q '"status":"ok"'; then
      echo "==> Service ${SERVICE} is running and healthy"
    else
      echo "==> WARNING: service started but /health did not answer yet (check: journalctl -u ${SERVICE})" >&2
    fi
  else
    echo "==> systemd not available — start manually: cd ${INSTALL_DIR} && ./${ASSET}"
  fi
fi

echo "==> PanDrive ${TAG} installed at ${INSTALL_DIR}/${ASSET}"
echo "    Update later with: sudo ${INSTALL_DIR}/$(basename "$0" 2>/dev/null || echo pandrive-update) 2>/dev/null || curl -fsSL https://raw.githubusercontent.com/${REPO}/master/deploy/vps-update.sh | bash -s --"
echo "    (or: sudo install -m 755 deploy/vps-update.sh /usr/local/bin/pandrive-update && pandrive-update)"
