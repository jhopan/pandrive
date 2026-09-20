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
