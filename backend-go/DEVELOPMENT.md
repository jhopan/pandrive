# Backend Development Guidelines

## ⚠️ CRITICAL: Database Safety

**NEVER delete production database for testing!**

```bash
# ❌ FORBIDDEN - destroys user data
rm -f data/pandrive.db*

# ✅ ALLOWED - run unit tests (use :memory: DB)
go test ./...

# ✅ ALLOWED - backup before risky changes
./backup.sh
```

## Database Location

**Production DB:** `data/pandrive.db` (SQLite with WAL mode)

**Contains:**
- User accounts (bcrypt passwords)
- Connected Google Drive accounts
- OAuth tokens (AES-GCM encrypted)
- File metadata and folders
- Upload sessions

**Losing this = user loses everything!**

## Safe Testing Workflow

1. **Unit tests** use in-memory DB (`:memory:`)
2. **Manual testing** uses production DB
3. **Never drop/recreate** production DB
4. **Always backup** before destructive operations

```bash
# Run tests (safe - uses memory DB)
go test ./...

# Start backend (uses production DB)
go run .

# Create backup before risky changes
./backup.sh
```

## Backup & Recovery

**Create backup:**
```bash
./backup.sh
# Creates: backups/pandrive_YYYYMMDD_HHMMSS.db
# Keeps last 7 backups
```

**Restore from backup:**
```bash
# List available backups
ls -la backups/

# Restore
cp backups/pandrive_20260830_123456.db data/pandrive.db
```

## Current Setup

**User credentials:**
- Email: `jhopanstore@gmail.com`
- Password: `jhopanstore`

**OAuth configs:**
- Bootstrap from `GOOGLE_CLIENT_ID` in `.env`
- Multiple configs via `_2`, `_3` suffixes
- Auto-switch at 8k requests/100s

## Development Commands

```bash
# Run tests
go test ./...

# Start backend (port 4000)
go run .

# Check DB contents
sqlite3 data/pandrive.db "SELECT email, name FROM users"

# Backup database
./backup.sh
```

## Remember

✅ Test with existing data
✅ Backup before changes
✅ Read code before editing
✅ Run `go test` before commit

❌ NEVER `rm data/pandrive.db`
❌ NEVER commit `.env` or `*.db` files
❌ NEVER drop tables in production
