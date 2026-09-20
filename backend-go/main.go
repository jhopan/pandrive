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
CREATE INDEX IF NOT EXISTS activity_log_action_idx ON activity_log(user_id, action);
`)
	return err
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
	if _, err = a.DB.Exec(`INSERT INTO users (id,name,email,password_hash) VALUES (?,?,?,?)`, randomID(), "Administrator", "admin@gmail.com", string(hash)); err != nil {
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
	mux.HandleFunc("GET /system/google-config", a.requireAuth(a.getGoogleConfig))
	mux.HandleFunc("POST /system/google-config", a.requireAuth(a.saveGoogleConfig))
	mux.HandleFunc("DELETE /system/google-config/{id}", a.requireAuth(a.deleteGoogleConfig))
	mux.HandleFunc("PATCH /system/google-config/{id}", a.requireAuth(a.updateGoogleConfig))
	mux.HandleFunc("POST /system/update", a.requireAuth(a.systemUpdate))
	mux.HandleFunc("GET /system/version", a.requireAuth(a.updateInfoHandler))
	mux.HandleFunc("GET /activity", a.requireAuth(a.listActivity))
	mux.HandleFunc("GET /connected-accounts", a.requireAuth(a.listAccounts))
	mux.HandleFunc("GET /connected-accounts/google/connect-url", a.requireAuth(a.googleConnectURL))
	mux.HandleFunc("GET /connected-accounts/google/callback", a.googleCallback)
	mux.HandleFunc("GET /storage/summary", a.requireAuth(a.storageSummary))
	mux.HandleFunc("GET /storage/breakdown", a.requireAuth(a.storageBreakdown))
	mux.HandleFunc("GET /storage/routing-policy", a.requireAuth(a.getRoutingPolicy))
	mux.HandleFunc("PATCH /storage/routing-policy", a.requireAuth(a.updateRoutingPolicy))
	mux.HandleFunc("POST /connected-accounts/{id}/sync-quota", a.requireAuth(a.syncQuota))
	mux.HandleFunc("GET /folders", a.requireAuth(a.listFolders))
	mux.HandleFunc("POST /folders", a.requireAuth(a.createFolder))
	mux.HandleFunc("GET /files", a.requireAuth(a.listFiles))
	mux.HandleFunc("POST /files/sync-google", a.requireAuth(a.syncGoogleFiles))
	mux.HandleFunc("GET /files/{id}/view-url", a.requireAuth(a.viewFileUrl))
	mux.HandleFunc("GET /files/{id}/download", a.requireAuth(a.downloadFile))
	mux.HandleFunc("POST /files/{id}/share", a.requireAuth(a.shareFileUrl))
	mux.HandleFunc("POST /files/{id}/public-permission", a.requireAuth(a.publicPermission))
	mux.HandleFunc("POST /files/batch-download", a.requireAuth(a.batchDownloadZip))
	mux.HandleFunc("GET /files/duplicates", a.requireAuth(a.findDuplicates))
	mux.HandleFunc("GET /storage/analyzer", a.requireAuth(a.storageAnalyzer))
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
	if err := a.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err == nil && count > 0 {
		writeError(w, http.StatusForbidden, "REGISTRATION_DISABLED", "Registration is disabled. Use the initial administrator account.")
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
	err := a.DB.QueryRow(`SELECT id,name,email,password_hash FROM users WHERE email = ? AND status = 'active'`, strings.ToLower(strings.TrimSpace(body.Email))).Scan(&user.ID, &user.Name, &user.Email, &hash)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password)) != nil {
		a.loginRecordFailure(ip)
		// Attribute the attempt to the account when it exists, so the owner sees it in their audit trail.
		attempted := strings.ToLower(strings.TrimSpace(body.Email))
		var ownerID string
		if a.DB.QueryRow(`SELECT id FROM users WHERE email=?`, attempted).Scan(&ownerID) == nil {
			a.logActivity(r, ownerID, "", "login_failed", "user", ownerID, attempted, 0, "Invalid credentials")
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

func (a *App) listFolders(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	parentID := r.URL.Query().Get("parentId")
	accountID := r.URL.Query().Get("accountId")
	query := `SELECT id,name,parent_id,color,created_at,updated_at FROM folders WHERE user_id=? AND deleted_at IS NULL`
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
	folders := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, color, createdAt, updatedAt string
		var parent sql.NullString
		if err := rows.Scan(&id, &name, &parent, &color, &createdAt, &updatedAt); err != nil {
			writeError(w, 500, "FOLDERS_FAILED", "Unable to read folders.")
			return
		}
		var parentID any
		if parent.Valid {
			parentID = parent.String
		}
		folders = append(folders, map[string]any{"id": id, "name": name, "parentId": parentID, "color": color, "createdAt": createdAt, "updatedAt": updatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
}

func (a *App) listFiles(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	folderID := r.URL.Query().Get("folderId")
	accountID := r.URL.Query().Get("accountId")
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	// status=deleted powers the Trash page; default lists active files only.
	statusFilter := "active"
	if r.URL.Query().Get("status") == "deleted" {
		statusFilter = "deleted"
	}
	query := `SELECT f.id,f.name,f.mime_type,f.size_bytes,f.provider_file_id,f.folder_id,f.created_at,f.updated_at,c.id,c.email,c.provider,COALESCE(d.name,''),COALESCE(f.deleted_at,'') FROM files f JOIN connected_accounts c ON c.id=f.connected_account_id LEFT JOIN folders d ON d.id=f.folder_id WHERE f.user_id=? AND f.status='` + statusFilter + `'`
	args := []any{user.ID}
	if folderID != "" {
		query += ` AND f.folder_id=?`
		args = append(args, folderID)
	}
	if accountID != "" {
		query += ` AND f.connected_account_id=?`
		args = append(args, accountID)
	}
	if search != "" {
		query += ` AND f.name LIKE ?`
		args = append(args, "%"+search+"%")
	}
	query += ` ORDER BY f.created_at DESC`
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
		var folderID sql.NullString
		if err := rows.Scan(&id, &name, &mimeType, &size, &providerFileID, &folderID, &createdAt, &updatedAt, &accountID, &email, &provider, &folderName, &deletedAt); err != nil {
			writeError(w, 500, "FILES_FAILED", "Unable to read files: "+err.Error())
			return
		}
		var folder any
		if folderID.Valid {
			folder = map[string]string{"id": folderID.String, "name": folderName}
		}
		files = append(files, map[string]any{"id": id, "name": name, "mimeType": mimeType, "sizeBytes": fmt.Sprint(size), "providerFileId": providerFileID, "folder": folder, "createdAt": createdAt, "updatedAt": updatedAt, "deletedAt": deletedAt, "connectedAccount": map[string]string{"id": accountID, "email": email, "provider": provider}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
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
	_, err = a.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, randomID(), user.ID, accountID, "google_drive", uploaded.ID, uploaded.Name, uploaded.MIMEType, size)
	if err != nil {
		writeError(w, 500, "UPLOAD_CHUNK_FAILED", "Unable to save uploaded file.")
		return
	}
	// Approximate local quota update; corrected at next quota sync.
	_, _ = a.DB.Exec(`UPDATE storage_accounts SET available_bytes=MAX(0, available_bytes-?), used_bytes=used_bytes+? WHERE connected_account_id=?`, size, size, accountID)
	_, _ = a.DB.Exec(`UPDATE upload_sessions SET status='completed',completed_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), id)
	a.logActivity(r, user.ID, accountID, "file_upload", "file", uploaded.ID, uploaded.Name, size, "Uploaded to Drive")
	writeJSON(w, 200, map[string]string{"status": "completed"})
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

func (a *App) downloadFile(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(authUser)
	fileID := r.PathValue("id")
	var providerFileID, name, mimeType, accountID string
	err := a.DB.QueryRow(`SELECT f.provider_file_id,f.name,f.mime_type,c.id FROM files f JOIN connected_accounts c ON c.id=f.connected_account_id WHERE f.id=? AND f.user_id=? AND f.status='active' AND c.provider='google_drive'`, fileID, user.ID).Scan(&providerFileID, &name, &mimeType, &accountID)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "FILE_NOT_FOUND", "File not found.")
		return
	}
	if err != nil {
		writeError(w, 500, "DOWNLOAD_FAILED", "Unable to load file.")
		return
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
	}
	// Paginated listing: loop through all pages via nextPageToken (Google caps 1000/page).
	var items []driveItem
	seen := map[string]bool{}
	pageToken := ""
	for {
		listURL := a.GoogleDriveAPIURL + `/files?pageSize=1000&orderBy=modifiedTime%20desc&fields=nextPageToken,files(id,name,mimeType,size,createdTime,modifiedTime,trashed,parents)`
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
			_, err = a.DB.Exec(`UPDATE files SET name=?,mime_type=?,size_bytes=?,folder_id=?,updated_at=? WHERE user_id=? AND connected_account_id=? AND provider_file_id=?`, item.Name, item.MIMEType, size, nullIfEmpty(folderLocal), item.Modified, userID, accountID, item.ID)
			if err == nil {
				updated++
			}
		} else {
			_, err = a.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes,folder_id,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, randomID(), userID, accountID, "google_drive", item.ID, item.Name, item.MIMEType, size, nullIfEmpty(folderLocal), item.Created, item.Modified)
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
	if body.Password != "" {
		if len(body.Password) < 8 {
			writeError(w, http.StatusBadRequest, "WEAK_PASSWORD", "Password must be at least 8 characters.")
			return
		}
		hash, _ := bcrypt.GenerateFromPassword([]byte(body.Password), 10)
		_, err := a.DB.Exec(`UPDATE users SET name=?, email=?, password_hash=? WHERE id=?`, body.Name, body.Email, hash, user.ID)
		if err != nil {
			writeError(w, http.StatusConflict, "EMAIL_IN_USE", "Email already in use.")
			return
		}
	} else {
		_, err := a.DB.Exec(`UPDATE users SET name=?, email=? WHERE id=?`, body.Name, body.Email, user.ID)
		if err != nil {
			writeError(w, http.StatusConflict, "EMAIL_IN_USE", "Email already in use.")
			return
		}
	}
	detail := "Profile updated"
	if body.Password != "" {
		detail = "Profile and password updated"
	}
	a.logActivity(r, user.ID, "", "account_update", "user", user.ID, body.Email, 0, detail)
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

func (a *App) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
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
	app := &App{DB: db, Config: config, HTTPClient: http.DefaultClient, GoogleEndpoint: google.Endpoint, GoogleUserInfoURL: "https://www.googleapis.com/oauth2/v2/userinfo", GoogleDriveAPIURL: "https://www.googleapis.com/drive/v3", GoogleUploadAPIURL: "https://www.googleapis.com/upload/drive/v3/files", loginFails: map[string]*loginFail{}}
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

func (a *App) viewFileUrl(w http.ResponseWriter, r *http.Request) {
	// For now, return empty URL so frontend falls back to stream preview
	writeJSON(w, http.StatusOK, map[string]string{"url": ""})
}

func (a *App) shareFileUrl(w http.ResponseWriter, r *http.Request) {
	// Future: generate signed URL or public link
	writeJSON(w, http.StatusOK, map[string]string{"url": a.Config.FrontendURL + "/files/" + r.PathValue("id")})
}

func (a *App) publicPermission(w http.ResponseWriter, r *http.Request) {
	// Future: Google Drive API permissions insert
	writeJSON(w, http.StatusOK, map[string]string{"url": "https://drive.google.com/open?id=not_implemented_yet"})
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
