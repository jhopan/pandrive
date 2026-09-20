package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Search must honour every filter the UI sends (the old /files silently ignored kind/size/date),
// escape LIKE wildcards, sort, paginate and report facets and totals.
func TestSearchFiltersSortingAndFacets(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "search@example.test")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "acc-a", user.ID, "google_drive", "a", "a@example.test", "[]")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "acc-b", user.ID, "google_drive", "b", "b@example.test", "[]")
	_, _ = app.DB.Exec(`INSERT INTO folders (id,user_id,name,color) VALUES (?,?,?,?)`, "fold", user.ID, "Photos", "text-blue-500")

	seed := func(id, account, name, mime string, size int64, created string, starred int) {
		t.Helper()
		if _, err := app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes,created_at,starred) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, user.ID, account, "google_drive", "p-"+id, name, mime, size, created, starred); err != nil {
			t.Fatal(err)
		}
	}
	seed("s1", "acc-a", "holiday.jpg", "image/jpeg", 1000, "2026-08-10 10:00:00", 1)
	seed("s2", "acc-a", "backup.zip", "application/x-zip-compressed", 50000, "2026-08-15 10:00:00", 0) // Windows zip
	seed("s3", "acc-b", "invoice.pdf", "application/pdf", 2000, "2026-09-01 10:00:00", 0)
	seed("s4", "acc-b", "notes.txt", "text/plain", 30, "2026-09-10 10:00:00", 0)
	seed("s5", "acc-a", "report 100% done.txt", "text/plain", 40, "2026-09-11 10:00:00", 0)
	seed("s6", "acc-a", "Naomi", "application/vnd.google-apps.folder", 0, "2026-09-12 10:00:00", 0)
	seed("s7", "acc-a", "trashed.pdf", "application/pdf", 999, "2026-09-13 10:00:00", 0)
	_, _ = app.DB.Exec(`UPDATE files SET status='deleted' WHERE id='s7'`)
	_, _ = app.DB.Exec(`UPDATE files SET folder_id='fold' WHERE id='s1'`)

	read := func(query string) (int, string, []map[string]any, map[string]any, int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/search"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("search%s = %d: %s", query, w.Code, w.Body.String())
		}
		var out struct {
			Files      []map[string]any `json:"files"`
			Total      int              `json:"total"`
			TotalBytes string           `json:"totalBytes"`
			Facets     map[string]any   `json:"facets"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Total, out.TotalBytes, out.Files, out.Facets, w.Code
	}

	// Baseline: active files only, favicon-deleted row excluded.
	total, bytes, _, facets, _ := read("")
	if total != 6 {
		t.Fatalf("baseline total = %d, want 6 (deleted excluded)", total)
	}
	if bytes != "53070" {
		t.Fatalf("baseline bytes = %s, want 53070", bytes)
	}
	kindFacets, _ := facets["kinds"].([]any)
	kindCounts := map[string]float64{}
	for _, k := range kindFacets {
		entry := k.(map[string]any)
		kindCounts[entry["key"].(string)] = entry["count"].(float64)
	}
	// The Windows-compressed zip must be classified as an archive, and the native folder as gapps.
	if kindCounts["archive"] != 1 || kindCounts["gapps"] != 1 || kindCounts["image"] != 1 {
		t.Fatalf("kind facets = %v", kindCounts)
	}

	if total, _, files, _, _ := read("?kind=archive"); total != 1 || files[0]["name"] != "backup.zip" {
		t.Fatalf("kind=archive -> %d %+v", total, files)
	}
	if total, _, _, _, _ := read("?kind=gapps"); total != 1 {
		t.Fatalf("kind=gapps -> %d", total)
	}
	if total, _, _, _, _ := read("?kind=doc"); total != 2 {
		t.Fatalf("kind=doc (text/plain x2) -> %d", total)
	}
	// holiday.jpg 1000, invoice.pdf 2000, backup.zip 50000 all fall inside the range.
	if total, _, _, _, _ := read("?minSize=1000&maxSize=60000"); total != 3 {
		t.Fatalf("size range -> %d, want 3", total)
	}
	if total, _, _, _, _ := read("?minSize=1001"); total != 2 {
		t.Fatalf("minSize only -> %d, want 2", total)
	}
	if total, _, _, _, _ := read("?startDate=2026-09-01&endDate=2026-09-30"); total != 4 {
		t.Fatalf("date range -> %d, want 4", total)
	}
	if total, _, files, _, _ := read("?starred=1"); total != 1 || files[0]["name"] != "holiday.jpg" {
		t.Fatalf("starred=1 -> %d %+v", total, files)
	}
	if total, _, files, _, _ := read("?folderId=fold"); total != 1 || files[0]["name"] != "holiday.jpg" {
		t.Fatalf("folder filter -> %d", total)
	}
	if total, _, _, _, _ := read("?accountId=acc-b"); total != 2 {
		t.Fatalf("account filter -> %d", total)
	}

	// LIKE escaping: "%" must be literal, not a wildcard.
	if total, _, files, _, _ := read("?q=100%25"); total != 1 || files[0]["name"] != "report 100% done.txt" {
		t.Fatalf("escaped %% search -> %d %+v", total, files)
	}
	if total, _, _, _, _ := read("?q=zzz-no-match"); total != 0 {
		t.Fatalf("no-match -> %d", total)
	}

	// Sorting and pagination.
	if _, _, files, _, _ := read("?sort=size"); files[0]["name"] != "backup.zip" {
		t.Fatalf("sort=size first = %v, want backup.zip", files[0]["name"])
	}
	// Ascending size starts with the zero-byte native folder, then the smallest text file.
	if _, _, files, _, _ := read("?sort=size_asc"); files[0]["name"] != "Naomi" || files[1]["name"] != "notes.txt" {
		t.Fatalf("sort=size_asc = %v, %v", files[0]["name"], files[1]["name"])
	}
	// COLLATE NOCASE ordering: "Naomi" sorts before "notes.txt" (a < o).
	_, _, byName, _, _ := read("?sort=name")
	want := []string{"backup.zip", "holiday.jpg", "invoice.pdf", "Naomi", "notes.txt", "report 100% done.txt"}
	for i, w2 := range want {
		if byName[i]["name"] != w2 {
			t.Fatalf("sort=name[%d] = %v, want %v (full=%v)", i, byName[i]["name"], w2, byName)
		}
	}
	_, _, page1, _, _ := read("?sort=size&limit=3&offset=0")
	_, _, page2, _, _ := read("?sort=size&limit=3&offset=3")
	if len(page1) != 3 || len(page2) != 3 || page1[0]["id"] == page2[0]["id"] {
		t.Fatalf("pagination broken: %d/%d", len(page1), len(page2))
	}

	// Bad numeric input is a 400, not a silent ignore.
	req := httptest.NewRequest(http.MethodGet, "/search?minSize=abc", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("minSize=abc -> %d, want 400", w.Code)
	}
}
