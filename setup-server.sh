#!/bin/bash
# PanDrive production setup helper for Linux servers (Debian/Ubuntu).
# Generates: .env template, systemd service, optional cloudflared service.
# Usage: sudo ./setup-server.sh /opt/pandrive
set -euo pipefail

TARGET_DIR="${1:-/opt/pandrive}"
BIN_NAME="$(ls "$TARGET_DIR"/pandrive-linux-* 2>/dev/null | head -1 || true)"
if [ -z "$BIN_NAME" ]; then
  echo "ERROR: no pandrive-linux-* binary found in $TARGET_DIR"
  exit 1
fi
echo "Using binary: $BIN_NAME"

# ---- .env ----
if [ ! -f "$TARGET_DIR/.env" ]; then
  cat > "$TARGET_DIR/.env" <<EOF
APP_PORT=4000
FRONTEND_URL=https://your-domain.example
DATABASE_URL=file:data/pandrive.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)
JWT_ACCESS_SECRET=$(head -c 32 /dev/urandom | base64 | tr -d '=+/\n' | head -c 48)
TOKEN_ENCRYPTION_KEY=$(head -c 32 /dev/urandom | base64 | tr -d '=+/' | head -c 32)
# GOOGLE_CLIENT_ID=xxx.apps.googleusercontent.com
# GOOGLE_CLIENT_SECRET=xxx
# GOOGLE_REDIRECT_URI=https://your-domain.example/connected-accounts/google/callback
# TUNNEL_TOKEN=  (Cloudflare Zero Trust tunnel token, optional)
EOF
  chmod 600 "$TARGET_DIR/.env"
  echo "Created $TARGET_DIR/.env (600) - EDIT IT: set FRONTEND_URL + Google OAuth vars"
else
  echo ".env exists, keeping it"
fi

# ---- systemd ----
cat > /etc/systemd/system/pandrive.service <<EOF
[Unit]
Description=PanDrive gateway
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$BIN_NAME
WorkingDirectory=$TARGET_DIR
Restart=always
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=$TARGET_DIR
EnvironmentFile=$TARGET_DIR/.env

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now pandrive
echo "service pandrive enabled. Check: systemctl status pandrive"

# ---- optional tunnel hint ----
if grep -q '^TUNNEL_TOKEN=..*' "$TARGET_DIR/.env" 2>/dev/null; then
  echo "TUNNEL_TOKEN set -> put cloudflared binary next to pandrive binary; it will auto-run."
fi
echo "Done."
