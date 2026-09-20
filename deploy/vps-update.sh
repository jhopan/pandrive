#!/usr/bin/env bash
# Update PanDrive on a server straight from GitHub Releases — no scp, no local build.
#
#   vps-update.sh                 install the latest release
#   vps-update.sh --check         compare installed vs latest, change nothing
#   vps-update.sh --version v1.2.3
#   vps-update.sh --list          show the last few releases
#
# Safety: the download is verified against the release's SHA256SUMS, the running binary is kept as
# <binary>.prev, the swap is an atomic mv, and a failed health check restores the previous binary
# automatically.
set -euo pipefail

REPO="${GITHUB_REPO:-jhopan/pandrive}"
INSTALL_DIR="${INSTALL_DIR:-/opt/9drive}"
SERVICE="${SERVICE:-9drive}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:4000/health}"
KEEP_RELEASES="${KEEP_RELEASES:-3}"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
ASSET="pandrive-linux-${ARCH}"

API="https://api.github.com/repos/${REPO}"
AUTH=()
[ -n "${GITHUB_TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer ${GITHUB_TOKEN}")

curl_json() { curl -fsSL --retry 3 --retry-delay 2 "${AUTH[@]}" -H 'Accept: application/vnd.github+json' "$1"; }

# "-deploy" was used by the old scp flow; treat it as the same release.
installed_version() { "${INSTALL_DIR}/${ASSET}" --version 2>/dev/null | head -1 || echo unknown; }
installed_base() { installed_version | sed 's/-deploy$//'; }

latest_tag() {
  curl_json "${API}/releases/latest" | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/'
}

usage() { sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; }

MODE=install
TAG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --check) MODE=check ;;
    --list) MODE=list ;;
    --version) MODE=install; TAG="${2:?--version needs a tag}"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage; exit 1 ;;
  esac
  shift
done

if [ "$MODE" = list ]; then
  curl_json "${API}/releases?per_page=${KEEP_RELEASES}" | grep '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/  \1/'
  exit 0
fi

[ -n "$TAG" ] || TAG="$(latest_tag)"
[ -n "$TAG" ] || { echo "could not resolve the latest release tag for ${REPO}" >&2; exit 1; }

CURRENT="$(installed_base)"
if [ "$MODE" = check ]; then
  echo "installed: ${CURRENT}"
  echo "latest:    ${TAG}   (${REPO})"
  [ "$CURRENT" = "$TAG" ] && echo "up to date" || echo "update available: ${CURRENT} -> ${TAG}"
  exit 0
fi

if [ "$CURRENT" = "$TAG" ]; then
  echo "already on ${TAG}; nothing to do"
  exit 0
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
BASE="https://github.com/${REPO}/releases/download/${TAG}"

echo "installing ${TAG} (${ASSET}) from ${REPO}"
curl -fsSL --retry 3 --retry-delay 2 -o "${TMP}/${ASSET}" "${BASE}/${ASSET}"
curl -fsSL --retry 3 --retry-delay 2 -o "${TMP}/SHA256SUMS" "${BASE}/SHA256SUMS"

# Verify BEFORE touching the installed binary.
( cd "$TMP" && grep " ${ASSET}\$" SHA256SUMS | sha256sum -c - ) \
  || { echo "checksum verification failed — nothing changed" >&2; exit 1; }

chmod +x "${TMP}/${ASSET}"

# Sanity: the new binary must at least report its version.
if ! "${TMP}/${ASSET}" --version >/dev/null 2>&1; then
  echo "downloaded binary does not run — nothing changed" >&2
  exit 1
fi

mkdir -p "$INSTALL_DIR"
PREV="${INSTALL_DIR}/${ASSET}.prev"
[ -f "${INSTALL_DIR}/${ASSET}" ] && cp -a "${INSTALL_DIR}/${ASSET}" "$PREV"

mv "${TMP}/${ASSET}" "${INSTALL_DIR}/${ASSET}.new"
chmod +x "${INSTALL_DIR}/${ASSET}.new"
mv -f "${INSTALL_DIR}/${ASSET}.new" "${INSTALL_DIR}/${ASSET}"

systemctl restart "$SERVICE"

for _ in $(seq 1 20); do
  if curl -fsS -m 5 "$HEALTH_URL" 2>/dev/null | grep -q '"status":"ok"'; then
    echo "healthy: $(installed_version)"
    ls -1dt /opt/9drive/backup-* 2>/dev/null | tail -n +$((KEEP_RELEASES + 1)) | xargs -r rm -rf
    exit 0
  fi
  sleep 1
done

echo "health check failed — rolling back to the previous binary" >&2
if [ -f "$PREV" ]; then
  mv -f "$PREV" "${INSTALL_DIR}/${ASSET}"
  systemctl restart "$SERVICE"
  for _ in $(seq 1 20); do
    curl -fsS -m 5 "$HEALTH_URL" 2>/dev/null | grep -q '"status":"ok"' && { echo "rolled back to $(installed_version)" >&2; exit 1; }
    sleep 1
  done
fi
echo "rollback did not come up healthy — service needs attention" >&2
exit 1
