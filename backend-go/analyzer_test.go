package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func seedFile(t *testing.T, app *App, userID, id, accountID, name, mime string, size int64) {
	t.Helper()
	if _, err := app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, id, userID, accountID, "google_drive", "p-"+id, name, mime, size); err != nil {
		t.Fatal(err)
	}
}

// Only same-name + same-size files across any account count as duplicates.
func TestFindDuplicatesGroupsMatchingFiles(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "dup@example.test")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "acc-a", user.ID, "google_drive", "a", "a@example.test", "[]")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "acc-b", user.ID, "google_drive", "b", "b@example.test", "[]")

	seedFile(t, app, user.ID, "d1", "acc-a", "report.pdf", "application/pdf", 500)
	seedFile(t, app, user.ID, "d2", "acc-b", "report.pdf", "application/pdf", 500) // duplicate
	seedFile(t, app, user.ID, "d3", "acc-a", "report.pdf", "application/pdf", 900) // same name, different size
	seedFile(t, app, user.ID, "d4", "acc-a", "unique.txt", "text/plain", 120)      // unique
	seedFile(t, app, user.ID, "d5", "acc-a", "empty.pdf", "application/pdf", 0)    // zero-byte, ignored

	req := httptest.NewRequest(http.MethodGet, "/files/duplicates", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Groups []struct {
			Name        string `json:"name"`
			Count       int    `json:"count"`
			WastedBytes string `json:"wastedBytes"`
			Files       []struct {
				ID string `json:"id"`
			} `json:"files"`
		} `json:"groups"`
		GroupCount       int    `json:"groupCount"`
		TotalWastedBytes string `json:"totalWastedBytes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.GroupCount != 1 {
		t.Fatalf("groupCount = %d, want 1 (groups=%+v)", out.GroupCount, out.Groups)
	}
	if out.Groups[0].Count != 2 || out.Groups[0].WastedBytes != "500" || out.TotalWastedBytes != "500" {
		t.Fatalf("unexpected group: %+v", out.Groups[0])
	}
}

// The analyzer must bucket files by coarse type and rank largest files first.
func TestStorageAnalyzerBucketsAndLargest(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "analyzer@example.test")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "acc", user.ID, "google_drive", "a", "a@example.test", "[]")
	seedFile(t, app, user.ID, "a1", "acc", "holiday.jpg", "image/jpeg", 1000)
	seedFile(t, app, user.ID, "a2", "acc", "clip.mp4", "video/mp4", 5000)
	seedFile(t, app, user.ID, "a3", "acc", "notes.txt", "text/plain", 50)
	seedFile(t, app, user.ID, "a4", "acc", "backup.zip", "application/zip", 20000)

	req := httptest.NewRequest(http.MethodGet, "/api/storage/analyzer", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		ByType []struct {
			Label string `json:"label"`
			Bytes string `json:"bytes"`
			Count int    `json:"count"`
		} `json:"byType"`
		Largest []struct {
			Name string `json:"name"`
		} `json:"largest"`
		Totals map[string]string `json:"totals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byLabel := map[string]string{}
	for _, row := range out.ByType {
		byLabel[row.Label] = row.Bytes
	}
	if byLabel["Archives"] != "20000" || byLabel["Video"] != "5000" || byLabel["Images"] != "1000" || byLabel["Documents"] != "50" {
		t.Fatalf("byType = %+v", out.ByType)
	}
	if len(out.ByType) == 0 || out.ByType[0].Label != "Archives" {
		t.Fatalf("types not sorted by bytes: %+v", out.ByType)
	}
	if out.Largest[0].Name != "backup.zip" {
		t.Fatalf("largest[0] = %q, want backup.zip", out.Largest[0].Name)
	}
	if out.Totals["bytes"] != "26050" || out.Totals["files"] != "4" {
		t.Fatalf("totals = %+v", out.Totals)
	}
}
