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
SERVICE="${SERVICE:-pandrive}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:4000/health}"
LIST_COUNT="${LIST_COUNT:-3}"

# Fail fast before touching anything if the tools we depend on are missing.
for tool in curl sha256sum systemctl; do
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

curl_json() { curl -fsSL --retry 3 --retry-delay 2 "${AUTH[@]}" -H 'Accept: application/vnd.github+json' "$1"; }
# Extract from an in-memory copy so curl is never upstream of an early-exiting grep: with `set -o pipefail`
# a SIGPIPE'd curl fails the whole pipeline (exit 23).
json_field() { sed -nE "s/.*\"$2\": *\"([^\"]+)\".*/\1/p" <<<"$1" | head -1; }

# Never execute the installed binary to read its version: an old build that predates --version would
# boot, run migrations and (with a different CWD) create a second database. Read the marker the updater
# writes, then fall back to what the service logged at boot.
VERSION_FILE="${INSTALL_DIR}/.installed-version"
installed_version() {
  if [ -f "$VERSION_FILE" ]; then cat "$VERSION_FILE"; return; fi
  local logged
  logged="$(journalctl -u "$SERVICE" --no-pager -n 500 2>/dev/null | grep -oE 'PanDrive v[^ ]+' | tail -1 | sed 's/^PanDrive //' || true)"
  if [ -n "$logged" ]; then printf '%s\n' "$logged"; else echo unknown; fi
}
# "-deploy" was used by the old scp flow; treat it as the same release.
installed_base() { installed_version | sed 's/-deploy$//'; }

latest_tag() {
  local payload
  payload="$(curl_json "${API}/releases/latest")" || return 1
  json_field "$payload" 'tag_name'
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
  local payload
  payload="$(curl_json "${API}/releases?per_page=${LIST_COUNT}")" || exit 1
  grep -oE '"tag_name": *"[^"]+"' <<<"$payload" | sed -E 's/.*"([^"]+)"$/  \1/'
  exit 0
fi

[ -n "$TAG" ] || TAG="$(latest_tag || true)"
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
if ! curl -fsSL --retry 3 --retry-delay 2 -o "${TMP}/${ASSET}" "${BASE}/${ASSET}"; then
  echo "release ${TAG} has no ${ASSET} asset — nothing changed" >&2
  exit 1
fi
# Releases cut before the checksum step was added have no SHA256SUMS: refuse rather than install blind.
if ! curl -fsSL --retry 3 --retry-delay 2 -o "${TMP}/SHA256SUMS" "${BASE}/SHA256SUMS"; then
  echo "release ${TAG} does not publish SHA256SUMS — refusing to install an unverified binary" >&2
  echo "nothing changed (still on ${CURRENT})" >&2
  exit 1
fi

# Verify BEFORE touching the installed binary.
( cd "$TMP" && grep " ${ASSET}\$" SHA256SUMS | sha256sum -c - ) \
  || { echo "checksum verification failed — nothing changed" >&2; exit 1; }

chmod +x "${TMP}/${ASSET}"

# Sanity: an ELF for linux (0x7f ELF). Running it would start the server, so check the magic bytes.
magic="$(head -c 4 "${TMP}/${ASSET}" | od -An -tx1 | tr -d ' \n')"
if [ "$magic" != "7f454c46" ]; then
  echo "downloaded file is not an ELF binary — nothing changed" >&2
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
    printf '%s\n' "$TAG" > "$VERSION_FILE"
    echo "healthy: $(installed_version)"
    echo "previous binary kept at ${ASSET}.prev"
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
