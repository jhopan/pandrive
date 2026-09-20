package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Folder mirroring: Drive folder hierarchy must land in the folders table with parent links,
// and files must be attached to their mirrored folder.
func TestSyncMirrorsFoldersAndAttachesFiles(t *testing.T) {
	app := newTestApp(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/drive/v3/files" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
			{"id": "fld-parent", "name": "Projects", "mimeType": "application/vnd.google-apps.folder", "modifiedTime": "2026-09-01T00:00:00Z", "parents": []string{"0ROOT"}},
			{"id": "fld-child", "name": "2026", "mimeType": "application/vnd.google-apps.folder", "modifiedTime": "2026-09-01T00:00:00Z", "parents": []string{"fld-parent"}},
			{"id": "doc-1", "name": "plan.txt", "mimeType": "text/plain", "size": "42", "createdTime": "2026-09-01T00:00:00Z", "modifiedTime": "2026-09-01T01:00:00Z", "parents": []string{"fld-child"}},
		}})
	}))
	defer api.Close()
	app.GoogleDriveAPIURL = api.URL + "/drive/v3"

	token, user := registerAndLogin(t, app, "folders@example.test")
	_, _ = app.DB.Exec(`INSERT INTO provider_configs (id,user_id,provider,client_id_encrypted,client_secret_encrypted,redirect_uri) VALUES (?,?,?,?,?,?)`, "config", user.ID, "google_drive", app.encrypt("client"), app.encrypt("secret"), "http://localhost:4000/callback")
	_, err := app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes,provider_config_id) VALUES (?,?,?,?,?,?,?,?,?,?)`, "account", user.ID, "google_drive", "google", "folders@example.test", app.encrypt("access"), app.encrypt("refresh"), "2099-01-01T00:00:00Z", "[]", "config")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/files/sync-google", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("sync = %d: %s", w.Code, w.Body.String())
	}

	// Both folders mirrored.
	var parentLocal, childLocal string
	if err := app.DB.QueryRow(`SELECT id FROM folders WHERE provider_folder_id='fld-parent'`).Scan(&parentLocal); err != nil {
		t.Fatalf("parent folder not mirrored: %v", err)
	}
	if err := app.DB.QueryRow(`SELECT id FROM folders WHERE provider_folder_id='fld-child'`).Scan(&childLocal); err != nil {
		t.Fatalf("child folder not mirrored: %v", err)
	}
	// Child points at mirrored parent; top-level parent stays root (NULL parent_id).
	var childParentID any
	if err := app.DB.QueryRow(`SELECT parent_id FROM folders WHERE id=?`, childLocal).Scan(&childParentID); err != nil {
		t.Fatal(err)
	}
	if childParentID != parentLocal {
		t.Fatalf("child parent_id = %v, want %v", childParentID, parentLocal)
	}
	var rootParent any
	_ = app.DB.QueryRow(`SELECT parent_id FROM folders WHERE id=?`, parentLocal).Scan(&rootParent)
	if rootParent != nil {
		t.Fatalf("top-level parent_id = %v, want NULL", rootParent)
	}
	// File attached to the nested folder.
	var fileFolder any
	if err := app.DB.QueryRow(`SELECT folder_id FROM files WHERE provider_file_id='doc-1'`).Scan(&fileFolder); err != nil {
		t.Fatalf("file not synced: %v", err)
	}
	if fileFolder != childLocal {
		t.Fatalf("file folder_id = %v, want %v", fileFolder, childLocal)
	}
}
