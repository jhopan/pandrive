# PanDrive Lite

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
- `PUT /auth/me` (profile only)
- `POST /auth/change-password` (requires the current password; signs out other sessions)
- `GET/POST /system/google-config`
- `GET /connected-accounts/google/connect-url`
- `GET /connected-accounts/google/callback`
- `GET /connected-accounts`
- `GET /storage/summary`
- `POST /sync/quota`
- `POST /sync/files`
- `GET /files` (`?accountId=` `?folderId=` `?q=` `?status=deleted`)
- `POST /files/{id}/transfer` (server-side cross-account move/copy)
- `POST /files/{id}/restore`
- `POST /files/{id}/purge`
- `GET /folders`
- `POST /folders`
- `POST /connected-accounts/{id}/empty-trash`
- `GET /files/{id}/download`
- `GET /files/duplicates` (same name + size across accounts)
- `GET /storage/analyzer` (per-account, by-type, largest files)
- `GET /activity` (`?action=` `?accountId=` `?limit=` `?offset=`) — audit trail
- `GET /starred` / `POST /files/{id}/star` / `POST /folders/{id}/star`
- `GET /recent` (`?limit=` `?accountId=`) — newest-first, with 24h/7d counters
- `GET /search` (`?q=` `?kind=` `?accountId=` `?folderId=` `?minSize=` `?maxSize=` `?startDate=` `?endDate=` `?starred=` `?sort=` `?limit=` `?offset=`) — results, total bytes and facets
- `POST /files/{id}/public-link` (create public Drive permission + link)
- `GET /shares` / `DELETE /shares/{id}` (list / revoke public links)
- `GET /permissions?targetType=&targetId=` (live Drive access list for a file or folder)
- `POST /invites` / `GET /invites` / `DELETE /invites/{id}` (grant, list and revoke per-person access)
- `GET /uploads/queue` (`?status=`) / `POST /uploads/queue/{id}/cancel` / `DELETE /uploads/queue/{id}`
- `GET /system/health` (runtime, database, backup, tunnel, accounts, OAuth quota)
- `GET /system/rate-limits` (live request rate, history, per-config window state)
- `GET /system/version` (update checker)
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
go build -o ../bin/pandrive .
```

## Run local Go backend

```powershell
cd backend-go
$env:APP_PORT="4000"
$env:FRONTEND_URL="http://localhost:5173"
$env:JWT_ACCESS_SECRET="replace-with-strong-random-secret"
$env:TOKEN_ENCRYPTION_KEY="replace-with-another-strong-random-secret"
$env:DATABASE_URL="file:data/pandrive.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
go run .
```

## Google OAuth setup

1. Create Google OAuth Web Application credential.
2. Enable Google Drive API.
3. Set authorized redirect URI:

```text
http://localhost:4000/connected-accounts/google/callback
```

4. Register/login to PanDrive.
5. Save Client ID and Client Secret through Settings UI.

## Menus

| Menu | What it does |
|------|--------------|
| **All Files** | Every synced file across all connected accounts; upload, rename, move, ZIP batch download, per-account filter |
| **Starred** | Files and folders pinned via right-click → Add to Starred; unstar or open from the page |
| **Recent** | Files by last change, grouped by day, with 24h/7d counters, account filter and inline star/download |
| **Search** | Name, type, account, folder, size range, date range and starred filters with sort, pagination, facets and saved searches |
| **Account Files** | Same view scoped to one account (click an account in the sidebar) |
| **Trash** | Locally deleted files (restorable) plus permanent delete and **Empty Drive trash** per account to actually free quota |
| **Duplicates** | Finds same-name + same-size files across every account, one-click select of the extra copies, reclaimable bytes total |
| **Storage** | Analyzer: bytes per account, breakdown by file type, and the 25 largest indexed files |
| **Activity** | Audit trail: sign-ins (including failed attempts), uploads, downloads, transfers, deletes/restores/purges, syncs and OAuth config changes |
| **Shared** | **People with access** (per-person Drive permissions with revoke) and **Public links** (anyone-with-the-link, copy/open/revoke) |
| **Uploads** | Resumable upload queue: what is running, what finished, what failed, plus cancel and clear |
| **Health** | Uptime, database size and writability, backup freshness, tunnel mode, per-account token/sync state, OAuth rotation quota |
| **Rate Limits** | Live Google API request rate in the 100s window, peak, per-second sparkline, per-config window countdown and the switch threshold |
| **Transfers** | Move or copy files between two connected accounts **server-side** — Google does the copying, so no bytes pass through this server or your bandwidth |
| **Quota Tracker** | Storage per account, upload routing mode, OAuth config rotation status |
| **Settings** | Connect Drive, OAuth config manager, Updates (auto update check), backup/restore, security |

### Transfers (server-side move)

Select files in **All Files** -> **Transfer** -> pick the destination account (and optionally "move", which deletes the source copy).

```text
PanDrive -> Drive API: share source file with the destination account (1 request)
         -> Drive API: copy using the destination account token (server-side in Google)
         -> revoke the temporary share
         -> optionally trash the source file
```

Consequences worth knowing:

- Your internet bandwidth is untouched; the file never downloads or uploads through PanDrive.
- The file needs **free space in the destination account** during the copy (it exists twice briefly).
- A "move" leaves the source copy in that account's **Drive trash**, which still counts toward quota. Use **Trash -> Empty Drive trash** to release it.
- Storage is rebalanced between accounts, not increased.

## Public access (Cloudflare Tunnel)

PanDrive can serve itself publicly without opening any port. Two modes, picked automatically by which env var is set:

| Mode | Env | Where ingress lives |
|------|-----|---------------------|
| Managed | `TUNNEL_TOKEN` | Cloudflare dashboard (Zero Trust -> Tunnels -> Public hostnames) |
| Locally managed | `TUNNEL_ID` | `tunnel.yml` next to the binary |

Working example (verified end to end):

```bash
# once, on a machine logged in to Cloudflare
cloudflared tunnel login
cloudflared tunnel create pandrive
cloudflared tunnel route dns pandrive drive.renunganbot.qzz.io
```

Copy the tunnel credentials JSON next to the binary, write `tunnel.yml`, then set `TUNNEL_ID` in `.env`:

```yaml
tunnel: <tunnel-uuid>
credentials-file: /opt/9drive/<tunnel-uuid>.json
ingress:
  - hostname: drive.renunganbot.qzz.io
    service: http://127.0.0.1:4000
  - service: http_status:404
```

The binary spawns `cloudflared` itself on startup (it looks next to itself, then in `PATH`). The OAuth redirect URL follows the incoming `X-Forwarded-Host`, so no `.env` edit is needed per domain — but the callback must be registered in the Google Cloud console:

```text
https://drive.renunganbot.qzz.io/connected-accounts/google/callback
```

## Update checker

`GET /system/version` compares the running build against the latest GitHub release.

- Checked automatically at startup, every 12 hours, and whenever Settings is opened.
- Cached for 6 hours; `?refresh=1` forces a re-check.
- Picks the correct release asset for the host OS/arch.
- Startup log lines: `up to date (v0.6.0)` or `UPDATE AVAILABLE: v0.5.0 -> v0.6.0 (<url>)`.
- Override the repo with `UPDATE_REPO=owner/name`; disable with `UPDATE_CHECK=off`.

## Updating a server from GitHub Releases

No scp, no building on your laptop — the server pulls the release itself:

```bash
# install once
sudo install -m 755 deploy/vps-update.sh /usr/local/bin/pandrive-update

pandrive-update --check        # installed vs latest, changes nothing
pandrive-update                # install the latest release
pandrive-update --version v0.19.0
pandrive-update --list         # recent releases
```

The script downloads the `pandrive-linux-<arch>` asset plus the release's `SHA256SUMS`, verifies the
checksum **before** touching the installed binary, keeps the previous binary as
`pandrive-linux-amd64.prev`, swaps atomically, restarts the systemd unit, then health-checks
`/health` for up to 20 s and rolls back automatically if the new build does not come up.

Environment overrides: `GITHUB_REPO`, `INSTALL_DIR` (default `/opt/9drive`), `SERVICE` (default
`9drive`), `HEALTH_URL`, `GITHUB_TOKEN` (private repos / API rate limits).

The binary embeds the frontend, so one release updates UI and API together. This replaced the old
manual `scp` + restart flow.

Releases cut before checksums existed have no `SHA256SUMS`; the updater refuses those rather than
installing an unverified binary (`nothing changed`).

Prefer zero-touch updates? The repo ships an optional timer:

```bash
sudo cp deploy/pandrive-update.service deploy/pandrive-update.timer /etc/systemd/system/
sudo systemctl enable --now pandrive-update.timer   # runs daily, updates only when a new tag exists
```

Leave it disabled if you want to choose exactly when the service restarts.

## Gallery, folder sizes, expiring links, notifications

- **Gallery** (`/gallery`): image/video grid. Thumbnails are Drive `thumbnailLink` URLs loaded by the
  browser straight from Google's CDN — PanDrive's bandwidth stays near zero and no Drive API quota is
  used for thumbnail views. Broken thumbnails fall back to icon tiles.
- **Folder sizes**: `/folders` returns `sizeBytes` per folder, a recursive aggregate (folder + all
  descendants), shown on folder cards.
- **Expiring public links**: create with `?expiresAt=<RFC3339>` (presets in the UI: 1h/1d/1w/30d/never).
  A background sweep revokes expired links in Drive and locally; `/shares` reports `expiresAt`/`expired`.
- **Notifications (ntfy)**: Settings → Notifications (ntfy) → server + topic + Send test. Events:
  upload finished, transfer done, public link created, login failed, link expired. Default server
  `https://ntfy.sh`; point it at a self-hosted instance any time.

## ID/EN language switch

The UI ships bilingual (English default, Indonesian included):

- Toggle: the **ID/EN pill** in the header (next to the theme button), the **Language** item in the
  profile menu, and a pill on the login card. The choice persists in `localStorage` (`pandrive.lang`).
- Implementation: a tiny context + dictionary (`src/lib/i18n.tsx`). Keys are the English source strings;
  a missing translation falls back to the key itself, so adding a page costs nothing in EN mode.
- `PageHeader` translates titles/descriptions centrally, so all pages switch at once.

## Gallery size badges

Every gallery card carries a coloured size badge so you can gauge mobile-data cost before opening:

- green `< 5 MB` — safe to open anywhere
- amber `5 MB – 200 MB` — watch your quota
- red `>= 200 MB` — open over Wi-Fi

Thumbnails themselves stay tiny (~10 KB each, lazy-loaded, straight from Google's CDN).

## In-app preview & branded share pages

- **Preview**: `GET /api/files/{id}/view-url` returns Drive's `webViewLink`; the UI opens Google's viewer
  in a new tab (gallery cards carry a small open-in-viewer button). Streaming/download flows browser ->
  Google — the VPS never proxies media.
- **Share pages**: public links are now `https://<host>/s/<shareId>` — a ~1.4 KB branded landing page
  (logo, file name, size, PanDrive credit) with a download button that goes straight to Google. The page
  shows a Wi-Fi warning for files >= 200 MB, is `noindex`, returns 404 for revoked links and 410 Gone for
  expired ones.

## Multi-user & trash auto-purge

- **Multi-user**: registration can be opened by an admin (settings switch `open_registration`); every
  user's files, accounts, shares and activity are isolated by `user_id`. Admins get a Users card in
  Settings (list accounts, disable/enable — instant sign-out — and promote/demote). The bootstrap admin
  account is promoted with `UPDATE users SET role='admin' WHERE email='...'` once.
- **Trash auto-purge (opt-in)**: Settings → Trash auto-purge → days (empty = off, 1–365). A background
  sweep (same 5-minute loop as expired shares) permanently deletes Drive trash older than the window via
  the Drive API (real deletion, quota freed) and notifies the owner. Every action lands in the activity
  log.

## Stability

v0.24.1 is a tested stability release: 52 backend tests pass, all API endpoints verified live (local +
production over the tunnel), every SPA page (incl. hard refresh deep-links), i18n switch, share pages,
multi-user gates and trash auto-purge were exercised end to end.

## One-line install (Linux / macOS / Termux)

Straight from GitHub Releases — checksum-verified, auto-detects OS and CPU (amd64 / arm64):

| Platform | Command |
|---|---|
| Linux (Debian/Ubuntu/Armbian, root) | `curl -fsSL https://raw.githubusercontent.com/jhopan/pandrive/master/deploy/install.sh \| bash` |
| macOS (Intel & Apple Silicon) | `curl -fsSL https://raw.githubusercontent.com/jhopan/pandrive/master/deploy/install.sh \| bash` (launchd agent) |
| Termux (Android) | same one-liner in Termux (binary only, run manually) |
| Windows | `powershell -ExecutionPolicy Bypass -File install.ps1` (download `deploy/install.ps1`) |
| Docker | `docker run -v pandrive-data:/data -p 4000:4000 ghcr.io/jhopan/pandrive` |

Pinned version: `... \| bash -s -- v0.24.4`. Optional extras: `--with-go` (Go toolchain, only needed to
build from source — the app itself is self-contained), `--with-caddy drive.domain.com` (HTTPS reverse proxy).
Re-running the same command upgrades in place (database untouched).

```bash
curl -fsSL https://raw.githubusercontent.com/jhopan/pandrive/master/deploy/install.sh | bash
```

Install a specific version instead of the latest:

```bash
curl -fsSL https://raw.githubusercontent.com/jhopan/pandrive/master/deploy/install.sh | bash -s -- v0.24.1
```

The script downloads `pandrive-linux-<arch>`, verifies it against the release's `SHA256SUMS`, installs to
`/opt/9drive`, seeds a fresh `.env` with random secrets (never overwrites one), and — when run as root —
creates and starts the `9drive` systemd service with automatic rollback kept as `.prev`. Re-running it
upgrades in place without touching the database.

## HTTPS with Caddy (or Cloudflare Tunnel)

In-app manager: **Reverse Proxy** menu (System group) — pick none / caddy / cloudflare, set the domain,
and generate the Caddyfile with one click (root+systemd: written to /etc/caddy and caddy restarted
automatically).

## HTTPS with Caddy (or Cloudflare Tunnel)

PanDrive is HTTP on `127.0.0.1:4000`. Two supported ways to get public HTTPS:

**Option A — Cloudflare Tunnel (no open ports, no TLS on the box):** see "Public access (Cloudflare Tunnel)"
above. `TUNNEL_ID` mode runs the tunnel inside the app.

**Option B — Caddy (direct, auto-HTTPS):** Caddy obtains and renews the certificate automatically.

Install together with the app:

```bash
curl -fsSL https://raw.githubusercontent.com/jhopan/pandrive/master/deploy/install.sh | bash -s -- --with-caddy drive.domainmu.com
```

Or manually:

1. Install Caddy (script above does it, or `apt install caddy` / `brew install caddy` / `winget install CaddyServer.Caddy`).
2. Point the domain's DNS A/AAAA record at the server; open ports 80 + 443.
3. `Caddyfile` (installer writes it, or write by hand):

```
drive.domainmu.com {
    reverse_proxy 127.0.0.1:4000
}
```

4. Run: `caddy start --config /opt/9drive/Caddyfile` (or systemd: `systemctl enable --now caddy`).

That is the entire configuration — HTTPS, certificate renewal and HTTP→HTTPS redirect are automatic.
Nginx works equally well but needs certbot + manual TLS config; Caddy is recommended for simplicity.

## Security

- Every response carries `Content-Security-Policy` (`default-src 'self'`, hashed inline bootstrap script, `object-src 'none'`, `frame-ancestors 'none'`), computed from the embedded SPA shell so a rebuilt frontend cannot silently break it.
- `Strict-Transport-Security` is sent only when the request arrived over TLS (directly or via the tunnel).
- The policy lists every third-party origin the UI actually loads (iconify, avatar CDNs, cdnjs for the video player, Office/Drive preview frames). Built-in folder icons render from bundled SVG, so no remote icon CDN is required.
- The service worker deliberately does **not** precache `index.html`: a cached shell would keep enforcing an old CSP/HSTS header long after the server changed it (that is exactly how folder icons stayed blocked after a CSP fix).
- Changing a password requires the current one (`POST /auth/change-password`); `/auth/me` refuses password changes so a stolen access token cannot lock the owner out. A successful change revokes every other session.
- The app mark is `frontend/public/logo.png` (512×512), with `logo-192.png`, `apple-touch-icon.png`, `favicon.png` and `maskable-icon.png` derived from it; `BrandLogo`, the login page, `index.html` and the PWA manifest all point at those files.
- Avatars are generated locally (`src/lib/avatar.ts`, deterministic initial + colour) and folder icons render from bundled SVGs, so the UI needs no third-party image CDN.
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
./pandrive-linux-amd64             # serves API + UI on :4000 (or APP_PORT from .env)
```

### Auto-start service

**Linux (systemd)** — `/etc/systemd/system/pandrive.service`:
```ini
[Unit]
Description=PanDrive
After=network-online.target

[Service]
ExecStart=/opt/pandrive/pandrive-linux-amd64
WorkingDirectory=/opt/pandrive
Restart=always
User=www-data

[Install]
WantedBy=multi-user.target
```
`sudo systemctl enable --now pandrive`

**Windows (NSSM)**:
```
nssm install pandrive C:\path\to\pandrive-windows-amd64.exe
nssm set pandrive AppDirectory C:\path\to
nssm start pandrive
```

**macOS (launchd)** — `~/Library/Launchers/com.jhopan.pandrive.plist`:
```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.jhopan.pandrive</string>
  <key>ProgramArguments</key><array><string>/opt/pandrive/pandrive-darwin-arm64</string></array>
  <key>WorkingDirectory</key><string>/opt/pandrive</string>
  <key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
</dict></plist>
```
`launchctl load ~/Library/Launchers/com.jhopan.pandrive.plist`

**Database:** `data/pandrive.db` next to the binary (WorkingDirectory). Back up this file; use Settings > Backup for download.


### Cloudflare Tunnel (optional, HTTPS tanpa reverse proxy)

Set `TUNNEL_TOKEN` di .env (Cloudflare Zero Trust > Networks > Tunnels > Create > copy token). Letakkan binary `cloudflared` di samping binary pandrive — backend otomatis menjalankannya saat startup. Aplikasi langsung reachable via HTTPS domain tunnel, tanpa nginx/Caddy.


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
  ghcr.io/jhopan/pandrive:latest
```

Data lives in `./data/pandrive.db` (bind mount). Backup = copy file.


## Credits

**PanDrive** — built and maintained by **[JhopanStore](https://github.com/jhopan)**.

## References & Acknowledgments

- [OmniCloud](https://github.com/dimartarmizi/OmniCloud) — original reference project that inspired the frontend architecture and Google Drive integration patterns
- [Google Drive API v3](https://developers.google.com/drive) — storage backend
- [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) — pure-Go SQLite driver (no CGO)
- [cloudflared](https://github.com/cloudflare/cloudflared) — Cloudflare Tunnel support
- [golang-jwt](https://github.com/golang-jwt/jwt) — JWT sessions
- [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto) — bcrypt password hashing


### Updates

PanDrive checks GitHub releases on startup and every 12h; the result shows in **Settings > Updates** (current vs latest, download button matching your OS/arch). Release binaries are updated by replacing the binary and restarting; git checkouts can use Settings > System Update.
