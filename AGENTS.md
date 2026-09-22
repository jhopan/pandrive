# Agent Instructions for PanDrive Development

## Critical Rules

### 🔴 Database Safety (MANDATORY)

**NEVER delete production database for testing!**

```bash
# ❌ ABSOLUTELY FORBIDDEN - destroys all user data
rm -f data/pandrive.db*
rm -rf backend-go/data/

# ✅ CORRECT - run unit tests (use :memory: DB)
cd backend-go && go test ./...

# ✅ CORRECT - backup before risky operations
cd backend-go && ./backup.sh
```

**Why this matters:**
- `data/pandrive.db` contains ALL user data
- User login credentials (bcrypt hashed)
- Connected Google Drive accounts
- OAuth tokens (AES-GCM encrypted)
- File metadata and folder structure
- Upload sessions

**Losing this file = user loses everything permanently!**

### Testing Backend Changes

**Safe workflow:**
1. Read existing code first
2. Run unit tests: `cd backend-go && go test ./...`
3. Start backend: `go run .` (uses production DB safely)
4. Test with API calls or browser
5. NEVER drop/recreate production DB

**Unit tests use in-memory SQLite (`:memory:`)** — safe to run anytime.

### 📤 Git Push Policy (MANDATORY)

**ALWAYS push changes to GitHub after committing!**

Every commit MUST be followed by push to origin/main.

```bash
# ✅ CORRECT workflow
git add -A
git commit -m "feat: add feature X"
git push origin main              # REQUIRED - never skip

# ❌ WRONG - commit without push
git commit -m "fix: bug Y"
# ... then forget to push = changes not backed up
```

**Why this matters:**
- User expects changes in GitHub repository
- Local commits without push = work not backed up
- Other developers/devices cannot see changes
- CI/CD pipelines will not trigger

**Rule:** Every `git commit` MUST be immediately followed by `git push origin main`.

### 🏷️ Release & Tag Policy (USER APPROVAL REQUIRED)

Pushing `main` alone is ALWAYS allowed and expected (backup policy above). But **releases are gated**:

**A release (git tag `v*`) is created ONLY when the user explicitly asks for it.**

- ✅ ALLOWED without asking: `git add`, `git commit`, `git push origin main`.
- ❌ NEVER do without an explicit user instruction in the current conversation:
  - `git tag vX.Y.Z` / `git push origin vX.Y.Z` (triggers the Release + Docker workflows)
  - building release binaries for deploy, scp/installing to the VPS
  - running `pandrive-update` on the VPS
- If the work is finished but no release was requested: stop after pushing `main` and tell the user
  "siap di-release — minta tag kalau mau" instead of tagging.
- Versioning when the user DOES ask: minor bump for a feature, patch for fixes/docs (previous tag + 1).

**Why:** every tag triggers GitHub Actions builds + a public release; unprompted releases create noise
and publish unfinished work. The user decides when a version ships.

## Project Overview

**PanDrive** — Multi-account cloud drive gateway with Google Drive integration.

**Tech stack:**
- Backend: Go 1.23+ (stdlib + modernc.org/sqlite)
- Frontend: React + Vite + TypeScript
- Database: SQLite with WAL mode
- Auth: JWT sessions + bcrypt passwords
- OAuth: Google Drive API with multiple project support

**Key features:**
- Multiple Google Drive accounts per user
- Multiple OAuth configs (rate limit avoidance)
- Smart quota tracking (8k/100s auto-switch)
- Resumable uploads (Google Drive API)
- File metadata sync
- Download routing

## Project Structure

```
pandrive/
├── backend-go/              # Go backend (port 4000)
│   ├── main.go             # Core server + all routes + handlers
│   ├── *_test.go           # Unit tests (use :memory: DB)
│   ├── data/               # SQLite database directory
│   │   └── pandrive.db       # 🔴 PRODUCTION DATABASE - NEVER DELETE
│   ├── backups/            # Auto backups (gitignored)
│   ├── backup.sh           # Backup script (keeps last 7)
│   ├── DEVELOPMENT.md      # Backend dev guidelines
│   └── .env.example        # OAuth config template
│
├── frontend/               # React frontend (port 5173)
│   ├── src/
│   │   ├── pages/         # Route pages
│   │   ├── components/    # UI components
│   │   │   └── drive/
│   │   │       └── OAuthConfigManager.tsx  # OAuth config UI
│   │   ├── context/       # React context
│   │   └── lib/           # API client
│   └── vite.config.ts
│
├── .gitignore             # Excludes *.db, .env, backups/
└── README.md              # User-facing documentation
```

## Development Workflow

### Backend Changes

1. **Read code first** — understand current implementation
2. **Check existing tests** — see what's covered
3. **Write/update tests** — add test cases for new features
4. **Run tests** — `cd backend-go && go test ./...`
5. **Test manually** — start backend, test with browser/curl
6. **Commit** — only after verification passes

### Frontend Changes

1. **Start dev server** — `cd frontend && npm run dev`
2. **Test in browser** — `http://localhost:5173`
3. **Check console** — no errors
4. **Verify API calls** — Network tab in DevTools
5. **Test responsive** — mobile/tablet/desktop
6. **Commit** — after visual verification

### Database Schema Changes

**Add migration in `main.go` init:**
```go
_, _ = db.Exec(`
  CREATE TABLE IF NOT EXISTS new_table (
    id TEXT PRIMARY KEY,
    ...
  )
`)
```

**Rules:**
- Migrations run once per database
- Always use `IF NOT EXISTS` or `IF NOT EXISTS COLUMN`
- Test migration on DB copy first
- Never drop tables in migration
- Add new columns with defaults

## Testing

### Unit Tests

```bash
cd backend-go
go test ./...                    # Run all tests
go test -v                       # Verbose output
go test -run TestSpecificFunc    # Run specific test
```

**Tests use in-memory DB** — safe to run anytime, won't touch production.

### Manual Testing

```bash
# Start backend (port 4000)
cd backend-go
go run .

# Start frontend (port 5173)
cd frontend
npm run dev

# Browser: http://localhost:5173
# Login: jhopanstore@gmail.com / jhopanstore
```

### API Testing

```bash
# Health check
curl http://localhost:4000/health

# Login
curl -X POST http://localhost:4000/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"jhopanstore@gmail.com","password":"jhopanstore"}'

# Get OAuth configs (requires token)
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:4000/system/google-config
```

## Git Workflow

```bash
# Check status
git status

# Stage changes
git add -A

# Commit with clear message
git commit -m "feat: add OAuth quota tracking"
git commit -m "fix: prevent delete last active config"
git commit -m "docs: update README with deployment steps"

# Push to main
git push origin main
```

**Commit message prefixes:**
- `feat:` — new feature
- `fix:` — bug fix
- `docs:` — documentation only
- `refactor:` — code restructuring
- `test:` — add/update tests
- `chore:` — build/config changes

## Environment Setup

### Backend Configuration

Create `backend-go/.env`:

```bash
# JWT & Encryption (change in production)
JWT_SECRET=your-secret-key-here
TOKEN_ENCRYPTION_KEY=12345678901234567890123456789012  # Must be exactly 32 bytes

# Primary Google OAuth Config (auto-bootstrapped)
GOOGLE_CLIENT_ID=your-client-id.apps.googleusercontent.com
GOOGLE_CLIENT_SECRET=your-client-secret
GOOGLE_REDIRECT_URI=http://localhost:4000/connected-accounts/google/callback

# Additional OAuth Configs (optional, for rate limit avoidance)
GOOGLE_CLIENT_ID_2=another-project-id.apps.googleusercontent.com
GOOGLE_CLIENT_SECRET_2=another-secret
GOOGLE_REDIRECT_URI_2=http://localhost:4000/connected-accounts/google/callback

# Up to GOOGLE_CLIENT_ID_10 supported
```

**OAuth config bootstrap:**
- Backend reads env vars at startup
- Auto-creates `provider_configs` rows if missing
- Label: "Primary", "Project 2", "Project 3", etc.
- All configs start with `status='active'`

### Google Cloud Console Setup

1. Go to https://console.cloud.google.com
2. Create project (or use existing)
3. Enable **Google Drive API**
4. Create OAuth 2.0 credentials (Web application)
5. Add authorized redirect URI: `http://localhost:4000/connected-accounts/google/callback`
6. Copy Client ID and Client Secret to `.env`

## Current User Setup

**Database:** `C:\Users\ACER\Documents\project\pandrive\backend-go\data\pandrive.db`

**Login credentials:**
- Email: `jhopanstore@gmail.com`
- Password: `jhopanstore` (bcrypt hashed in DB)

**OAuth configs:**
- Primary config bootstrapped from env
- Label: "Primary"
- Status: active
- Quota tracking enabled (8k/100s threshold)

## Common Tasks

### View Database Contents

```bash
cd backend-go

# List all users
sqlite3 data/pandrive.db "SELECT id, name, email FROM users"

# List connected accounts
sqlite3 data/pandrive.db "SELECT id, provider, email FROM connected_accounts"

# List OAuth configs
sqlite3 data/pandrive.db "SELECT id, label, status FROM provider_configs"

# Count files
sqlite3 data/pandrive.db "SELECT COUNT(*) FROM files"
```

### Create Database Backup

```bash
cd backend-go
./backup.sh

# Output: backups/pandrive_YYYYMMDD_HHMMSS.db
# Keeps last 7 backups, auto-deletes older
```

### Restore From Backup

```bash
cd backend-go

# List available backups
ls -la backups/

# Restore (example timestamp)
cp backups/pandrive_20260830_123456.db data/pandrive.db

# Restart backend to use restored DB
```

### Add New OAuth Config

**Option 1: Via environment (recommended)**

1. Add to `backend-go/.env`:
   ```
   GOOGLE_CLIENT_ID_2=new-project.apps.googleusercontent.com
   GOOGLE_CLIENT_SECRET_2=new-secret
   ```
2. Restart backend (auto-bootstraps)

**Option 2: Via API**

```bash
curl -X POST http://localhost:4000/system/google-config \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "clientId": "new-project.apps.googleusercontent.com",
    "clientSecret": "new-secret",
    "redirectUri": "http://localhost:4000/connected-accounts/google/callback",
    "label": "Project 3"
  }'
```

### Update User Credentials

```bash
# Via API (requires login first)
curl -X PUT http://localhost:4000/auth/me \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "New Name",
    "email": "newemail@gmail.com",
    "password": "newpassword"
  }'
```

## Troubleshooting

### "Login failed" with correct credentials

**Check if user exists:**
```bash
sqlite3 backend-go/data/pandrive.db "SELECT * FROM users"
```

**If empty:** Database was reset. Admin bootstrap creates `admin@gmail.com` / `admin` on first run.

### "Connected accounts empty" after restart

**Check database file exists:**
```bash
ls -la backend-go/data/pandrive.db
```

**If missing:** Database was deleted. Restore from backup or user must reconnect accounts.

### OAuth redirect fails

**Check redirect URI matches exactly:**
- Google Console: `http://localhost:4000/connected-accounts/google/callback`
- `.env` file: `GOOGLE_REDIRECT_URI=http://localhost:4000/connected-accounts/google/callback`
- Must match exactly (http vs https, trailing slash)

### Quota tracking not working

**Check provider_config_quota table:**
```bash
sqlite3 backend-go/data/pandrive.db \
  "SELECT * FROM provider_config_quota"
```

**Should show:**
- `request_count` incrementing per OAuth request
- `window_start` timestamp within last 100 seconds
- Auto-resets after 100 seconds

### Frontend can't connect to backend

**Check backend is running:**
```bash
curl http://localhost:4000/health
# Should return: {"status":"ok"}
```

**Check frontend API_URL:**
```typescript
// frontend/src/lib/api.ts
const API_URL = import.meta.env.VITE_API_URL || 'http://localhost:4000'
```

## Security Notes

### Credentials Storage

- **Passwords:** bcrypt hashed (cost 10) in `users.password_hash`
- **OAuth tokens:** AES-GCM encrypted in `connected_accounts.encrypted_token`
- **JWT secrets:** Environment variables only (never commit)
- **Encryption key:** Must be exactly 32 bytes for AES-256

### Never Commit

```gitignore
*.db                # Database files
*.db-wal            # SQLite WAL files
*.db-shm            # SQLite shared memory
.env                # Environment secrets
backups/            # Database backups
```

**Already in `.gitignore`** — double-check before committing.

## Rate Limit Strategy

### Google Drive API Limits

- **Per project:** 10,000 requests / 100 seconds
- **Threshold:** 8,000 requests (80% of limit)
- **Auto-switch:** When config hits 8k, use next config

### Multiple OAuth Configs

**Why:**
- Single project = 10k req/100s limit
- 5 projects = 50k req/100s total capacity

**How it works:**
1. User connects Google Drive account
2. Backend picks least-used OAuth config
3. Tracks `request_count` per config per 100s window
4. Auto-switches to next config at 8k threshold
5. Window resets after 100 seconds

**Frontend UI:**
- Settings → "Google OAuth Configs" section
- Visual quota bars (orange > 80%)
- Add/delete/toggle configs
- Auto-refresh every 10 seconds

## Common Mistakes to Avoid

❌ **Deleting production database for testing**
   - Use `go test ./...` instead (memory DB)

❌ **Committing `.env` or `*.db` files**
   - Check `.gitignore` includes them

❌ **Hardcoding secrets in code**
   - Always use environment variables

❌ **Forgetting to backup before schema changes**
   - Run `./backup.sh` first

❌ **Testing OAuth with wrong redirect URI**
   - Must match Google Console exactly

❌ **Assuming data persists without DB file**
   - SQLite = file on disk, losing file = losing data

## Best Practices

✅ **Read before write** — understand existing code first
✅ **Test before commit** — `go test ./...` must pass
✅ **Backup before changes** — especially schema migrations
✅ **Use stdlib first** — avoid dependencies when possible
✅ **Shortest diff wins** — minimal changes preferred
✅ **Comments for pitfalls** — explain non-obvious decisions
✅ **Update tests** — when changing behavior

## Quick Reference

```bash
# Backend
cd backend-go
go test ./...                    # Run tests
go run .                         # Start server (port 4000)
./backup.sh                      # Create backup

# Frontend  
cd frontend
npm run dev                      # Start dev server (port 5173)
npm run build                    # Production build

# Database
sqlite3 data/pandrive.db           # Open DB shell
.tables                          # List tables
SELECT * FROM users;             # Query users
.quit                            # Exit

# Git
git add -A                       # Stage all
git commit -m "feat: X"          # Commit
git push origin main             # Push
```

## Remember

🔴 **NEVER delete `data/pandrive.db` for testing**
✅ **Always backup before risky operations**
✅ **Test with `go test ./...` (uses :memory: DB)**
✅ **User data is sacred — losing DB = losing everything**

---

**Repository:** https://github.com/jhopan/pandrive
**Owner:** Jhopan (jhopanstore@gmail.com)
**Current date:** 2026-08-30

### 📤 Git Push Policy (MANDATORY)

**ALWAYS push changes to GitHub after committing!**

Every commit MUST be followed by push to origin/main.

```bash
# Correct workflow
git add -A
git commit -m "feat: add feature X"
git push origin main              # REQUIRED - never skip

# Wrong - commit without push
git commit -m "fix: bug Y"
# ... then forget to push = changes not backed up
```

**Why this matters:**
- User expects changes in GitHub repository
- Local commits without push = work not backed up
- Other developers/devices cannot see changes
- CI/CD pipelines will not trigger

**Rule:** Every git commit MUST be immediately followed by git push origin main.

### 🎨 Branding & UI Assets

**Single source of truth for the app mark:** `frontend/public/logo.png` (512×512).

Derived assets, all generated from it (do not hand-edit them separately):

| File | Size | Used by |
|---|---|---|
| `logo.png` | 512×512 | `BrandLogo` (sidebar + mobile header), login page, PWA 512 icon |
| `logo-192.png` | 192×192 | PWA 192 icon, `<link rel="icon" sizes="192x192">` |
| `apple-touch-icon.png` | 180×180 | iOS home-screen icon |
| `favicon.png` | 64×64 | browser tab icon |
| `maskable-icon.png` | 512×512 | Android adaptive icon (logo at ~78% on a matching background) |

To replace the brand mark: drop the new square PNG over `frontend/public/logo.png`, regenerate the four
derivatives (Pillow: `Image.open(src).convert('RGBA').resize((n, n), Image.LANCZOS)`, `optimize=True`), then
rebuild the frontend and re-embed `frontend/dist` into `backend-go/dist`.

**No third-party image CDNs in the UI.** Folder icons render from bundled SVGs (`FolderVisual` maps
`lucide:*` names to local components; a custom `iconUrl` still falls back to the local folder icon on
error), and avatars come from `src/lib/avatar.ts` (deterministic initial + colour as an inline SVG data
URL). Third-party hosts only make the UI break under CSP changes, privacy blockers, or offline use — the
only remaining external request is the Google Fonts stylesheet.

**Header profile menu** (`components/drive/ProfileMenu.tsx`) sits next to the bell in *both* header
variants and owns: Edit profile (name/email), Change password, Settings, Log out. The sidebar keeps nav
only, grouped Files / Cleanup / System / Accounts with an internally scrolling `<nav>`.

**Password changes:** `POST /auth/change-password` requires the current password, enforces ≥8 characters,
rejects an unchanged password, revokes every other session and returns a fresh session pair;
`PUT /auth/me` refuses a `password` field (`USE_CHANGE_PASSWORD`) so a stolen access token cannot rotate
it. Both outcomes are audited (`password_change`, `password_change_failed`).

### 🚀 Deployment: GitHub Releases, not scp

The VPS installs and updates **from GitHub Releases** (`deploy/vps-update.sh`, installed as
`/usr/local/bin/pandrive-update`). Do not scp binaries by hand anymore.

- Tagging `v*` triggers `.github/workflows/release.yml`, which builds six binaries **and** `SHA256SUMS`.
- `pandrive-update` verifies the sha256 from `SHA256SUMS` before replacing anything, keeps the previous
  binary as `.prev`, restarts systemd, health-checks `/health`, and rolls back if the new build fails.
- `-deploy` suffixed versions (from the old scp flow) are treated as equal to the matching tag.
- The binary embeds the frontend, so a release updates UI and API together.
- `--version` in `main.go` prints the build version and is what the updater reads.
- Releases without `SHA256SUMS` are refused (installed version stays untouched).
- Optional zero-touch schedule: `deploy/pandrive-update.service` + `.timer` (daily), disabled unless the
  operator enables the timer.
- Never probe an installed binary by executing it to read its version — a pre-`--version` build boots,
  runs migrations and can create a second database under a different CWD. Read
  `/opt/9drive/.installed-version` (written on success) or the service log instead.

### 🖼️ Gallery / folder sizes / expiry / ntfy (v0.20.0)

- Files sync stores `thumbnail_link` (Drive `thumbnailLink`); `GET /gallery` filters media and the
  frontend loads thumbnails directly from `lh3.googleusercontent.com` — NOT proxied through the VPS.
- Folder sizes: recursive CTE (`tree(id, root)`) aggregates `files.size_bytes` per root folder; returned
  as `sizeBytes` by `/folders`.
- Public links: `expires_at` + `auto_revoke` columns; `revokeExpiredShares(grace)` runs in the background
  loop (same goroutine as the 5-minute sync) and removes the Drive `anyone` permission.
- Notifications: `app_settings` kv table (`ntfy_server`, `ntfy_topic`); `a.notify(user, title, msg, tag,
  priority)` is fire-and-forget (goroutine, 15 s timeout, never fails the caller).
- **Route shadowing fix**: unprefixed API GETs (`/recent`, `/search`, `/starred`, `/gallery`, `/activity`,
  `/uploads/queue`, `/storage/*`) collided with SPA page paths — hard refresh returned 401 JSON. They now
  live ONLY under `/api/*`; the unprefixed path falls through to the SPA. Never register a plain
  `GET /<page-path>` API route again.

### 🌐 i18n ID/EN (v0.21.0)

- `src/lib/i18n.tsx`: context + `ID` dictionary; keys are the EN source strings, missing key = fallback to
  the key itself. Provider wraps the app in `main.tsx`; choice persisted at `pandrive.lang`.
- Toggles: header pill (desktop + mobile), profile-menu Language row, login-card pill.
- `PageHeader` applies `t()` centrally to `title`/`description` when they are plain strings, so every
  page translates without per-page edits.
- Sidebar labels are wrapped in `t()` in `DriveLayout` (groups + items + Accounts heading + Log Out).
- When adding user-visible strings, put the English source as-is; add an `ID` entry only for Indonesian.

### 🔗 Preview redirect + branded /s/ share page (v0.23.0)

- `viewFileUrl` now returns the real `webViewLink` (via `driveWebViewLink`); the "empty URL" stub is gone.
  Any media preview MUST go browser -> Google; never stream video through the VPS.
- `publicPermission` stores the **branded page URL** (`<scheme>://<host>/s/<shareID>`, host from
  `X-Forwarded-Host`) instead of the raw Drive link; response `url` is the page URL too.
- `sharePage` (`GET /s/{id}`, public): scans `COALESCE(s.expires_at,'')` (raw NULL breaks the string scan —
  this exact bug cost a debugging round), serves ~1.4 KB of inline HTML with `/logo.png`, file name, size
  label, heavy-file warning (>= 200 MB), `X-Robots-Tag: noindex`; 404 revoked, 410 expired.
- Old share rows keep their stored Drive URL — the page only falls back to `uc?export=download` when the
  stored URL is not a drive.google.com link.

### 👥 Multi-user + trash auto-purge (v0.24.0)

- `users.role` (admin|user) + `users.disabled` columns; bootstrap admin INSERT carries `role='admin'`.
  EXISTING installs promote their owner manually: `UPDATE users SET role='admin' WHERE email='...';`
- Login: disabled=1 → 403 `ACCOUNT_DISABLED` (checked BEFORE password compare); role is loaded into the
  JWT (`authUser.Role`).
- `requireAdmin(next)` gate; `GET/PATCH /api/admin/users[/{id}]` (list/disable/enable/role). Self-disable
  or self-demote → 400 `SELF_LOCKOUT`; disabling revokes all sessions immediately.
- Registration stays closed unless `app_settings.open_registration == '1'` (admin Settings switch).
- Trash auto-purge: `trash_autopurge_days` in `app_settings` (empty = off, 1–365). `purgeOldTrash()` runs
  in the background loop: `files.delete` on Drive (REAL deletion — quota freed) for rows
  `status='deleted'` older than the window, removes the local row, notifies the owner. Tested live:
  40-day-old trash purged, fresh trash kept, disabled = no-op.

### ⚠️ Kernel write-loss lesson (this session)

Three execute_code cells reported success but the file was later found at the committed state (git clean,
changes gone). Recovery: re-applied ALL patches in one cell with an immediate on-disk verify
(`chk = open(p).read()` in the SAME cell) and committed the WIP right away (`git commit` before tests).
When a write matters, always verify content on disk in the same cell and commit early.

### 📦 One-line installer (deploy/install.sh)

`curl -fsSL https://raw.githubusercontent.com/jhopan/pandrive/master/deploy/install.sh | bash` installs
the LATEST release; `| bash -s -- v0.24.1` pins a version (with or without the leading v). Verified
against real releases: latest mode + version mode both OK in a sandbox, checksum gate refuses releases
without SHA256SUMS, fresh `.env` seeded with random secrets only when absent, systemd unit created and
health-checked when run as root, `.prev` kept on upgrade. Env overrides: `GITHUB_REPO`, `INSTALL_DIR`,
`SERVICE`. Note: GitHub objects redirect can 504 transiently — the retry loop in the test harness
covered it; keep `--retry 3` on every curl.

### 🧰 Installer v2 (multi-platform + Go/Caddy extras)

- `deploy/install.sh`: auto-detects OS (Linux/macOS/Termux/Windows-MSYS via `TERMUX_VERSION`, `uname -s`)
  and arch (amd64/arm64; armv7 mapped to arm64 asset). Asset `pandrive-<goos>-<arch>[.exe]`.
- Service per platform: systemd (root/Linux), launchd agent (macOS), manual hint (Termux, MSYS).
- `--with-go`: installs the Go toolchain **only for building from source** (apt golang-go or go.dev tarball
  to /usr/local/go; pkg in Termux; brew on macOS; winget on Windows) — the app binary is self-contained
  and never needs Go.
- `--with-caddy domain`: downloads Caddy, writes a Caddyfile (`reverse_proxy 127.0.0.1:4000`), starts the
  service when root. HTTPS cert + renewal automatic (DNS + ports 80/443 required).
- `deploy/install.ps1` for native Windows: same flow (checksum, .env once, sc.exe service, winget
  extras). Written with UTF-8 **BOM** (PS5 misparses multibyte without it — keep the BOM!) and plain
  hyphens (no em-dash) in strings.
- Verified: bash syntax OK; sandbox latest + pinned-0.24.0 on MSYS (windows asset path); PS Parser OK.

### 🌐 Reverse Proxy menu (v0.26.0)

- `GET/PUT /api/settings/proxy` (`proxy_provider` = none|caddy|cloudflare, `proxy_domain`);
  `POST /api/settings/proxy/caddyfile` generates a Caddyfile (`reverse_proxy 127.0.0.1:<port>`) into
  `configDir()/pandrive/Caddyfile`; on root+linux+systemd ALSO to /etc/caddy and restarts caddy.
- `configDir()` = os.UserConfigDir()/pandrive fallback "." — XDG_CONFIG_HOME/AppData override works in
  tests (t.Setenv both).
- Tunnel status reads TUNNEL_ID/TUNNEL_TOKEN from env or `.env` (Config struct has no tunnel fields).
- UI: `/proxy` page (System group). Provider validation: only none/caddy/cloudflare; domain must
  contain a dot.

### 🔑 API Keys (v0.27.x)

- Table `api_keys`: `prefix` (11 chars, lookup) + `key_hash` (SHA-256 of `pd_...`; plaintext NEVER stored).
- `requireAuth` accepts `Bearer pd_...` BEFORE JWT parsing → `userFromAPIKey` (hash lookup, revoked/
  disabled checks, last-used stamp throttled 1/minute).
- Endpoints: `GET/POST /api/keys`, `DELETE /api/keys/{id}` (revoke), `DELETE /api/keys/{id}/hard`.
- Scopes column exists but all keys currently act as their owner; admin/settings/auth endpoints must
  stay session-JWT-only for future scope work.
- Max 10 active keys per user (400 TOO_MANY_KEYS).

### 📱 PWA + mobile polish

- Manifest/SW/icons were already correct; do NOT precache index.html in the SW (stale CSP trap — see
  folder-icons reference).
- index.html: `viewport-fit=cover`, `apple-mobile-web-app-capable` + `status-bar-style
  black-translucent` + `apple-mobile-web-app-title`, theme-color `#0f172a` (matches the dark shell).
- `style.css` block `pandrive-mobile-harden`: safe-area insets (left/right/top on shell+header, bottom
  on the content section via `pb-[max(1rem,env(safe-area-inset-bottom))]`), `overscroll-behavior-y:
  none`, tap-highlight off, 16px inputs on ≤640px (stops iOS focus zoom), 40px touch targets for
  `aside nav a` + `header button` on ≤640px.
- Tailwind minifier rewrites `(max-width:640px)` to `(width<=640px)` — grep for `width<=640px` when
  checking the built CSS, or you will falsely conclude the rules are missing.
- AllFilesPage toolbar: `flex-wrap` + "New Folder" shortens to "Folder" below `sm`.
