package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Starring is a local flag: files and folders round-trip through /starred, and the
// migration that adds the column must be safe to run against an existing database.
func TestStarredFilesAndFolders(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "star@example.test")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "acc", user.ID, "google_drive", "p", "star@example.test", "[]")
	_, _ = app.DB.Exec(`INSERT INTO folders (id,user_id,name,color) VALUES (?,?,?,?)`, "fold1", user.ID, "Projects", "text-blue-500")
	_, _ = app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, "file1", user.ID, "acc", "google_drive", "pid1", "spec.pdf", "application/pdf", 1234)
	_, _ = app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, "file2", user.ID, "acc", "google_drive", "pid2", "other.pdf", "application/pdf", 12)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		return w
	}

	// Nothing starred initially.
	if w := call(http.MethodGet, "/api/starred", ""); w.Code != http.StatusOK {
		t.Fatalf("starred = %d: %s", w.Code, w.Body.String())
	}
	var page struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
		Folders []struct {
			Name string `json:"name"`
		} `json:"folders"`
		Total int `json:"total"`
	}
	_ = json.Unmarshal(call(http.MethodGet, "/api/starred", "").Body.Bytes(), &page)
	if page.Total != 0 {
		t.Fatalf("initial total = %d, want 0", page.Total)
	}

	if w := call(http.MethodPost, "/files/file1/star", `{"starred":true}`); w.Code != http.StatusOK {
		t.Fatalf("star file = %d: %s", w.Code, w.Body.String())
	}
	if w := call(http.MethodPost, "/folders/fold1/star", `{"starred":true}`); w.Code != http.StatusOK {
		t.Fatalf("star folder = %d: %s", w.Code, w.Body.String())
	}

	_ = json.Unmarshal(call(http.MethodGet, "/api/starred", "").Body.Bytes(), &page)
	if page.Total != 2 || len(page.Files) != 1 || page.Files[0].Name != "spec.pdf" || page.Folders[0].Name != "Projects" {
		t.Fatalf("starred payload = %+v", page)
	}

	// Both list endpoints must expose the flag so the UI can render the star state.
	var files struct {
		Files []struct {
			ID      string `json:"id"`
			Starred bool   `json:"starred"`
		} `json:"files"`
	}
	_ = json.Unmarshal(call(http.MethodGet, "/files", "").Body.Bytes(), &files)
	flags := map[string]bool{}
	for _, f := range files.Files {
		flags[f.ID] = f.Starred
	}
	if !flags["file1"] || flags["file2"] {
		t.Fatalf("starred flags = %v, want file1 true / file2 false", flags)
	}
	var folders struct {
		Folders []struct {
			Starred bool `json:"starred"`
		} `json:"folders"`
	}
	_ = json.Unmarshal(call(http.MethodGet, "/folders", "").Body.Bytes(), &folders)
	if len(folders.Folders) != 1 || !folders.Folders[0].Starred {
		t.Fatalf("folder starred flag = %+v", folders.Folders)
	}

	// Unstar removes it again.
	if w := call(http.MethodPost, "/files/file1/star", `{"starred":false}`); w.Code != http.StatusOK {
		t.Fatalf("unstar = %d", w.Code)
	}
	_ = json.Unmarshal(call(http.MethodGet, "/api/starred", "").Body.Bytes(), &page)
	if page.Total != 1 || len(page.Files) != 0 {
		t.Fatalf("after unstar = %+v", page)
	}

	// Missing/invalid input is rejected rather than silently flipping the flag.
	if w := call(http.MethodPost, "/files/file1/star", `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty body = %d, want 400", w.Code)
	}
	if w := call(http.MethodPost, "/files/does-not-exist/star", `{"starred":true}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown file = %d, want 404", w.Code)
	}
	if w := call(http.MethodPost, "/folders/does-not-exist/star", `{"starred":true}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown folder = %d, want 404", w.Code)
	}

	// Re-running the migration (the ALTER TABLE path) must not fail on an existing DB.
	if err := app.migrate(); err != nil {
		t.Fatalf("second migrate() = %v, want nil (ALTER must tolerate the existing column)", err)
	}
}
