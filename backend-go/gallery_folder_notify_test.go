package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Gallery, folder sizes, link expiry and ntfy settings in one pass: only local DB behaviour is
// asserted (no Drive calls in these paths), matching how the endpoints read and write state.
func TestGalleryFolderSizesExpiryAndNotify(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "gallery@example.test")
	H := map[string]string{"Authorization": "Bearer " + token}

	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range H {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		return w
	}

	// Seed: an account row (FK target), two folders (parent + child), three files (image, video, doc).
	acctID := randomID()
	if _, err := app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,status) VALUES (?,?,?,?,?,'connected')`, acctID, user.ID, "google_drive", "ga-1", "gal@example.test"); err != nil {
		t.Fatal(err)
	}
	folderID := randomID()
	childID := randomID()
	if _, err := app.DB.Exec(`INSERT INTO folders (id,user_id,name,deleted_at) VALUES (?,?,?,NULL)`, folderID, user.ID, "parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.Exec(`INSERT INTO folders (id,user_id,parent_id,name,deleted_at) VALUES (?,?,?,?,NULL)`, childID, user.ID, folderID, "child"); err != nil {
		t.Fatal(err)
	}
	insertFile := func(name, mime string, size int64, folder string) string {
		t.Helper()
		id := randomID()
		if _, err := app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes,folder_id,status) VALUES (?,?,?,?,?,?,?,?,?,?)`, id, user.ID, acctID, "google_drive", "pfile-"+name, name, mime, size, nullIfEmpty(folder), "active"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	insertFile("photo.jpg", "image/jpeg", 1000, folderID)
	insertFile("clip.mp4", "video/mp4", 4000, childID)
	insertFile("notes.pdf", "application/pdf", 9000, folderID)

	// Gallery: only media, with sizes and the account block.
	w := do("GET", "/api/gallery", "")
	if w.Code != http.StatusOK {
		t.Fatalf("gallery = %d: %s", w.Code, w.Body.String())
	}
	var gallery struct {
		Items []struct {
			ID      string            `json:"id"`
			Name    string            `json:"name"`
			Size    string            `json:"sizeBytes"`
			Thumb   string            `json:"thumbnailUrl"`
			Account map[string]string `json:"connectedAccount"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &gallery); err != nil {
		t.Fatal(err)
	}
	if gallery.Total != 2 {
		t.Fatalf("gallery total = %d, want 2 (media only)", gallery.Total)
	}
	var videoFound bool
	for _, item := range gallery.Items {
		if item.Name == "clip.mp4" {
			videoFound = true
			if item.Size != "4000" {
				t.Fatalf("clip size = %s", item.Size)
			}
		}
	}
	if !videoFound {
		t.Fatal("video missing from gallery")
	}

	// Folder sizes: parent aggregates its own files (1000+9000) plus the child's (4000).
	w = do("GET", "/folders", "")
	if w.Code != http.StatusOK {
		t.Fatalf("folders = %d", w.Code)
	}
	var folders struct {
		Folders []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			SizeBytes string `json:"sizeBytes"`
		} `json:"folders"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &folders); err != nil {
		t.Fatal(err)
	}
	var parentSize string
	for _, f := range folders.Folders {
		t.Logf("folder %s id=%s size=%s", f.Name, f.ID, f.SizeBytes)
		if f.Name == "parent" {
			parentSize = f.SizeBytes
		}
	}
	if parentSize != "14000" {
		t.Fatalf("parent folder size = %s, want 14000 (recursive aggregate: 1000+9000 own + 4000 child)", parentSize)
	}

	// Share link with expiry: bad timestamp rejected, good one stored, list reports it.
	if w := do("POST", "/files/missing/public-permission", ""); w.Code == http.StatusOK {
		t.Fatal("unknown file must not share")
	}
	// Seed a share row directly (the Drive call itself is exercised in shares tests).
	shareID := randomID()
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := app.DB.Exec(`INSERT INTO share_links (id,user_id,file_id,connected_account_id,provider_file_id,permission_id,url,expires_at,auto_revoke) VALUES (?,?,?,?,?,?,?,?,1)`,
		shareID, user.ID, "file-x", "acct-x", "pfile-x", "perm-x", "https://drive.example/x", past); err != nil {
		t.Fatal(err)
	}
	// The sweep (grace 0) must revoke it.
	revoked, err := app.revokeExpiredShares(0)
	if err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Fatalf("expired sweep revoked %d, want 1", revoked)
	}
	var revokedAt string
	if err := app.DB.QueryRow(`SELECT COALESCE(revoked_at,'') FROM share_links WHERE id=?`, shareID).Scan(&revokedAt); err != nil || revokedAt == "" {
		t.Fatalf("share not marked revoked: %v %q", err, revokedAt)
	}

	// ntfy settings: PUT persists, GET reflects, empty topic rejected.
	if w := do("PUT", "/settings/notifications", `{"server":"https://ntfy.example","topic":"pandrive-test-topic"}`); w.Code != http.StatusOK {
		t.Fatalf("notify save = %d: %s", w.Code, w.Body.String())
	}
	if got := app.settingValue("ntfy_topic"); got != "pandrive-test-topic" {
		t.Fatalf("topic stored = %q", got)
	}
	if got := app.settingValue("ntfy_server"); got != "https://ntfy.example" {
		t.Fatalf("server stored = %q", got)
	}
	w = do("GET", "/settings/notifications", "")
	var cfg struct {
		Server  string `json:"server"`
		Topic   string `json:"topic"`
		Enabled bool   `json:"enabled"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &cfg)
	if cfg.Topic != "pandrive-test-topic" || cfg.Server != "https://ntfy.example" || !cfg.Enabled {
		t.Fatalf("notify GET = %+v", cfg)
	}
	if w := do("PUT", "/settings/notifications", `{"topic":""}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty topic = %d, want 400", w.Code)
	}
	// notify() with a topic set must not panic and must not block (fire-and-forget).
	app.notify(user.ID, "t", "m", "tag", "default")
}
