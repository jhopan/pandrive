package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Public links must be created in Drive, recorded locally, listed, and revocable.
func TestPublicLinkCreateListRevoke(t *testing.T) {
	app := newTestApp(t)

	var mu sync.Mutex
	var created, deleted int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/permissions"):
			body := make([]byte, r.ContentLength)
			r.Body.Read(body)
			if !strings.Contains(string(body), `"anyone"`) {
				t.Errorf("permission body = %s, want type anyone", body)
			}
			created++
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "perm-99"})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"webViewLink": "https://drive.google.com/file/d/pid/view"})
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/permissions/"):
			deleted++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	app.GoogleDriveAPIURL = api.URL + "/drive/v3"

	token, user := registerAndLogin(t, app, "share@example.test")
	_, _ = app.DB.Exec(`INSERT INTO provider_configs (id,user_id,provider,client_id_encrypted,client_secret_encrypted,redirect_uri) VALUES (?,?,?,?,?,?)`, "cfg", user.ID, "google_drive", app.encrypt("cid"), app.encrypt("cs"), "http://localhost/cb")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes,provider_config_id) VALUES (?,?,?,?,?,?,?,?,?,?)`, "acc", user.ID, "google_drive", "p", "share@example.test", app.encrypt("tok"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]", "cfg")
	_, _ = app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, "f1", user.ID, "acc", "google_drive", "pid", "report.pdf", "application/pdf", 999)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, reader)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		return w
	}

	w := call(http.MethodPost, "/files/f1/public-link", "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("create share = %d: %s", w.Code, w.Body.String())
	}
	var createdResp struct {
		URL     string `json:"url"`
		ShareID string `json:"shareId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &createdResp)
	if createdResp.URL != "https://drive.google.com/file/d/pid/view" || createdResp.ShareID == "" {
		t.Fatalf("share response = %+v", createdResp)
	}

	w = call(http.MethodGet, "/shares", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list shares = %d", w.Code)
	}
	var list struct {
		Shares []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"shares"`
		Total int `json:"total"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if list.Total != 1 || list.Shares[0].Name != "report.pdf" {
		t.Fatalf("shares = %+v", list)
	}

	w = call(http.MethodDelete, "/shares/"+createdResp.ShareID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", w.Code, w.Body.String())
	}
	// The Drive permission must actually be deleted, and the list must go empty.
	mu.Lock()
	if created != 1 || deleted != 1 {
		t.Fatalf("drive calls created=%d deleted=%d, want 1/1", created, deleted)
	}
	mu.Unlock()
	var revoked string
	_ = app.DB.QueryRow(`SELECT COALESCE(revoked_at,'') FROM share_links WHERE id=?`, createdResp.ShareID).Scan(&revoked)
	if revoked == "" {
		t.Fatal("share not marked revoked")
	}
	w = call(http.MethodGet, "/shares", "")
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if list.Total != 0 {
		t.Fatalf("shares after revoke = %d, want 0", list.Total)
	}
}

// Queue: list, filter, cancel a running upload, refuse removing a running one, remove a cancelled one.
func TestUploadQueueLifecycle(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "queue@example.test")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "acc", user.ID, "google_drive", "p", "queue@example.test", "[]")
	_, _ = app.DB.Exec(`INSERT INTO upload_sessions (id,user_id,target_connected_account_id,file_name,mime_type,size_bytes,status) VALUES (?,?,?,?,?,?,?)`, "u1", user.ID, "acc", "big.iso", "application/octet-stream", 5000, "uploading")
	_, _ = app.DB.Exec(`INSERT INTO upload_sessions (id,user_id,target_connected_account_id,file_name,mime_type,size_bytes,status,error_message) VALUES (?,?,?,?,?,?,?,?)`, "u2", user.ID, "acc", "broken.zip", "application/zip", 700, "failed", "network reset")

	rebuild := func(method, path string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w, out
	}

	w, out := rebuild(http.MethodGet, "/api/uploads/queue")
	if w.Code != http.StatusOK {
		t.Fatalf("queue = %d: %s", w.Code, w.Body.String())
	}
	if out["total"].(float64) != 2 {
		t.Fatalf("queue total = %v, want 2", out["total"])
	}
	counts, _ := out["counts"].(map[string]any)
	if counts["uploading"].(float64) != 1 || counts["failed"].(float64) != 1 {
		t.Fatalf("counts = %v", counts)
	}

	if _, out := rebuild(http.MethodGet, "/api/uploads/queue?status=uploading"); out["total"].(float64) != 1 {
		t.Fatalf("filtered total = %v", out["total"])
	}

	// A running upload cannot be removed, only cancelled.
	if w, _ := rebuild(http.MethodDelete, "/uploads/queue/u1"); w.Code != http.StatusBadRequest {
		t.Fatalf("remove running = %d, want 400", w.Code)
	}
	if w, _ := rebuild(http.MethodPost, "/uploads/queue/u1/cancel"); w.Code != http.StatusOK {
		t.Fatalf("cancel = %d: %s", w.Code, w.Body.String())
	}
	var status string
	_ = app.DB.QueryRow(`SELECT status FROM upload_sessions WHERE id='u1'`).Scan(&status)
	if status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", status)
	}
	// Cancelling twice is not allowed.
	if w, _ := rebuild(http.MethodPost, "/uploads/queue/u1/cancel"); w.Code != http.StatusBadRequest {
		t.Fatalf("double cancel = %d, want 400", w.Code)
	}
	if w, _ := rebuild(http.MethodDelete, "/uploads/queue/u1"); w.Code != http.StatusOK {
		t.Fatalf("remove cancelled = %d: %s", w.Code, w.Body.String())
	}
	if _, out := rebuild(http.MethodGet, "/api/uploads/queue"); out["total"].(float64) != 1 {
		t.Fatalf("queue after remove = %v", out["total"])
	}
}

// Health must report runtime, database, backup, tunnel, account and config state.
func TestSystemHealthReport(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "health@example.test")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes,status) VALUES (?,?,?,?,?,?,?)`, "acc", user.ID, "google_drive", "p", "health@example.test", "[]", "connected")
	_, _ = app.DB.Exec(`INSERT INTO storage_accounts (id,connected_account_id,total_bytes,used_bytes,available_bytes,trash_bytes,last_synced_at) VALUES (?,?,?,?,?,?,?)`, "sa", "acc", 100, 40, 60, 0, "2026-01-01T00:00:00Z")
	_, _ = app.DB.Exec(`INSERT INTO provider_configs (id,user_id,provider,client_id_encrypted,client_secret_encrypted,redirect_uri,label) VALUES (?,?,?,?,?,?,?)`, "cfg", user.ID, "google_drive", app.encrypt("cid"), app.encrypt("cs"), "http://localhost/cb", "Primary")
	_, _ = app.DB.Exec(`INSERT INTO provider_config_quota (id,provider_config_id,request_count,window_start) VALUES (?,?,?,?)`, "q1", "cfg", 120, "2026-01-01T00:00:00Z")
	_, _ = app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, "f1", user.ID, "acc", "google_drive", "pid", "a.txt", "text/plain", 1500)

	req := httptest.NewRequest(http.MethodGet, "/system/health", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("health = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Version  string `json:"version"`
		Database struct {
			Writable      bool   `json:"writable"`
			BackupAgeSecs int64  `json:"backupAgeSeconds"`
			BackupExists  bool   `json:"backupExists"`
			SizeBytes     string `json:"sizeBytes"`
			Path          string `json:"path"`
		} `json:"database"`
		Tunnel struct {
			Enabled bool   `json:"enabled"`
			Mode    string `json:"mode"`
		} `json:"tunnel"`
		Accounts []struct {
			Email     string `json:"email"`
			Status    string `json:"status"`
			FileCount int    `json:"fileCount"`
			UsedBytes string `json:"usedBytes"`
		} `json:"accounts"`
		OAuthConfigs []struct {
			Label        string `json:"label"`
			RequestCount int    `json:"requestCount"`
		} `json:"oauthConfigs"`
		Totals struct {
			Files int    `json:"files"`
			Bytes string `json:"bytes"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// In-memory test databases have no file on disk, so only the version is asserted here;
	// the file/backup fields are covered by the live check against a real database.
	if out.Version == "" || out.Database.SizeBytes == "" {
		t.Fatalf("runtime fields missing: %+v", out)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].FileCount != 1 || out.Accounts[0].UsedBytes != "40" {
		t.Fatalf("accounts = %+v", out.Accounts)
	}
	if len(out.OAuthConfigs) != 1 || out.OAuthConfigs[0].Label != "Primary" || out.OAuthConfigs[0].RequestCount != 120 {
		t.Fatalf("configs = %+v", out.OAuthConfigs)
	}
	if out.Totals.Files != 1 || out.Totals.Bytes != "1500" {
		t.Fatalf("totals = %+v", out.Totals)
	}
	if out.Tunnel.Enabled || out.Tunnel.Mode != "off" {
		t.Fatalf("tunnel = %+v, want disabled by default", out.Tunnel)
	}
}
