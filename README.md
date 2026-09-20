# 9Drive Lite

Google Drive multi-account gateway. Native Go backend, SQLite database, React frontend.

## Status

Migration from TypeScript/Prisma to Go is complete. All API routes, OAuth flow, multiple account persistence, file metadata sync, resumable upload routing, and range download streams have been implemented in Go. The legacy TypeScript backend has been removed.

## Runtime target

```text
VPS: 1 vCPU / 1 GB RAM
Database: SQLite WAL
Backend: one Go binary
Frontend: static Vite build
No Docker
No MySQL
No S3
```

## Go backend features

Implemented and tested:

- `GET /health`
- `POST /auth/register` (Bootstrap only, disabled afterwards)
- `POST /auth/login`
- `POST /auth/refresh`
- `POST /auth/logout`
- `GET /auth/me`
- `PUT /auth/me`
- `GET/POST /system/google-config`
- `GET /connected-accounts/google/connect-url`
- `GET /connected-accounts/google/callback`
- `GET /connected-accounts`
- `GET /storage/summary`
- `POST /sync/quota`
- `POST /sync/files`
- `GET /files`
- `GET /folders`
- `POST /folders`
- `GET /files/{id}/download`
- `POST /upload/resumable`
- `PUT /upload/resumable/{id}`
- `GET /upload/resumable/{id}`
- JWT access tokens, hashed refresh tokens, SQLite sessions
- AES-GCM encrypted Google OAuth credentials
- OAuth state hashing and 10-minute expiry
- Resumable uploads directly to Google Drive chunks
- Range-based byte stream downloads

Build:

```bash
cd backend-go
go test ./...
go build -o ../bin/9drive .
```

## Run local Go backend

```powershell
cd backend-go
$env:APP_PORT="4000"
$env:FRONTEND_URL="http://localhost:5173"
$env:JWT_ACCESS_SECRET="replace-with-strong-random-secret"
$env:TOKEN_ENCRYPTION_KEY="replace-with-another-strong-random-secret"
$env:DATABASE_URL="file:data/9drive.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
go run .
```

## Google OAuth setup

1. Create Google OAuth Web Application credential.
2. Enable Google Drive API.
3. Set authorized redirect URI:

```text
http://localhost:4000/connected-accounts/google/callback
```

4. Register/login to 9Drive.
5. Save Client ID and Client Secret through Settings UI.

## Security

- Do not commit `.env` or SQLite DB files.
- Keep backend bound to `127.0.0.1` behind Nginx on VPS.
- Use HTTPS before exposing outside localhost.
- Google Client Secret, OAuth tokens, and token encryption key are secrets.

## License

MIT


## Deployment (universal)

Single binary contains both API and frontend (embedded). Ports/paths via env (see `backend-go/.env.example`).

### Build

```bash
./build-release.sh v1.0.0        # local: builds all 6 targets into backend-go/release/
# or push a tag: git tag v1.0.0 && git push origin v1.0.0  -> GitHub Actions builds + releases
```

Targets: windows/amd64, windows/arm64, linux/amd64, linux/arm64, darwin/amd64, darwin/arm64.

### Run

```bash
./9drive-linux-amd64             # serves API + UI on :4000 (or APP_PORT from .env)
```

### Auto-start service

**Linux (systemd)** — `/etc/systemd/system/9drive.service`:
```ini
[Unit]
Description=9Drive
After=network-online.target

[Service]
ExecStart=/opt/9drive/9drive-linux-amd64
WorkingDirectory=/opt/9drive
Restart=always
User=www-data

[Install]
WantedBy=multi-user.target
```
`sudo systemctl enable --now 9drive`

**Windows (NSSM)**:
```
nssm install 9drive C:\path\to\9drive-windows-amd64.exe
nssm set 9drive AppDirectory C:\path\to
nssm start 9drive
```

**macOS (launchd)** — `~/Library/Launchers/com.jhopan.9drive.plist`:
```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.jhopan.9drive</string>
  <key>ProgramArguments</key><array><string>/opt/9drive/9drive-darwin-arm64</string></array>
  <key>WorkingDirectory</key><string>/opt/9drive</string>
  <key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
</dict></plist>
```
`launchctl load ~/Library/Launchers/com.jhopan.9drive.plist`

**Database:** `data/9drive.db` next to the binary (WorkingDirectory). Back up this file; use Settings > Backup for download.


### Cloudflare Tunnel (optional, HTTPS tanpa reverse proxy)

Set `TUNNEL_TOKEN` di .env (Cloudflare Zero Trust > Networks > Tunnels > Create > copy token). Letakkan binary `cloudflared` di samping binary 9drive — backend otomatis menjalankannya saat startup. Aplikasi langsung reachable via HTTPS domain tunnel, tanpa nginx/Caddy.


### Tunnel dua mode

**Managed (dashboard):** `.env` -> `TUNNEL_TOKEN=...`. Mapping hostname->port diatur di dashboard Cloudflare (Public Hostname -> service `http://localhost:4000`).

**Locally-managed (custom penuh):** `.env` -> `TUNNEL_ID=<uuid>` + file `tunnel.yml` di samping binary (lihat `tunnel.yml.example`). Semua mapping hostname/port/path ada di file — bisa banyak domain, beda port, bahkan path routing.

**URL menyesuaikan otomatis:** frontend disajikan dari binary yang sama (satu origin), fetch API pakai `/api/*` relative. OAuth redirect URI di-rebuild dari `X-Forwarded-Host`/`Host` request — jadi akses dari domain mana pun (`drive.jhopan.my.id`, IP, dst), redirect URI ikut domain itu tanpa ganti .env. Catatan: `X-Forwarded-*` dipercaya hanya untuk rebuild localhost redirect; pastikan hanya proxy/tunnel kamu yang bisa mengirim header itu (default tunnel/CF memang begitu).


### Docker

```bash
# Build & run (frontend + backend built inside Docker, multi-arch)
docker compose up -d

# With Cloudflare Tunnel sidecar:
echo "TUNNEL_TOKEN=..." >> .env
docker compose --profile tunnel up -d

# From GHCR (built by CI on tag):
docker run -d -p 4000:4000 -v ./data:/data \
  -e JWT_ACCESS_SECRET=... -e TOKEN_ENCRYPTION_KEY=<32-char> \
  ghcr.io/jhopan/9drive:latest
```

Data lives in `./data/9drive.db` (bind mount). Backup = copy file.
