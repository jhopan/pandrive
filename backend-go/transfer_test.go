package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Transfer must run entirely server-side: share the source with the destination account,
// copy using the destination token, then record the new file against the destination.
func TestTransferFileCopiesViaDriveServerSide(t *testing.T) {
	app := newTestApp(t)

	var mu sync.Mutex
	var shareCalls, copyCalls, revokeCalls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/permissions"):
			shareCalls++
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "perm-1"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/copy"):
			copyCalls++
			if r.Header.Get("Authorization") != "Bearer dest-token" {
				t.Errorf("copy used %q token, want destination token", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "copied-id", "name": "report.pdf"})
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/permissions/"):
			revokeCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	app.GoogleDriveAPIURL = api.URL + "/drive/v3"

	token, user := registerAndLogin(t, app, "transfer@example.test")
	_, _ = app.DB.Exec(`INSERT INTO provider_configs (id,user_id,provider,client_id_encrypted,client_secret_encrypted,redirect_uri) VALUES (?,?,?,?,?,?)`, "cfg", user.ID, "google_drive", app.encrypt("cid"), app.encrypt("secret"), "http://localhost:4000/cb")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes,provider_config_id) VALUES (?,?,?,?,?,?,?,?,?,?)`, "src-acc", user.ID, "google_drive", "src", "src@example.test", app.encrypt("src-token"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]", "cfg")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes,provider_config_id) VALUES (?,?,?,?,?,?,?,?,?,?)`, "dst-acc", user.ID, "google_drive", "dst", "dst@example.test", app.encrypt("dest-token"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]", "cfg")
	_, err := app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, "file-1", user.ID, "src-acc", "google_drive", "drive-file", "report.pdf", "application/pdf", 2048)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/files/file-1/transfer", strings.NewReader(`{"targetAccountId":"dst-acc","deleteSource":false}`))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("transfer = %d: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if shareCalls != 1 || copyCalls != 1 {
		t.Fatalf("share=%d copy=%d, want 1/1", shareCalls, copyCalls)
	}
	var count int
	var providerID string
	if err := app.DB.QueryRow(`SELECT COUNT(*), MAX(provider_file_id) FROM files WHERE connected_account_id='dst-acc'`).Scan(&count, &providerID); err != nil {
		t.Fatal(err)
	}
	if count != 1 || providerID != "copied-id" {
		t.Fatalf("destination rows = %d provider = %q", count, providerID)
	}
	// Source must stay active when deleteSource=false.
	var status string
	if err := app.DB.QueryRow(`SELECT status FROM files WHERE id='file-1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("source status = %q, want active", status)
	}
}

// A local delete only hides the file; restore must bring it back.
func TestDeleteThenRestoreFile(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "restore@example.test")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "acc", user.ID, "google_drive", "p", "restore@example.test", "[]")
	_, _ = app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, "f1", user.ID, "acc", "google_drive", "pid", "a.txt", "text/plain", 10)

	del := httptest.NewRequest(http.MethodDelete, "/files/f1", nil)
	del.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, del)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	var status string
	_ = app.DB.QueryRow(`SELECT status FROM files WHERE id='f1'`).Scan(&status)
	if status != "deleted" {
		t.Fatalf("status after delete = %q", status)
	}

	restore := httptest.NewRequest(http.MethodPost, "/files/f1/restore", nil)
	restore.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	app.Router().ServeHTTP(w, restore)
	if w.Code != http.StatusOK {
		t.Fatalf("restore = %d: %s", w.Code, w.Body.String())
	}
	_ = app.DB.QueryRow(`SELECT status FROM files WHERE id='f1'`).Scan(&status)
	if status != "active" {
		t.Fatalf("status after restore = %q", status)
	}
}
