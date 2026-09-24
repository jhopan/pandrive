package main

import (
	"archive/zip"
	"bytes"
	"embed"

	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	_ "modernc.org/sqlite"
)

type Config struct {
	DatabaseURL        string
	AppPort            string
	FrontendURL        string
	JWTSecret          string
	TokenKey           string
	GoogleClientID     string
	GoogleClientSecret string
	GoogleRedirectURI  string
	UpdateRepo         string
}

// buildVersion is injected at link time via -ldflags "-X main.buildVersion=...".
var buildVersion = "dev"

type App struct {
	DB                 *sql.DB
	Config             Config
	HTTPClient         *http.Client
	GoogleEndpoint     oauth2.Endpoint
	GoogleUserInfoURL  string
	GoogleDriveAPIURL  string
	GoogleUploadAPIURL string
	loginFails         map[string]*loginFail
	loginMu            sync.Mutex
	RateMeter          *rateMeter
}

type loginFail struct {
	count        int
	blockedUntil time.Time
}

// Login limiter: 5 failures per IP -> 15 minute block.
func (a *App) loginBlocked(ip string) (bool, time.Duration) {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	f, ok := a.loginFails[ip]
	if !ok {
		return false, 0
	}
	if !f.blockedUntil.IsZero() && time.Now().Before(f.blockedUntil) {
		return true, time.Until(f.blockedUntil)
	}
	return false, 0
}

func (a *App) loginRecordFailure(ip string) {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	f := a.loginFails[ip]
	if f == nil {
		f = &loginFail{}
		a.loginFails[ip] = f
	}
	f.count++
	if f.count >= 5 {
		f.blockedUntil = time.Now().Add(15 * time.Minute)
		f.count = 0
	}
}

func (a *App) loginRecordSuccess(ip string) {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	delete(a.loginFails, ip)
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type authUser struct {
	ID    string `json:"id"`
	Role  string `json:"role,omitempty"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type ctxKey string

const userKey ctxKey = "user"

// dbFilePathFromURL extracts the plain file path of a sqlite file: URL.
func dbFilePathFromURL(databaseURL string) string {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return ""
	}
	path := u.Path
	if path == "" {
		path = u.Opaque
	}
	if i := strings.Index(path, "?"); i >= 0 {
		path = path[:i]
	}
	return path
}

// dataDirFromURL extracts the directory part of a sqlite file: URL, if any.
func dataDirFromURL(databaseURL string) string {
	u, err := url.Parse(databaseURL)
	if err != nil || u.Opaque == "" && u.Path == "" {
		return ""
	}
	path := u.Path
	if path == "" {
		path = u.Opaque
	}
	// Strip query part of opaque form file:data/pandrive.db?_pragma=...
	if i := strings.Index(path, "?"); i >= 0 {
		path = path[:i]
	}
	return filepath.Dir(path)
}

func loadConfig() Config {
	_ = godotenv.Load()
	getenv := func(key, fallback string) string {
		if value := os.Getenv(key); value != "" {
			return value
		}
		return fallback
	}
	return Config{
		DatabaseURL:        getenv("DATABASE_URL", "file:data/pandrive.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"),
		AppPort:            getenv("APP_PORT", "4000"),
		FrontendURL:        getenv("FRONTEND_URL", "http://localhost:5173"),
		JWTSecret:          getenv("JWT_ACCESS_SECRET", "change-this-jwt-secret-before-production"),
		TokenKey:           getenv("TOKEN_ENCRYPTION_KEY", "change-this-token-key-before-production"),
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		GoogleRedirectURI:  os.Getenv("GOOGLE_REDIRECT_URI"),
		UpdateRepo:         getenv("UPDATE_REPO", "jhopan/pandrive"),
	}
}

// Sentinel errors for share targets: the message is shown to the user, so keep it actionable.
var (
	errTargetNotFound    = errors.New("target not found")
	errTargetNotMirrored = errors.New("this folder is not mirrored to Drive, so it cannot be shared")
	errTargetUnavailable = errors.New("unable to resolve the target")
)

// processStartedAt powers the uptime reported by /system/health.
var processStartedAt = time.Now()

func (a *App) migrate() error {
	_, err := a.DB.Exec(`
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, email TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS user_sessions (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, refresh_token_hash TEXT NOT NULL,
  expires_at TEXT NOT NULL, revoked_at TEXT, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS user_sessions_user_id_idx ON user_sessions(user_id);
CREATE TABLE IF NOT EXISTS provider_configs (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, provider TEXT NOT NULL,
  client_id_encrypted TEXT NOT NULL, client_secret_encrypted TEXT NOT NULL,
  redirect_uri TEXT NOT NULL, scopes TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL DEFAULT 'active',
  label TEXT, last_used_at TEXT,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS provider_config_quota (
  id TEXT PRIMARY KEY, provider_config_id TEXT NOT NULL UNIQUE, 
  request_count INTEGER NOT NULL DEFAULT 0, window_start TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(provider_config_id) REFERENCES provider_configs(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS oauth_states (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, provider_config_id TEXT NOT NULL,
  flow TEXT NOT NULL DEFAULT 'connect', state_hash TEXT NOT NULL UNIQUE, expires_at TEXT NOT NULL,
  used_at TEXT, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
  FOREIGN KEY(provider_config_id) REFERENCES provider_configs(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS connected_accounts (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, provider_config_id TEXT,
  provider TEXT NOT NULL DEFAULT 'google_drive', provider_account_id TEXT NOT NULL,
  email TEXT NOT NULL, display_name TEXT, avatar_url TEXT, access_token_encrypted TEXT,
  refresh_token_encrypted TEXT, token_expires_at TEXT, scopes TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL DEFAULT 'connected', last_error TEXT,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(user_id, provider, provider_account_id),
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
  FOREIGN KEY(provider_config_id) REFERENCES provider_configs(id) ON DELETE SET NULL
);
CREATE TABLE IF NOT EXISTS storage_accounts (
  id TEXT PRIMARY KEY, connected_account_id TEXT NOT NULL UNIQUE, total_bytes INTEGER,
  used_bytes INTEGER NOT NULL DEFAULT 0, available_bytes INTEGER, trash_bytes INTEGER,
  last_synced_at TEXT, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(connected_account_id) REFERENCES connected_accounts(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS upload_routing_policies (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL UNIQUE, mode TEXT NOT NULL DEFAULT 'most_available',
  priority_account_ids TEXT NOT NULL DEFAULT '[]', round_robin_cursor INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS folders (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, parent_id TEXT, connected_account_id TEXT,
  provider TEXT NOT NULL DEFAULT 'google_drive', provider_folder_id TEXT, name TEXT NOT NULL,
  color TEXT NOT NULL DEFAULT 'text-blue-500', icon_url TEXT, deleted_at TEXT,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
  FOREIGN KEY(parent_id) REFERENCES folders(id) ON DELETE SET NULL,
  FOREIGN KEY(connected_account_id) REFERENCES connected_accounts(id) ON DELETE SET NULL
);
CREATE TABLE IF NOT EXISTS files (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, connected_account_id TEXT NOT NULL, folder_id TEXT,
  provider TEXT NOT NULL DEFAULT 'google_drive', provider_file_id TEXT NOT NULL, name TEXT NOT NULL,
  mime_type TEXT NOT NULL, size_bytes INTEGER NOT NULL, checksum TEXT,
  status TEXT NOT NULL DEFAULT 'active', deleted_at TEXT,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
  FOREIGN KEY(connected_account_id) REFERENCES connected_accounts(id) ON DELETE CASCADE,
  FOREIGN KEY(folder_id) REFERENCES folders(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS files_user_folder_idx ON files(user_id, status, folder_id, created_at);
CREATE TABLE IF NOT EXISTS upload_sessions (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, target_connected_account_id TEXT, folder_id TEXT,
  file_name TEXT NOT NULL, mime_type TEXT NOT NULL, size_bytes INTEGER NOT NULL, status TEXT NOT NULL,
  google_session_uri TEXT, error_message TEXT, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, completed_at TEXT,
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
  FOREIGN KEY(target_connected_account_id) REFERENCES connected_accounts(id) ON DELETE SET NULL,
  FOREIGN KEY(folder_id) REFERENCES folders(id) ON DELETE SET NULL
);
CREATE TABLE IF NOT EXISTS activity_log (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, connected_account_id TEXT,
  action TEXT NOT NULL, target_type TEXT NOT NULL DEFAULT '', target_id TEXT NOT NULL DEFAULT '',
  target_name TEXT NOT NULL DEFAULT '', size_bytes INTEGER NOT NULL DEFAULT 0,
  detail TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS activity_log_user_time_idx ON activity_log(user_id, created_at DESC);
CREATE TABLE IF NOT EXISTS share_links (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, file_id TEXT NOT NULL, connected_account_id TEXT NOT NULL,
  provider_file_id TEXT NOT NULL, permission_id TEXT NOT NULL DEFAULT '', url TEXT NOT NULL DEFAULT '',
  revoked_at TEXT, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS share_links_user_idx ON share_links(user_id, revoked_at);
CREATE TABLE IF NOT EXISTS split_files (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, name TEXT NOT NULL, mime_type TEXT NOT NULL,
  size_bytes INTEGER NOT NULL, part_count INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'incomplete',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS split_files_user_idx ON split_files(user_id);
CREATE TABLE IF NOT EXISTS split_parts (
  id TEXT PRIMARY KEY, split_id TEXT NOT NULL, part_index INTEGER NOT NULL,
  file_id TEXT, connected_account_id TEXT NOT NULL, size_bytes INTEGER NOT NULL,
  FOREIGN KEY(split_id) REFERENCES split_files(id) ON DELETE CASCADE,
  FOREIGN KEY(file_id) REFERENCES files(id) ON DELETE SET NULL,
  FOREIGN KEY(connected_account_id) REFERENCES connected_accounts(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS split_parts_split_idx ON split_parts(split_id);
CREATE TABLE IF NOT EXISTS permission_grants (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, connected_account_id TEXT NOT NULL,
  target_type TEXT NOT NULL, target_id TEXT NOT NULL, provider_file_id TEXT NOT NULL,
  email TEXT NOT NULL, role TEXT NOT NULL, permission_id TEXT NOT NULL DEFAULT '',
  revoked_at TEXT, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS permission_grants_user_idx ON permission_grants(user_id, revoked_at);
CREATE INDEX IF NOT EXISTS activity_log_action_idx ON activity_log(user_id, action);
CREATE TABLE IF NOT EXISTS app_settings (
  key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS api_keys (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, name TEXT NOT NULL,
  prefix TEXT NOT NULL, key_hash TEXT NOT NULL,
  scopes TEXT NOT NULL DEFAULT 'read',
  last_used_at TEXT, revoked_at TEXT, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS api_keys_user_idx ON api_keys(user_id, revoked_at);
CREATE INDEX IF NOT EXISTS api_keys_prefix_idx ON api_keys(prefix);
`)
	if err != nil {
		return err
	}
	// SQLite cannot add a column with IF NOT EXISTS, so ignore the "duplicate column" error
	// on databases created before the column existed.
	for _, stmt := range []string{
		`ALTER TABLE files ADD COLUMN starred INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE folders ADD COLUMN starred INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE files ADD COLUMN thumbnail_link TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE share_links ADD COLUMN expires_at TEXT`,
		`ALTER TABLE share_links ADD COLUMN auto_revoke INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'user'`,
		`ALTER TABLE upload_sessions ADD COLUMN split_id TEXT`,
		`ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := a.DB.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

// logActivity records an audit entry. Failures are logged but never break the request
// that triggered them: auditing must not take down the feature it observes.
func (a *App) logActivity(r *http.Request, userID, accountID, action, targetType, targetID, targetName string, sizeBytes int64, detail string) {
	ip := ""
	if r != nil {
		ip = clientIP(r)
	}
	if _, err := a.DB.Exec(`INSERT INTO activity_log (id,user_id,connected_account_id,action,target_type,target_id,target_name,size_bytes,detail,ip) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		randomID(), userID, nullIfEmpty(accountID), action, targetType, targetID, targetName, sizeBytes, detail, ip); err != nil {
		log.Printf("activity log failed (%s): %v", action, err)
	}
}

// listActivity returns the audit trail, newest first, with optional filters.
func (a *App) listActivity(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	action := strings.TrimSpace(r.URL.Query().Get("action"))
	accountID := strings.TrimSpace(r.URL.Query().Get("accountId"))
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 500 {
		limit = v
	}
	offset := 0
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v >= 0 {
		offset = v
	}

	where := "WHERE a.user_id=?"
	args := []any{user.ID}
	if action != "" && action != "all" {
		where += " AND a.action=?"
		args = append(args, action)
	}
	if accountID != "" && accountID != "all" {
		where += " AND a.connected_account_id=?"
		args = append(args, accountID)
	}

	var total int
	if err := a.DB.QueryRow(`SELECT COUNT(*) FROM activity_log a `+where, args...).Scan(&total); err != nil {
		writeError(w, 500, "ACTIVITY_FAILED", "Unable to count activity.")
		return
	}

	query := `SELECT a.id,a.action,a.target_type,a.target_id,a.target_name,a.size_bytes,a.detail,a.ip,COALESCE(a.created_at,''),COALESCE(c.email,'') 
		FROM activity_log a LEFT JOIN connected_accounts c ON c.id=a.connected_account_id ` + where + ` ORDER BY a.id DESC LIMIT ? OFFSET ?`
	rows, err := a.DB.Query(query, append(args, limit, offset)...)
	if err != nil {
		writeError(w, 500, "ACTIVITY_FAILED", "Unable to read activity: "+err.Error())
		return
	}
	defer rows.Close()

	entries := []map[string]any{}
	for rows.Next() {
		var id, act, targetType, targetID, targetName, detail, ip, createdAt, email string
		var size int64
		if err := rows.Scan(&id, &act, &targetType, &targetID, &targetName, &size, &detail, &ip, &createdAt, &email); err != nil {
			writeError(w, 500, "ACTIVITY_FAILED", "Unable to read activity.")
			return
		}
		entries = append(entries, map[string]any{
			"id": id, "action": act, "targetType": targetType, "targetId": targetID, "targetName": targetName,
			"sizeBytes": fmt.Sprint(size), "detail": detail, "ip": ip, "createdAt": createdAt, "accountEmail": email,
		})
	}

	// Distinct actions seen, for the filter dropdown.
	actionRows, err := a.DB.Query(`SELECT DISTINCT action FROM activity_log WHERE user_id=? ORDER BY action`, user.ID)
	actions := []string{}
	if err == nil {
		defer actionRows.Close()
		for actionRows.Next() {
			var act string
			if actionRows.Scan(&act) == nil {
				actions = append(actions, act)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "total": total, "limit": limit, "offset": offset, "actions": actions})
}

func (a *App) ensureInitialAdmin() error {
	_, err := a.ensureInitialAdminPassword()
	return err
}

// ensureInitialAdminPassword creates the bootstrap admin and returns the generated password (empty if admin already exists).
func (a *App) ensureInitialAdminPassword() (string, error) {
	var count int
	if err := a.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return "", err
	}
	if count != 0 {
		return "", nil
	}
	// Random one-time password, shown once in the log on first run.
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	password := hex.EncodeToString(buf)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	if _, err = a.DB.Exec(`INSERT INTO users (id,name,email,password_hash,role) VALUES (?,?,?,?,?)`, randomID(), "Administrator", "admin@gmail.com", string(hash), "admin"); err != nil {
		return "", err
	}
	log.Printf("Initial admin account created: admin@gmail.com / %s  (change this password after first login)", password)
	return password, nil
}

func (a *App) bootstrapGoogleConfig() error {
	if a.Config.GoogleClientID == "" || a.Config.GoogleClientSecret == "" {
		return nil
	}
	var adminID string
	if err := a.DB.QueryRow(`SELECT id FROM users WHERE email='admin@gmail.com'`).Scan(&adminID); err != nil {
		return nil
	}

	// Bootstrap primary config
	var exists int
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM provider_configs WHERE provider='google_drive' AND user_id=?`, adminID).Scan(&exists)
	if exists == 0 {
		redirectURI := a.Config.GoogleRedirectURI
		if redirectURI == "" {
			redirectURI = "http://localhost:4000/connected-accounts/google/callback"
		}
		_, _ = a.DB.Exec(`INSERT INTO provider_configs (id,user_id,provider,client_id_encrypted,client_secret_encrypted,redirect_uri,scopes,label) VALUES (?,?,?,?,?,?,?,?)`,
			randomID(), adminID, "google_drive", a.encrypt(a.Config.GoogleClientID), a.encrypt(a.Config.GoogleClientSecret), redirectURI,
			`["https://www.googleapis.com/auth/drive","https://www.googleapis.com/auth/userinfo.profile","https://www.googleapis.com/auth/userinfo.email"]`, "Primary")
	}

	// Bootstrap additional configs from GOOGLE_CLIENT_ID_2, GOOGLE_CLIENT_SECRET_2, etc.
	for i := 2; i <= 10; i++ {
		clientID := os.Getenv(fmt.Sprintf("GOOGLE_CLIENT_ID_%d", i))
		clientSecret := os.Getenv(fmt.Sprintf("GOOGLE_CLIENT_SECRET_%d", i))
		if clientID == "" || clientSecret == "" {
			continue
		}
		var configExists int
		encryptedID := a.encrypt(clientID)
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM provider_configs WHERE client_id_encrypted=?`, encryptedID).Scan(&configExists)
		if configExists > 0 {
			continue
		}
		redirectURI := os.Getenv(fmt.Sprintf("GOOGLE_REDIRECT_URI_%d", i))
		if redirectURI == "" {
			redirectURI = "http://localhost:4000/connected-accounts/google/callback"
		}
		_, _ = a.DB.Exec(`INSERT INTO provider_configs (id,user_id,provider,client_id_encrypted,client_secret_encrypted,redirect_uri,scopes,label) VALUES (?,?,?,?,?,?,?,?)`,
			randomID(), adminID, "google_drive", encryptedID, a.encrypt(clientSecret), redirectURI,
			`["https://www.googleapis.com/auth/drive","https://www.googleapis.com/auth/userinfo.profile","https://www.googleapis.com/auth/userinfo.email"]`, fmt.Sprintf("Project %d", i))
	}
	return nil
}

//go:embed all:dist
var distFS embed.FS

// serveSPA serves the embedded frontend build with SPA fallback to index.html.
func serveSPA() http.HandlerFunc {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return func(w http.ResponseWriter, r *http.Request) {
			writeError(w, 500, "EMBED_FAILED", "Frontend assets unavailable.")
		}
	}
	fileServer := http.FileServer(http.FS(sub))
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(sub, path); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback: unknown paths render the app shell.
		index, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			writeError(w, 500, "EMBED_FAILED", "Frontend index unavailable.")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	}
}

func (a *App) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.health)
	mux.HandleFunc("POST /auth/register", a.register)
	mux.HandleFunc("POST /auth/login", a.login)
	mux.HandleFunc("POST /auth/refresh", a.refresh)
	mux.HandleFunc("POST /auth/logout", a.requireAuth(a.logout))
	mux.HandleFunc("GET /auth/me", a.requireAuth(a.me))
	mux.HandleFunc("PUT /auth/me", a.requireAuth(a.updateMe))
	mux.HandleFunc("POST /auth/change-password", a.requireAuth(a.changePassword))
	mux.HandleFunc("GET /settings/notifications", a.requireAuth(a.getNotificationSettings))
	mux.HandleFunc("PUT /settings/notifications", a.requireAuth(a.putNotificationSettings))
	mux.HandleFunc("POST /settings/notifications/test", a.requireAuth(a.testNotification))
	mux.HandleFunc("GET /api/admin/users", a.requireAuth(a.requireAdmin(a.listUsers)))
	mux.HandleFunc("PATCH /api/admin/users/{id}", a.requireAuth(a.requireAdmin(a.updateUser)))
	mux.HandleFunc("GET /api/settings/trash", a.requireAuth(a.getTrashSettings))
	mux.HandleFunc("PUT /api/settings/trash", a.requireAuth(a.putTrashSettings))
	mux.HandleFunc("GET /api/settings/proxy", a.requireAuth(a.getProxySettings))
	mux.HandleFunc("PUT /api/settings/proxy", a.requireAuth(a.putProxySettings))
	mux.HandleFunc("POST /api/settings/proxy/caddyfile", a.requireAuth(a.writeCaddyfile))
	mux.HandleFunc("GET /api/keys", a.requireAuth(a.listAPIKeys))
	mux.HandleFunc("POST /api/keys", a.requireAuth(a.createAPIKey))
	mux.HandleFunc("DELETE /api/keys/{id}", a.requireAuth(a.revokeAPIKey))
	mux.HandleFunc("DELETE /api/keys/{id}/hard", a.requireAuth(a.deleteAPIKey))
	mux.HandleFunc("GET /system/google-config", a.requireAuth(a.getGoogleConfig))
	mux.HandleFunc("POST /system/google-config", a.requireAuth(a.saveGoogleConfig))
	mux.HandleFunc("DELETE /system/google-config/{id}", a.requireAuth(a.deleteGoogleConfig))
	mux.HandleFunc("PATCH /system/google-config/{id}", a.requireAuth(a.updateGoogleConfig))
	mux.HandleFunc("POST /system/update", a.requireAuth(a.systemUpdate))
	mux.HandleFunc("GET /system/version", a.requireAuth(a.updateInfoHandler))
	mux.HandleFunc("GET /connected-accounts", a.requireAuth(a.listAccounts))
	mux.HandleFunc("GET /connected-accounts/google/connect-url", a.requireAuth(a.googleConnectURL))
	mux.HandleFunc("GET /connected-accounts/google/callback", a.googleCallback)
	mux.HandleFunc("PATCH /storage/routing-policy", a.requireAuth(a.updateRoutingPolicy))
	mux.HandleFunc("POST /connected-accounts/{id}/sync-quota", a.requireAuth(a.syncQuota))
	mux.HandleFunc("GET /folders", a.requireAuth(a.listFolders))
	mux.HandleFunc("POST /folders", a.requireAuth(a.createFolder))
	mux.HandleFunc("GET /files", a.requireAuth(a.listFiles))
	mux.HandleFunc("POST /files/sync-google", a.requireAuth(a.syncGoogleFiles))
	mux.HandleFunc("GET /files/{id}/view-url", a.requireAuth(a.viewFileUrl))
	mux.HandleFunc("GET /files/{id}/download", a.requireAuth(a.downloadFile))
	mux.HandleFunc("GET /files/{id}/stream", a.requireAuth(a.streamFile))
	mux.HandleFunc("POST /files/{id}/share", a.requireAuth(a.shareFileUrl))
	mux.HandleFunc("POST /files/{id}/public-permission", a.requireAuth(a.publicPermission))
	mux.HandleFunc("POST /files/{id}/public-link", a.requireAuth(a.publicPermission))
	mux.HandleFunc("POST /files/{id}/star", a.requireAuth(a.starFile))
	mux.HandleFunc("POST /folders/{id}/star", a.requireAuth(a.starFolder))
	mux.HandleFunc("GET /permissions", a.requireAuth(a.listPermissions))
	mux.HandleFunc("POST /invites", a.requireAuth(a.grantAccess))
	mux.HandleFunc("GET /invites", a.requireAuth(a.listInvites))
	mux.HandleFunc("DELETE /invites/{id}", a.requireAuth(a.revokeInvite))
	mux.HandleFunc("GET /shares", a.requireAuth(a.listShares))
	mux.HandleFunc("DELETE /shares/{id}", a.requireAuth(a.revokeShare))
	mux.HandleFunc("POST /uploads/queue/{id}/cancel", a.requireAuth(a.cancelUpload))
	mux.HandleFunc("DELETE /uploads/queue/{id}", a.requireAuth(a.removeUploadRecord))
	mux.HandleFunc("GET /system/health", a.requireAuth(a.systemHealth))
	mux.HandleFunc("GET /system/rate-limits", a.requireAuth(a.rateLimits))
	mux.HandleFunc("POST /files/batch-download", a.requireAuth(a.batchDownloadZip))
	mux.HandleFunc("GET /files/duplicates", a.requireAuth(a.findDuplicates))
	mux.HandleFunc("POST /files/{id}/transfer", a.requireAuth(a.transferFile))
	mux.HandleFunc("POST /files/{id}/restore", a.requireAuth(a.restoreFile))
	mux.HandleFunc("POST /files/{id}/purge", a.requireAuth(a.purgeFile))
	mux.HandleFunc("POST /connected-accounts/{id}/empty-trash", a.requireAuth(a.emptyAccountTrash))
	mux.HandleFunc("PATCH /files/{id}", a.requireAuth(a.updateFile))
	mux.HandleFunc("DELETE /files/{id}", a.requireAuth(a.deleteFile))
	mux.HandleFunc("PATCH /files/batch", a.requireAuth(a.batchUpdateFiles))
	mux.HandleFunc("DELETE /files/batch", a.requireAuth(a.batchDeleteFiles))
	mux.HandleFunc("PATCH /folders/{id}", a.requireAuth(a.updateFolder))
	mux.HandleFunc("DELETE /folders/{id}", a.requireAuth(a.deleteFolder))
	mux.HandleFunc("POST /uploads/target", a.requireAuth(a.selectUploadTarget))
	mux.HandleFunc("POST /uploads/resumable/init", a.requireAuth(a.initResumableUpload))
	mux.HandleFunc("GET /uploads/resumable/status/{id}", a.requireAuth(a.resumableStatus))
	mux.HandleFunc("PUT /uploads/resumable/chunk/{id}", a.requireAuth(a.resumableChunk))
	mux.HandleFunc("POST /uploads/split-init", a.requireAuth(a.initSplitUpload))
	// API routes duplicated under /api/* so page paths (/recent, /search, ...) stay SPA deep-links.
	// The unprefixed forms shadowed the SPA and returned 401 JSON on hard refresh.
	mux.HandleFunc("GET /api/recent", a.requireAuth(a.listRecent))
	mux.HandleFunc("GET /api/search", a.requireAuth(a.searchFiles))
	mux.HandleFunc("GET /api/starred", a.requireAuth(a.listStarred))
	mux.HandleFunc("GET /api/gallery", a.requireAuth(a.listGallery))
	mux.HandleFunc("GET /api/activity", a.requireAuth(a.listActivity))
	mux.HandleFunc("GET /api/uploads/queue", a.requireAuth(a.uploadQueue))
	mux.HandleFunc("GET /api/storage/summary", a.requireAuth(a.storageSummary))
	mux.HandleFunc("GET /api/storage/breakdown", a.requireAuth(a.storageBreakdown))
	mux.HandleFunc("GET /api/storage/analyzer", a.requireAuth(a.storageAnalyzer))
	mux.HandleFunc("GET /api/storage/routing-policy", a.requireAuth(a.getRoutingPolicy))
	mux.HandleFunc("POST /api/settings/notifications", a.requireAuth(a.putNotificationSettings))
	mux.HandleFunc("GET /api/settings/notifications", a.requireAuth(a.getNotificationSettings))
	mux.HandleFunc("POST /api/settings/notifications/test", a.requireAuth(a.testNotification))
	mux.HandleFunc("GET /s/{id}", a.sharePage)
	mux.HandleFunc("/", serveSPA())
	// Support same-origin deployments where the frontend calls /api/*: strip the prefix and forward.
	apiStrip := http.StripPrefix("/api", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	mux.Handle("/api/", apiStrip)
	return a.cors(mux)
}

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) register(w http.ResponseWriter, r *http.Request) {
	var count int
	if err := a.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err == nil && count > 0 && a.settingValue("open_registration") != "1" {
		writeError(w, http.StatusForbidden, "REGISTRATION_DISABLED", "Registration is disabled. Ask an administrator or enable it in Settings.")
		return
	}

	var body struct{ Name, Email, Password string }
	if err := decodeJSON(r, &body); err != nil || body.Email == "" || body.Password == "" || body.Name == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Name, email, and password are required.")
		return
	}
	if len(body.Password) < 8 {
		writeError(w, http.StatusBadRequest, "WEAK_PASSWORD", "Password must be at least 8 characters.")
		return
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(body.Password), 10)
	user := authUser{ID: randomID(), Name: body.Name, Email: body.Email}
	_, err := a.DB.Exec(`INSERT INTO users (id,name,email,password_hash) VALUES (?,?,?,?)`, user.ID, user.Name, user.Email, hash)
	if err != nil {
		writeError(w, http.StatusConflict, "EMAIL_IN_USE", "Email already registered.")
		return
	}
	a.respondSession(w, http.StatusCreated, user)
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if blocked, wait := a.loginBlocked(ip); blocked {
		writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", fmt.Sprintf("Too many failed attempts. Try again in %d minutes.", int(wait.Minutes())+1))
		return
	}
	var body struct{ Email, Password string }
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	var user authUser
	var hash string
	var disabled int
	err := a.DB.QueryRow(`SELECT id,name,email,COALESCE(role,'user'),password_hash,COALESCE(disabled,0) FROM users WHERE email = ? AND status = 'active'`, strings.ToLower(strings.TrimSpace(body.Email))).Scan(&user.ID, &user.Name, &user.Email, &user.Role, &hash, &disabled)
	if err == nil && disabled == 1 {
		writeError(w, http.StatusForbidden, "ACCOUNT_DISABLED", "This account has been disabled by an administrator.")
		return
	}
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password)) != nil {
		a.loginRecordFailure(ip)
		// Attribute the attempt to the account when it exists, so the owner sees it in their audit trail.
		attempted := strings.ToLower(strings.TrimSpace(body.Email))
		var ownerID string
		if a.DB.QueryRow(`SELECT id FROM users WHERE email=?`, attempted).Scan(&ownerID) == nil {
			a.logActivity(r, ownerID, "", "login_failed", "user", ownerID, attempted, 0, "Invalid credentials")
			a.notify(ownerID, "Login gagal", "Percobaan login gagal untuk "+attempted+".", "warning", "high")
		} else {
			// No matching account: nothing to attribute the row to, so it is only visible in the
			// server log. Without this a typo'd email leaves no trace at all.
			log.Printf("login failed for unknown email %q from %s", attempted, clientIP(r))
		}
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Invalid email or password.")
		return
	}
	a.loginRecordSuccess(ip)
	a.logActivity(r, user.ID, "", "login", "user", user.ID, user.Email, 0, "Signed in")
	a.respondSession(w, http.StatusOK, user)
}

func (a *App) respondSession(w http.ResponseWriter, status int, user authUser) {
	accessToken, err := a.signAccessToken(user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "TOKEN_FAILED", "Unable to create session.")
		return
	}
	refreshToken := randomToken()
	_, err = a.DB.Exec(`INSERT INTO user_sessions (id,user_id,refresh_token_hash,expires_at) VALUES (?,?,?,?)`, randomID(), user.ID, hashToken(refreshToken), time.Now().Add(30*24*time.Hour).UTC().Format(time.RFC3339Nano))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "SESSION_FAILED", "Unable to create session.")
		return
	}
	writeJSON(w, status, map[string]any{"accessToken": accessToken, "refreshToken": refreshToken, "user": user})
}

func (a *App) refresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := decodeJSON(r, &body); err != nil || body.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Refresh token is required.")
		return
	}
	var user authUser
	var sessionID string
	err := a.DB.QueryRow(`SELECT s.id,u.id,u.name,u.email FROM user_sessions s JOIN users u ON u.id=s.user_id WHERE s.refresh_token_hash=? AND s.revoked_at IS NULL AND s.expires_at > ?`, hashToken(body.RefreshToken), time.Now().UTC().Format(time.RFC3339Nano)).Scan(&sessionID, &user.ID, &user.Name, &user.Email)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "AUTH_SESSION_EXPIRED", "Refresh token expired.")
		return
	}
	// Rotation: revoke the used refresh token and issue a fresh pair.
	_, _ = a.DB.Exec(`UPDATE user_sessions SET revoked_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), sessionID)
	a.respondSession(w, http.StatusOK, user)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	_, _ = a.DB.Exec(`UPDATE user_sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), user.ID)
	a.logActivity(r, user.ID, "", "logout", "user", user.ID, user.Email, 0, "Signed out")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) getGoogleConfig(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	rows, err := a.DB.Query(`
		SELECT p.id, p.label, p.redirect_uri, p.status, COALESCE(p.last_used_at,''), p.created_at,
		       COALESCE(q.request_count, 0), COALESCE(q.window_start, '')
		FROM provider_configs p 
		LEFT JOIN provider_config_quota q ON q.provider_config_id = p.id 
		WHERE p.user_id=? AND p.provider='google_drive' 
		ORDER BY p.created_at ASC`, user.ID)
	if err != nil {
		writeError(w, 500, "CONFIG_FAILED", "Unable to list configs.")
		return
	}
	defer rows.Close()
	configs := make([]map[string]any, 0)
	windowStart := time.Now().UTC().Add(-100 * time.Second).Format(time.RFC3339Nano)
	for rows.Next() {
		var id, label, redirectURI, status, lastUsed, createdAt, quotaWindowStart string
		var requestCount int
		if err := rows.Scan(&id, &label, &redirectURI, &status, &lastUsed, &createdAt, &requestCount, &quotaWindowStart); err != nil {
			continue
		}
		// Reset count if window expired
		if quotaWindowStart < windowStart {
			requestCount = 0
		}
		configs = append(configs, map[string]any{
			"id": id, "label": label, "redirectUri": redirectURI, "status": status,
			"lastUsedAt": lastUsed, "createdAt": createdAt,
			"quotaUsed": requestCount, "quotaLimit": 8000,
		})
	}
	defaultRedirect := "http://" + r.Host + "/connected-accounts/google/callback"
	writeJSON(w, http.StatusOK, map[string]any{"configs": configs, "defaultRedirectUri": defaultRedirect})
}

func (a *App) saveGoogleConfig(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		RedirectURI  string `json:"redirectUri"`
		Label        string `json:"label"`
	}
	if err := decodeJSON(r, &body); err != nil || body.ClientID == "" || body.ClientSecret == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Client ID and Client Secret are required.")
		return
	}
	if body.RedirectURI == "" {
		body.RedirectURI = "http://" + r.Host + "/connected-accounts/google/callback"
	}
	if _, err := url.ParseRequestURI(body.RedirectURI); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid redirect URI.")
		return
	}
	if body.Label == "" {
		var count int
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM provider_configs WHERE user_id=? AND provider='google_drive'`, user.ID).Scan(&count)
		body.Label = fmt.Sprintf("Project %d", count+1)
	}
	// Check if this client_id already exists (prevent duplicates)
	encryptedID := a.encrypt(body.ClientID)
	var exists int
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM provider_configs WHERE client_id_encrypted=? AND user_id=?`, encryptedID, user.ID).Scan(&exists)
	if exists > 0 {
		writeError(w, http.StatusBadRequest, "CONFIG_EXISTS", "This OAuth config already exists.")
		return
	}
	_, err := a.DB.Exec(`INSERT INTO provider_configs (id,user_id,provider,client_id_encrypted,client_secret_encrypted,redirect_uri,scopes,status,label) VALUES (?,?,?,?,?,?,?,'active',?)`, randomID(), user.ID, "google_drive", encryptedID, a.encrypt(body.ClientSecret), body.RedirectURI, `["https://www.googleapis.com/auth/drive","https://www.googleapis.com/auth/userinfo.email","https://www.googleapis.com/auth/userinfo.profile"]`, body.Label)
	if err != nil {
		writeError(w, 500, "GOOGLE_CONFIG_FAILED", "Unable to save Google config.")
		return
	}
	a.logActivity(r, user.ID, "", "oauth_config_add", "config", "", body.Label, 0, "OAuth config added")
	writeJSON(w, http.StatusCreated, map[string]string{"message": "Google OAuth config added."})
}

func (a *App) deleteGoogleConfig(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	configID := r.PathValue("id")
	// Check ownership
	var ownerID string
	err := a.DB.QueryRow(`SELECT user_id FROM provider_configs WHERE id=?`, configID).Scan(&ownerID)
	if err != nil || ownerID != user.ID {
		writeError(w, http.StatusNotFound, "CONFIG_NOT_FOUND", "Config not found.")
		return
	}
	// Prevent deleting last config
	var count int
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM provider_configs WHERE user_id=? AND provider='google_drive' AND status='active'`, user.ID).Scan(&count)
	if count <= 1 {
		writeError(w, http.StatusBadRequest, "LAST_CONFIG", "Cannot delete the last active config.")
		return
	}
	var cfgLabel string
	_ = a.DB.QueryRow(`SELECT COALESCE(label,'') FROM provider_configs WHERE id=?`, configID).Scan(&cfgLabel)
	_, _ = a.DB.Exec(`DELETE FROM provider_configs WHERE id=?`, configID)
	a.logActivity(r, user.ID, "", "oauth_config_delete", "config", configID, cfgLabel, 0, "OAuth config deleted")
	writeJSON(w, http.StatusOK, map[string]string{"message": "Config deleted."})
}

func (a *App) updateGoogleConfig(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	configID := r.PathValue("id")
	var body struct {
		Status string `json:"status"`
		Label  string `json:"label"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid body.")
		return
	}
	// Check ownership
	var ownerID string
	err := a.DB.QueryRow(`SELECT user_id FROM provider_configs WHERE id=?`, configID).Scan(&ownerID)
	if err != nil || ownerID != user.ID {
		writeError(w, http.StatusNotFound, "CONFIG_NOT_FOUND", "Config not found.")
		return
	}
	if body.Status != "" && body.Status != "active" && body.Status != "disabled" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Status must be 'active' or 'disabled'.")
		return
	}
	if body.Label != "" {
		_, _ = a.DB.Exec(`UPDATE provider_configs SET label=? WHERE id=?`, body.Label, configID)
	}
	if body.Status != "" {
		_, _ = a.DB.Exec(`UPDATE provider_configs SET status=? WHERE id=?`, body.Status, configID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Config updated."})
}

// ---- update checking -------------------------------------------------------

type updateInfo struct {
	Current    string `json:"current"`
	Latest     string `json:"latest"`
	Available  bool   `json:"updateAvailable"`
	ReleaseURL string `json:"releaseUrl"`
	AssetURL   string `json:"assetUrl"`
	AssetName  string `json:"assetName"`
	CheckedAt  string `json:"checkedAt"`
	Error      string `json:"error,omitempty"`
}

var (
	updateMu      sync.Mutex
	updateCached  *updateInfo
	updateFetched time.Time
)

// normalizeVersion strips a leading v and any -suffix so semver compare works ("v0.4.0-deploy" -> "0.4.0").
func normalizeVersion(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	return v
}

// versionNewer reports whether latest is a strictly newer semver-ish value than current.
func versionNewer(current, latest string) bool {
	c, l := normalizeVersion(current), normalizeVersion(latest)
	if c == "" || l == "" {
		return false
	}
	cp, lp := strings.Split(c, "."), strings.Split(l, ".")
	for i := 0; i < len(lp) || i < len(cp); i++ {
		var cv, lv int
		if i < len(cp) {
			_, _ = fmt.Sscan(strings.TrimFunc(cp[i], func(r rune) bool { return r < '0' || r > '9' }), &cv)
		}
		if i < len(lp) {
			_, _ = fmt.Sscan(strings.TrimFunc(lp[i], func(r rune) bool { return r < '0' || r > '9' }), &lv)
		}
		if lv != cv {
			return lv > cv
		}
	}
	return false
}

// fetchLatestRelease queries the GitHub releases API for this repository.
func (a *App) fetchLatestRelease(ctx context.Context) updateInfo {
	info := updateInfo{Current: buildVersion, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	repo := a.Config.UpdateRepo
	if repo == "" {
		info.Error = "update repo not configured"
		return info
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "pandrive/"+buildVersion)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		info.Error = fmt.Sprintf("github release lookup failed (%d)", resp.StatusCode)
		return info
	}
	var payload struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		info.Error = err.Error()
		return info
	}
	info.Latest = payload.TagName
	info.ReleaseURL = payload.HTMLURL
	info.Available = versionNewer(buildVersion, payload.TagName)
	// Match the asset for this OS/arch (release names: pandrive-<os>-<arch>[.exe]).
	want := "pandrive-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	info.AssetName = want
	for _, asset := range payload.Assets {
		if asset.Name == want {
			info.AssetURL = asset.URL
			break
		}
	}
	return info
}

// updateInfoHandler returns cached release info (refresh=1 forces a re-check).
func (a *App) updateInfoHandler(w http.ResponseWriter, r *http.Request) {
	updateMu.Lock()
	fresh := updateCached != nil && time.Since(updateFetched) < 6*time.Hour
	cached := updateCached
	updateMu.Unlock()
	if r.URL.Query().Get("refresh") != "" || !fresh {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		info := a.fetchLatestRelease(ctx)
		cancel()
		updateMu.Lock()
		updateCached = &info
		updateFetched = time.Now()
		updateMu.Unlock()
		writeJSON(w, http.StatusOK, info)
		return
	}
	writeJSON(w, http.StatusOK, cached)
}

// startUpdateChecker logs available updates at startup and re-checks every 12h.
func (a *App) startUpdateChecker() {
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			info := a.fetchLatestRelease(ctx)
			cancel()
			updateMu.Lock()
			updateCached = &info
			updateFetched = time.Now()
			updateMu.Unlock()
			switch {
			case info.Error != "":
				log.Printf("update check failed: %s", info.Error)
			case info.Available:
				log.Printf("UPDATE AVAILABLE: %s -> %s (%s)", info.Current, info.Latest, info.ReleaseURL)
			default:
				log.Printf("up to date (%s)", info.Current)
			}
			time.Sleep(12 * time.Hour)
		}
	}()
}

func (a *App) systemUpdate(w http.ResponseWriter, r *http.Request) {
	go func() {
		exec.Command("git", "pull", "origin", "main").Run()
	}()
	writeJSON(w, http.StatusOK, map[string]string{"message": "System update initiated"})
}

func (a *App) googleConnectURL(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var id, encryptedID, redirectURI, scopes string
	// Pick config with quota < 8000 in current 100s window, fallback to least recent usage
	now := time.Now().UTC()
	windowStart := now.Add(-100 * time.Second).Format(time.RFC3339Nano)
	err := a.DB.QueryRow(`
		SELECT p.id, p.client_id_encrypted, p.redirect_uri, p.scopes 
		FROM provider_configs p 
		LEFT JOIN provider_config_quota q ON q.provider_config_id = p.id 
		WHERE p.user_id=? AND p.provider='google_drive' AND p.status='active' 
		  AND (q.request_count IS NULL OR q.request_count < 8000 OR q.window_start < ?)
		ORDER BY p.last_used_at IS NULL DESC, p.last_used_at ASC 
		LIMIT 1`, user.ID, windowStart).Scan(&id, &encryptedID, &redirectURI, &scopes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "GOOGLE_NOT_CONFIGURED", "Configure Google OAuth first or all configs at quota limit.")
		return
	}
	// Update last_used_at and increment quota counter
	_, _ = a.DB.Exec(`UPDATE provider_configs SET last_used_at=? WHERE id=?`, now.Format(time.RFC3339Nano), id)
	_, _ = a.DB.Exec(`
		INSERT INTO provider_config_quota (id, provider_config_id, request_count, window_start) 
		VALUES (?, ?, 1, ?) 
		ON CONFLICT(provider_config_id) DO UPDATE SET 
			request_count = CASE WHEN window_start < ? THEN 1 ELSE request_count + 1 END,
			window_start = CASE WHEN window_start < ? THEN ? ELSE window_start END,
			updated_at = CURRENT_TIMESTAMP`,
		randomID(), id, now.Format(time.RFC3339Nano), windowStart, windowStart, now.Format(time.RFC3339Nano))
	clientID, err := a.decrypt(encryptedID)
	if err != nil {
		writeError(w, 500, "GOOGLE_CONFIG_FAILED", "Unable to read Google config.")
		return
	}
	state := randomToken()
	_, err = a.DB.Exec(`INSERT INTO oauth_states (id,user_id,provider_config_id,flow,state_hash,expires_at) VALUES (?,?,?,?,?,?)`, randomID(), user.ID, id, "connect", hashToken(state), time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339Nano))
	if err != nil {
		writeError(w, 500, "OAUTH_STATE_FAILED", "Unable to create OAuth session.")
		return
	}
	// Host-relative redirect: when the stored redirect URI points at localhost (dev default) but the
	// request arrives via a real domain (tunnel/proxy), rebuild it against the request host so the
	// same config works from any domain without re-registering each one in Google Console.
	if u, err := url.Parse(redirectURI); err == nil && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1") {
		if host := forwardedHost(r); host != "" {
			u.Host = host
			u.Scheme = requestScheme(r)
			redirectURI = u.String()
		}
	}
	values := url.Values{"client_id": {clientID}, "redirect_uri": {redirectURI}, "response_type": {"code"}, "access_type": {"offline"}, "prompt": {"consent"}, "include_granted_scopes": {"true"}, "scope": {strings.Join(parseScopes(scopes), " ")}, "state": {state}}
	writeJSON(w, http.StatusOK, map[string]string{"url": "https://accounts.google.com/o/oauth2/v2/auth?" + values.Encode()})
}

func (a *App) listAccounts(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	rows, err := a.DB.Query(`SELECT c.id,c.provider,c.email,COALESCE(c.display_name,''),c.status,COALESCE(s.total_bytes,0),COALESCE(s.used_bytes,0),COALESCE(s.available_bytes,0),COALESCE(s.last_synced_at,'') FROM connected_accounts c LEFT JOIN storage_accounts s ON s.connected_account_id=c.id WHERE c.user_id=? AND c.provider='google_drive' AND c.status='connected' ORDER BY c.created_at DESC`, user.ID)
	if err != nil {
		writeError(w, 500, "ACCOUNTS_FAILED", "Unable to list connected accounts.")
		return
	}
	defer rows.Close()
	accounts := make([]map[string]any, 0)
	for rows.Next() {
		var id, provider, email, displayName, status, lastSynced string
		var total, used, available int64
		if err := rows.Scan(&id, &provider, &email, &displayName, &status, &total, &used, &available, &lastSynced); err != nil {
			writeError(w, 500, "ACCOUNTS_FAILED", "Unable to read connected accounts.")
			return
		}
		// Cheap token health check: refresh-token failure marks the account for reconnect.
		needsReconnect := false
		if _, err := a.getGoogleToken(r.Context(), id, false); err != nil {
			needsReconnect = true
		}
		accounts = append(accounts, map[string]any{"id": id, "provider": provider, "email": email, "displayName": displayName, "status": status, "needsReconnect": needsReconnect, "storageAccount": map[string]string{"totalBytes": fmt.Sprint(total), "usedBytes": fmt.Sprint(used), "availableBytes": fmt.Sprint(available), "lastSyncedAt": lastSynced}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

func (a *App) storageSummary(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var total, used, available int64
	err := a.DB.QueryRow(`SELECT COALESCE(SUM(s.total_bytes),0),COALESCE(SUM(s.used_bytes),0),COALESCE(SUM(s.available_bytes),0) FROM connected_accounts c LEFT JOIN storage_accounts s ON s.connected_account_id=c.id WHERE c.user_id=? AND c.status='connected'`, user.ID).Scan(&total, &used, &available)
	if err != nil {
		writeError(w, 500, "STORAGE_FAILED", "Unable to calculate storage.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"totalBytes": fmt.Sprint(total), "usedBytes": fmt.Sprint(used), "availableBytes": fmt.Sprint(available)})
}

func (a *App) storageBreakdown(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var photo, video, doc int64
	rows, err := a.DB.Query(`SELECT mime_type, COALESCE(SUM(size_bytes), 0) FROM files WHERE user_id=? AND status='active' GROUP BY mime_type`, user.ID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var mime string
			var size int64
			if err := rows.Scan(&mime, &size); err == nil {
				if strings.HasPrefix(mime, "image/") {
					photo += size
				} else if strings.HasPrefix(mime, "video/") {
					video += size
				} else {
					doc += size
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"photo":    fmt.Sprint(photo),
		"video":    fmt.Sprint(video),
		"document": fmt.Sprint(doc),
	})
}

func (a *App) getRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var mode, p string
	err := a.DB.QueryRow(`SELECT mode, priority_account_ids FROM upload_routing_policies WHERE user_id=?`, user.ID).Scan(&mode, &p)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusOK, map[string]any{"policy": map[string]any{"mode": "most_available", "priorityAccountIds": []string{}}})
		return
	}
	var accs []string
	json.Unmarshal([]byte(p), &accs)
	writeJSON(w, http.StatusOK, map[string]any{"policy": map[string]any{"mode": mode, "priorityAccountIds": accs}})
}

func (a *App) updateRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		Mode               string   `json:"mode"`
		PriorityAccountIds []string `json:"priorityAccountIds"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body")
		return
	}
	accBytes, _ := json.Marshal(body.PriorityAccountIds)
	_, _ = a.DB.Exec(`INSERT INTO upload_routing_policies (id, user_id, mode, priority_account_ids) VALUES (?, ?, ?, ?) ON CONFLICT(user_id) DO UPDATE SET mode=excluded.mode, priority_account_ids=excluded.priority_account_ids`, randomID(), user.ID, body.Mode, string(accBytes))
	writeJSON(w, http.StatusOK, map[string]any{"policy": body})
}

func (a *App) getGoogleToken(ctx context.Context, accountID string, forceRefresh bool) (string, error) {
	var encryptedToken, encryptedRefresh, expiresAt, configID string
	// COALESCE guards NULL columns (a fresh account may not have a provider config yet):
	// scanning NULL into a string returns a driver error, which used to surface as a confusing failure.
	err := a.DB.QueryRow(`SELECT COALESCE(access_token_encrypted,''), COALESCE(refresh_token_encrypted,''), COALESCE(token_expires_at,''), COALESCE(provider_config_id,'') FROM connected_accounts WHERE id=?`, accountID).Scan(&encryptedToken, &encryptedRefresh, &expiresAt, &configID)
	if err != nil {
		return "", err
	}

	exp, err := time.Parse(time.RFC3339Nano, expiresAt)
	if !forceRefresh && err == nil && exp.After(time.Now().Add(5*time.Minute)) {
		return a.decrypt(encryptedToken)
	}

	// Token expired or forced refresh
	refreshToken, err := a.decrypt(encryptedRefresh)
	if err != nil || refreshToken == "" {
		return "", errors.New("refresh token missing or invalid")
	}

	var encryptedClientID, encryptedClientSecret string
	err = a.DB.QueryRow(`SELECT client_id_encrypted, client_secret_encrypted FROM provider_configs WHERE id=?`, configID).Scan(&encryptedClientID, &encryptedClientSecret)
	if err != nil {
		return "", err
	}
	clientID, _ := a.decrypt(encryptedClientID)
	clientSecret, _ := a.decrypt(encryptedClientSecret)

	conf := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     a.GoogleEndpoint,
	}

	token := &oauth2.Token{RefreshToken: refreshToken}
	tokenSource := conf.TokenSource(ctx, token)
	newToken, err := tokenSource.Token()
	if err != nil {
		return "", err
	}

	// Update new token in DB
	newEncryptedRefresh := encryptedRefresh
	if newToken.RefreshToken != "" && newToken.RefreshToken != refreshToken {
		newEncryptedRefresh = a.encrypt(newToken.RefreshToken)
	}

	_, _ = a.DB.Exec(`UPDATE connected_accounts SET access_token_encrypted=?, refresh_token_encrypted=?, token_expires_at=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		a.encrypt(newToken.AccessToken), newEncryptedRefresh, newToken.Expiry.UTC().Format(time.RFC3339Nano), accountID)

	return newToken.AccessToken, nil
}

func (a *App) syncAccountQuota(ctx context.Context, accountID string) error {
	accessToken, err := a.getGoogleToken(ctx, accountID, false)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.GoogleDriveAPIURL+`/about?fields=storageQuota`, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := a.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("google quota request rejected (%d)", response.StatusCode)
	}
	var payload struct {
		StorageQuota struct {
			Limit string `json:"limit"`
			Usage string `json:"usage"`
			Trash string `json:"usageInDriveTrash"`
		} `json:"storageQuota"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return err
	}
	var total, used, trash int64
	_, _ = fmt.Sscan(payload.StorageQuota.Limit, &total)
	_, _ = fmt.Sscan(payload.StorageQuota.Usage, &used)
	_, _ = fmt.Sscan(payload.StorageQuota.Trash, &trash)
	available := total - used
	if available < 0 {
		available = 0
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = a.DB.Exec(`INSERT INTO storage_accounts (id,connected_account_id,total_bytes,used_bytes,available_bytes,trash_bytes,last_synced_at) VALUES (?,?,?,?,?,?,?) ON CONFLICT(connected_account_id) DO UPDATE SET total_bytes=excluded.total_bytes,used_bytes=excluded.used_bytes,available_bytes=excluded.available_bytes,trash_bytes=excluded.trash_bytes,last_synced_at=excluded.last_synced_at`, randomID(), accountID, total, used, available, trash, now)
	return err
}

func (a *App) syncQuota(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	accountID := r.PathValue("id")
	var ownerID string
	err := a.DB.QueryRow(`SELECT user_id FROM connected_accounts WHERE id=?`, accountID).Scan(&ownerID)
	if err != nil || ownerID != user.ID {
		writeError(w, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "Account not found.")
		return
	}
	if err := a.syncAccountQuota(r.Context(), accountID); err != nil {
		writeError(w, 502, "QUOTA_FAILED", "Unable to sync account quota.")
		return
	}
	var total, used, available, trash int64
	_ = a.DB.QueryRow(`SELECT COALESCE(total_bytes,0),COALESCE(used_bytes,0),COALESCE(available_bytes,0),COALESCE(trash_bytes,0) FROM storage_accounts WHERE connected_account_id=?`, accountID).Scan(&total, &used, &available, &trash)
	writeJSON(w, http.StatusOK, map[string]any{"quota": map[string]string{"totalBytes": fmt.Sprint(total), "usedBytes": fmt.Sprint(used), "availableBytes": fmt.Sprint(available), "trashBytes": fmt.Sprint(trash)}})
}

func (a *App) createFolder(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		Name     string  `json:"name"`
		ParentID *string `json:"parentId"`
		Color    string  `json:"color"`
	}
	if err := decodeJSON(r, &body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Folder name is required.")
		return
	}
	if body.ParentID != nil {
		var exists int
		err := a.DB.QueryRow(`SELECT 1 FROM folders WHERE id=? AND user_id=? AND deleted_at IS NULL`, *body.ParentID, user.ID).Scan(&exists)
		if err != nil {
			writeError(w, http.StatusBadRequest, "PARENT_NOT_FOUND", "Parent folder not found.")
			return
		}
	}
	color := body.Color
	if color == "" {
		color = "text-blue-500"
	}
	id := randomID()
	_, err := a.DB.Exec(`INSERT INTO folders (id,user_id,parent_id,name,color) VALUES (?,?,?,?,?)`, id, user.ID, body.ParentID, strings.TrimSpace(body.Name), color)
	if err != nil {
		writeError(w, 500, "FOLDER_CREATE_FAILED", "Unable to create folder.")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"folder": map[string]any{"id": id, "name": strings.TrimSpace(body.Name), "parentId": body.ParentID, "color": color}})
}

// folderSizeBytes aggregates the size of every active file under each folder, including all
// descendant folders (recursive CTE over parent_id), in one query per request.
func (a *App) folderSizeBytes(userID string) (map[string]int64, error) {
	rows, err := a.DB.Query(`WITH RECURSIVE tree(id, root) AS (
	  SELECT id, id FROM folders WHERE user_id=? AND deleted_at IS NULL
	  UNION ALL
	  SELECT f.id, t.root FROM folders f JOIN tree t ON f.parent_id=t.id WHERE f.deleted_at IS NULL
	)
	SELECT t.root, COALESCE(SUM(fi.size_bytes),0)
	FROM tree t LEFT JOIN files fi ON fi.folder_id=t.id AND fi.status='active'
	GROUP BY t.root`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sizes := map[string]int64{}
	for rows.Next() {
		var id string
		var total int64
		if err := rows.Scan(&id, &total); err != nil {
			return nil, err
		}
		sizes[id] = total
	}
	return sizes, rows.Err()
}

func (a *App) listFolders(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	parentID := r.URL.Query().Get("parentId")
	accountID := r.URL.Query().Get("accountId")
	query := `SELECT id,name,parent_id,color,created_at,updated_at,COALESCE(starred,0),COALESCE(provider_folder_id,''),COALESCE(connected_account_id,'') FROM folders WHERE user_id=? AND deleted_at IS NULL`
	args := []any{user.ID}
	if parentID == "" {
		query += ` AND parent_id IS NULL`
	} else {
		query += ` AND parent_id=?`
		args = append(args, parentID)
	}
	// When browsing a single account, show its mirrored folders plus local (account-less) ones.
	if accountID != "" {
		query += ` AND (connected_account_id=? OR connected_account_id IS NULL)`
		args = append(args, accountID)
	}
	query += ` ORDER BY name COLLATE NOCASE`
	rows, err := a.DB.Query(query, args...)
	if err != nil {
		writeError(w, 500, "FOLDERS_FAILED", "Unable to list folders.")
		return
	}
	defer rows.Close()
	// sizes cover direct + nested content; skip silently on failure (cosmetic field).
	sizes, _ := a.folderSizeBytes(user.ID)
	folders := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, color, createdAt, updatedAt, providerFolderID, connectedAccountID string
		var starred int
		var parent sql.NullString
		if err := rows.Scan(&id, &name, &parent, &color, &createdAt, &updatedAt, &starred, &providerFolderID, &connectedAccountID); err != nil {
			writeError(w, 500, "FOLDERS_FAILED", "Unable to read folders.")
			return
		}
		var parentID any
		if parent.Valid {
			parentID = parent.String
		}
		folders = append(folders, map[string]any{"id": id, "name": name, "parentId": parentID, "color": color, "createdAt": createdAt, "updatedAt": updatedAt, "starred": starred == 1, "providerFolderId": providerFolderID, "connectedAccountId": connectedAccountID, "sizeBytes": fmt.Sprint(sizes[id])})
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
}

// fileKindClause maps a UI "kind" to a mime_type predicate.
func fileKindClause(kind string) (string, []any) {
	switch kind {
	case "image":
		return `f.mime_type LIKE 'image/%'`, nil
	case "video":
		return `f.mime_type LIKE 'video/%'`, nil
	case "audio":
		return `f.mime_type LIKE 'audio/%'`, nil
	case "pdf":
		return `f.mime_type = 'application/pdf'`, nil
	case "doc":
		return `(f.mime_type LIKE 'text/%' OR f.mime_type LIKE 'application/msword%' OR f.mime_type LIKE 'application/vnd.openxmlformats-officedocument%' OR f.mime_type LIKE 'application/vnd.oasis.opendocument%' OR f.mime_type LIKE 'application/rtf%')`, nil
	case "archive":
		// Real Drive files use platform-specific types: Windows zips arrive as
		// application/x-zip-compressed, not application/zip. Match both families.
		return `(f.mime_type LIKE 'application/zip%' OR f.mime_type LIKE 'application/x-zip%' OR f.mime_type LIKE 'application/x-compressed%' OR f.mime_type LIKE 'application/x-rar%' OR f.mime_type LIKE 'application/vnd.rar%' OR f.mime_type LIKE 'application/x-7z%' OR f.mime_type LIKE 'application/x-tar%' OR f.mime_type LIKE 'application/gzip%' OR f.mime_type LIKE 'application/x-gzip%' OR f.mime_type LIKE 'application/x-bzip%')`, nil
	case "gapps":
		// Google-native items (Docs/Sheets/Slides + folders mirrored from Drive) report a
		// vnd.google-apps.* mime and no size.
		return `f.mime_type LIKE 'application/vnd.google-apps.%'`, nil
	case "other":
		return `NOT (f.mime_type LIKE 'image/%' OR f.mime_type LIKE 'video/%' OR f.mime_type LIKE 'audio/%' OR f.mime_type = 'application/pdf' OR f.mime_type LIKE 'text/%' OR f.mime_type LIKE 'application/msword%' OR f.mime_type LIKE 'application/vnd.openxmlformats-officedocument%' OR f.mime_type LIKE 'application/vnd.oasis.opendocument%' OR f.mime_type LIKE 'application/rtf%' OR f.mime_type LIKE 'application/zip%' OR f.mime_type LIKE 'application/x-zip%' OR f.mime_type LIKE 'application/x-rar%' OR f.mime_type LIKE 'application/x-7z%' OR f.mime_type LIKE 'application/x-tar%' OR f.mime_type LIKE 'application/gzip%' OR f.mime_type LIKE 'application/x-compressed%' OR f.mime_type LIKE 'application/vnd.google-apps.%')`, nil
	}
	return "", nil
}

// fileFilterClause builds the shared WHERE fragment for /files and /search.
// Extracted so the two endpoints cannot drift: the frontend already sent kind/minSize/
// maxSize/date filters that /files used to ignore silently.
func fileFilterClause(r *http.Request, userID string) (string, []any, error) {
	statusFilter := "active"
	if r.URL.Query().Get("status") == "deleted" {
		statusFilter = "deleted"
	}
	where := `f.user_id=? AND f.status='` + statusFilter + `'`
	args := []any{userID}

	if v := strings.TrimSpace(r.URL.Query().Get("folderId")); v != "" {
		if v == "none" {
			where += ` AND f.folder_id IS NULL`
		} else {
			where += ` AND f.folder_id=?`
			args = append(args, v)
		}
	}
	if v := strings.TrimSpace(r.URL.Query().Get("accountId")); v != "" {
		where += ` AND f.connected_account_id=?`
		args = append(args, v)
	}
	if v := strings.TrimSpace(r.URL.Query().Get("q")); v != "" {
		// Escape LIKE wildcards so a literal % or _ in a filename search stays literal.
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(v)
		where += ` AND f.name LIKE ? ESCAPE '\'`
		args = append(args, "%"+escaped+"%")
	}
	if clause, _ := fileKindClause(strings.TrimSpace(r.URL.Query().Get("kind"))); clause != "" {
		where += ` AND ` + clause
	}
	if v := strings.TrimSpace(r.URL.Query().Get("minSize")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return "", nil, errors.New("minSize must be an integer")
		}
		where += ` AND f.size_bytes >= ?`
		args = append(args, n)
	}
	if v := strings.TrimSpace(r.URL.Query().Get("maxSize")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return "", nil, errors.New("maxSize must be an integer")
		}
		where += ` AND f.size_bytes <= ?`
		args = append(args, n)
	}
	// Dates are parsed with datetime() so both stored timestamp formats compare correctly.
	if v := strings.TrimSpace(r.URL.Query().Get("startDate")); v != "" {
		where += ` AND datetime(f.created_at) >= datetime(?)`
		args = append(args, v)
	}
	if v := strings.TrimSpace(r.URL.Query().Get("endDate")); v != "" {
		where += ` AND datetime(f.created_at) <= datetime(?)`
		args = append(args, v)
	}
	switch r.URL.Query().Get("starred") {
	case "1", "true":
		where += ` AND f.starred=1`
	case "0", "false":
		where += ` AND COALESCE(f.starred,0)=0`
	}
	return where, args, nil
}

// searchFiles is /files with sorting, pagination, totals and facets.
func (a *App) searchFiles(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	where, args, err := fileFilterClause(r, user.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}

	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 500 {
		limit = v
	}
	offset := 0
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v >= 0 {
		offset = v
	}

	order := "datetime(f.updated_at) DESC"
	switch r.URL.Query().Get("sort") {
	case "name":
		order = "f.name COLLATE NOCASE ASC"
	case "name_desc":
		order = "f.name COLLATE NOCASE DESC"
	case "size":
		order = "f.size_bytes DESC"
	case "size_asc":
		order = "f.size_bytes ASC"
	case "oldest":
		order = "datetime(f.created_at) ASC"
	case "created":
		order = "datetime(f.created_at) DESC"
	}

	var total int
	var totalBytes int64
	if err := a.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(f.size_bytes),0) FROM files f WHERE `+where, args...).Scan(&total, &totalBytes); err != nil {
		writeError(w, 500, "SEARCH_FAILED", "Unable to count results: "+err.Error())
		return
	}

	rows, err := a.DB.Query(`SELECT f.id,f.name,f.mime_type,f.size_bytes,COALESCE(f.created_at,''),COALESCE(f.updated_at,''),
		COALESCE(c.id,''),COALESCE(c.email,''),COALESCE(d.id,''),COALESCE(d.name,''),f.folder_id,COALESCE(f.starred,0),(SELECT COUNT(*) FROM split_parts sp JOIN split_files sf ON sf.id=sp.split_id WHERE sp.file_id=f.id AND sf.status='complete')
		FROM files f
		LEFT JOIN connected_accounts c ON c.id=f.connected_account_id
		LEFT JOIN folders d ON d.id=f.folder_id
		WHERE `+where+` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		writeError(w, 500, "SEARCH_FAILED", "Unable to search files: "+err.Error())
		return
	}
	defer rows.Close()

	results := []map[string]any{}
	for rows.Next() {
		var id, name, mimeType, createdAt, updatedAt, accountID, email, folderIDOut, folderName string
		var size int64
		var starred int
		var splitParts int
		var folderID sql.NullString
		if err := rows.Scan(&id, &name, &mimeType, &size, &createdAt, &updatedAt, &accountID, &email, &folderIDOut, &folderName, &folderID, &starred, &splitParts); err != nil {
			writeError(w, 500, "SEARCH_FAILED", "Unable to read results.")
			return
		}
		var folder any
		if folderID.Valid {
			folder = map[string]string{"id": folderID.String, "name": folderName}
		}
		results = append(results, map[string]any{"id": id, "name": name, "mimeType": mimeType, "sizeBytes": fmt.Sprint(size),
			"createdAt": createdAt, "updatedAt": updatedAt, "starred": starred == 1, "folder": folder,
			"splitParts": splitParts, "connectedAccount": map[string]string{"id": accountID, "email": email}})
	}

	// Facets: per-account and per-kind counts over the SAME filter set (minus their own dimension
	// would be ideal, but a single pass keeps this cheap and predictable).
	type facet struct {
		Key   string `json:"key"`
		Label string `json:"label"`
		Count int    `json:"count"`
	}
	accountFacets := []facet{}
	arows, err := a.DB.Query(`SELECT c.id,COALESCE(c.email,''),COUNT(*) FROM files f LEFT JOIN connected_accounts c ON c.id=f.connected_account_id WHERE `+where+` GROUP BY c.id ORDER BY COUNT(*) DESC`, args...)
	if err == nil {
		defer arows.Close()
		for arows.Next() {
			var f2 facet
			if arows.Scan(&f2.Key, &f2.Label, &f2.Count) == nil {
				accountFacets = append(accountFacets, f2)
			}
		}
	}
	kindFacets := []facet{}
	for _, kind := range []string{"image", "video", "audio", "pdf", "doc", "archive", "gapps", "other"} {
		clause, _ := fileKindClause(kind)
		var n int
		if err := a.DB.QueryRow(`SELECT COUNT(*) FROM files f WHERE `+where+` AND `+clause, args...).Scan(&n); err == nil && n > 0 {
			kindFacets = append(kindFacets, facet{Key: kind, Label: kind, Count: n})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"files": results, "total": total, "limit": limit, "offset": offset,
		"totalBytes": fmt.Sprint(totalBytes),
		"facets":     map[string]any{"accounts": accountFacets, "kinds": kindFacets},
	})
}

func (a *App) listFiles(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	where, args, err := fileFilterClause(r, user.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	query := `SELECT f.id,f.name,f.mime_type,f.size_bytes,f.provider_file_id,f.folder_id,f.created_at,f.updated_at,c.id,c.email,c.provider,COALESCE(d.name,''),COALESCE(f.deleted_at,''),COALESCE(f.starred,0),COALESCE(f.thumbnail_link,''),(SELECT COUNT(*) FROM split_parts sp JOIN split_files sf ON sf.id=sp.split_id WHERE sp.file_id=f.id AND sf.status='complete') FROM files f LEFT JOIN connected_accounts c ON c.id=f.connected_account_id LEFT JOIN folders d ON d.id=f.folder_id WHERE ` + where + ` ORDER BY f.created_at DESC`
	rows, err := a.DB.Query(query, args...)
	if err != nil {
		writeError(w, 500, "FILES_FAILED", "Unable to list files.")
		return
	}
	defer rows.Close()
	files := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, mimeType, providerFileID, createdAt, updatedAt, accountID, email, provider, folderName, deletedAt string
		var size int64
		var starred int
		var thumbnail string
		var splitParts int
		var folderID sql.NullString
		if err := rows.Scan(&id, &name, &mimeType, &size, &providerFileID, &folderID, &createdAt, &updatedAt, &accountID, &email, &provider, &folderName, &deletedAt, &starred, &thumbnail, &splitParts); err != nil {
			writeError(w, 500, "FILES_FAILED", "Unable to read files: "+err.Error())
			return
		}
		var folder any
		if folderID.Valid {
			folder = map[string]string{"id": folderID.String, "name": folderName}
		}
		files = append(files, map[string]any{"id": id, "name": name, "mimeType": mimeType, "sizeBytes": fmt.Sprint(size), "providerFileId": providerFileID, "folder": folder, "createdAt": createdAt, "updatedAt": updatedAt, "deletedAt": deletedAt, "starred": starred == 1, "thumbnailUrl": thumbnail, "splitParts": splitParts, "connectedAccount": map[string]string{"id": accountID, "email": email, "provider": provider}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// listGallery returns media files (images/videos) with their Drive thumbnails. The frontend loads
// thumbnails straight from Google's CDN, so PanDrive's bandwidth stays near zero.
func (a *App) listGallery(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	query := `SELECT f.id,f.name,f.mime_type,f.size_bytes,COALESCE(f.thumbnail_link,''),f.updated_at,c.id,c.email,COALESCE(f.folder_id,''),COALESCE(d.name,'')
		FROM files f LEFT JOIN connected_accounts c ON c.id=f.connected_account_id LEFT JOIN folders d ON d.id=f.folder_id
		WHERE f.user_id=? AND f.status='active' AND (f.mime_type LIKE 'image/%' OR f.mime_type LIKE 'video/%')`
	args := []any{user.ID}
	if accountID := r.URL.Query().Get("accountId"); accountID != "" {
		query += ` AND f.connected_account_id=?`
		args = append(args, accountID)
	}
	query += ` ORDER BY f.updated_at DESC LIMIT 500`
	rows, err := a.DB.Query(query, args...)
	if err != nil {
		writeError(w, 500, "GALLERY_FAILED", "Unable to read gallery.")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, mimeType, thumb, updatedAt, accountID, email, folderID, folderName string
		var size int64
		if err := rows.Scan(&id, &name, &mimeType, &size, &thumb, &updatedAt, &accountID, &email, &folderID, &folderName); err != nil {
			writeError(w, 500, "GALLERY_FAILED", "Unable to read gallery.")
			return
		}
		item := map[string]any{"id": id, "name": name, "mimeType": mimeType, "sizeBytes": fmt.Sprint(size), "thumbnailUrl": thumb, "updatedAt": updatedAt, "connectedAccount": map[string]string{"id": accountID, "email": email}}
		if folderID != "" {
			item["folder"] = map[string]string{"id": folderID, "name": folderName}
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (a *App) initResumableUpload(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		FileName        string  `json:"fileName"`
		MIMEType        string  `json:"mimeType"`
		SizeBytes       string  `json:"sizeBytes"`
		FolderID        *string `json:"folderId"`
		TargetAccountID string  `json:"targetAccountId"`
	}
	if err := decodeJSON(r, &body); err != nil || strings.TrimSpace(body.FileName) == "" {
		writeError(w, 400, "BAD_REQUEST", "File name is required.")
		return
	}
	var size int64
	_, err := fmt.Sscan(body.SizeBytes, &size)
	if err != nil || size <= 0 {
		writeError(w, 400, "BAD_REQUEST", "sizeBytes must be a positive integer.")
		return
	}
	mime := body.MIMEType
	if mime == "" {
		mime = "application/octet-stream"
	}
	account, err := a.selectAccountForUpload(user.ID, size, body.TargetAccountID)
	if err == sql.ErrNoRows {
		writeError(w, 400, "NO_ACCOUNT_WITH_ENOUGH_SPACE", "No connected Drive account has enough space.")
		return
	}
	if err != nil {
		writeError(w, 500, "UPLOAD_TARGET_FAILED", "Unable to select upload account.")
		return
	}
	var encryptedToken string
	err = a.DB.QueryRow(`SELECT access_token_encrypted FROM connected_accounts WHERE id=?`, account).Scan(&encryptedToken)
	if err != nil {
		writeError(w, 500, "UPLOAD_INIT_FAILED", "Unable to load Drive account.")
		return
	}
	accessToken, err := a.decrypt(encryptedToken)
	if err != nil {
		writeError(w, 500, "UPLOAD_INIT_FAILED", "Unable to read Drive token.")
		return
	}
	metadata, _ := json.Marshal(map[string]string{"name": body.FileName, "mimeType": mime})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.GoogleUploadAPIURL+`?uploadType=resumable`, strings.NewReader(string(metadata)))
	if err != nil {
		writeError(w, 500, "UPLOAD_INIT_FAILED", "Unable to create Google upload.")
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Upload-Content-Type", mime)
	req.Header.Set("X-Upload-Content-Length", fmt.Sprint(size))
	response, err := a.HTTPClient.Do(req)
	if err != nil {
		writeError(w, 502, "GOOGLE_UNAVAILABLE", "Google upload init failed.")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		writeError(w, 502, "GOOGLE_UPLOAD_INIT_FAILED", "Google rejected upload init.")
		return
	}
	googleSession := response.Header.Get("Location")
	if googleSession == "" {
		writeError(w, 502, "GOOGLE_UPLOAD_INIT_FAILED", "Google did not return upload session.")
		return
	}
	sessionID := randomID()
	_, err = a.DB.Exec(`INSERT INTO upload_sessions (id,user_id,target_connected_account_id,folder_id,file_name,mime_type,size_bytes,status,google_session_uri) VALUES (?,?,?,?,?,?,?,?,?)`, sessionID, user.ID, account, body.FolderID, body.FileName, mime, size, "uploading", a.encrypt(googleSession))
	if err != nil {
		writeError(w, 500, "UPLOAD_INIT_FAILED", "Unable to save upload session.")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"sessionId": sessionID, "provider": "google_drive"})
}

func (a *App) resumableStatus(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var status, accountID, encryptedURI string
	var size int64
	err := a.DB.QueryRow(`SELECT status,target_connected_account_id,google_session_uri,size_bytes FROM upload_sessions WHERE id=? AND user_id=?`, r.PathValue("id"), user.ID).Scan(&status, &accountID, &encryptedURI, &size)
	if err == sql.ErrNoRows {
		writeError(w, 404, "UPLOAD_NOT_FOUND", "Upload session not found.")
		return
	}
	if err != nil {
		writeError(w, 500, "UPLOAD_STATUS_FAILED", "Unable to read upload session.")
		return
	}
	if status == "completed" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "completed", "offset": fmt.Sprint(size)})
		return
	}
	var encryptedToken string
	if err := a.DB.QueryRow(`SELECT access_token_encrypted FROM connected_accounts WHERE id=? AND user_id=?`, accountID, user.ID).Scan(&encryptedToken); err != nil {
		writeError(w, 500, "UPLOAD_STATUS_FAILED", "Unable to load Drive account.")
		return
	}
	uri, err := a.decrypt(encryptedURI)
	if err != nil {
		writeError(w, 500, "UPLOAD_STATUS_FAILED", "Unable to read Google upload session.")
		return
	}
	accessToken, err := a.decrypt(encryptedToken)
	if err != nil {
		writeError(w, 500, "UPLOAD_STATUS_FAILED", "Unable to read Drive token.")
		return
	}
	probe, err := http.NewRequestWithContext(r.Context(), http.MethodPut, uri, nil)
	if err != nil {
		writeError(w, 500, "UPLOAD_STATUS_FAILED", "Unable to create Google upload probe.")
		return
	}
	probe.Header.Set("Authorization", "Bearer "+accessToken)
	probe.Header.Set("Content-Length", "0")
	probe.Header.Set("Content-Range", "bytes */"+fmt.Sprint(size))
	response, err := a.HTTPClient.Do(probe)
	if err != nil {
		writeError(w, 502, "GOOGLE_UNAVAILABLE", "Google upload status request failed.")
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusPermanentRedirect {
		offset := nextUploadOffset(response.Header.Get("Range"))
		writeJSON(w, http.StatusOK, map[string]string{"status": "uploading", "offset": fmt.Sprint(offset)})
		return
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		writeJSON(w, http.StatusOK, map[string]string{"status": "completed", "offset": fmt.Sprint(size)})
		return
	}
	writeError(w, 502, "GOOGLE_UPLOAD_STATUS_FAILED", "Google rejected upload status request.")
}

func (a *App) resumableChunk(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	id := r.PathValue("id")
	var accountID, fileName, mimeType, encryptedURI, status string
	var size int64
	err := a.DB.QueryRow(`SELECT target_connected_account_id,file_name,mime_type,size_bytes,google_session_uri,status FROM upload_sessions WHERE id=? AND user_id=?`, id, user.ID).Scan(&accountID, &fileName, &mimeType, &size, &encryptedURI, &status)
	if err == sql.ErrNoRows {
		writeError(w, 404, "UPLOAD_NOT_FOUND", "Upload session not found.")
		return
	}
	if err != nil {
		writeError(w, 500, "UPLOAD_CHUNK_FAILED", "Unable to read upload session.")
		return
	}
	if status == "completed" {
		writeJSON(w, 200, map[string]string{"status": "completed"})
		return
	}
	uri, err := a.decrypt(encryptedURI)
	if err != nil {
		writeError(w, 500, "UPLOAD_CHUNK_FAILED", "Unable to read Google upload session.")
		return
	}
	var encryptedToken string
	err = a.DB.QueryRow(`SELECT access_token_encrypted FROM connected_accounts WHERE id=? AND user_id=?`, accountID, user.ID).Scan(&encryptedToken)
	if err != nil {
		writeError(w, 500, "UPLOAD_CHUNK_FAILED", "Unable to read Drive account.")
		return
	}
	accessToken, err := a.decrypt(encryptedToken)
	if err != nil {
		writeError(w, 500, "UPLOAD_CHUNK_FAILED", "Unable to read Drive token.")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPut, uri, http.MaxBytesReader(w, r.Body, size))
	if err != nil {
		writeError(w, 500, "UPLOAD_CHUNK_FAILED", "Unable to create Google chunk request.")
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("Content-Range", r.Header.Get("Content-Range"))
	if length := r.Header.Get("Content-Length"); length != "" {
		req.Header.Set("Content-Length", length)
	}
	response, err := a.HTTPClient.Do(req)
	if err != nil {
		writeError(w, 502, "GOOGLE_UNAVAILABLE", "Google upload chunk failed.")
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusPermanentRedirect {
		offset := nextUploadOffset(response.Header.Get("Range"))
		writeJSON(w, 200, map[string]string{"status": "uploading", "offset": fmt.Sprint(offset)})
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		writeError(w, 502, "GOOGLE_UPLOAD_FAILED", "Google rejected upload chunk.")
		return
	}
	var uploaded struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		MIMEType string `json:"mimeType"`
		Size     string `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&uploaded); err != nil || uploaded.ID == "" {
		writeError(w, 502, "GOOGLE_UPLOAD_FAILED", "Invalid Google upload response.")
		return
	}
	var splitID sql.NullString
	_ = a.DB.QueryRow(`SELECT split_id FROM upload_sessions WHERE id=?`, id).Scan(&splitID)
	if splitID.Valid && splitID.String != "" {
		// Split part: record the physical file and link it to its part slot.
		if _, err := a.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, randomID(), user.ID, accountID, "google_drive", uploaded.ID, uploaded.Name, uploaded.MIMEType, size); err != nil {
			writeError(w, 500, "UPLOAD_CHUNK_FAILED", "Unable to save uploaded part.")
			return
		}
		var partFileID string
		if err := a.DB.QueryRow(`SELECT id FROM files WHERE user_id=? AND provider_file_id=?`, user.ID, uploaded.ID).Scan(&partFileID); err == nil {
			_, _ = a.DB.Exec(`INSERT INTO split_parts (id,split_id,part_index,file_id,connected_account_id,size_bytes) VALUES (?,?,?,?,?,?)`, randomID(), splitID.String, a.splitPartIndex(id), partFileID, accountID, size)
		}
		// Completed when every part row exists.
		var want, have int
		_ = a.DB.QueryRow(`SELECT part_count FROM split_files WHERE id=?`, splitID.String).Scan(&want)
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM split_parts WHERE split_id=?`, splitID.String).Scan(&have)
		if want > 0 && have == want {
			_, _ = a.DB.Exec(`UPDATE split_files SET status='complete' WHERE id=?`, splitID.String)
			a.logActivity(r, user.ID, accountID, "split_upload_done", "split", splitID.String, fileName, size, "All parts uploaded")
			a.notify(user.ID, "Upload ter-split selesai", fmt.Sprintf("%s lengkap (%d part).", fileName, want), "white_check_mark", "default")
		}
		_, _ = a.DB.Exec(`UPDATE upload_sessions SET status='completed',completed_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), id)
		a.logActivity(r, user.ID, accountID, "file_upload", "file", uploaded.ID, uploaded.Name, size, "Uploaded split part")
		writeJSON(w, 200, map[string]string{"status": "completed"})
		return
	}
	_, err = a.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, randomID(), user.ID, accountID, "google_drive", uploaded.ID, uploaded.Name, uploaded.MIMEType, size)
	if err != nil {
		writeError(w, 500, "UPLOAD_CHUNK_FAILED", "Unable to save uploaded file.")
		return
	}
	// Approximate local quota update; corrected at next quota sync.
	_, _ = a.DB.Exec(`UPDATE storage_accounts SET available_bytes=MAX(0, available_bytes-?), used_bytes=used_bytes+? WHERE connected_account_id=?`, size, size, accountID)
	_, _ = a.DB.Exec(`UPDATE upload_sessions SET status='completed',completed_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), id)
	a.logActivity(r, user.ID, accountID, "file_upload", "file", uploaded.ID, uploaded.Name, size, "Uploaded to Drive")
	a.notify(user.ID, "Upload selesai", uploaded.Name+" berhasil diupload.", "white_check_mark", "default")
	writeJSON(w, 200, map[string]string{"status": "completed"})
}

// splitPartIndex derives the 1-based part index from a part session's file name
// (pd-split-<splitID>.partNNN). Sessions are recorded in split order, so the
// fallback is row order among the split's sessions.
func (a *App) splitPartIndex(sessionID string) int {
	var fileName string
	_ = a.DB.QueryRow(`SELECT file_name FROM upload_sessions WHERE id=?`, sessionID).Scan(&fileName)
	if i := strings.LastIndex(fileName, ".part"); i >= 0 {
		var n int
		if _, err := fmt.Sscan(fileName[i+5:], &n); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func nextUploadOffset(rangeValue string) int64 {
	parts := strings.Split(rangeValue, "-")
	if len(parts) != 2 {
		return 0
	}
	var last int64
	if _, err := fmt.Sscan(parts[1], &last); err != nil {
		return 0
	}
	return last + 1
}

func (a *App) selectAccountForUpload(userID string, size int64, target string) (string, error) {
	query := `SELECT c.id FROM connected_accounts c JOIN storage_accounts s ON s.connected_account_id=c.id WHERE c.user_id=? AND c.provider='google_drive' AND c.status='connected' AND s.available_bytes>=?`
	args := []any{userID, size}
	if target != "" {
		query += ` AND c.id=?`
		args = append(args, target)
	}
	query += ` ORDER BY s.available_bytes DESC LIMIT 1`
	var id string
	err := a.DB.QueryRow(query, args...).Scan(&id)
	return id, err
}

func (a *App) selectUploadTarget(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		SizeBytes string `json:"sizeBytes"`
		AccountID string `json:"accountId"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "BAD_REQUEST", "Invalid upload target request.")
		return
	}
	var size int64
	_, err := fmt.Sscan(body.SizeBytes, &size)
	if err != nil || size <= 0 {
		writeError(w, 400, "BAD_REQUEST", "sizeBytes must be a positive integer.")
		return
	}
	query := `SELECT c.id,c.email,COALESCE(s.available_bytes,0) FROM connected_accounts c JOIN storage_accounts s ON s.connected_account_id=c.id WHERE c.user_id=? AND c.provider='google_drive' AND c.status='connected' AND s.available_bytes>=?`
	args := []any{user.ID, size}
	if body.AccountID != "" {
		query += ` AND c.id=?`
		args = append(args, body.AccountID)
	}
	query += ` ORDER BY s.available_bytes DESC LIMIT 1`
	var accountID, email string
	var available int64
	err = a.DB.QueryRow(query, args...).Scan(&accountID, &email, &available)
	if err == sql.ErrNoRows {
		writeError(w, 400, "NO_ACCOUNT_WITH_ENOUGH_SPACE", "No connected Drive account has enough space.")
		return
	}
	if err != nil {
		writeError(w, 500, "UPLOAD_TARGET_FAILED", "Unable to select upload account.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"accountId": accountID, "email": email, "availableBytes": fmt.Sprint(available)})
}

// streamSplitDownload concatenates the split's parts (ordered by part_index) into
// a single response body. Bytes pass through the server (this path cannot redirect
// to Google because no single account holds the whole file).
// streamSplitDownload concatenates the split's parts (ordered by part_index) into one
// response. With a Range header it serves exactly the requested window: parts outside
// the window are skipped, boundary parts get an adjusted Range against Google. This is
// what makes both resume-after-disconnect and video seek work on split files.
func (a *App) streamSplitDownload(w http.ResponseWriter, r *http.Request, userID, splitID string) {
	rows, err := a.DB.Query(`SELECT sp.part_index, sp.size_bytes, f.provider_file_id, f.mime_type, sp.connected_account_id, sf.name, sf.mime_type, sf.size_bytes
		FROM split_parts sp
		JOIN files f ON f.id=sp.file_id
		JOIN split_files sf ON sf.id=sp.split_id
		WHERE sp.split_id=? AND sf.user_id=? AND sf.status='complete'
		ORDER BY sp.part_index`, splitID, userID)
	if err != nil {
		writeError(w, 500, "DOWNLOAD_FAILED", "Unable to read split parts.")
		return
	}
	type splitPart struct {
		index      int
		size       int64
		providerID string
		mime       string
		accountID  string
	}
	var parts []splitPart
	var logicalName, logicalMime string
	var logicalSize int64
	for rows.Next() {
		var p splitPart
		if err := rows.Scan(&p.index, &p.size, &p.providerID, &p.mime, &p.accountID, &logicalName, &logicalMime, &logicalSize); err != nil {
			rows.Close()
			writeError(w, 500, "DOWNLOAD_FAILED", "Unable to read split parts.")
			return
		}
		parts = append(parts, p)
	}
	rows.Close()
	if len(parts) == 0 {
		writeError(w, http.StatusNotFound, "FILE_NOT_FOUND", "Split has no downloadable parts.")
		return
	}
	var total int64
	for _, p := range parts {
		total += p.size
	}
	if total != logicalSize {
		writeError(w, http.StatusConflict, "SPLIT_INCONSISTENT", "Split parts no longer match the logical file size.")
		return
	}

	// Window: full file by default, or the byte range the client asked for.
	rangeStart, rangeEnd := int64(0), logicalSize-1
	rangeRequested := false
	if spec := r.Header.Get("Range"); spec != "" {
		if rs, re, ok := parseByteRange(spec, logicalSize); ok {
			rangeStart, rangeEnd = rs, re
			rangeRequested = true
		}
	}
	windowLen := rangeEnd - rangeStart + 1

	w.Header().Set("Content-Type", logicalMime)
	disposition := `attachment; filename="` + strings.ReplaceAll(logicalName, `"`, "'") + `"`
	if r.Header.Get("X-PanDrive-Inline") == "1" {
		disposition = `inline; filename="` + strings.ReplaceAll(logicalName, `"`, "'") + `"`
	}
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Split-Parts", fmt.Sprint(len(parts)))
	w.Header().Set("Content-Length", fmt.Sprint(windowLen))
	if rangeRequested {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rangeStart, rangeEnd, logicalSize))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	var offset int64 // start offset of the current part inside the logical file
	for _, p := range parts {
		partStart, partEnd := offset, offset+p.size-1
		offset += p.size
		// Part entirely outside the window: skip.
		if partEnd < rangeStart || partStart > rangeEnd {
			continue
		}
		// Intersection of window with this part.
		wantStart := rangeStart
		if partStart > wantStart {
			wantStart = partStart
		}
		wantEnd := rangeEnd
		if partEnd < wantEnd {
			wantEnd = partEnd
		}
		copyLen := wantEnd - wantStart + 1

		select {
		case <-r.Context().Done():
			return
		default:
		}
		accessToken, err := a.getGoogleToken(r.Context(), p.accountID, false)
		if err != nil {
			return
		}
		fetchURL := a.GoogleDriveAPIURL + `/files/` + url.PathEscape(p.providerID) + `?alt=media`
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, fetchURL, nil)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		// Partial part: forward the window to Google so we never pull unneeded bytes.
		if copyLen != p.size {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", wantStart-partStart, wantEnd-partStart))
		}
		response, err := a.HTTPClient.Do(req)
		if err == nil && response.StatusCode == http.StatusUnauthorized {
			response.Body.Close()
			if accessToken, err = a.getGoogleToken(r.Context(), p.accountID, true); err == nil {
				req, _ = http.NewRequestWithContext(r.Context(), http.MethodGet, fetchURL, nil)
				req.Header.Set("Authorization", "Bearer "+accessToken)
				if copyLen != p.size {
					req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", wantStart-partStart, wantEnd-partStart))
				}
				response, err = a.HTTPClient.Do(req)
			}
		}
		if err != nil || response == nil || (response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent) {
			if response != nil {
				response.Body.Close()
			}
			return
		}
		_, _ = io.CopyN(w, response.Body, copyLen)
		response.Body.Close()
	}
}

// parseByteRange parses "bytes=A-B" (also open-ended "bytes=A-") against size.
// Suffix ranges ("bytes=-N") are honored from the tail. Returns ok=false when the
// header is unsatisfiable or malformed (caller then serves the full file).
func parseByteRange(spec string, size int64) (start, end int64, ok bool) {
	spec = strings.TrimPrefix(spec, "bytes=")
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	first, second := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])
	if first == "" && second == "" {
		return 0, 0, false
	}
	if first == "" {
		// suffix: last N bytes
		var n int64
		if _, err := fmt.Sscan(second, &n); err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	if _, err := fmt.Sscan(first, &start); err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end = size - 1
	if second != "" {
		if _, err := fmt.Sscan(second, &end); err != nil || end < start {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true
}

// streamFile is downloadFile's inline twin (no attachment header) for players.
func (a *App) streamFile(w http.ResponseWriter, r *http.Request) {
	r.Header.Set("X-PanDrive-Inline", "1")
	a.downloadFile(w, r)
}

func (a *App) downloadFile(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	fileID := r.PathValue("id")
	// Split file? Stream all parts in order into one response (single byte seam).
	var splitID string
	var splitStatus string
	err := a.DB.QueryRow(`SELECT sf.id, sf.status FROM split_files sf JOIN split_parts sp ON sp.split_id=sf.id WHERE sp.file_id=? AND sf.user_id=?`, fileID, user.ID).Scan(&splitID, &splitStatus)
	if err == nil && splitStatus == "complete" {
		a.streamSplitDownload(w, r, user.ID, splitID)
		return
	}
	var providerFileID, name, mimeType, accountID string
	err = a.DB.QueryRow(`SELECT f.provider_file_id,f.name,f.mime_type,c.id FROM files f JOIN connected_accounts c ON c.id=f.connected_account_id WHERE f.id=? AND f.user_id=? AND f.status='active' AND c.provider='google_drive'`, fileID, user.ID).Scan(&providerFileID, &name, &mimeType, &accountID)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "FILE_NOT_FOUND", "File not found.")
		return
	}
	if err != nil {
		writeError(w, 500, "DOWNLOAD_FAILED", "Unable to load file.")
		return
	}
	// Non-split file: hand the browser Google's direct link (zero server bandwidth).
	// The proxy path below stays as fallback for accounts where the link is missing.
	if r.URL.Query().Get("proxy") != "1" {
		fallback := "https://drive.google.com/uc?export=download&id=" + providerFileID
		if link := a.driveFileLink(r.Context(), accountID, providerFileID, "webContentLink", fallback); link != "" {
			http.Redirect(w, r, link, http.StatusFound)
			return
		}
	}
	accessToken, err := a.getGoogleToken(r.Context(), accountID, false)
	if err != nil {
		writeError(w, 500, "DOWNLOAD_FAILED", "Unable to read Drive token.")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.GoogleDriveAPIURL+`/files/`+url.PathEscape(providerFileID)+`?alt=media`, nil)
	if err != nil {
		writeError(w, 500, "DOWNLOAD_FAILED", "Unable to create Drive request.")
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	response, err := a.HTTPClient.Do(req)

	if err == nil && response.StatusCode == http.StatusUnauthorized {
		if response != nil {
			response.Body.Close()
		}
		accessToken, err = a.getGoogleToken(r.Context(), accountID, true)
		if err == nil {
			req, _ = http.NewRequestWithContext(r.Context(), http.MethodGet, a.GoogleDriveAPIURL+`/files/`+url.PathEscape(providerFileID)+`?alt=media`, nil)
			req.Header.Set("Authorization", "Bearer "+accessToken)
			if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
				req.Header.Set("Range", rangeHeader)
			}
			response, err = a.HTTPClient.Do(req)
		}
	}

	if err != nil {
		writeError(w, 502, "GOOGLE_UNAVAILABLE", "Google Drive download failed.")
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		writeError(w, response.StatusCode, "GOOGLE_DOWNLOAD_FAILED", "Google Drive rejected download.")
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "'")+`"`)
	for _, header := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := response.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

var (
	// never split below splitMinSize bytes: overhead not worth it
	splitMinSize = int64(100 << 20)
	// keep this much headroom per account
	splitBufferBytes = int64(128 << 20)
)

type partPlan struct {
	AccountID string
	Size      int64
}

// planSplit divides a file size across connected accounts by their ACTUAL free space.
// Accounts get parts proportional to available bytes; each account keeps a small buffer.
// Returns nil when no split is possible (callers fall back to the single-account path).
func (a *App) planSplit(userID string, size int64) ([]partPlan, error) {
	if size < splitMinSize {
		return nil, nil
	}
	rows, err := a.DB.Query(`SELECT c.id, COALESCE(s.available_bytes,0) FROM connected_accounts c
		JOIN storage_accounts s ON s.connected_account_id=c.id
		WHERE c.user_id=? AND c.provider='google_drive' AND c.status='connected'
		ORDER BY s.available_bytes DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type acct struct {
		id    string
		avail int64
	}
	var accts []acct
	for rows.Next() {
		var ac acct
		if rows.Scan(&ac.id, &ac.avail) == nil && ac.avail > splitBufferBytes {
			accts = append(accts, ac)
		}
	}
	if len(accts) < 2 {
		return nil, nil
	}
	var total int64
	for _, ac := range accts {
		total += ac.avail - splitBufferBytes
	}
	if total < size {
		return nil, nil
	}
	// Greedy largest-first: fill the account with the most headroom, then the next,
	// until the whole file is placed. Keeps parts few and allocation deterministic.
	plans := make([]partPlan, 0, len(accts))
	var assigned int64
	for _, ac := range accts {
		if assigned >= size {
			break
		}
		capacity := ac.avail - splitBufferBytes
		share := size - assigned
		if share > capacity {
			share = capacity
		}
		if share <= 0 {
			continue
		}
		assigned += share
		plans = append(plans, partPlan{AccountID: ac.id, Size: share})
	}
	if assigned < size {
		return nil, nil
	}
	// Drop zero-size plans and re-index.
	out := plans[:0]
	for _, pl := range plans {
		if pl.Size > 0 {
			out = append(out, pl)
		}
	}
	if len(out) < 2 {
		return nil, nil
	}
	return out, nil
}

// initSplitUpload plans a multi-account split, opens one resumable session per part,
// and returns the ordered part list. The browser then streams each part with the
// existing /uploads/resumable/chunk endpoint (chunk routing is by session id).
func (a *App) initSplitUpload(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		FileName  string  `json:"fileName"`
		MIMEType  string  `json:"mimeType"`
		SizeBytes string  `json:"sizeBytes"`
		FolderID  *string `json:"folderId"`
	}
	if err := decodeJSON(r, &body); err != nil || strings.TrimSpace(body.FileName) == "" {
		writeError(w, 400, "BAD_REQUEST", "File name is required.")
		return
	}
	var size int64
	_, err := fmt.Sscan(body.SizeBytes, &size)
	if err != nil || size <= 0 {
		writeError(w, 400, "BAD_REQUEST", "sizeBytes must be a positive integer.")
		return
	}
	mime := body.MIMEType
	if mime == "" {
		mime = "application/octet-stream"
	}
	plans, err := a.planSplit(user.ID, size)
	if err != nil {
		writeError(w, 500, "SPLIT_PLAN_FAILED", "Unable to plan split upload.")
		return
	}
	if plans == nil {
		writeError(w, 400, "NO_SPLIT_POSSIBLE", "No account has enough free space and a split would not fit either (after safety buffers).")
		return
	}
	// Register the logical file first so part sessions can reference it.
	splitID := randomID()
	if _, err := a.DB.Exec(`INSERT INTO split_files (id,user_id,name,mime_type,size_bytes,part_count,status) VALUES (?,?,?,?,?,?, 'incomplete')`, splitID, user.ID, body.FileName, mime, size, len(plans)); err != nil {
		writeError(w, 500, "SPLIT_INIT_FAILED", "Unable to record split file.")
		return
	}
	type partOut struct {
		Index        string `json:"index"`
		Size         string `json:"sizeBytes"`
		SessionID    string `json:"sessionId"`
		AccountID    string `json:"accountId"`
		AccountEmail string `json:"accountEmail"`
	}
	parts := make([]partOut, 0, len(plans))
	for i, pl := range plans {
		var encryptedToken string
		err := a.DB.QueryRow(`SELECT access_token_encrypted FROM connected_accounts WHERE id=?`, pl.AccountID).Scan(&encryptedToken)
		if err != nil {
			a.cleanupIncompleteSplit(user.ID, splitID)
			writeError(w, 500, "SPLIT_INIT_FAILED", "Unable to load Drive account for part "+fmt.Sprint(i+1)+".")
			return
		}
		accessToken, err := a.decrypt(encryptedToken)
		if err != nil {
			a.cleanupIncompleteSplit(user.ID, splitID)
			writeError(w, 500, "SPLIT_INIT_FAILED", "Unable to read Drive token.")
			return
		}
		partName := fmt.Sprintf("pd-split-%s.part%03d", splitID, i+1)
		metadata, _ := json.Marshal(map[string]string{"name": partName, "mimeType": mime})
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.GoogleUploadAPIURL+`?uploadType=resumable`, strings.NewReader(string(metadata)))
		if err != nil {
			a.cleanupIncompleteSplit(user.ID, splitID)
			writeError(w, 500, "SPLIT_INIT_FAILED", "Unable to create Google upload.")
			return
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		req.Header.Set("X-Upload-Content-Type", mime)
		req.Header.Set("X-Upload-Content-Length", fmt.Sprint(pl.Size))
		response, err := a.HTTPClient.Do(req)
		if err == nil {
			defer response.Body.Close()
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				err = fmt.Errorf("google status %d", response.StatusCode)
			}
		}
		if err != nil {
			a.cleanupIncompleteSplit(user.ID, splitID)
			writeError(w, 502, "GOOGLE_UNAVAILABLE", "Google rejected part "+fmt.Sprint(i+1)+" init.")
			return
		}
		googleSession := response.Header.Get("Location")
		if googleSession == "" {
			a.cleanupIncompleteSplit(user.ID, splitID)
			writeError(w, 502, "GOOGLE_UPLOAD_INIT_FAILED", "Google did not return a session for part "+fmt.Sprint(i+1)+".")
			return
		}
		sessionID := randomID()
		if _, err := a.DB.Exec(`INSERT INTO upload_sessions (id,user_id,target_connected_account_id,folder_id,file_name,mime_type,size_bytes,status,google_session_uri,split_id) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			sessionID, user.ID, pl.AccountID, body.FolderID, partName, mime, pl.Size, "uploading", a.encrypt(googleSession), splitID); err != nil {
			a.cleanupIncompleteSplit(user.ID, splitID)
			writeError(w, 500, "SPLIT_INIT_FAILED", "Unable to record part session.")
			return
		}
		var email string
		_ = a.DB.QueryRow(`SELECT email FROM connected_accounts WHERE id=?`, pl.AccountID).Scan(&email)
		parts = append(parts, partOut{Index: fmt.Sprint(i + 1), Size: fmt.Sprint(pl.Size), SessionID: sessionID, AccountID: pl.AccountID, AccountEmail: email})
	}
	a.logActivity(r, user.ID, "", "split_upload_init", "split", splitID, body.FileName, size, fmt.Sprintf("Split into %d parts", len(plans)))
	writeJSON(w, http.StatusOK, map[string]any{"splitId": splitID, "status": "incomplete", "parts": parts})
}

// cleanupIncompleteSplit removes the logical row (sessions are garbage collected on next status check).
func (a *App) cleanupIncompleteSplit(userID, splitID string) {
	_, _ = a.DB.Exec(`DELETE FROM split_files WHERE id=? AND user_id=? AND status='incomplete'`, splitID, userID)
}

func (a *App) syncAccountFiles(ctx context.Context, userID, accountID string) (created, updated int, err error) {
	accessToken, err := a.getGoogleToken(ctx, accountID, false)
	if err != nil {
		return 0, 0, err
	}
	type driveItem struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		MIMEType string   `json:"mimeType"`
		Size     string   `json:"size"`
		Created  string   `json:"createdTime"`
		Modified string   `json:"modifiedTime"`
		Trashed  bool     `json:"trashed"`
		Parents  []string `json:"parents"`
		Thumb    string   `json:"thumbnailLink"`
	}
	// Paginated listing: loop through all pages via nextPageToken (Google caps 1000/page).
	var items []driveItem
	seen := map[string]bool{}
	pageToken := ""
	for {
		listURL := a.GoogleDriveAPIURL + `/files?pageSize=1000&orderBy=modifiedTime%20desc&fields=nextPageToken,files(id,name,mimeType,size,createdTime,modifiedTime,trashed,parents,thumbnailLink)`
		if pageToken != "" {
			listURL += `&pageToken=` + url.QueryEscape(pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
		if err != nil {
			return created, updated, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		response, err := a.HTTPClient.Do(req)
		if err != nil {
			return created, updated, err
		}
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			response.Body.Close()
			return created, updated, fmt.Errorf("google drive listing rejected (%d): %s", response.StatusCode, string(body))
		}
		var payload struct {
			NextPageToken string      `json:"nextPageToken"`
			Files         []driveItem `json:"files"`
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&payload)
		response.Body.Close()
		if err != nil {
			return created, updated, err
		}
		for _, item := range payload.Files {
			if item.ID == "" || item.Trashed {
				continue
			}
			seen[item.ID] = true
			items = append(items, item)
		}
		if payload.NextPageToken == "" {
			break
		}
		pageToken = payload.NextPageToken
	}

	// Pass 1: upsert folders (Drive folder hierarchy -> local folders table).
	// Pass 2: link parent_id (parents may appear in any order in the listing).
	folderIDByProvider := map[string]string{} // provider folder id -> local folder id
	isFolder := func(mime string) bool { return mime == "application/vnd.google-apps.folder" }
	for _, item := range items {
		if !isFolder(item.MIMEType) {
			continue
		}
		var existing string
		errDB := a.DB.QueryRow(`SELECT id FROM folders WHERE user_id=? AND connected_account_id=? AND provider_folder_id=?`, userID, accountID, item.ID).Scan(&existing)
		if errDB == sql.ErrNoRows {
			local := randomID()
			_, err = a.DB.Exec(`INSERT INTO folders (id,user_id,connected_account_id,provider,provider_folder_id,name,deleted_at) VALUES (?,?,?,?,?,?,NULL)`, local, userID, accountID, "google_drive", item.ID, item.Name)
			if err == nil {
				folderIDByProvider[item.ID] = local
			} else {
				log.Printf("sync: folder insert failed for %s (%s): %v", item.Name, item.ID, err)
			}
		} else {
			folderIDByProvider[item.ID] = existing
			_, _ = a.DB.Exec(`UPDATE folders SET name=?, deleted_at=NULL, updated_at=? WHERE id=?`, item.Name, item.Modified, existing)
		}
	}
	for _, item := range items {
		if !isFolder(item.MIMEType) || len(item.Parents) == 0 {
			continue
		}
		local, ok := folderIDByProvider[item.ID]
		if !ok {
			continue
		}
		parentLocal := ""
		if p, ok := folderIDByProvider[item.Parents[0]]; ok {
			parentLocal = p
		}
		if parentLocal != "" {
			_, _ = a.DB.Exec(`UPDATE folders SET parent_id=? WHERE id=?`, parentLocal, local)
		}
	}

	// Pass 3: files - upsert metadata and attach to their mirrored folder.
	for _, item := range items {
		if isFolder(item.MIMEType) {
			continue
		}
		var size int64
		_, _ = fmt.Sscan(item.Size, &size)
		folderLocal := ""
		if len(item.Parents) > 0 {
			folderLocal = folderIDByProvider[item.Parents[0]]
		}
		var exists int
		_ = a.DB.QueryRow(`SELECT 1 FROM files WHERE user_id=? AND connected_account_id=? AND provider_file_id=?`, userID, accountID, item.ID).Scan(&exists)
		if exists == 1 {
			_, err = a.DB.Exec(`UPDATE files SET name=?,mime_type=?,size_bytes=?,folder_id=?,updated_at=?,thumbnail_link=? WHERE user_id=? AND connected_account_id=? AND provider_file_id=?`, item.Name, item.MIMEType, size, nullIfEmpty(folderLocal), item.Modified, item.Thumb, userID, accountID, item.ID)
			if err == nil {
				updated++
			}
		} else {
			_, err = a.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes,folder_id,created_at,updated_at,thumbnail_link) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, randomID(), userID, accountID, "google_drive", item.ID, item.Name, item.MIMEType, size, nullIfEmpty(folderLocal), item.Created, item.Modified, item.Thumb)
			if err == nil {
				created++
			}
		}
	}

	// Trash handling: files/folders in DB that Google no longer lists are gone. Soft-delete them.
	rows, err := a.DB.Query(`SELECT provider_file_id FROM files WHERE user_id=? AND connected_account_id=? AND status='active' AND provider='google_drive'`, userID, accountID)
	if err != nil {
		return created, updated, err
	}
	var missing []string
	for rows.Next() {
		var pfid string
		if rows.Scan(&pfid) == nil && pfid != "" && !seen[pfid] {
			missing = append(missing, pfid)
		}
	}
	rows.Close()
	if len(missing) > 0 {
		placeholders := strings.Repeat("?,", len(missing))
		placeholders = placeholders[:len(placeholders)-1]
		args := []any{time.Now().UTC().Format(time.RFC3339Nano), userID, accountID}
		for _, pfid := range missing {
			args = append(args, pfid)
		}
		res, err := a.DB.Exec(`UPDATE files SET status='deleted', deleted_at=? WHERE user_id=? AND connected_account_id=? AND status='active' AND provider_file_id IN (`+placeholders+`)`, args...)
		if err != nil {
			return created, updated, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("sync: marked %d file(s) as deleted for account %s", n, accountID)
		}
	}
	// Same for folders: gone from Drive -> soft-delete locally.
	frows, err := a.DB.Query(`SELECT provider_folder_id FROM folders WHERE user_id=? AND connected_account_id=? AND deleted_at IS NULL`, userID, accountID)
	if err != nil {
		return created, updated, err
	}
	var missingFolders []string
	for frows.Next() {
		var pfid string
		if frows.Scan(&pfid) == nil && pfid != "" && !seen[pfid] {
			missingFolders = append(missingFolders, pfid)
		}
	}
	frows.Close()
	if len(missingFolders) > 0 {
		ph := strings.Repeat("?,", len(missingFolders))
		ph = ph[:len(ph)-1]
		fargs := []any{time.Now().UTC().Format(time.RFC3339Nano), userID, accountID}
		for _, pfid := range missingFolders {
			fargs = append(fargs, pfid)
		}
		_, _ = a.DB.Exec(`UPDATE folders SET deleted_at=? WHERE user_id=? AND connected_account_id=? AND provider_folder_id IN (`+ph+`)`, fargs...)
	}
	return created, updated, nil
}

// nullIfEmpty returns nil for empty strings so nullable FK columns stay NULL.
func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func (a *App) syncGoogleFiles(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	requestedID := r.URL.Query().Get("connectedAccountId")
	query := `SELECT id FROM connected_accounts WHERE user_id=? AND provider='google_drive' AND status='connected'`
	args := []any{user.ID}
	if requestedID != "" {
		query += ` AND id=?`
		args = append(args, requestedID)
	}
	rows, err := a.DB.Query(query, args...)
	if err != nil {
		writeError(w, 500, "SYNC_FAILED", "Unable to list Drive accounts.")
		return
	}
	defer rows.Close()
	results := make([]map[string]any, 0)
	for rows.Next() {
		var accountID string
		if err := rows.Scan(&accountID); err != nil {
			writeError(w, 500, "SYNC_FAILED", "Unable to read Drive account.")
			return
		}
		created, updated, err := a.syncAccountFiles(r.Context(), user.ID, accountID)
		if err != nil {
			results = append(results, map[string]any{"accountId": accountID, "error": err.Error()})
			continue
		}
		results = append(results, map[string]any{"accountId": accountID, "created": created, "updated": updated})
	}
	a.logActivity(r, user.ID, requestedID, "sync_files", "account", requestedID, "", 0, fmt.Sprintf("Synced %d account(s)", len(results)))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "results": results})
}

func parseScopes(raw string) []string {
	var scopes []string
	_ = json.Unmarshal([]byte(raw), &scopes)
	return scopes
}

func (a *App) googleCallback(w http.ResponseWriter, r *http.Request) {
	wantsJSON := r.Header.Get("Accept") == "application/json" || r.Header.Get("Content-Type") == "application/json" || r.Header.Get("Authorization") != ""

	redirectError := func() {
		if wantsJSON {
			writeError(w, http.StatusBadRequest, "OAUTH_FAILED", "OAuth connection failed")
		} else {
			http.Redirect(w, r, a.Config.FrontendURL+"/google-connected?status=error", http.StatusFound)
		}
	}
	query := r.URL.Query()
	state, code := query.Get("state"), query.Get("code")
	if state == "" || code == "" || query.Get("error") != "" {
		redirectError()
		return
	}

	var stateID, userID, configID, expiresAt, encryptedID, encryptedSecret, redirectURI, scopes string
	err := a.DB.QueryRow(`SELECT s.id,s.user_id,s.provider_config_id,s.expires_at,p.client_id_encrypted,p.client_secret_encrypted,p.redirect_uri,p.scopes FROM oauth_states s JOIN provider_configs p ON p.id=s.provider_config_id WHERE s.state_hash=? AND s.flow='connect' AND s.used_at IS NULL`, hashToken(state)).Scan(&stateID, &userID, &configID, &expiresAt, &encryptedID, &encryptedSecret, &redirectURI, &scopes)
	if err != nil {
		redirectError()
		return
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !expires.After(time.Now()) {
		redirectError()
		return
	}
	clientID, err := a.decrypt(encryptedID)
	if err != nil {
		redirectError()
		return
	}
	clientSecret, err := a.decrypt(encryptedSecret)
	if err != nil {
		redirectError()
		return
	}

	conf := &oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURI, Endpoint: a.GoogleEndpoint, Scopes: parseScopes(scopes)}
	token, err := conf.Exchange(r.Context(), code)
	if err != nil || token.AccessToken == "" {
		log.Printf("Google OAuth token exchange failed: %v", err)
		redirectError()
		return
	}

	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.GoogleUserInfoURL, nil)
	if err != nil {
		redirectError()
		return
	}
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	response, err := a.HTTPClient.Do(request)
	if err != nil {
		log.Printf("Google userinfo request failed: %v", err)
		redirectError()
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		log.Printf("Google userinfo status: %d", response.StatusCode)
		redirectError()
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		log.Printf("Google userinfo read failed: %v", err)
		redirectError()
		return
	}
	var profile struct {
		ID      string `json:"id"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		Picture string `json:"picture"`
	}
	if err := json.Unmarshal(body, &profile); err != nil || profile.ID == "" || profile.Email == "" {
		log.Printf("Google userinfo payload invalid: %v", err)
		redirectError()
		return
	}
	refreshToken := token.RefreshToken
	if refreshToken == "" {
		_ = a.DB.QueryRow(`SELECT refresh_token_encrypted FROM connected_accounts WHERE user_id=? AND provider='google_drive' AND provider_account_id=?`, userID, profile.ID).Scan(&refreshToken)
		if refreshToken != "" {
			refreshToken, _ = a.decrypt(refreshToken)
		}
	}
	if refreshToken == "" {
		redirectError()
		return
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	expiresToken := token.Expiry.UTC().Format(time.RFC3339Nano)
	accountID := randomID()
	_, err = a.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider_config_id,provider,provider_account_id,email,display_name,avatar_url,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes,status,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,'connected',?) ON CONFLICT(user_id,provider,provider_account_id) DO UPDATE SET provider_config_id=excluded.provider_config_id,email=excluded.email,display_name=excluded.display_name,avatar_url=excluded.avatar_url,access_token_encrypted=excluded.access_token_encrypted,refresh_token_encrypted=excluded.refresh_token_encrypted,token_expires_at=excluded.token_expires_at,scopes=excluded.scopes,status='connected',updated_at=excluded.updated_at`, accountID, userID, configID, "google_drive", profile.ID, profile.Email, profile.Name, profile.Picture, a.encrypt(token.AccessToken), a.encrypt(refreshToken), expiresToken, scopes, now)
	if err != nil {
		log.Printf("Google account persistence failed: %v", err)
		redirectError()
		return
	}
	_, _ = a.DB.Exec(`UPDATE oauth_states SET used_at=? WHERE id=?`, now, stateID)
	// Auto-sync quota + file metadata so the new account shows storage and files immediately.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := a.syncAccountQuota(ctx, accountID); err != nil {
			log.Printf("auto quota sync after connect failed for account %s: %v", accountID, err)
		}
		if _, _, err := a.syncAccountFiles(ctx, userID, accountID); err != nil {
			log.Printf("auto file sync after connect failed for account %s: %v", accountID, err)
		}
	}()
	a.logActivity(r, userID, accountID, "account_connect", "account", accountID, profile.Email, 0, "Google Drive account connected")
	if wantsJSON {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	} else {
		http.Redirect(w, r, a.Config.FrontendURL+"/google-connected?status=success", http.StatusFound)
	}
}

func (a *App) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, r.Context().Value(userKey).(authUser))
}

// changePassword requires the current password, then rotates it and invalidates every
// existing session (other devices are logged out) while returning a fresh pair for this one.
func (a *App) changePassword(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	if body.CurrentPassword == "" {
		writeError(w, http.StatusBadRequest, "CURRENT_PASSWORD_REQUIRED", "Your current password is required.")
		return
	}
	if len(body.NewPassword) < 8 {
		writeError(w, http.StatusBadRequest, "WEAK_PASSWORD", "Password must be at least 8 characters.")
		return
	}
	if body.NewPassword == body.CurrentPassword {
		writeError(w, http.StatusBadRequest, "SAME_PASSWORD", "The new password must differ from the current one.")
		return
	}

	var storedHash string
	if err := a.DB.QueryRow(`SELECT password_hash FROM users WHERE id=? AND status='active'`, user.ID).Scan(&storedHash); err != nil {
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Account not found.")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(body.CurrentPassword)) != nil {
		a.logActivity(r, user.ID, "", "password_change_failed", "user", user.ID, user.Email, 0, "Current password did not match")
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Your current password is incorrect.")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), 10)
	if err != nil {
		writeError(w, 500, "HASH_FAILED", "Unable to update the password.")
		return
	}
	if _, err := a.DB.Exec(`UPDATE users SET password_hash=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, newHash, user.ID); err != nil {
		writeError(w, 500, "UPDATE_FAILED", "Unable to update the password.")
		return
	}
	// Every other session becomes invalid the moment the password changes.
	_, _ = a.DB.Exec(`UPDATE user_sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), user.ID)
	a.logActivity(r, user.ID, "", "password_change", "user", user.ID, user.Email, 0, "Password changed; other sessions signed out")
	a.respondSession(w, http.StatusOK, user)
}

func (a *App) updateMe(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Name == "" || body.Email == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Name and email are required.")
		return
	}
	// Password changes are deliberately NOT accepted here: without the current password this route
	// would let a stolen access token rotate the password and lock the owner out. Use /auth/change-password.
	if body.Password != "" {
		writeError(w, http.StatusBadRequest, "USE_CHANGE_PASSWORD", "Use the change-password endpoint to set a new password.")
		return
	}
	if _, err := a.DB.Exec(`UPDATE users SET name=?, email=? WHERE id=?`, body.Name, body.Email, user.ID); err != nil {
		writeError(w, http.StatusConflict, "EMAIL_IN_USE", "Email already in use.")
		return
	}
	a.logActivity(r, user.ID, "", "account_update", "user", user.ID, body.Email, 0, "Profile updated")
	user.Name = body.Name
	user.Email = body.Email
	a.respondSession(w, http.StatusOK, user)
}

func (a *App) signAccessToken(user authUser) (string, error) {
	claims := jwt.MapClaims{"sub": user.ID, "name": user.Name, "email": user.Email, "exp": time.Now().Add(15 * time.Minute).Unix()}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(a.Config.JWTSecret))
}

func (a *App) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokenString := ""
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) == 2 && parts[0] == "Bearer" {
			tokenString = parts[1]
		} else if qToken := r.URL.Query().Get("token"); qToken != "" {
			tokenString = qToken
		}

		if tokenString == "" {
			writeError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication required.")
			return
		}
		// API keys (pd_...) authenticate without a browser session: hashed lookup, last-used stamp,
		// scope check. A valid key acts as its owner for read/write endpoints (never admin ones).
		if strings.HasPrefix(tokenString, "pd_") {
			user, ok := a.userFromAPIKey(tokenString)
			if !ok {
				writeError(w, http.StatusUnauthorized, "AUTH_INVALID", "Invalid or revoked API key.")
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
			return
		}
		token, err := jwt.Parse(tokenString, func(t *jwt.Token) (any, error) {
			if t.Method != jwt.SigningMethodHS256 {
				return nil, errors.New("unexpected signing method")
			}
			return []byte(a.Config.JWTSecret), nil
		})
		if err != nil || !token.Valid {
			writeError(w, http.StatusUnauthorized, "AUTH_INVALID", "Invalid or expired session.")
			return
		}
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			writeError(w, http.StatusUnauthorized, "AUTH_INVALID", "Invalid session.")
			return
		}
		id, _ := claims.GetSubject()
		name, _ := claims["name"].(string)
		email, _ := claims["email"].(string)
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, authUser{ID: id, Name: name, Email: email})))
	}
}

// rateMeter tracks requests per second in a sliding 100-second window.
// Google allows 10k requests/100s per OAuth client project, so this is the number that
// actually matters when several accounts share configs.
type rateMeter struct {
	mu      sync.Mutex
	buckets [100]int64
	stamp   [100]int64 // unix second each bucket belongs to (-1 = empty)
	peak    int64
}

func (m *rateMeter) add(n int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sec := time.Now().Unix()
	idx := sec % 100
	if m.stamp[idx] != sec {
		m.stamp[idx] = sec
		m.buckets[idx] = 0
	}
	m.buckets[idx] += n
	if total := m.totalLocked(sec); total > m.peak {
		m.peak = total
	}
}

func (m *rateMeter) totalLocked(nowSec int64) int64 {
	var total int64
	for i := range m.buckets {
		if m.stamp[i] > nowSec-100 {
			total += m.buckets[i]
		}
	}
	return total
}

// snapshot returns the current 100s total and per-second counts oldest-first.
func (m *rateMeter) snapshot() (int64, []int64) {
	if m == nil {
		return 0, make([]int64, 100)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	nowSec := time.Now().Unix()
	history := make([]int64, 100)
	for i := 0; i < 100; i++ {
		sec := nowSec - 99 + int64(i)
		idx := sec % 100
		if m.stamp[idx] == sec {
			history[i] = m.buckets[idx]
		}
	}
	total := m.totalLocked(nowSec)
	if total > m.peak {
		m.peak = total
	}
	return total, history
}

func (m *rateMeter) peakTotal() int64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak
}

// countingTransport counts every request that leaves the process through a.HTTPClient.
// The client is only used for Google API calls, so this is the app's real Drive request rate.
type countingTransport struct {
	base  http.RoundTripper
	meter *rateMeter
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.meter.add(1)
	return t.base.RoundTrip(req)
}

// rateLimits reports the live request rate and each OAuth config's window state.
func (a *App) rateLimits(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	total, history := a.RateMeter.snapshot()

	type configState struct {
		ID             string `json:"id"`
		Label          string `json:"label"`
		Status         string `json:"status"`
		FlowStarts     int    `json:"flowStarts"`
		WindowStart    string `json:"windowStart"`
		WindowResetsIn int64  `json:"windowResetsInSeconds"`
		LastUsedAt     string `json:"lastUsedAt"`
		Accounts       int    `json:"accounts"`
	}
	configs := []configState{}
	rows, err := a.DB.Query(`SELECT p.id,COALESCE(p.label,''),p.status,COALESCE(q.request_count,0),COALESCE(q.window_start,''),COALESCE(p.last_used_at,''),
		(SELECT COUNT(*) FROM connected_accounts c WHERE c.provider_config_id=p.id)
		FROM provider_configs p LEFT JOIN provider_config_quota q ON q.provider_config_id=p.id
		WHERE p.user_id=? AND p.provider='google_drive' ORDER BY p.created_at`, user.ID)
	if err != nil {
		writeError(w, 500, "RATE_LIMITS_FAILED", "Unable to read configs: "+err.Error())
		return
	}
	defer rows.Close()
	for rows.Next() {
		var cs configState
		var windowStart string
		if err := rows.Scan(&cs.ID, &cs.Label, &cs.Status, &cs.FlowStarts, &windowStart, &cs.LastUsedAt, &cs.Accounts); err != nil {
			writeError(w, 500, "RATE_LIMITS_FAILED", "Unable to read configs.")
			return
		}
		cs.WindowStart = windowStart
		cs.WindowResetsIn = 0
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, windowStart); err == nil {
				if remaining := int64(100 - time.Since(t).Seconds()); remaining > 0 {
					cs.WindowResetsIn = remaining
				}
				break
			}
		}
		configs = append(configs, cs)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"windowSeconds":    100,
		"limit":            10000,
		"threshold":        8000,
		"requestsLast100s": total,
		"peakLast100s":     a.RateMeter.peakTotal(),
		"history":          history,
		"configs":          configs,
		"notes": []string{
			"Requests counted are Google API calls made by this server (listings, uploads, downloads, permissions).",
			"Token refreshes go through the OAuth library's own client and are not included.",
			"Per-config counters count OAuth connect flows, which is what the automatic config switch keys on.",
		},
	})
}

var (
	cspOnce   sync.Once
	cspPolicy string
)

// contentSecurityPolicy builds a policy that allows exactly what the embedded SPA needs.
// Inline script hashes are derived from the shipped index.html, so a rebuilt frontend does
// not silently break the theme bootstrap script.
func (a *App) contentSecurityPolicy() string {
	cspOnce.Do(func() {
		scriptSrc := "'self'"
		if sub, err := fs.Sub(distFS, "dist"); err == nil {
			if index, err := fs.ReadFile(sub, "index.html"); err == nil {
				for _, block := range inlineScriptBlocks(index) {
					sum := sha256.Sum256(block)
					scriptSrc += " 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
				}
			}
		}
		// Third-party origins the UI genuinely loads, discovered by grepping the frontend for
		// external URLs: iconify (folder icons), dicebear/pravatar/gravatar (avatars), googleusercontent
		// (Google account avatars), cdnjs (plyr video player), officeapps + drive (file previews).
		// A too-strict policy silently breaks all of these: every remote image renders as a broken
		// placeholder, which is how the folder icons disappeared.
		cspPolicy = strings.Join([]string{
			"default-src 'self'",
			"script-src " + scriptSrc + " https://cdnjs.cloudflare.com https://www.google.com https://www.gstatic.com",
			"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com https://cdnjs.cloudflare.com",
			"font-src 'self' data: https://fonts.gstatic.com",
			"img-src 'self' data: blob: https://api.iconify.design https://api.dicebear.com https://i.pravatar.cc https://www.gravatar.com https://*.googleusercontent.com https://*.google.com",
			"media-src 'self' data: blob: https://cdnjs.cloudflare.com",
			"frame-src 'self' https://view.officeapps.live.com https://drive.google.com https://docs.google.com https://www.google.com",
			"connect-src 'self' https://api.iconify.design",
			"object-src 'none'",
			"base-uri 'self'",
			"form-action 'self'",
			"frame-ancestors 'none'",
		}, "; ")
	})
	return cspPolicy
}

// inlineScriptBlocks returns the bodies of attribute-less <script> tags in the SPA shell.
func inlineScriptBlocks(html []byte) [][]byte {
	var blocks [][]byte
	rest := html
	for {
		start := bytes.Index(rest, []byte("<script>"))
		if start < 0 {
			return blocks
		}
		rest = rest[start+len("<script>"):]
		end := bytes.Index(rest, []byte("</script>"))
		if end < 0 {
			return blocks
		}
		blocks = append(blocks, rest[:end])
		rest = rest[end+len("</script>"):]
	}
}

func (a *App) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", a.contentSecurityPolicy())
		// Only advertise HSTS when the request really arrived over TLS (directly or via the tunnel).
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		w.Header().Set("Access-Control-Allow-Origin", a.Config.FrontendURL)
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(r *http.Request, value any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(value)
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

// idCounter guarantees uniqueness even when the system clock has coarse resolution
// (Windows timer granularity is ~1-15ms, so UnixNano alone collided on rapid inserts).
var idCounter atomic.Uint64

func randomID() string {
	return fmt.Sprintf("%d-%d-%d", time.Now().UnixNano(), os.Getpid(), idCounter.Add(1))
}

func randomToken() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes)
}

func hashToken(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (a *App) encryptionKey() []byte {
	sum := sha256.Sum256([]byte(a.Config.TokenKey))
	return sum[:]
}

func (a *App) encrypt(value string) string {
	block, err := aes.NewCipher(a.encryptionKey())
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return hex.EncodeToString(append(nonce, gcm.Seal(nil, nonce, []byte(value), nil)...))
}

func (a *App) decrypt(value string) (string, error) {
	raw, err := hex.DecodeString(value)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(a.encryptionKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("invalid encrypted value")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	return string(plain), err
}

// requestScheme detects https behind proxies/tunnels.
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	if os.Getenv("FORCE_HTTPS") == "true" {
		return "https"
	}
	return "http"
}

// forwardedHost returns the public host (X-Forwarded-Host or Host) that the client used.
func forwardedHost(r *http.Request) string {
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		return strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	if r.Host != "" && r.Host != "127.0.0.1:4000" && r.Host != "localhost:4000" {
		return r.Host
	}
	return ""
}

// runCloudflared spawns cloudflared (adjacent binary or in PATH) with the given args.
func runCloudflared(args ...string) {
	// exec.Command resolves bare names through PATH, so a sibling binary must be
	// referenced with an explicit relative/absolute path or it will never be found.
	exe := ""
	for _, cand := range []string{"./cloudflared.exe", "./cloudflared"} {
		if _, err := os.Stat(cand); err == nil {
			exe = cand
			break
		}
	}
	if exe == "" {
		if _, err := exec.LookPath("cloudflared"); err == nil {
			exe = "cloudflared"
		} else if self, err := os.Executable(); err == nil {
			dir := filepath.Dir(self)
			for _, cand := range []string{filepath.Join(dir, "cloudflared.exe"), filepath.Join(dir, "cloudflared")} {
				if _, err := os.Stat(cand); err == nil {
					exe = cand
					break
				}
			}
		}
	}
	if exe == "" {
		log.Printf("cloudflared not found (looked next to the binary and in PATH); tunnel not started")
		return
	}
	full := append([]string{exe}, args...)
	log.Printf("starting %s", strings.Join(full, " "))
	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("cloudflared exited: %v", err)
	}
}

// tunnelConfigPath returns the tunnel.yml path next to the executable.
func tunnelConfigPath() string {
	exePath, err := os.Executable()
	if err != nil {
		return "tunnel.yml"
	}
	return filepath.Join(filepath.Dir(exePath), "tunnel.yml")
}

func main() {
	// --version is what deploy/vps-update.sh reads to decide if an update is needed.
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(buildVersion)
		return
	}
	config := loadConfig()
	// Ensure the data directory exists (SQLite cannot create parent dirs).
	if dir := dataDirFromURL(config.DatabaseURL); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	db, err := sql.Open("sqlite", config.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	meter := &rateMeter{}
	app := &App{DB: db, Config: config, GoogleEndpoint: google.Endpoint, GoogleUserInfoURL: "https://www.googleapis.com/oauth2/v2/userinfo", GoogleDriveAPIURL: "https://www.googleapis.com/drive/v3", GoogleUploadAPIURL: "https://www.googleapis.com/upload/drive/v3/files", loginFails: map[string]*loginFail{}, RateMeter: meter}
	// Every Google API call made by the app flows through this client, so counting here
	// yields the real request rate without instrumenting each call site.
	app.HTTPClient = &http.Client{Transport: &countingTransport{base: http.DefaultTransport, meter: meter}, Timeout: 30 * time.Minute}
	if err := app.migrate(); err != nil {
		log.Fatal(err)
	}
	if err := app.ensureInitialAdmin(); err != nil {
		log.Fatal(err)
	}
	if err := app.bootstrapGoogleConfig(); err != nil {
		log.Fatal(err)
	}
	// Production guard: refuse default secrets once real OAuth credentials are configured.
	// Bypass with APP_ENV=development for local testing.
	if os.Getenv("APP_ENV") != "development" && config.GoogleClientID != "" {
		if config.JWTSecret == "change-this-jwt-secret-before-production" || config.TokenKey == "change-this-token-key-before-production" || len(config.TokenKey) != 32 {
			log.Fatal(" refusing to start with default JWT_ACCESS_SECRET/TOKEN_ENCRYPTION_KEY. Set 32-byte TOKEN_ENCRYPTION_KEY and a strong JWT_ACCESS_SECRET in .env")
		}
	}
	log.Printf("PanDrive %s listening on http://127.0.0.1:%s", buildVersion, config.AppPort)

	// Cloudflare Tunnel (optional): set TUNNEL_TOKEN (managed tunnel) or leave unset.
	if token := os.Getenv("TUNNEL_TOKEN"); token != "" {
		// Managed tunnel: hostname->service mapping lives in the Cloudflare dashboard.
		go runCloudflared("tunnel", "run", "--token", token)
	} else if id := os.Getenv("TUNNEL_ID"); id != "" {
		// Locally-managed tunnel: hostname->service mapping lives in tunnel.yml next to the binary.
		args := []string{"tunnel", "--config", tunnelConfigPath(), "run", id}
		go runCloudflared(args...)
	}

	// Daily backup: VACUUM INTO a temp file (consistent snapshot even under WAL), then atomically overwrite the single .bak file.
	go func() {
		dbPath := dbFilePathFromURL(config.DatabaseURL)
		if dbPath == "" {
			return
		}
		backupPath := dbPath + ".bak"
		for {
			// Run immediately on startup if no backup yet or the last one is stale (>24h).
			if st, err := os.Stat(backupPath); err != nil || time.Since(st.ModTime()) > 24*time.Hour {
				tmp := backupPath + ".tmp"
				_ = os.Remove(tmp)
				if _, err := app.DB.Exec(`VACUUM INTO ?`, tmp); err == nil {
					if err := os.Rename(tmp, backupPath); err == nil {
						log.Printf("backup written: %s", backupPath)
					}
				} else {
					_ = os.Remove(tmp)
				}
			}
			time.Sleep(24 * time.Hour)
			tmp := backupPath + ".tmp"
			_ = os.Remove(tmp)
			if _, err := app.DB.Exec(`VACUUM INTO ?`, tmp); err != nil {
				log.Printf("daily backup failed (vacuum): %v", err)
				continue
			}
			if err := os.Rename(tmp, backupPath); err != nil {
				_ = os.Remove(tmp)
				log.Printf("daily backup failed (rename): %v", err)
				continue
			}
			log.Printf("daily backup written: %s", backupPath)
		}
	}()

	// Background sync: quota + file metadata for all connected accounts every 5 minutes.
	go func() {
		for {
			type acct struct {
				id, owner string
			}
			rows, err := app.DB.Query(`SELECT id, user_id FROM connected_accounts WHERE status='connected' AND provider='google_drive'`)
			if err == nil {
				accs := []acct{}
				for rows.Next() {
					var a2 acct
					if rows.Scan(&a2.id, &a2.owner) == nil {
						accs = append(accs, a2)
					}
				}
				rows.Close()
				for _, a2 := range accs {
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					if err := app.syncAccountQuota(ctx, a2.id); err != nil {
						log.Printf("background quota sync failed for account %s: %v", a2.id, err)
					}
					if _, _, err := app.syncAccountFiles(ctx, a2.owner, a2.id); err != nil {
						log.Printf("background file sync failed for account %s: %v", a2.id, err)
					}
					cancel()
				}
				// Public links past their expires_at are revoked (Drive permission removed) once per loop.
				if revoked, err := app.revokeExpiredShares(5 * time.Minute); err != nil {
					log.Printf("expired share sweep failed: %v", err)
				} else if revoked > 0 {
					log.Printf("auto-revoked %d expired share link(s)", revoked)
				}
				if purged, err := app.purgeOldTrash(); err != nil {
					log.Printf("trash auto-purge failed: %v", err)
				} else if purged > 0 {
					log.Printf("trash auto-purge: %d file(s) permanently deleted", purged)
				}
			}
			time.Sleep(5 * time.Minute)
		}
	}()

	app.startUpdateChecker()

	bind := os.Getenv("APP_BIND")
	if bind == "" {
		bind = "127.0.0.1" // safe default: localhost only; set APP_BIND=0.0.0.0 for direct external access
	}
	log.Printf("binding on %s:%s", bind, config.AppPort)
	log.Fatal(http.ListenAndServe(bind+":"+config.AppPort, app.Router()))
}

// viewFileUrl hands the browser Google's own viewer URL (webViewLink). Playback/download then flows
// browser -> Google directly: no VPS bandwidth, no quota cost beyond what the user chooses to open.
func (a *App) viewFileUrl(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	fileID := r.PathValue("id")
	var providerFileID, accountID string
	var splitParts int
	err := a.DB.QueryRow(`SELECT f.provider_file_id, f.connected_account_id, (SELECT COUNT(*) FROM split_parts sp JOIN split_files sf ON sf.id=sp.split_id WHERE sp.file_id=f.id AND sf.status='complete') FROM files f WHERE f.id=? AND f.user_id=? AND f.status='active'`, fileID, user.ID).Scan(&providerFileID, &accountID, &splitParts)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "FILE_NOT_FOUND", "File not found.")
		return
	}
	if err != nil {
		writeError(w, 500, "VIEW_FAILED", "Unable to load file.")
		return
	}
	if splitParts > 0 {
		// No Google viewer exists for a merged split file; the client streams /files/{id}/stream.
		writeJSON(w, http.StatusOK, map[string]string{"url": "/api/files/" + fileID + "/stream", "split": "true"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	link := a.driveWebViewLink(ctx, accountID, providerFileID)
	if link == "" {
		writeError(w, 502, "VIEW_FAILED", "Unable to read the Drive viewer link.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": link})
}

func (a *App) shareFileUrl(w http.ResponseWriter, r *http.Request) {
	// Internal viewer URL. Public Drive links come from publicPermission below.
	writeJSON(w, http.StatusOK, map[string]string{"url": a.Config.FrontendURL + "/files/" + r.PathValue("id")})
}

// driveWebViewLink asks Drive for the canonical shareable URL of a file.
// driveFileLink fetches one link field (webContentLink or webViewLink) for a Drive file,
// with a static fallback URL when Google omits the field.
func (a *App) driveFileLink(ctx context.Context, accountID, providerFileID, field, fallback string) string {
	accessToken, err := a.getGoogleToken(ctx, accountID, false)
	if err != nil {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.GoogleDriveAPIURL+"/files/"+url.PathEscape(providerFileID)+"?fields="+field, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<15))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	var out map[string]string
	_ = json.Unmarshal(body, &out)
	if link := out[field]; link != "" {
		return link
	}
	return fallback
}

func (a *App) driveWebViewLink(ctx context.Context, accountID, providerFileID string) string {
	accessToken, err := a.getGoogleToken(ctx, accountID, false)
	if err != nil {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.GoogleDriveAPIURL+"/files/"+url.PathEscape(providerFileID)+"?fields=webViewLink", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<15))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	var out struct {
		WebViewLink string `json:"webViewLink"`
	}
	_ = json.Unmarshal(body, &out)
	if out.WebViewLink == "" {
		return "https://drive.google.com/file/d/" + providerFileID + "/view"
	}
	return out.WebViewLink
}

// publicPermission grants anyone-with-the-link read access in Google Drive and records the share.
func (a *App) publicPermission(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	fileID := r.PathValue("id")

	var providerFileID, accountID, name string
	var size int64
	err := a.DB.QueryRow(`SELECT provider_file_id,connected_account_id,name,size_bytes FROM files WHERE id=? AND user_id=? AND status='active'`, fileID, user.ID).Scan(&providerFileID, &accountID, &name, &size)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "FILE_NOT_FOUND", "File not found.")
		return
	}
	if err != nil {
		writeError(w, 500, "SHARE_FAILED", "Unable to load file.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	accessToken, err := a.getGoogleToken(ctx, accountID, false)
	if err != nil {
		writeError(w, 500, "SHARE_FAILED", "Unable to read account token.")
		return
	}

	payload, _ := json.Marshal(map[string]any{"role": "reader", "type": "anyone"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.GoogleDriveAPIURL+"/files/"+url.PathEscape(providerFileID)+"/permissions?fields=id", bytes.NewReader(payload))
	if err != nil {
		writeError(w, 500, "SHARE_FAILED", "Unable to build Drive request.")
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		writeError(w, 502, "SHARE_FAILED", "Drive request failed.")
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<15))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeError(w, 502, "SHARE_FAILED", fmt.Sprintf("Drive rejected the share (%d): %s", resp.StatusCode, string(body)))
		return
	}
	var perm struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &perm)

	shareID := randomID()
	// Optional expiry: the client sends an absolute RFC3339 timestamp; blank = never expires.
	expiresAt := strings.TrimSpace(r.URL.Query().Get("expiresAt"))
	if expiresAt != "" {
		if _, err := time.Parse(time.RFC3339, expiresAt); err != nil {
			writeError(w, 400, "BAD_REQUEST", "expiresAt must be an RFC3339 timestamp.")
			return
		}
	}
	// The stored URL is PanDrive's branded page (/s/<id>); the page itself links to Google's viewer,
	// so downloads flow browser -> Google and the VPS only ever serves ~2 KB of HTML.
	scheme := "http"
	host := r.Host
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	pageURL := scheme + "://" + host + "/s/" + shareID
	if _, err := a.DB.Exec(`INSERT INTO share_links (id,user_id,file_id,connected_account_id,provider_file_id,permission_id,url,expires_at,auto_revoke) VALUES (?,?,?,?,?,?,?,?,1)`,
		shareID, user.ID, fileID, accountID, providerFileID, perm.ID, pageURL, nullIfEmpty(expiresAt)); err != nil {
		writeError(w, 500, "SHARE_FAILED", "Link created in Drive but could not be saved locally.")
		return
	}
	a.logActivity(r, user.ID, accountID, "file_share", "file", fileID, name, size, "Public link created")
	a.notify(user.ID, "Link publik dibuat", "Public link untuk "+name+" sudah aktif.", "link", "default")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "url": pageURL, "shareId": shareID})
}

// drivePermissionRole maps a UI role onto a Drive role.
func drivePermissionRole(role string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "viewer", "reader", "view":
		return "reader", true
	case "editor", "writer", "edit":
		return "writer", true
	case "commenter", "comment":
		return "commenter", true
	}
	return "", false
}

// resolveShareTarget turns a local file/folder id into the Drive id + account that owns it.
// Only user-actionable problems are returned verbatim; driver errors are logged and replaced
// with a generic message so a raw SQL error never reaches the client.
func (a *App) resolveShareTarget(userID, targetType, targetID string) (providerFileID, accountID, name string, err error) {
	switch targetType {
	case "folder":
		var providerFolderID sql.NullString
		// COALESCE: a locally created folder has no account and no Drive id (both NULL).
		err = a.DB.QueryRow(`SELECT COALESCE(name,''), COALESCE(provider_folder_id,''), COALESCE(connected_account_id,'') FROM folders WHERE id=? AND user_id=? AND deleted_at IS NULL`, targetID, userID).Scan(&name, &providerFolderID, &accountID)
		if err == nil && (!providerFolderID.Valid || providerFolderID.String == "") {
			return "", "", "", errTargetNotMirrored
		}
		if err == nil {
			providerFileID = providerFolderID.String
		}
	default: // file
		err = a.DB.QueryRow(`SELECT name, COALESCE(provider_file_id,''), COALESCE(connected_account_id,'') FROM files WHERE id=? AND user_id=? AND status='active'`, targetID, userID).Scan(&name, &providerFileID, &accountID)
	}
	if err == sql.ErrNoRows {
		return "", "", "", errTargetNotFound
	}
	if err != nil {
		log.Printf("resolve share target failed (%s %s): %v", targetType, targetID, err)
		return "", "", "", errTargetUnavailable
	}
	return providerFileID, accountID, name, nil
}

// listPermissions reports Drive's own view of who can access a file or folder —
// the source of truth, not our local grant table.
func (a *App) listPermissions(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	targetType := r.URL.Query().Get("targetType")
	if targetType != "folder" {
		targetType = "file"
	}
	targetID := strings.TrimSpace(r.URL.Query().Get("targetId"))
	if targetID == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "targetId is required.")
		return
	}
	providerFileID, accountID, name, err := a.resolveShareTarget(user.ID, targetType, targetID)
	if err != nil {
		writeError(w, http.StatusNotFound, "TARGET_NOT_FOUND", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	accessToken, err := a.getGoogleToken(ctx, accountID, false)
	if err != nil {
		writeError(w, 500, "PERMISSIONS_FAILED", "Unable to read account token.")
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.GoogleDriveAPIURL+"/files/"+url.PathEscape(providerFileID)+"/permissions?fields=permissions(id,type,role,emailAddress,displayName,domain)", nil)
	if err != nil {
		writeError(w, 500, "PERMISSIONS_FAILED", "Unable to build Drive request.")
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		writeError(w, 502, "PERMISSIONS_FAILED", "Drive request failed.")
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeError(w, 502, "PERMISSIONS_FAILED", fmt.Sprintf("Drive rejected the request (%d): %s", resp.StatusCode, string(body)))
		return
	}
	var out struct {
		Permissions []map[string]any `json:"permissions"`
	}
	_ = json.Unmarshal(body, &out)
	if out.Permissions == nil {
		out.Permissions = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"targetName": name, "permissions": out.Permissions, "total": len(out.Permissions)})
}

// grantAccess shares a Drive file or folder with a specific Google account.
func (a *App) grantAccess(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		Email      string `json:"email"`
		Role       string `json:"role"`
		TargetType string `json:"targetType"`
		TargetID   string `json:"targetId"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	email := strings.TrimSpace(strings.ToLower(body.Email))
	if !strings.Contains(email, "@") || strings.HasPrefix(email, "@") || strings.HasSuffix(email, "@") {
		writeError(w, http.StatusBadRequest, "BAD_EMAIL", "A valid email address is required.")
		return
	}
	role, ok := drivePermissionRole(body.Role)
	if !ok {
		writeError(w, http.StatusBadRequest, "BAD_ROLE", "Role must be viewer, commenter or editor.")
		return
	}
	targetType := "file"
	if body.TargetType == "folder" {
		targetType = "folder"
	}
	if strings.TrimSpace(body.TargetID) == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "targetId is required.")
		return
	}

	providerFileID, accountID, name, err := a.resolveShareTarget(user.ID, targetType, body.TargetID)
	if err != nil {
		writeError(w, http.StatusNotFound, "TARGET_NOT_FOUND", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	accessToken, err := a.getGoogleToken(ctx, accountID, false)
	if err != nil {
		writeError(w, 500, "SHARE_FAILED", "Unable to read account token.")
		return
	}

	payload, _ := json.Marshal(map[string]any{"role": role, "type": "user", "emailAddress": email})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.GoogleDriveAPIURL+"/files/"+url.PathEscape(providerFileID)+"/permissions?fields=id&sendNotificationEmail=true", bytes.NewReader(payload))
	if err != nil {
		writeError(w, 500, "SHARE_FAILED", "Unable to build Drive request.")
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		writeError(w, 502, "SHARE_FAILED", "Drive request failed.")
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<15))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeError(w, 502, "SHARE_FAILED", fmt.Sprintf("Drive rejected the share (%d): %s", resp.StatusCode, string(respBody)))
		return
	}
	var perm struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(respBody, &perm)

	grantID := randomID()
	if _, err := a.DB.Exec(`INSERT INTO permission_grants (id,user_id,connected_account_id,target_type,target_id,provider_file_id,email,role,permission_id) VALUES (?,?,?,?,?,?,?,?,?)`,
		grantID, user.ID, accountID, targetType, body.TargetID, providerFileID, email, role, perm.ID); err != nil {
		writeError(w, 500, "SHARE_FAILED", "Access granted in Drive but could not be saved locally.")
		return
	}
	a.logActivity(r, user.ID, accountID, "permission_grant", targetType, body.TargetID, name, 0, fmt.Sprintf("Granted %s to %s", role, email))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "inviteId": grantID, "email": email, "role": role, "targetName": name})
}

// listInvites returns active per-person grants.
func (a *App) listInvites(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	rows, err := a.DB.Query(`SELECT g.id,g.target_type,g.target_id,g.email,g.role,COALESCE(g.created_at,''),
		COALESCE(f.name, fo.name, ''), COALESCE(c.email,'')
		FROM permission_grants g
		LEFT JOIN files f ON g.target_type='file' AND f.id=g.target_id
		LEFT JOIN folders fo ON g.target_type='folder' AND fo.id=g.target_id
		LEFT JOIN connected_accounts c ON c.id=g.connected_account_id
		WHERE g.user_id=? AND g.revoked_at IS NULL
		ORDER BY g.id DESC`, user.ID)
	if err != nil {
		writeError(w, 500, "INVITES_FAILED", "Unable to list invites: "+err.Error())
		return
	}
	defer rows.Close()
	invites := []map[string]any{}
	for rows.Next() {
		var id, targetType, targetID, email, role, createdAt, name, accountEmail string
		if err := rows.Scan(&id, &targetType, &targetID, &email, &role, &createdAt, &name, &accountEmail); err != nil {
			writeError(w, 500, "INVITES_FAILED", "Unable to read invites.")
			return
		}
		invites = append(invites, map[string]any{"id": id, "targetType": targetType, "targetId": targetID,
			"email": email, "role": role, "createdAt": createdAt, "targetName": name, "accountEmail": accountEmail})
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": invites, "total": len(invites)})
}

// revokeInvite removes a per-person Drive permission.
func (a *App) revokeInvite(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	grantID := r.PathValue("id")
	var accountID, providerFileID, permissionID, email, targetType, targetID string
	err := a.DB.QueryRow(`SELECT connected_account_id,provider_file_id,permission_id,email,target_type,target_id FROM permission_grants WHERE id=? AND user_id=? AND revoked_at IS NULL`, grantID, user.ID).Scan(&accountID, &providerFileID, &permissionID, &email, &targetType, &targetID)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "INVITE_NOT_FOUND", "Invite not found.")
		return
	}
	if err != nil {
		writeError(w, 500, "REVOKE_FAILED", "Unable to load invite.")
		return
	}

	if permissionID != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		if accessToken, err := a.getGoogleToken(ctx, accountID, false); err == nil {
			req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.GoogleDriveAPIURL+"/files/"+url.PathEscape(providerFileID)+"/permissions/"+url.PathEscape(permissionID), nil)
			if err == nil {
				req.Header.Set("Authorization", "Bearer "+accessToken)
				if resp, err := a.HTTPClient.Do(req); err == nil {
					resp.Body.Close()
				} else {
					// The permission may already be gone; surfacing the Drive error would block the
					// local cleanup, so the grant is marked revoked and the failure is logged.
					log.Printf("drive permission revoke failed for %s: %v", permissionID, err)
				}
			}
		} else {
			log.Printf("drive permission revoke skipped, token unavailable for account %s: %v", accountID, err)
		}
	}
	_, _ = a.DB.Exec(`UPDATE permission_grants SET revoked_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=?`, grantID, user.ID)
	var name string
	_ = a.DB.QueryRow(`SELECT COALESCE(name,'') FROM files WHERE id=?`, targetID).Scan(&name)
	a.logActivity(r, user.ID, accountID, "permission_revoke", targetType, targetID, name, 0, "Revoked access for "+email)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// listShares returns active public links.
func (a *App) listShares(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	rows, err := a.DB.Query(`SELECT s.id,s.url,COALESCE(s.created_at,''),f.id,f.name,f.size_bytes,COALESCE(f.mime_type,''),COALESCE(c.email,''),s.expires_at,
		CASE WHEN s.expires_at IS NOT NULL AND datetime(s.expires_at) <= datetime('now') THEN 1 ELSE 0 END
		FROM share_links s
		JOIN files f ON f.id=s.file_id
		LEFT JOIN connected_accounts c ON c.id=s.connected_account_id
		WHERE s.user_id=? AND s.revoked_at IS NULL
		ORDER BY s.id DESC`, user.ID)
	if err != nil {
		writeError(w, 500, "SHARES_FAILED", "Unable to list shares: "+err.Error())
		return
	}
	defer rows.Close()
	shares := []map[string]any{}
	for rows.Next() {
		var id, link, createdAt, fileID, name, mimeType, email string
		var size int64
		var expires sql.NullString
		var expired int
		if err := rows.Scan(&id, &link, &createdAt, &fileID, &name, &size, &mimeType, &email, &expires, &expired); err != nil {
			writeError(w, 500, "SHARES_FAILED", "Unable to read shares.")
			return
		}
		var expiresOut any
		if expires.Valid {
			expiresOut = expires.String
		}
		shares = append(shares, map[string]any{"id": id, "url": link, "createdAt": createdAt, "fileId": fileID, "name": name, "sizeBytes": fmt.Sprint(size), "mimeType": mimeType, "accountEmail": email, "expiresAt": expiresOut, "expired": expired == 1})
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": shares, "total": len(shares)})
}

// revokeShare removes the public permission in Drive and marks the record revoked.
func (a *App) revokeShare(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	shareID := r.PathValue("id")
	var accountID, providerFileID, permissionID, fileID, name string
	err := a.DB.QueryRow(`SELECT s.connected_account_id,s.provider_file_id,s.permission_id,s.file_id,COALESCE(f.name,'') FROM share_links s LEFT JOIN files f ON f.id=s.file_id WHERE s.id=? AND s.user_id=? AND s.revoked_at IS NULL`, shareID, user.ID).Scan(&accountID, &providerFileID, &permissionID, &fileID, &name)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "SHARE_NOT_FOUND", "Share not found.")
		return
	}
	if err != nil {
		writeError(w, 500, "REVOKE_FAILED", "Unable to load share.")
		return
	}

	if permissionID != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		accessToken, err := a.getGoogleToken(ctx, accountID, false)
		if err == nil {
			req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.GoogleDriveAPIURL+"/files/"+url.PathEscape(providerFileID)+"/permissions/"+url.PathEscape(permissionID), nil)
			if err == nil {
				req.Header.Set("Authorization", "Bearer "+accessToken)
				if resp, err := a.HTTPClient.Do(req); err == nil {
					resp.Body.Close()
				}
			}
		}
	}
	_, _ = a.DB.Exec(`UPDATE share_links SET revoked_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=?`, shareID, user.ID)
	a.logActivity(r, user.ID, accountID, "file_unshare", "file", fileID, name, 0, "Public link revoked")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// sharePage serves a tiny branded landing page for a public link: name, size, PanDrive mark and a
// download button that points at Google directly (browser -> Google; the VPS sends only this HTML).
func (a *App) sharePage(w http.ResponseWriter, r *http.Request) {
	shareID := r.PathValue("id")
	var name, mimeType, storedURL, expiresAt string
	var size int64
	err := a.DB.QueryRow(`SELECT COALESCE(f.name,''), COALESCE(f.mime_type,''), s.url, COALESCE(s.expires_at,''), COALESCE(f.size_bytes,0)
		FROM share_links s LEFT JOIN files f ON f.id=s.file_id
		WHERE s.id=? AND s.revoked_at IS NULL`, shareID).Scan(&name, &mimeType, &storedURL, &expiresAt, &size)
	if err != nil {
		http.Error(w, `<!doctype html><meta charset="utf-8"><body style="font-family:system-ui;background:#0f172a;color:#e2e8f0;display:grid;place-items:center;height:100vh"><div style="text-align:center"><h1>404</h1><p>Link tidak ditemukan atau sudah dicabut.</p></div>`, http.StatusNotFound)
		return
	}
	expired := expiresAt != "" && func() bool {
		t, err := time.Parse(time.RFC3339, expiresAt)
		return err == nil && !t.After(time.Now())
	}()
	if expired {
		http.Error(w, `<!doctype html><meta charset="utf-8"><body style="font-family:system-ui;background:#0f172a;color:#e2e8f0;display:grid;place-items:center;height:100vh"><div style="text-align:center"><h1>Link kedaluwarsa</h1><p>Link ini sudah melewati masa berlaku dan dicabut.</p></div>`, http.StatusGone)
		return
	}
	// Google's own download/viewer link: direct download for binary types, viewer for docs.
	var driveURL string
	if storedURL != "" && strings.Contains(storedURL, "drive.google.com") {
		driveURL = storedURL
	} else {
		var providerFileID string
		_ = a.DB.QueryRow(`SELECT provider_file_id FROM share_links WHERE id=?`, shareID).Scan(&providerFileID)
		driveURL = "https://drive.google.com/uc?export=download&id=" + url.QueryEscape(providerFileID)
	}
	sizeLabel := fmt.Sprintf("%.1f KB", float64(size)/1024)
	if size >= 1024*1024 {
		sizeLabel = fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
	if size >= 1024*1024*1024 {
		sizeLabel = fmt.Sprintf("%.2f GB", float64(size)/(1024*1024*1024))
	}
	heavy := size >= 200*1024*1024
	warn := ""
	if heavy {
		warn = `<p style="margin-top:8px;font-size:13px;color:#fbbf24">File besar (≥ 200 MB) — unduh lewat Wi-Fi untuk menghemat kuota.</p>`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Robots-Tag", "noindex")
	fmt.Fprintf(w, `<!doctype html><html lang="id"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s — PanDrive</title>
<style>body{font-family:system-ui,-apple-system,Segoe UI,sans-serif;background:#0f172a;color:#e2e8f0;display:grid;place-items:center;min-height:100vh;margin:0;padding:24px}
.card{background:#1e293b;border:1px solid #334155;border-radius:16px;padding:32px;max-width:420px;width:100%%;text-align:center}
.logo{width:56px;height:56px;border-radius:14px;margin-bottom:12px}
h1{font-size:18px;margin:8px 0;word-break:break-word}.meta{font-size:13px;color:#94a3b8;margin:4px 0 20px}
a.btn{display:inline-block;background:#2563eb;color:#fff;text-decoration:none;font-weight:700;padding:12px 28px;border-radius:12px;font-size:14px}
a.btn:hover{background:#1d4ed8}.foot{margin-top:20px;font-size:11px;color:#64748b}</style></head>
<body><div class="card"><img class="logo" src="/logo.png" alt="PanDrive"><h1>%s</h1>
<p class="meta">%s · dibagikan lewat PanDrive</p>%s
<a class="btn" href="%s" rel="noopener">Unduh / Buka file</a>
<p class="foot">PanDrive by JhopanStore</p></div></body></html>`,
		htmlEscape(name), htmlEscape(name), htmlEscape(sizeLabel), warn, htmlEscapeAttr(driveURL))
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;")
	return r.Replace(s)
}

func htmlEscapeAttr(s string) string {
	return htmlEscape(s)
}

// revokeExpiredShares marks expired public links revoked and removes their "anyone" permission in
// Drive. grace limits how fresh an expiry may be before we touch it (avoids racing the creator).
func (a *App) revokeExpiredShares(grace time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-grace).Format(time.RFC3339)
	rows, err := a.DB.Query(`SELECT s.id, s.connected_account_id, s.provider_file_id, COALESCE(f.name,''), s.user_id
		FROM share_links s LEFT JOIN files f ON f.id=s.file_id
		WHERE s.revoked_at IS NULL AND s.expires_at IS NOT NULL AND s.expires_at != '' AND datetime(s.expires_at) <= datetime(?)`, cutoff)
	if err != nil {
		return 0, err
	}
	type expired struct {
		id, accountID, providerFileID, name, userID string
	}
	var targets []expired
	for rows.Next() {
		var t expired
		if err := rows.Scan(&t.id, &t.accountID, &t.providerFileID, &t.name, &t.userID); err != nil {
			rows.Close()
			return 0, err
		}
		targets = append(targets, t)
	}
	rows.Close()
	revoked := 0
	for _, t := range targets {
		var permissionID string
		_ = a.DB.QueryRow(`SELECT permission_id FROM share_links WHERE id=?`, t.id).Scan(&permissionID)
		if permissionID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			token, err := a.getGoogleToken(ctx, t.accountID, false)
			if err == nil {
				req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.GoogleDriveAPIURL+"/files/"+url.PathEscape(t.providerFileID)+"/permissions/"+url.PathEscape(permissionID), nil)
				if err == nil {
					req.Header.Set("Authorization", "Bearer "+token)
					if resp, err := a.HTTPClient.Do(req); err == nil {
						resp.Body.Close()
					}
				}
			}
			cancel()
		}
		res, err := a.DB.Exec(`UPDATE share_links SET revoked_at=CURRENT_TIMESTAMP WHERE id=? AND revoked_at IS NULL`, t.id)
		if err != nil {
			return revoked, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			revoked++
			a.notify(t.userID, "Link kedaluwarsa", fmt.Sprintf("Public link untuk %s sudah kadaluarsa dan dicabut.", t.name), "share_expired", "low")
		}
	}
	return revoked, nil
}

// ---- notifications (ntfy) ----

// settingValue reads a key from app_settings.
func (a *App) settingValue(key string) string {
	var value string
	_ = a.DB.QueryRow(`SELECT value FROM app_settings WHERE key=?`, key).Scan(&value)
	return value
}

// setSetting upserts a key in app_settings.
func (a *App) setSetting(key, value string) error {
	_, err := a.DB.Exec(`INSERT INTO app_settings (key,value,updated_at) VALUES (?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=CURRENT_TIMESTAMP`, key, value)
	return err
}

// notify posts an event to the user's configured ntfy topic. Fire-and-forget: notification
// failures must never fail the underlying operation. topicOverride lets the test button try a
// value before it is saved.
func (a *App) notify(userID, title, message, tag, priority string) {
	topic := a.settingValue("ntfy_topic")
	if topic == "" {
		return
	}
	server := a.settingValue("ntfy_server")
	if server == "" {
		server = "https://ntfy.sh"
	}
	go func(server, topic string) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/"+topic, strings.NewReader(message))
		if err != nil {
			return
		}
		req.Header.Set("Title", title)
		req.Header.Set("Tags", tag)
		req.Header.Set("Priority", priority)
		client := &http.Client{Timeout: 15 * time.Second}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}(server, topic)
}

// getNotificationSettings returns the user's ntfy config (never the secret).
func (a *App) getNotificationSettings(w http.ResponseWriter, r *http.Request) {
	_ = r.Context().Value(userKey).(authUser) // auth gate only; settings are app-wide by design
	writeJSON(w, http.StatusOK, map[string]any{"server": a.settingValue("ntfy_server"), "topic": a.settingValue("ntfy_topic"), "enabled": a.settingValue("ntfy_topic") != ""})
}

// putNotificationSettings stores the ntfy config.
func (a *App) putNotificationSettings(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		Server string `json:"server"`
		Topic  string `json:"topic"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "BAD_REQUEST", "Invalid request body.")
		return
	}
	body.Server = strings.TrimRight(strings.TrimSpace(body.Server), "/")
	body.Topic = strings.TrimSpace(body.Topic)
	if body.Topic == "" {
		writeError(w, 400, "TOPIC_REQUIRED", "Topic is required.")
		return
	}
	if body.Server != "" && !strings.HasPrefix(body.Server, "http") {
		writeError(w, 400, "BAD_SERVER", "Server must start with http(s)://")
		return
	}
	if body.Server == "" {
		body.Server = "https://ntfy.sh"
	}
	if err := a.setSetting("ntfy_server", body.Server); err != nil {
		writeError(w, 500, "SETTINGS_FAILED", "Unable to save settings.")
		return
	}
	if err := a.setSetting("ntfy_topic", body.Topic); err != nil {
		writeError(w, 500, "SETTINGS_FAILED", "Unable to save settings.")
		return
	}
	a.logActivity(r, user.ID, "", "settings_update", "user", user.ID, user.Email, 0, "Notification settings saved ("+body.Server+"/"+body.Topic+")")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "server": body.Server, "topic": body.Topic})
}

// testNotification fires a real notification with the request values (or the saved ones).
func (a *App) testNotification(w http.ResponseWriter, r *http.Request) {
	_ = r.Context().Value(userKey).(authUser) // auth gate only
	var body struct {
		Server string `json:"server"`
		Topic  string `json:"topic"`
	}
	_ = decodeJSON(r, &body)
	topic := body.Topic
	server := strings.TrimRight(strings.TrimSpace(body.Server), "/")
	if topic == "" {
		topic = a.settingValue("ntfy_topic")
	}
	if server == "" {
		server = a.settingValue("ntfy_server")
	}
	if server == "" {
		server = "https://ntfy.sh"
	}
	if topic == "" {
		writeError(w, 400, "TOPIC_REQUIRED", "Save a topic first or pass one in the request.")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, server+"/"+topic, strings.NewReader("Notifikasi PanDrive aktif. Kalau kamu melihat ini, pengaturan sudah benar."))
	if err != nil {
		writeError(w, 500, "NOTIFY_FAILED", "Unable to build request.")
		return
	}
	req.Header.Set("Title", "PanDrive test")
	req.Header.Set("Tags", "white_check_mark")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeError(w, 502, "NOTIFY_FAILED", "ntfy server unreachable: "+err.Error())
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		writeError(w, 502, "NOTIFY_FAILED", fmt.Sprintf("ntfy rejected the message (%d).", resp.StatusCode))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent", "server": server, "topic": topic})
}

// requireAdmin wraps a handler for role='admin' users only.
func (a *App) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := r.Context().Value(userKey).(authUser)
		var role string
		_ = a.DB.QueryRow(`SELECT COALESCE(role,'user') FROM users WHERE id=?`, user.ID).Scan(&role)
		if role != "admin" {
			writeError(w, http.StatusForbidden, "ADMIN_REQUIRED", "Administrator access required.")
			return
		}
		next(w, r)
	}
}

// listUsers shows every account with its connected-Drive count (admin only).
func (a *App) listUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query(`SELECT u.id, u.name, u.email, COALESCE(u.role,'user'), COALESCE(u.disabled,0), u.created_at,
		(SELECT COUNT(*) FROM connected_accounts c WHERE c.user_id=u.id AND c.status='connected'),
		(SELECT COUNT(*) FROM files f WHERE f.user_id=u.id AND f.status='active')
		FROM users u ORDER BY u.created_at`)
	if err != nil {
		writeError(w, 500, "USERS_FAILED", "Unable to list users.")
		return
	}
	defer rows.Close()
	users := []map[string]any{}
	for rows.Next() {
		var id, name, email, role, createdAt string
		var disabled, accounts, files int
		if err := rows.Scan(&id, &name, &email, &role, &disabled, &createdAt, &accounts, &files); err != nil {
			writeError(w, 500, "USERS_FAILED", "Unable to read users.")
			return
		}
		users = append(users, map[string]any{"id": id, "name": name, "email": email, "role": role, "disabled": disabled == 1, "createdAt": createdAt, "connectedAccounts": accounts, "files": files})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "total": len(users)})
}

// updateUser disables/enables an account or toggles its role. Self-demotion is blocked so an
// admin cannot lock themselves out of the admin area.
func (a *App) updateUser(w http.ResponseWriter, r *http.Request) {
	caller := r.Context().Value(userKey).(authUser)
	targetID := r.PathValue("id")
	var body struct {
		Disabled *bool   `json:"disabled"`
		Role     *string `json:"role"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "BAD_REQUEST", "Invalid request body.")
		return
	}
	var exists string
	if err := a.DB.QueryRow(`SELECT id FROM users WHERE id=?`, targetID).Scan(&exists); err != nil {
		writeError(w, http.StatusNotFound, "USER_NOT_FOUND", "User not found.")
		return
	}
	if targetID == caller.ID && ((body.Disabled != nil && *body.Disabled) || (body.Role != nil && *body.Role != "admin")) {
		writeError(w, 400, "SELF_LOCKOUT", "You cannot disable your own account or demote yourself.")
		return
	}
	if body.Disabled != nil {
		if _, err := a.DB.Exec(`UPDATE users SET disabled=? WHERE id=?`, boolToInt(*body.Disabled), targetID); err != nil {
			writeError(w, 500, "UPDATE_FAILED", "Unable to update the user.")
			return
		}
		if *body.Disabled {
			a.DB.Exec(`UPDATE user_sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), targetID)
		}
	}
	if body.Role != nil {
		role := strings.TrimSpace(*body.Role)
		if role != "admin" && role != "user" {
			writeError(w, 400, "BAD_ROLE", "Role must be admin or user.")
			return
		}
		if _, err := a.DB.Exec(`UPDATE users SET role=? WHERE id=?`, role, targetID); err != nil {
			writeError(w, 500, "UPDATE_FAILED", "Unable to update the user.")
			return
		}
	}
	a.logActivity(r, caller.ID, "", "user_update", "user", targetID, targetID, 0, "Admin updated a user")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- trash auto-purge settings ----

func (a *App) getTrashSettings(w http.ResponseWriter, r *http.Request) {
	days := a.settingValue("trash_autopurge_days")
	writeJSON(w, http.StatusOK, map[string]any{"enabled": days != "" && days != "0", "days": days})
}

// putTrashSettings stores the purge age in days (0/empty = disabled; 1..365 when enabled).
func (a *App) putTrashSettings(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		Days string `json:"days"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "BAD_REQUEST", "Invalid request body.")
		return
	}
	days := strings.TrimSpace(body.Days)
	if days == "" || days == "0" {
		if err := a.setSetting("trash_autopurge_days", ""); err != nil {
			writeError(w, 500, "SETTINGS_FAILED", "Unable to save settings.")
			return
		}
		a.logActivity(r, user.ID, "", "settings_update", "user", user.ID, user.Email, 0, "Trash auto-purge disabled")
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "days": ""})
		return
	}
	var n int
	if _, err := fmt.Sscan(days, &n); err != nil || n < 1 || n > 365 {
		writeError(w, 400, "BAD_REQUEST", "days must be between 1 and 365.")
		return
	}
	if err := a.setSetting("trash_autopurge_days", fmt.Sprint(n)); err != nil {
		writeError(w, 500, "SETTINGS_FAILED", "Unable to save settings.")
		return
	}
	a.logActivity(r, user.ID, "", "settings_update", "user", user.ID, user.Email, 0, "Trash auto-purge enabled: "+fmt.Sprint(n)+" days")
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "days": n})
}

// ---- reverse proxy settings (Caddy / Cloudflare Tunnel) ----

// getProxySettings reports the chosen provider, domain, and whether the config files exist.
func (a *App) getProxySettings(w http.ResponseWriter, r *http.Request) {
	provider := a.settingValue("proxy_provider")
	domain := a.settingValue("proxy_domain")
	dir := configDir()
	caddyfile := dir + "/Caddyfile"
	caddyExists := false
	if st, err := os.Stat(caddyfile); err == nil && !st.IsDir() {
		caddyExists = true
	}
	tunnelID := os.Getenv("TUNNEL_ID")
	tunnelToken := os.Getenv("TUNNEL_TOKEN")
	if tunnelID == "" || tunnelToken == "" {
		if b, err := os.ReadFile(".env"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
				if len(parts) != 2 {
					continue
				}
				if parts[0] == "TUNNEL_ID" && tunnelID == "" {
					tunnelID = parts[1]
				}
				if parts[0] == "TUNNEL_TOKEN" && tunnelToken == "" {
					tunnelToken = parts[1]
				}
			}
		}
	}
	enabled := tunnelID != "" || tunnelToken != ""
	tunnel := map[string]any{"enabled": enabled, "mode": map[bool]string{true: "TUNNEL_ID (tunnel.yml)", false: "off"}[tunnelID != ""]}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": provider, "domain": domain,
		"caddy":  map[string]any{"configPath": caddyfile, "written": caddyExists, "installed": commandExists("caddy")},
		"tunnel": tunnel,
	})
}

// putProxySettings stores provider (none|caddy|cloudflare) + domain.
func (a *App) putProxySettings(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		Provider string `json:"provider"`
		Domain   string `json:"domain"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "BAD_REQUEST", "Invalid request body.")
		return
	}
	provider := strings.ToLower(strings.TrimSpace(body.Provider))
	domain := strings.ToLower(strings.TrimSpace(body.Domain))
	if provider != "" && provider != "none" && provider != "caddy" && provider != "cloudflare" {
		writeError(w, 400, "BAD_PROVIDER", "Provider must be none, caddy or cloudflare.")
		return
	}
	if domain != "" && !strings.Contains(domain, ".") {
		writeError(w, 400, "BAD_DOMAIN", "Domain must look like drive.example.com.")
		return
	}
	if err := a.setSetting("proxy_provider", provider); err != nil {
		writeError(w, 500, "SETTINGS_FAILED", "Unable to save settings.")
		return
	}
	if err := a.setSetting("proxy_domain", domain); err != nil {
		writeError(w, 500, "SETTINGS_FAILED", "Unable to save settings.")
		return
	}
	a.logActivity(r, user.ID, "", "settings_update", "user", user.ID, user.Email, 0, "Reverse proxy set to "+provider+" ("+domain+")")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "provider": provider, "domain": domain})
}

// writeCaddyfile generates a minimal Caddyfile for the stored domain (or the one posted) and,
// when run as root with systemd, drops it into /etc/caddy and restarts caddy.
func (a *App) writeCaddyfile(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		Domain  string `json:"domain"`
		AppPort string `json:"appPort"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "BAD_REQUEST", "Invalid request body.")
		return
	}
	domain := strings.ToLower(strings.TrimSpace(body.Domain))
	if domain == "" {
		domain = a.settingValue("proxy_domain")
	}
	if domain == "" || !strings.Contains(domain, ".") {
		writeError(w, 400, "BAD_DOMAIN", "Set a domain first (drive.example.com).")
		return
	}
	port := strings.TrimSpace(body.AppPort)
	if port == "" {
		port = a.Config.AppPort
	}
	if port == "" {
		port = "4000"
	}
	if _, err := fmt.Sscan(port, new(int)); err != nil {
		writeError(w, 400, "BAD_REQUEST", "appPort must be a number.")
		return
	}
	content := fmt.Sprintf("# PanDrive HTTPS reverse proxy (generated %s)\n%s {\n    reverse_proxy 127.0.0.1:%s\n}\n", time.Now().UTC().Format(time.RFC3339), domain, port)
	dir := configDir()
	local := dir + "/Caddyfile"
	if err := os.WriteFile(local, []byte(content), 0o644); err != nil {
		writeError(w, 500, "CADDYFILE_FAILED", "Unable to write Caddyfile.")
		return
	}
	reloaded := false
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		_ = os.MkdirAll("/etc/caddy", 0o755)
		if err := os.WriteFile("/etc/caddy/Caddyfile", []byte(content), 0o644); err == nil {
			if b, err := exec.Command("systemctl", "restart", "caddy").CombinedOutput(); err == nil {
				reloaded = true
			} else {
				log.Printf("caddy restart failed: %s", strings.TrimSpace(string(b)))
			}
		}
	}
	a.logActivity(r, user.ID, "", "settings_update", "user", user.ID, user.Email, 0, "Caddyfile written for "+domain)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "path": local, "domain": domain, "systemdReloaded": reloaded})
}

// commandExists reports whether an executable is on PATH.
func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// ---- API keys (pd_...) for programmatic access (Android / scripts) ----

// generateAPIKey returns (plaintext, prefix, hash). Only the SHA-256 hash is stored; the plaintext is
// shown to the user exactly once at creation. The 8-char prefix enables lookup + identification.
func generateAPIKey() (string, string, string) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", "", ""
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	key := "pd_" + secret
	prefix := key[:11]
	sum := sha256.Sum256([]byte(key))
	return key, prefix, hex.EncodeToString(sum[:])
}

// apiKeyHash is the stored form of a plaintext key.
func apiKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// userFromAPIKey authenticates a pd_ key: hash lookup, revocation + user-disabled checks, last-used
// stamp (throttled to once per minute to avoid write amplification), and returns the owning user.
func (a *App) userFromAPIKey(key string) (authUser, bool) {
	prefix := key
	if len(prefix) > 11 {
		prefix = prefix[:11]
	}
	var userID, scopes, revokedAt string
	var disabled int
	err := a.DB.QueryRow(`SELECT k.user_id, k.scopes, COALESCE(k.revoked_at,''), COALESCE(u.disabled,0)
		FROM api_keys k JOIN users u ON u.id=k.user_id
		WHERE k.prefix=? AND k.key_hash=?`, prefix, apiKeyHash(key)).Scan(&userID, &scopes, &revokedAt, &disabled)
	if err != nil || revokedAt != "" || disabled == 1 {
		return authUser{}, false
	}
	// stamp last-used at most once per minute per key
	_, _ = a.DB.Exec(`UPDATE api_keys SET last_used_at=?
		WHERE prefix=? AND (last_used_at IS NULL OR datetime(last_used_at) <= datetime('now','-1 minute'))`,
		time.Now().UTC().Format(time.RFC3339Nano), prefix)
	_ = scopes // reserved for fine-grained scopes; all keys currently act as their owner
	var name, email, role string
	if err := a.DB.QueryRow(`SELECT name, email, COALESCE(role,'user') FROM users WHERE id=?`, userID).Scan(&name, &email, &role); err != nil {
		return authUser{}, false
	}
	return authUser{ID: userID, Name: name, Email: email, Role: role}, true
}

// listAPIKeys returns the user's keys WITHOUT secrets (prefix only + status).
func (a *App) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	rows, err := a.DB.Query(`SELECT id, name, prefix, scopes, COALESCE(last_used_at,''), COALESCE(revoked_at,''), created_at
		FROM api_keys WHERE user_id=? ORDER BY created_at DESC`, user.ID)
	if err != nil {
		writeError(w, 500, "KEYS_FAILED", "Unable to list API keys.")
		return
	}
	defer rows.Close()
	keys := []map[string]any{}
	for rows.Next() {
		var id, name, prefix, scopes, lastUsed, revoked, created string
		if err := rows.Scan(&id, &name, &prefix, &scopes, &lastUsed, &revoked, &created); err != nil {
			writeError(w, 500, "KEYS_FAILED", "Unable to read API keys.")
			return
		}
		keys = append(keys, map[string]any{
			"id": id, "name": name, "prefix": prefix, "scopes": scopes,
			"lastUsedAt": lastUsed, "revoked": revoked != "", "createdAt": created,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys, "total": len(keys)})
}

// createAPIKey mints a key. The plaintext is returned ONCE in this response.
func (a *App) createAPIKey(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "BAD_REQUEST", "Invalid request body.")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "API key"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	// cap active keys per user to keep the table sane
	var active int
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE user_id=? AND revoked_at IS NULL`, user.ID).Scan(&active)
	if active >= 10 {
		writeError(w, 400, "TOO_MANY_KEYS", "Revoke an existing key first (max 10 active).")
		return
	}
	key, prefix, hash := generateAPIKey()
	id := randomID()
	if _, err := a.DB.Exec(`INSERT INTO api_keys (id,user_id,name,prefix,key_hash,scopes) VALUES (?,?,?,?,?,?)`,
		id, user.ID, name, prefix, hash, "read"); err != nil {
		writeError(w, 500, "KEYS_FAILED", "Unable to create the API key.")
		return
	}
	a.logActivity(r, user.ID, "", "api_key_create", "user", user.ID, user.Email, 0, "API key created: "+name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"status": "ok", "id": id, "name": name, "prefix": prefix,
		"key": key, "note": "Copy this key now - it is shown only once.",
	})
}

// revokeAPIKey marks a key revoked (the hash stays for audit; the key stops working instantly).
func (a *App) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	id := r.PathValue("id")
	res, err := a.DB.Exec(`UPDATE api_keys SET revoked_at=? WHERE id=? AND user_id=? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), id, user.ID)
	if err != nil {
		writeError(w, 500, "KEYS_FAILED", "Unable to revoke the API key.")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "KEY_NOT_FOUND", "API key not found or already revoked.")
		return
	}
	var name string
	_ = a.DB.QueryRow(`SELECT name FROM api_keys WHERE id=?`, id).Scan(&name)
	a.logActivity(r, user.ID, "", "api_key_revoke", "user", user.ID, user.Email, 0, "API key revoked: "+name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// deleteAPIKey removes the row entirely.
func (a *App) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	id := r.PathValue("id")
	res, err := a.DB.Exec(`DELETE FROM api_keys WHERE id=? AND user_id=?`, id, user.ID)
	if err != nil {
		writeError(w, 500, "KEYS_FAILED", "Unable to delete the API key.")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "KEY_NOT_FOUND", "API key not found.")
		return
	}
	a.logActivity(r, user.ID, "", "api_key_revoke", "user", user.ID, user.Email, 0, "API key deleted")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// configDir returns a writable per-user config directory (falls back to the working dir).
func configDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		full := dir + "/pandrive"
		if err := os.MkdirAll(full, 0o755); err == nil {
			return full
		}
	}
	return "."
}

// purgeOldTrash permanently deletes Drive trash files older than the configured age, per account,
// and clears the local rows. Opt-in via trash_autopurge_days; every deletion is notified + logged.
func (a *App) purgeOldTrash() (int, error) {
	days := a.settingValue("trash_autopurge_days")
	if days == "" || days == "0" {
		return 0, nil
	}
	var n int
	if _, err := fmt.Sscan(days, &n); err != nil || n < 1 {
		return 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -n).Format(time.RFC3339Nano)
	rows, err := a.DB.Query(`SELECT f.id, f.connected_account_id, f.provider_file_id, f.user_id, COALESCE(f.name,'')
		FROM files f WHERE f.status='deleted' AND f.deleted_at IS NOT NULL AND datetime(f.deleted_at) <= datetime(?)`, cutoff)
	if err != nil {
		return 0, err
	}
	type victim struct{ id, accountID, providerFileID, userID, name string }
	var targets []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.accountID, &v.providerFileID, &v.userID, &v.name); err == nil {
			targets = append(targets, v)
		}
	}
	rows.Close()
	purged := 0
	for _, v := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		token, err := a.getGoogleToken(ctx, v.accountID, false)
		if err == nil {
			req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.GoogleDriveAPIURL+"/files/"+url.PathEscape(v.providerFileID), nil)
			if err == nil {
				req.Header.Set("Authorization", "Bearer "+token)
				if resp, err := a.HTTPClient.Do(req); err == nil {
					resp.Body.Close()
				}
			}
		}
		cancel()
		if _, err := a.DB.Exec(`DELETE FROM files WHERE id=?`, v.id); err != nil {
			return purged, err
		}
		purged++
		a.notify(v.userID, "Sampah dibersihkan", v.name+" dihapus permanen dari Drive (lewat "+days+" hari di sampah).", "wastebasket", "low")
	}
	if purged > 0 {
		log.Printf("auto-purge: permanently deleted %d trashed file(s) older than %s day(s)", purged, days)
	}
	return purged, nil
}

// uploadQueue lists resumable-upload sessions so stuck or failed uploads can be seen.
func (a *App) uploadQueue(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	where := "WHERE u.user_id=?"
	args := []any{user.ID}
	if status != "" && status != "all" {
		where += " AND u.status=?"
		args = append(args, status)
	}
	rows, err := a.DB.Query(`SELECT u.id,u.file_name,u.mime_type,u.size_bytes,u.status,COALESCE(u.error_message,''),COALESCE(u.created_at,''),COALESCE(u.completed_at,''),
		COALESCE(c.email,''),COALESCE(d.name,''),CASE WHEN COALESCE(u.google_session_uri,'')='' THEN 0 ELSE 1 END
		FROM upload_sessions u
		LEFT JOIN connected_accounts c ON c.id=u.target_connected_account_id
		LEFT JOIN folders d ON d.id=u.folder_id
		`+where+` ORDER BY u.id DESC LIMIT 200`, args...)
	if err != nil {
		writeError(w, 500, "QUEUE_FAILED", "Unable to read upload queue: "+err.Error())
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	counts := map[string]int{}
	for rows.Next() {
		var id, fileName, mimeType, status2, errorMsg, createdAt, completedAt, email, folder string
		var size int64
		var resumable int
		if err := rows.Scan(&id, &fileName, &mimeType, &size, &status2, &errorMsg, &createdAt, &completedAt, &email, &folder, &resumable); err != nil {
			writeError(w, 500, "QUEUE_FAILED", "Unable to read upload queue.")
			return
		}
		counts[status2]++
		items = append(items, map[string]any{"id": id, "fileName": fileName, "mimeType": mimeType, "sizeBytes": fmt.Sprint(size),
			"status": status2, "error": errorMsg, "createdAt": createdAt, "completedAt": completedAt,
			"accountEmail": email, "folder": folder, "resumable": resumable == 1})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "counts": counts, "total": len(items)})
}

// cancelUpload marks a queued upload as cancelled. Google keeps the session until it expires,
// but PanDrive stops tracking and resuming it.
func (a *App) cancelUpload(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	id := r.PathValue("id")
	res, err := a.DB.Exec(`UPDATE upload_sessions SET status='cancelled' WHERE id=? AND user_id=? AND status IN ('uploading','pending','in_progress')`, id, user.ID)
	if err != nil {
		writeError(w, 500, "CANCEL_FAILED", "Unable to cancel upload.")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusBadRequest, "NOT_CANCELLABLE", "Only pending or in-progress uploads can be cancelled.")
		return
	}
	var fileName, accountID string
	var size int64
	_ = a.DB.QueryRow(`SELECT file_name,COALESCE(target_connected_account_id,''),size_bytes FROM upload_sessions WHERE id=?`, id).Scan(&fileName, &accountID, &size)
	a.logActivity(r, user.ID, accountID, "upload_cancel", "upload", id, fileName, size, "Upload cancelled")
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// removeUploadRecord deletes a finished queue entry (never a running one).
func (a *App) removeUploadRecord(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	id := r.PathValue("id")
	res, err := a.DB.Exec(`DELETE FROM upload_sessions WHERE id=? AND user_id=? AND status IN ('completed','failed','cancelled')`, id, user.ID)
	if err != nil {
		writeError(w, 500, "REMOVE_FAILED", "Unable to remove upload record.")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusBadRequest, "NOT_REMOVABLE", "Only completed, failed or cancelled uploads can be removed.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// listRecent returns the most recently updated files, newest first.
func (a *App) listRecent(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	accountID := strings.TrimSpace(r.URL.Query().Get("accountId"))

	where := "WHERE f.user_id=? AND f.status='active'"
	args := []any{user.ID}
	if accountID != "" && accountID != "all" {
		where += " AND f.connected_account_id=?"
		args = append(args, accountID)
	}

	rows, err := a.DB.Query(`SELECT f.id,f.name,f.mime_type,f.size_bytes,COALESCE(f.updated_at,''),COALESCE(f.created_at,''),
		COALESCE(c.email,''),COALESCE(d.name,''),f.folder_id,COALESCE(f.starred,0)
		FROM files f
		LEFT JOIN connected_accounts c ON c.id=f.connected_account_id
		LEFT JOIN folders d ON d.id=f.folder_id
		`+where+` ORDER BY datetime(f.updated_at) DESC, f.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		writeError(w, 500, "RECENT_FAILED", "Unable to list recent files: "+err.Error())
		return
	}
	defer rows.Close()

	files := []map[string]any{}
	for rows.Next() {
		var id, name, mimeType, updatedAt, createdAt, email, folderName string
		var size int64
		var starred int
		var folderID sql.NullString
		if err := rows.Scan(&id, &name, &mimeType, &size, &updatedAt, &createdAt, &email, &folderName, &folderID, &starred); err != nil {
			writeError(w, 500, "RECENT_FAILED", "Unable to read recent files.")
			return
		}
		var folder any
		if folderID.Valid {
			folder = map[string]string{"id": folderID.String, "name": folderName}
		}
		files = append(files, map[string]any{"id": id, "name": name, "mimeType": mimeType, "sizeBytes": fmt.Sprint(size),
			"updatedAt": updatedAt, "createdAt": createdAt, "accountEmail": email, "folder": folder, "starred": starred == 1})
	}

	// Activity counters for the header cards. SQLite compares ISO-8601 text correctly.
	var last24h, last7d int
	// datetime() normalises both stored formats (Go writes RFC3339 with 'T', CURRENT_TIMESTAMP
	// writes a space) — raw string comparison between them sorts wrong and breaks the windows.
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE user_id=? AND status='active' AND datetime(updated_at) >= datetime('now','-1 day')`, user.ID).Scan(&last24h)
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE user_id=? AND status='active' AND datetime(updated_at) >= datetime('now','-7 day')`, user.ID).Scan(&last7d)

	writeJSON(w, http.StatusOK, map[string]any{"files": files, "total": len(files), "limit": limit, "last24h": last24h, "last7d": last7d})
}

// starFile toggles the starred flag on a file.
func (a *App) starFile(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	fileID := r.PathValue("id")
	var body struct {
		Starred *bool `json:"starred"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Starred == nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "starred (boolean) is required.")
		return
	}
	value := 0
	if *body.Starred {
		value = 1
	}
	res, err := a.DB.Exec(`UPDATE files SET starred=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=?`, value, fileID, user.ID)
	if err != nil {
		writeError(w, 500, "STAR_FAILED", "Unable to update file.")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "FILE_NOT_FOUND", "File not found.")
		return
	}
	var name, accountID string
	_ = a.DB.QueryRow(`SELECT name,connected_account_id FROM files WHERE id=?`, fileID).Scan(&name, &accountID)
	detail := "Removed from starred"
	if value == 1 {
		detail = "Added to starred"
	}
	a.logActivity(r, user.ID, accountID, "file_star", "file", fileID, name, 0, detail)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "starred": value == 1})
}

// starFolder toggles the starred flag on a folder.
func (a *App) starFolder(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	folderID := r.PathValue("id")
	var body struct {
		Starred *bool `json:"starred"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Starred == nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "starred (boolean) is required.")
		return
	}
	value := 0
	if *body.Starred {
		value = 1
	}
	res, err := a.DB.Exec(`UPDATE folders SET starred=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=?`, value, folderID, user.ID)
	if err != nil {
		writeError(w, 500, "STAR_FAILED", "Unable to update folder.")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "FOLDER_NOT_FOUND", "Folder not found.")
		return
	}
	var name string
	_ = a.DB.QueryRow(`SELECT name FROM folders WHERE id=?`, folderID).Scan(&name)
	detail := "Removed from starred"
	if value == 1 {
		detail = "Added to starred"
	}
	a.logActivity(r, user.ID, "", "folder_star", "folder", folderID, name, 0, detail)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "starred": value == 1})
}

// listStarred returns starred files and folders.
func (a *App) listStarred(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)

	files := []map[string]any{}
	rows, err := a.DB.Query(`SELECT f.id,f.name,f.mime_type,f.size_bytes,COALESCE(f.created_at,''),COALESCE(f.updated_at,''),COALESCE(c.email,''),COALESCE(d.name,'')
		FROM files f
		LEFT JOIN connected_accounts c ON c.id=f.connected_account_id
		LEFT JOIN folders d ON d.id=f.folder_id
		WHERE f.user_id=? AND f.status='active' AND f.starred=1
		ORDER BY datetime(f.updated_at) DESC, f.id DESC`, user.ID)
	if err != nil {
		writeError(w, 500, "STARRED_FAILED", "Unable to list starred files: "+err.Error())
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, mimeType, createdAt, updatedAt, email, folder string
		var size int64
		if err := rows.Scan(&id, &name, &mimeType, &size, &createdAt, &updatedAt, &email, &folder); err != nil {
			writeError(w, 500, "STARRED_FAILED", "Unable to read starred files.")
			return
		}
		files = append(files, map[string]any{"id": id, "name": name, "mimeType": mimeType, "sizeBytes": fmt.Sprint(size),
			"createdAt": createdAt, "updatedAt": updatedAt, "accountEmail": email, "folder": folder})
	}

	folders := []map[string]any{}
	frows, err := a.DB.Query(`SELECT id,name,COALESCE(color,''),COALESCE(icon_url,''),COALESCE(updated_at,'')
		FROM folders WHERE user_id=? AND starred=1 AND deleted_at IS NULL ORDER BY datetime(updated_at) DESC, id DESC`, user.ID)
	if err != nil {
		writeError(w, 500, "STARRED_FAILED", "Unable to list starred folders: "+err.Error())
		return
	}
	defer frows.Close()
	for frows.Next() {
		var id, name, color, iconURL, updatedAt string
		if err := frows.Scan(&id, &name, &color, &iconURL, &updatedAt); err != nil {
			continue
		}
		folders = append(folders, map[string]any{"id": id, "name": name, "color": color, "iconUrl": iconURL, "updatedAt": updatedAt})
	}

	writeJSON(w, http.StatusOK, map[string]any{"files": files, "folders": folders, "total": len(files) + len(folders)})
}

// systemHealth reports the operational state: version, uptime, backup freshness, tunnel mode,
// per-account token/sync status and OAuth config quota usage.
func (a *App) systemHealth(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)

	dbPath := dbFilePathFromURL(a.Config.DatabaseURL)
	dbSize := int64(0)
	dbWritable := false
	if dbPath != "" {
		// WAL mode keeps recent writes in <db>-wal, so sum the sidecar files too;
		// reporting only the main file understates the real footprint.
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if st, err := os.Stat(dbPath + suffix); err == nil {
				dbSize += st.Size()
			}
		}
		if f, err := os.OpenFile(dbPath, os.O_RDWR, 0o600); err == nil {
			dbWritable = true
			f.Close()
		}
	}
	backupPath := dbPath + ".bak"
	backupAge := int64(-1)
	backupExists := false
	if dbPath != "" {
		if st, err := os.Stat(backupPath); err == nil {
			backupExists = true
			backupAge = int64(time.Since(st.ModTime()).Seconds())
		}
	}
	tunnelMode := "off"
	if os.Getenv("TUNNEL_TOKEN") != "" {
		tunnelMode = "managed"
	} else if os.Getenv("TUNNEL_ID") != "" {
		tunnelMode = "locally-managed"
	}

	type accountHealth struct {
		ID             string `json:"id"`
		Email          string `json:"email"`
		Status         string `json:"status"`
		LastError      string `json:"lastError"`
		TokenExpiresAt string `json:"tokenExpiresAt"`
		TokenExpiresIn int64  `json:"tokenExpiresInSeconds"`
		FileCount      int    `json:"fileCount"`
		UsedBytes      string `json:"usedBytes"`
		AvailableBytes string `json:"availableBytes"`
		LastSyncedAt   string `json:"lastSyncedAt"`
		SyncAge        int64  `json:"syncAgeSeconds"`
	}
	accounts := []accountHealth{}
	rows, err := a.DB.Query(`SELECT c.id,c.email,c.status,COALESCE(c.last_error,''),COALESCE(c.token_expires_at,''),
		(SELECT COUNT(*) FROM files f WHERE f.connected_account_id=c.id AND f.status='active'),
		COALESCE(s.used_bytes,0),COALESCE(s.available_bytes,0),COALESCE(s.last_synced_at,'')
		FROM connected_accounts c LEFT JOIN storage_accounts s ON s.connected_account_id=c.id
		WHERE c.user_id=? ORDER BY c.email`, user.ID)
	if err != nil {
		writeError(w, 500, "HEALTH_FAILED", "Unable to read accounts.")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ah accountHealth
		var used, avail int64
		var expiresAt, syncedAt string
		if err := rows.Scan(&ah.ID, &ah.Email, &ah.Status, &ah.LastError, &expiresAt, &ah.FileCount, &used, &avail, &syncedAt); err != nil {
			writeError(w, 500, "HEALTH_FAILED", "Unable to read account health.")
			return
		}
		ah.UsedBytes = fmt.Sprint(used)
		ah.AvailableBytes = fmt.Sprint(avail)
		ah.TokenExpiresAt = expiresAt
		ah.TokenExpiresIn = -1
		if t, err := time.Parse(time.RFC3339Nano, expiresAt); err == nil {
			ah.TokenExpiresIn = int64(time.Until(t).Seconds())
		}
		ah.LastSyncedAt = syncedAt
		ah.SyncAge = -1
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, syncedAt); err == nil {
				ah.SyncAge = int64(time.Since(t).Seconds())
				break
			}
		}
		if ah.LastError == "" && ah.TokenExpiresIn >= 0 && ah.TokenExpiresIn < 300 {
			ah.LastError = "Access token expiring soon (auto-refreshes on use)"
		}
		accounts = append(accounts, ah)
	}

	type configHealth struct {
		ID           string `json:"id"`
		Label        string `json:"label"`
		Status       string `json:"status"`
		RequestCount int    `json:"requestCount"`
		WindowStart  string `json:"windowStart"`
		LastUsedAt   string `json:"lastUsedAt"`
	}
	configs := []configHealth{}
	crows, err := a.DB.Query(`SELECT p.id,COALESCE(p.label,''),p.status,COALESCE(q.request_count,0),COALESCE(q.window_start,''),COALESCE(p.last_used_at,'')
		FROM provider_configs p LEFT JOIN provider_config_quota q ON q.provider_config_id=p.id
		WHERE p.user_id=? AND p.provider='google_drive' ORDER BY p.created_at`, user.ID)
	if err == nil {
		defer crows.Close()
		for crows.Next() {
			var ch configHealth
			if crows.Scan(&ch.ID, &ch.Label, &ch.Status, &ch.RequestCount, &ch.WindowStart, &ch.LastUsedAt) == nil {
				configs = append(configs, ch)
			}
		}
	}

	var totalFiles int
	var totalBytes, trashRows int64
	_ = a.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(size_bytes),0) FROM files WHERE user_id=? AND status='active'`, user.ID).Scan(&totalFiles, &totalBytes)
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE user_id=? AND status='deleted'`, user.ID).Scan(&trashRows)

	writeJSON(w, http.StatusOK, map[string]any{
		"version":       buildVersion,
		"startedAt":     processStartedAt.UTC().Format(time.RFC3339),
		"uptimeSeconds": int64(time.Since(processStartedAt).Seconds()),
		"database": map[string]any{
			"path": dbPath, "sizeBytes": fmt.Sprint(dbSize), "writable": dbWritable,
			"backupPath": backupPath, "backupExists": backupExists, "backupAgeSeconds": backupAge,
		},
		"tunnel":       map[string]any{"mode": tunnelMode, "enabled": tunnelMode != "off"},
		"sync":         map[string]any{"intervalMinutes": 5, "quotaThreshold": 8000, "quotaWindowMax": 10000},
		"accounts":     accounts,
		"oauthConfigs": configs,
		"totals":       map[string]any{"files": totalFiles, "bytes": fmt.Sprint(totalBytes), "trashedFiles": trashRows},
	})
}

func (a *App) updateFile(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	fileID := r.PathValue("id")
	var body struct {
		Name     *string `json:"name"`
		FolderID *string `json:"folderId"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid body")
		return
	}
	if body.Name != nil {
		_, _ = a.DB.Exec(`UPDATE files SET name=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND status='active'`, *body.Name, fileID, user.ID)
	}
	if body.FolderID != nil {
		if *body.FolderID == "" {
			_, _ = a.DB.Exec(`UPDATE files SET folder_id=NULL, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND status='active'`, fileID, user.ID)
		} else {
			_, _ = a.DB.Exec(`UPDATE files SET folder_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND status='active'`, *body.FolderID, fileID, user.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// shareFileForTransfer grants a target account read access to a file in another account.
// Returns the permission id so it can be revoked after the copy.
func (a *App) shareFileForTransfer(ctx context.Context, sourceAccountID, providerFileID, targetEmail string) (string, error) {
	accessToken, err := a.getGoogleToken(ctx, sourceAccountID, false)
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]any{"role": "reader", "type": "user", "emailAddress": targetEmail})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.GoogleDriveAPIURL+"/files/"+url.PathEscape(providerFileID)+"/permissions?sendNotificationEmail=false&fields=id", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("share failed (%d): %s", resp.StatusCode, string(body))
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &out)
	return out.ID, nil
}

func (a *App) revokeTransferShare(ctx context.Context, sourceAccountID, providerFileID, permissionID string) {
	if permissionID == "" {
		return
	}
	accessToken, err := a.getGoogleToken(ctx, sourceAccountID, false)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.GoogleDriveAPIURL+"/files/"+url.PathEscape(providerFileID)+"/permissions/"+url.PathEscape(permissionID), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if resp, err := a.HTTPClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

// copyFileAsAccount copies a (shared) Drive file into another account using that account's token.
func (a *App) copyFileAsAccount(ctx context.Context, targetAccountID, providerFileID, name string) (string, error) {
	accessToken, err := a.getGoogleToken(ctx, targetAccountID, false)
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]any{"name": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.GoogleDriveAPIURL+"/files/"+url.PathEscape(providerFileID)+"/copy?fields=id,name,mimeType,size", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("copy failed (%d): %s", resp.StatusCode, string(body))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.ID == "" {
		return "", fmt.Errorf("copy returned no file id")
	}
	return out.ID, nil
}

// deleteDriveFile moves a file to the Drive trash (permanent when already trashed).
func (a *App) deleteDriveFile(ctx context.Context, accountID, providerFileID string) error {
	accessToken, err := a.getGoogleToken(ctx, accountID, false)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.GoogleDriveAPIURL+"/files/"+url.PathEscape(providerFileID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil // already gone
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("drive delete failed (%d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// transferFile moves (or copies) a file between two connected accounts server-side:
// share source -> copy with target token -> revoke share -> optionally delete source.
// No file bytes ever pass through this server.
func (a *App) transferFile(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	fileID := r.PathValue("id")
	var body struct {
		TargetAccountID string `json:"targetAccountId"`
		DeleteSource    bool   `json:"deleteSource"`
	}
	if err := decodeJSON(r, &body); err != nil || body.TargetAccountID == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "targetAccountId is required.")
		return
	}

	var providerFileID, name, mimeType, sourceAccountID string
	var size int64
	err := a.DB.QueryRow(`SELECT f.provider_file_id,f.name,f.mime_type,f.size_bytes,f.connected_account_id FROM files f WHERE f.id=? AND f.user_id=? AND f.status='active' AND f.provider='google_drive'`, fileID, user.ID).Scan(&providerFileID, &name, &mimeType, &size, &sourceAccountID)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "FILE_NOT_FOUND", "File not found.")
		return
	}
	if err != nil {
		writeError(w, 500, "TRANSFER_FAILED", "Unable to load file.")
		return
	}
	if sourceAccountID == body.TargetAccountID {
		writeError(w, http.StatusBadRequest, "SAME_ACCOUNT", "Source and destination account are the same.")
		return
	}

	var targetEmail string
	err = a.DB.QueryRow(`SELECT email FROM connected_accounts WHERE id=? AND user_id=? AND status='connected'`, body.TargetAccountID, user.ID).Scan(&targetEmail)
	if err != nil {
		writeError(w, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "Destination account not found.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	// 1. Share the source file with the destination account (reader).
	permissionID, err := a.shareFileForTransfer(ctx, sourceAccountID, providerFileID, targetEmail)
	if err != nil {
		writeError(w, 502, "TRANSFER_SHARE_FAILED", "Unable to share file with destination account: "+err.Error())
		return
	}
	// 2. Copy it into the destination account (server-side, no bandwidth here).
	newProviderID, err := a.copyFileAsAccount(ctx, body.TargetAccountID, providerFileID, name)
	if err != nil {
		a.revokeTransferShare(ctx, sourceAccountID, providerFileID, permissionID)
		writeError(w, 502, "TRANSFER_COPY_FAILED", "Unable to copy file to destination account: "+err.Error())
		return
	}
	// 3. Clean up the temporary share.
	go a.revokeTransferShare(context.Background(), sourceAccountID, providerFileID, permissionID)

	// 4. Record the new file against the destination account.
	newFileID := randomID()
	if _, err := a.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, newFileID, user.ID, body.TargetAccountID, "google_drive", newProviderID, name, mimeType, size); err != nil {
		writeError(w, 500, "TRANSFER_SAVE_FAILED", "Copied in Drive but could not save locally.")
		return
	}

	// 5. Optional source removal (goes to Drive trash - empty trash to actually free quota).
	sourceDeleted := false
	if body.DeleteSource {
		if err := a.deleteDriveFile(ctx, sourceAccountID, providerFileID); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"status": "partial", "newFileId": newFileID, "moved": false, "message": "Copied, but deleting the source failed: " + err.Error()})
			return
		}
		_, _ = a.DB.Exec(`UPDATE files SET status='deleted', deleted_at=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=?`, fileID, user.ID)
		sourceDeleted = true
	}

	// Refresh both accounts' quota in the background (2 API calls).
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = a.syncAccountQuota(c, sourceAccountID)
		_ = a.syncAccountQuota(c, body.TargetAccountID)
	}()

	mode := "Copied"
	if sourceDeleted {
		mode = "Moved"
	}
	a.logActivity(r, user.ID, sourceAccountID, "file_transfer", "file", fileID, name, size, fmt.Sprintf("%s to %s (server-side)", mode, targetEmail))
	a.notify(user.ID, "Transfer selesai", mode+" "+name+" → "+targetEmail, "truck", "default")
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "newFileId": newFileID, "moved": sourceDeleted, "sourceInTrash": sourceDeleted})
}

// restoreFile undoes a local (soft) delete. The Drive file was never removed by a local delete.
func (a *App) restoreFile(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	fileID := r.PathValue("id")
	res, err := a.DB.Exec(`UPDATE files SET status='active', deleted_at=NULL, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND status='deleted'`, fileID, user.ID)
	if err != nil {
		writeError(w, 500, "RESTORE_FAILED", "Unable to restore file.")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "FILE_NOT_FOUND", "No deleted file with that id.")
		return
	}
	var name, accountID string
	var size int64
	_ = a.DB.QueryRow(`SELECT name,connected_account_id,size_bytes FROM files WHERE id=? AND user_id=?`, fileID, user.ID).Scan(&name, &accountID, &size)
	a.logActivity(r, user.ID, accountID, "file_restore", "file", fileID, name, size, "Restored from trash")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// purgeFile permanently deletes the file in Drive (and locally).
func (a *App) purgeFile(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	fileID := r.PathValue("id")
	var providerFileID, accountID string
	err := a.DB.QueryRow(`SELECT provider_file_id, connected_account_id FROM files WHERE id=? AND user_id=?`, fileID, user.ID).Scan(&providerFileID, &accountID)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "FILE_NOT_FOUND", "File not found.")
		return
	}
	if err != nil {
		writeError(w, 500, "PURGE_FAILED", "Unable to load file.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := a.deleteDriveFile(ctx, accountID, providerFileID); err != nil {
		writeError(w, 502, "PURGE_DRIVE_FAILED", "Drive delete failed: "+err.Error())
		return
	}
	var purgedName string
	var purgedSize int64
	_ = a.DB.QueryRow(`SELECT name,size_bytes FROM files WHERE id=? AND user_id=?`, fileID, user.ID).Scan(&purgedName, &purgedSize)
	_, _ = a.DB.Exec(`UPDATE split_files SET status='incomplete' WHERE id IN (SELECT split_id FROM split_parts WHERE file_id=?)`, fileID)
	_, _ = a.DB.Exec(`DELETE FROM split_parts WHERE file_id=?`, fileID)
	_, _ = a.DB.Exec(`DELETE FROM files WHERE id=? AND user_id=?`, fileID, user.ID)
	a.logActivity(r, user.ID, accountID, "file_purge", "file", fileID, purgedName, purgedSize, "Permanently deleted from Drive")
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = a.syncAccountQuota(c, accountID)
	}()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// emptyAccountTrash permanently empties the Drive trash for one account (frees quota).
func (a *App) emptyAccountTrash(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	accountID := r.PathValue("id")
	var owner string
	if err := a.DB.QueryRow(`SELECT user_id FROM connected_accounts WHERE id=?`, accountID).Scan(&owner); err != nil || owner != user.ID {
		writeError(w, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "Account not found.")
		return
	}
	accessToken, err := a.getGoogleToken(r.Context(), accountID, false)
	if err != nil {
		writeError(w, 500, "EMPTY_TRASH_FAILED", "Unable to read account token.")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodDelete, a.GoogleDriveAPIURL+"/files/trash", nil)
	if err != nil {
		writeError(w, 500, "EMPTY_TRASH_FAILED", "Unable to create Drive request.")
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		writeError(w, 502, "EMPTY_TRASH_FAILED", "Drive request failed.")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeError(w, 502, "EMPTY_TRASH_FAILED", fmt.Sprintf("Drive rejected empty trash (%d): %s", resp.StatusCode, string(body)))
		return
	}
	a.logActivity(r, user.ID, accountID, "trash_empty", "account", accountID, "", 0, "Drive trash emptied")
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = a.syncAccountQuota(c, accountID)
	}()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// findDuplicates groups active files by (name, size) across every connected account.
// Groups with more than one member are potential reclaimable space.
func (a *App) findDuplicates(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	rows, err := a.DB.Query(`SELECT f.id,f.name,f.size_bytes,f.mime_type,COALESCE(f.created_at,''),COALESCE(f.updated_at,''),c.email,COALESCE(d.name,'')
		FROM files f
		JOIN connected_accounts c ON c.id=f.connected_account_id
		LEFT JOIN folders d ON d.id=f.folder_id
		WHERE f.user_id=? AND f.status='active' AND f.size_bytes > 0
		  AND (f.name, f.size_bytes) IN (
			SELECT name, size_bytes FROM files
			WHERE user_id=? AND status='active' AND size_bytes > 0
			GROUP BY name, size_bytes HAVING COUNT(*) > 1
		  )
		ORDER BY f.size_bytes DESC, f.name, c.email`, user.ID, user.ID)
	if err != nil {
		writeError(w, 500, "DUPLICATES_FAILED", "Unable to scan duplicates: "+err.Error())
		return
	}
	defer rows.Close()

	type dupFile struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		SizeBytes    string `json:"sizeBytes"`
		MimeType     string `json:"mimeType"`
		CreatedAt    string `json:"createdAt"`
		UpdatedAt    string `json:"updatedAt"`
		AccountEmail string `json:"accountEmail"`
		Folder       string `json:"folder"`
	}
	groups := map[string]*struct {
		Name        string    `json:"name"`
		SizeBytes   string    `json:"sizeBytes"`
		Count       int       `json:"count"`
		WastedBytes string    `json:"wastedBytes"`
		Files       []dupFile `json:"files"`
	}{}
	var order []string
	var totalWasted int64
	for rows.Next() {
		var id, name, mimeType, createdAt, updatedAt, email, folder string
		var size int64
		if err := rows.Scan(&id, &name, &size, &mimeType, &createdAt, &updatedAt, &email, &folder); err != nil {
			writeError(w, 500, "DUPLICATES_FAILED", "Unable to read duplicates.")
			return
		}
		key := name + "\x00" + fmt.Sprint(size)
		g, ok := groups[key]
		if !ok {
			g = &struct {
				Name        string    `json:"name"`
				SizeBytes   string    `json:"sizeBytes"`
				Count       int       `json:"count"`
				WastedBytes string    `json:"wastedBytes"`
				Files       []dupFile `json:"files"`
			}{Name: name, SizeBytes: fmt.Sprint(size)}
			groups[key] = g
			order = append(order, key)
		}
		g.Files = append(g.Files, dupFile{ID: id, Name: name, SizeBytes: fmt.Sprint(size), MimeType: mimeType, CreatedAt: createdAt, UpdatedAt: updatedAt, AccountEmail: email, Folder: folder})
	}
	if err := rows.Err(); err != nil {
		writeError(w, 500, "DUPLICATES_FAILED", "Unable to read duplicates.")
		return
	}
	out := make([]any, 0, len(order))
	for _, key := range order {
		g := groups[key]
		g.Count = len(g.Files)
		// Copies beyond the first are the reclaimable ones.
		wasted := int64(g.Count-1) * mustInt64(g.SizeBytes)
		g.WastedBytes = fmt.Sprint(wasted)
		totalWasted += wasted
		out = append(out, g)
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out, "groupCount": len(out), "totalWastedBytes": fmt.Sprint(totalWasted)})
}

func mustInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// storageAnalyzer summarises where space goes: per account, by file type, and the largest files.
func (a *App) storageAnalyzer(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)

	type accountRow struct {
		ID             string `json:"id"`
		Email          string `json:"email"`
		FileCount      int    `json:"fileCount"`
		TotalBytes     string `json:"totalBytes"`
		UsedBytes      string `json:"usedBytes"`
		AvailableBytes string `json:"availableBytes"`
	}
	accounts := []accountRow{}
	rows, err := a.DB.Query(`SELECT c.id,c.email,COUNT(f.id),COALESCE(SUM(f.size_bytes),0),COALESCE(s.used_bytes,0),COALESCE(s.available_bytes,0)
		FROM connected_accounts c
		LEFT JOIN files f ON f.connected_account_id=c.id AND f.status='active'
		LEFT JOIN storage_accounts s ON s.connected_account_id=c.id
		WHERE c.user_id=? GROUP BY c.id ORDER BY SUM(f.size_bytes) DESC`, user.ID)
	if err != nil {
		writeError(w, 500, "ANALYZER_FAILED", "Unable to summarise accounts.")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ac accountRow
		var count int
		var total, used, avail int64
		if err := rows.Scan(&ac.ID, &ac.Email, &count, &total, &used, &avail); err != nil {
			writeError(w, 500, "ANALYZER_FAILED", "Unable to summarise accounts.")
			return
		}
		ac.FileCount = count
		ac.TotalBytes = fmt.Sprint(total)
		ac.UsedBytes = fmt.Sprint(used)
		ac.AvailableBytes = fmt.Sprint(avail)
		accounts = append(accounts, ac)
	}

	// Extension breakdown, computed by streaming name+size (cheap and dialect-safe).
	type typeRow struct {
		Label string `json:"label"`
		Bytes string `json:"bytes"`
		Count int    `json:"count"`
	}
	typeBuckets := map[string]*typeRow{}
	var typeOrder []string
	all, err := a.DB.Query(`SELECT name, size_bytes FROM files WHERE user_id=? AND status='active'`, user.ID)
	if err != nil {
		writeError(w, 500, "ANALYZER_FAILED", "Unable to summarise file types.")
		return
	}
	defer all.Close()
	var totalFiles int
	var totalBytes int64
	for all.Next() {
		var name string
		var size int64
		if err := all.Scan(&name, &size); err != nil {
			continue
		}
		totalFiles++
		totalBytes += size
		label := fileTypeLabel(name)
		b, ok := typeBuckets[label]
		if !ok {
			b = &typeRow{Label: label}
			typeBuckets[label] = b
			typeOrder = append(typeOrder, label)
		}
		b.Bytes = fmt.Sprint(mustInt64(b.Bytes) + size)
		b.Count++
	}
	sort.SliceStable(typeOrder, func(i, j int) bool {
		return mustInt64(typeBuckets[typeOrder[i]].Bytes) > mustInt64(typeBuckets[typeOrder[j]].Bytes)
	})
	byType := make([]typeRow, 0, len(typeOrder))
	for _, label := range typeOrder {
		byType = append(byType, *typeBuckets[label])
	}

	type largestFile struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		SizeBytes    string `json:"sizeBytes"`
		MimeType     string `json:"mimeType"`
		AccountEmail string `json:"accountEmail"`
		Folder       string `json:"folder"`
		CreatedAt    string `json:"createdAt"`
	}
	largest := []largestFile{}
	lrows, err := a.DB.Query(`SELECT f.id,f.name,f.size_bytes,f.mime_type,c.email,COALESCE(d.name,''),COALESCE(f.created_at,'')
		FROM files f JOIN connected_accounts c ON c.id=f.connected_account_id
		LEFT JOIN folders d ON d.id=f.folder_id
		WHERE f.user_id=? AND f.status='active' ORDER BY f.size_bytes DESC LIMIT 25`, user.ID)
	if err != nil {
		writeError(w, 500, "ANALYZER_FAILED", "Unable to list largest files.")
		return
	}
	defer lrows.Close()
	for lrows.Next() {
		var lf largestFile
		var size int64
		if err := lrows.Scan(&lf.ID, &lf.Name, &size, &lf.MimeType, &lf.AccountEmail, &lf.Folder, &lf.CreatedAt); err != nil {
			continue
		}
		lf.SizeBytes = fmt.Sprint(size)
		largest = append(largest, lf)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": accounts,
		"byType":   byType,
		"largest":  largest,
		"totals":   map[string]string{"files": fmt.Sprint(totalFiles), "bytes": fmt.Sprint(totalBytes)},
	})
}

// fileTypeLabel buckets a filename into a coarse type for the storage breakdown.
func fileTypeLabel(name string) string {
	dot := strings.LastIndex(name, ".")
	if dot < 0 || dot == len(name)-1 {
		return "Other"
	}
	ext := strings.ToLower(name[dot+1:])
	switch ext {
	case "jpg", "jpeg", "png", "gif", "webp", "bmp", "svg", "heic", "avif":
		return "Images"
	case "mp4", "mkv", "mov", "avi", "webm", "m4v", "flv":
		return "Video"
	case "mp3", "wav", "flac", "m4a", "aac", "ogg", "opus":
		return "Audio"
	case "zip", "rar", "7z", "tar", "gz", "bz2", "xz":
		return "Archives"
	case "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "md", "csv", "json", "xml", "odt", "ods":
		return "Documents"
	case "apk", "exe", "msi", "deb", "rpm", "dmg", "iso", "appimage":
		return "Installers"
	case "js", "ts", "go", "py", "java", "kt", "c", "cpp", "rs", "sh", "html", "css", "sql", "yaml", "yml":
		return "Code"
	}
	return "Other"
}

func (a *App) deleteFile(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	fileID := r.PathValue("id")
	_, _ = a.DB.Exec(`UPDATE files SET status='deleted', deleted_at=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=?`, fileID, user.ID)
	var name, accountID string
	var size int64
	_ = a.DB.QueryRow(`SELECT name,connected_account_id,size_bytes FROM files WHERE id=? AND user_id=?`, fileID, user.ID).Scan(&name, &accountID, &size)
	_, _ = a.DB.Exec(`UPDATE split_files SET status='incomplete' WHERE id IN (SELECT split_id FROM split_parts WHERE file_id=?)`, fileID)
	a.logActivity(r, user.ID, accountID, "file_delete", "file", fileID, name, size, "Moved to trash")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) batchUpdateFiles(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		FileIDs  []string `json:"fileIds"`
		FolderID *string  `json:"folderId"`
	}
	if err := decodeJSON(r, &body); err != nil || len(body.FileIDs) == 0 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid body")
		return
	}
	if body.FolderID != nil {
		for _, id := range body.FileIDs {
			if *body.FolderID == "" {
				_, _ = a.DB.Exec(`UPDATE files SET folder_id=NULL, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND status='active'`, id, user.ID)
			} else {
				_, _ = a.DB.Exec(`UPDATE files SET folder_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND status='active'`, *body.FolderID, id, user.ID)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) batchDeleteFiles(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var body struct {
		FileIDs []string `json:"fileIds"`
	}
	if err := decodeJSON(r, &body); err != nil || len(body.FileIDs) == 0 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid body")
		return
	}
	for _, id := range body.FileIDs {
		_, _ = a.DB.Exec(`UPDATE files SET status='deleted', deleted_at=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=?`, id, user.ID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) updateFolder(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	folderID := r.PathValue("id")
	var body struct {
		Name     *string `json:"name"`
		Color    *string `json:"color"`
		IconUrl  *string `json:"iconUrl"`
		ParentID *string `json:"parentId"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid body")
		return
	}
	if body.Name != nil {
		_, _ = a.DB.Exec(`UPDATE folders SET name=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND deleted_at IS NULL`, *body.Name, folderID, user.ID)
	}
	if body.Color != nil {
		_, _ = a.DB.Exec(`UPDATE folders SET color=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND deleted_at IS NULL`, *body.Color, folderID, user.ID)
	}
	if body.ParentID != nil {
		if *body.ParentID == "" {
			_, _ = a.DB.Exec(`UPDATE folders SET parent_id=NULL, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND deleted_at IS NULL`, folderID, user.ID)
		} else {
			_, _ = a.DB.Exec(`UPDATE folders SET parent_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND deleted_at IS NULL`, *body.ParentID, folderID, user.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) deleteFolder(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	folderID := r.PathValue("id")
	_, _ = a.DB.Exec(`UPDATE folders SET deleted_at=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=?`, folderID, user.ID)
	// Optionally mark all files inside as deleted as well, or leave them unreachable via list but physically there.
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) batchDownloadZip(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	var fileIDs []string

	if r.Header.Get("Content-Type") == "application/json" {
		var body struct {
			FileIDs []string `json:"fileIds"`
		}
		if err := decodeJSON(r, &body); err == nil {
			fileIDs = body.FileIDs
		}
	} else {
		// Form POST
		val := r.FormValue("fileIds")
		_ = json.Unmarshal([]byte(val), &fileIDs)
	}

	if len(fileIDs) == 0 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid fileIds")
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="pandrive-download.zip"`)

	zw := zip.NewWriter(w)
	var errorLog strings.Builder

	for _, fileID := range fileIDs {
		var provider, providerFileID, accID, fileName, mimeType string
		err := a.DB.QueryRow(`SELECT provider, provider_file_id, connected_account_id, name, mime_type FROM files WHERE id=? AND user_id=? AND status='active'`, fileID, user.ID).Scan(&provider, &providerFileID, &accID, &fileName, &mimeType)
		if err != nil {
			errorLog.WriteString(fmt.Sprintf("File ID %s: Not found in local database\n", fileID))
			continue
		}

		if strings.HasPrefix(mimeType, "application/vnd.google-apps.") {
			errorLog.WriteString(fmt.Sprintf("File %s: Google native document formats (docs, sheets, forms) cannot be downloaded directly via ZIP.\n", fileName))
			continue
		}

		if provider == "google_drive" {
			accessToken, err := a.getGoogleToken(r.Context(), accID, false)
			if err != nil {
				errorLog.WriteString(fmt.Sprintf("File %s: Account token missing or expired (%v)\n", fileName, err))
				continue
			}
			url := fmt.Sprintf("%s/files/%s?alt=media", a.GoogleDriveAPIURL, providerFileID)
			req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
			req.Header.Set("Authorization", "Bearer "+accessToken)
			resp, err := a.HTTPClient.Do(req)
			if err == nil && resp.StatusCode == http.StatusUnauthorized {
				if resp != nil {
					resp.Body.Close()
				}
				accessToken, err = a.getGoogleToken(r.Context(), accID, true)
				if err == nil {
					req, _ = http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
					req.Header.Set("Authorization", "Bearer "+accessToken)
					resp, err = a.HTTPClient.Do(req)
				}
			}
			if err != nil {
				errorLog.WriteString(fmt.Sprintf("File %s: Google API request failed (%v)\n", fileName, err))
				continue
			}
			if resp.StatusCode != http.StatusOK {
				bodyBytes, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				errorLog.WriteString(fmt.Sprintf("File %s: Google API error %d - %s\n", fileName, resp.StatusCode, string(bodyBytes)))
				continue
			}

			f, err := zw.Create(fileName)
			if err == nil {
				io.Copy(f, resp.Body)
			}
			resp.Body.Close()
		}
	}

	if errorLog.Len() > 0 {
		f, _ := zw.Create("pandrive-errors.txt")
		io.WriteString(f, errorLog.String())
	}
	zw.Close()
}
