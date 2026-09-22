#!/usr/bin/env bash
# PanDrive multi-platform installer — installs the binary (self-contained: frontend embedded,
# SQLite pure-Go, NO Go runtime needed) from GitHub Releases, checksum-verified.
#
# One-liners:
#   curl -fsSL https://raw.githubusercontent.com/jhopan/pandrive/master/deploy/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/jhopan/pandrive/master/deploy/install.sh | bash -s -- v0.24.4
#
# Supported platforms (auto-detected):
#   Linux (Debian/Ubuntu/Armbian/…)  systemd service when root
#   macOS Intel & Apple Silicon      launchd agent
#   Termux (Android)                 binary + run instructions (services optional)
#   Windows (via Git-bash/MSYS)      binary + run instructions; native: install.ps1
#
# Optional extras:
#   --with-go            install the Go toolchain (ONLY for building from source; the app does not need it)
#   --with-caddy DOMAIN  install the Caddy web server and generate an HTTPS reverse-proxy config
#   --no-system          skip service creation   |  --system  force service creation
#
# Env overrides: GITHUB_REPO, INSTALL_DIR, SERVICE, GITHUB_TOKEN
set -euo pipefail

REPO="${GITHUB_REPO:-jhopan/pandrive}"
SERVICE="${SERVICE:-pandrive}"
GO_MIN="1.26"

WANT_GO=no; WANT_CADDY=no; CADDY_DOMAIN=""; WANT_SYSTEM=auto; VERSION_ARG=""
for arg in "$@"; do
  case "$arg" in
    --with-go) WANT_GO=yes ;;
    --with-caddy) WANT_CADDY=yes ;;
    --no-caddy) WANT_CADDY=no ;;
    --system) WANT_SYSTEM=yes ;;
    --no-system) WANT_SYSTEM=no ;;
    v*|0*|1*|2*|3*|4*|5*|6*|7*|8*|9*) VERSION_ARG="$arg" ;;
    *) VERSION_ARG="$arg" ;;
  esac
done
# A non-flag arg right after --with-caddy is the domain: install.sh --with-caddy drive.example.com
if [ "$WANT_CADDY" = "yes" ]; then
  for arg in "$@"; do
    case "$arg" in
      --*) ;;
      *.*) CADDY_DOMAIN="$arg" ;;
    esac
  done
fi

for tool in curl sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required command: ${tool}" >&2; exit 1; }
done

# ---------- platform detection ----------
OS="$(uname -s)"
ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l|armv8l) ARCH=arm64 ;; # closest release asset
  *) echo "unsupported architecture: ${ARCH_RAW}" >&2; exit 1 ;;
esac

IS_TERMUX=no; IS_WIN=no; GOOS=linux; EXE=""
if [ -n "${TERMUX_VERSION:-}" ] || { [ -n "${PREFIX:-}" ] && [[ "$PREFIX" == *com.termux* ]]; }; then
  IS_TERMUX=yes; GOOS=linux
elif [[ "$OS" == MINGW* || "$OS" == MSYS* || "$OS" == CYGWIN* ]]; then
  IS_WIN=yes; GOOS=windows; EXE=".exe"
elif [[ "$OS" == Darwin ]]; then
  GOOS=darwin
fi
ASSET="pandrive-${GOOS}-${ARCH}${EXE}"

case "$IS_TERMUX:$GOOS" in
  yes:*) INSTALL_DIR="${INSTALL_DIR:-${HOME}/.pandrive}" ;;
  no:windows) INSTALL_DIR="${INSTALL_DIR:-${HOME}/PanDrive}" ;;
  no:darwin) INSTALL_DIR="${INSTALL_DIR:-${HOME}/.pandrive}" ;;
  *) INSTALL_DIR="${INSTALL_DIR:-/opt/9drive}" ;;
esac

API="https://api.github.com/repos/${REPO}"
AUTH=()
[ -n "${GITHUB_TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
json_field() { sed -nE "s/.*\"$2\": *\"([^\"]+)\".*/\1/p" <<<"$1" | head -1; }
fetch() { # fetch <url> <out>: 3 attempts (CDN redirects sometimes reset mid-transfer)
  local i
  for i in 1 2 3; do
    if curl -fsSL --retry 2 --retry-delay 2 -o "$2" "$1"; then return 0; fi
    sleep 3
  done
  return 1
}

# ---------- version resolve ----------
TAG=""
if [ -n "$VERSION_ARG" ]; then
  TAG="$VERSION_ARG"; case "$TAG" in v*) ;; *) TAG="v${TAG}";; esac
else
  PAYLOAD="$(curl -fsSL --retry 3 --retry-delay 2 "${AUTH[@]}" -H 'Accept: application/vnd.github+json' "${API}/releases/latest")" \
    || { echo "could not reach ${API}" >&2; exit 1; }
  TAG="$(json_field "$PAYLOAD" tag_name)"
  [ -n "$TAG" ] || { echo "could not resolve the latest release for ${REPO}" >&2; exit 1; }
fi

# ---------- download + verify ----------
BASE="https://github.com/${REPO}/releases/download/${TAG}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> Downloading ${TAG} (${ASSET}) for ${GOOS}/${ARCH}"
fetch "${BASE}/${ASSET}" "${TMP}/${ASSET}" \
  || { echo "release ${TAG} has no ${ASSET} asset (or download failed)" >&2; exit 1; }
fetch "${BASE}/SHA256SUMS" "${TMP}/SHA256SUMS" \
  || { echo "release ${TAG} does not publish SHA256SUMS — refusing to install unverified" >&2; exit 1; }
( cd "$TMP" && grep " ${ASSET}\$" SHA256SUMS | sha256sum -c - ) \
  || { echo "checksum verification failed — nothing changed" >&2; exit 1; }
echo "==> Checksum OK"

# ---------- install ----------
mkdir -p "$INSTALL_DIR"
[ -f "${INSTALL_DIR}/${ASSET}" ] && cp -a "${INSTALL_DIR}/${ASSET}" "${INSTALL_DIR}/${ASSET}.prev"
install -m 755 "${TMP}/${ASSET}" "${INSTALL_DIR}/${ASSET}" 2>/dev/null || { cp "${TMP}/${ASSET}" "${INSTALL_DIR}/${ASSET}"; chmod 755 "${INSTALL_DIR}/${ASSET}"; }
printf '%s\n' "$TAG" > "${INSTALL_DIR}/.installed-version"

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
  chmod 600 "${INSTALL_DIR}/.env" 2>/dev/null || true
  echo "==> Wrote fresh ${INSTALL_DIR}/.env (random secrets)"
fi

# ---------- service ----------
start_systemd() {
  command -v systemctl >/dev/null 2>&1 || return 1
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
  curl -fsS -m 5 http://127.0.0.1:4000/health 2>/dev/null | grep -q '"status":"ok"' \
    && echo "==> Service ${SERVICE} is running and healthy" \
    || echo "==> WARNING: service may not be healthy yet (journalctl -u ${SERVICE})" >&2
}
start_launchd() {
  PLIST="$HOME/Library/LaunchAgents/app.pandrive.plist"
  mkdir -p "$HOME/Library/LaunchAgents"
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>app.pandrive</string>
  <key>ProgramArguments</key><array><string>${INSTALL_DIR}/${ASSET}</string></array>
  <key>WorkingDirectory</key><string>${INSTALL_DIR}</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
EOF
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load "$PLIST"
  echo "==> launchd agent installed (${PLIST}); app on http://127.0.0.1:4000"
}

if [ "$WANT_SYSTEM" != "no" ]; then
  if [ "$IS_TERMUX" = "yes" ]; then
    echo "==> Termux: start manually with:  cd ${INSTALL_DIR} && ./${ASSET}"
    command -v sv >/dev/null 2>&1 && echo "    (termux-services detected: mkdir -p \$PREFIX/var/service/pandrive && ln -s to a run script)"
  elif [ "$GOOS" = "windows" ]; then
    echo "==> Windows: run with:  ${INSTALL_DIR}\\${ASSET}   (then open http://127.0.0.1:4000)"
    echo "    For a native installer/service use deploy/install.ps1 (PowerShell)."
  elif [ "$GOOS" = "darwin" ]; then
    [ "$WANT_SYSTEM" = "yes" ] || [ -t 0 ] && start_launchd || start_launchd
  else
    if [ "$(id -u)" = "0" ]; then
      start_systemd
    else
      echo "==> Not root: start manually with:  cd ${INSTALL_DIR} && ./${ASSET}"
      echo "    (re-run with sudo for the systemd service)"
    fi
  fi
fi

# ---------- optional: Go toolchain (only for building from source) ----------
go_version_ok() {
  command -v go >/dev/null 2>&1 || return 1
  HAVE="$(go version 2>/dev/null | sed -E 's/.*go([0-9.]+).*/\1/')"
  [ -z "$HAVE" ] && return 1
  [ "$(printf '%s\n%s\n' "$HAVE" "$GO_MIN" | sort -V | head -1)" = "$GO_MIN" ]
}
if [ "$WANT_GO" = "yes" ]; then
  if go_version_ok; then
    echo "==> Go $(go version | sed -E 's/.*go([0-9.]+).*/\1/') already >= ${GO_MIN} — skip"
  elif [ "$IS_TERMUX" = "yes" ]; then
    pkg install -y golang && echo "==> Go installed (Termux)"
  elif [ "$GOOS" = "darwin" ]; then
    command -v brew >/dev/null 2>&1 && brew install go || { echo "install Homebrew or Go from go.dev/dl" >&2; exit 1; }
  elif [ "$GOOS" = "windows" ]; then
    echo "==> Run: winget install GoLang.Go   (then reopen the terminal)"
    command -v winget >/dev/null 2>&1 && winget install -e --id GoLang.Go || true
  else
    GO_VER="1.26.0"
    if [ "$(id -u)" = "0" ] && command -v apt-get >/dev/null 2>&1; then
      apt-get update -qq && apt-get install -y -qq golang-go >/dev/null 2>&1 || true
    fi
    if ! go_version_ok; then
      fetch "https://go.dev/dl/go${GO_VER}.linux-${ARCH}.tar.gz" "${TMP}/go.tgz" || { echo "go download failed" >&2; exit 1; }
      rm -rf /usr/local/go && tar -C /usr/local -xzf "${TMP}/go.tgz"
      ln -sf /usr/local/go/bin/go /usr/local/bin/go
    fi
    go_version_ok && echo "==> Go ${GO_VER} installed" || { echo "Go install failed" >&2; exit 1; }
  fi
  echo "    Go is ONLY needed to build from source — the PanDrive binary itself is self-contained."
fi

# ---------- optional: Caddy (HTTPS reverse proxy) ----------
if [ "$WANT_CADDY" = "yes" ]; then
  if ! command -v caddy >/dev/null 2>&1; then
    echo "==> Installing Caddy"
    CADDY_ARCH="$ARCH"; [ "$GOOS" = "windows" ] && CADDY_OS=windows || CADDY_OS="$GOOS"
    fetch "https://caddyserver.com/api/download?os=${CADDY_OS}&arch=${CADDY_ARCH}" "${INSTALL_DIR}/caddy${EXE}" \
      || { echo "caddy download failed — install from https://caddyserver.com" >&2; exit 1; }
    chmod +x "${INSTALL_DIR}/caddy${EXE}" 2>/dev/null || true
  else
    echo "==> Caddy already installed ($(caddy version 2>/dev/null | head -1))"
  fi
  DOMAIN="${CADDY_DOMAIN:-drive.example.com}"
  CF="${INSTALL_DIR}/Caddyfile"
  cat > "$CF" <<EOF
# PanDrive HTTPS reverse proxy (Caddy auto-obtains & renews the certificate)
${DOMAIN} {
    reverse_proxy 127.0.0.1:4000
}
EOF
  echo "==> Caddyfile written: ${CF} (domain: ${DOMAIN})"
  if [ "$(id -u)" = "0" ] && command -v systemctl >/dev/null 2>&1; then
    mkdir -p /etc/caddy && cp "$CF" /etc/caddy/Caddyfile
    systemctl enable --now caddy >/dev/null 2>&1 || systemctl restart caddy
    echo "==> Caddy service running — https://${DOMAIN} (once DNS points at this server; ports 80/443 open)"
  else
    echo "==> Run: ${INSTALL_DIR}/caddy start --config ${CF}   (needs ports 80/443 and DNS pointed here)"
  fi
fi

echo "==> PanDrive ${TAG} installed: ${INSTALL_DIR}/${ASSET}"
echo "    Open:   http://127.0.0.1:4000   (first login is printed once in the log)"
echo "    Update: re-run this one-liner (upgrade in place, database untouched)"
