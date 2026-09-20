package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Recent = active files ordered by updated_at DESC, with a limit, an account filter and counters.
func TestRecentFilesOrderingAndFilters(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "recent@example.test")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "acc-a", user.ID, "google_drive", "a", "a@example.test", "[]")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "acc-b", user.ID, "google_drive", "b", "b@example.test", "[]")
	_, _ = app.DB.Exec(`INSERT INTO folders (id,user_id,name,color) VALUES (?,?,?,?)`, "fold", user.ID, "Projects", "text-blue-500")

	seed := func(id, account, name, updated string, status string) {
		t.Helper()
		if _, err := app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes,status,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, user.ID, account, "google_drive", "p-"+id, name, "application/pdf", 10, status, updated); err != nil {
			t.Fatal(err)
		}
	}
	// Relative timestamps keep this deterministic whatever the wall clock says.
	stamp := func(offset string) string {
		var v string
		if err := app.DB.QueryRow(`SELECT datetime('now',?)`, offset).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	seed("r1", "acc-a", "newest.pdf", stamp("-2 hours"), "active")
	seed("r2", "acc-b", "middle.pdf", stamp("-3 days"), "active")
	seed("r3", "acc-a", "oldest.pdf", stamp("-40 days"), "active")
	seed("r4", "acc-a", "trashed.pdf", stamp("-1 hours"), "deleted") // must never appear
	_, _ = app.DB.Exec(`UPDATE files SET folder_id='fold' WHERE id='r2'`)
	_, _ = app.DB.Exec(`UPDATE files SET starred=1 WHERE id='r3'`)

	read := func(query string) (int, []map[string]any, int, int, int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/recent"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("recent%s = %d: %s", query, w.Code, w.Body.String())
		}
		var out struct {
			Files   []map[string]any `json:"files"`
			Total   int              `json:"total"`
			Limit   int              `json:"limit"`
			Last24h int              `json:"last24h"`
			Last7d  int              `json:"last7d"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Total, out.Files, out.Limit, out.Last24h, out.Last7d
	}

	total, files, limit, _, _ := read("")
	if total != 3 || limit != 50 {
		t.Fatalf("total=%d limit=%d, want 3/50", total, limit)
	}
	if files[0]["name"] != "newest.pdf" || files[1]["name"] != "middle.pdf" || files[2]["name"] != "oldest.pdf" {
		t.Fatalf("order = %v %v %v", files[0]["name"], files[1]["name"], files[2]["name"])
	}
	// The trashed file has a newer timestamp than everything else and still must not show up.
	for _, f := range files {
		if f["name"] == "trashed.pdf" {
			t.Fatal("deleted file leaked into recent")
		}
	}
	// Joined fields and flags travel with the row.
	if files[1]["folder"] == nil || files[2]["starred"] != true {
		t.Fatalf("folder/starred not mapped: %+v %+v", files[1], files[2])
	}

	if total, _, _, _, _ := read("?limit=2"); total != 2 {
		t.Fatalf("limit=2 -> %d", total)
	}
	if _, _, limit, _, _ := read("?limit=9999"); limit != 50 {
		t.Fatalf("limit clamp -> %d, want 50", limit)
	}
	if total, files, _, _, _ := read("?accountId=acc-b"); total != 1 || files[0]["name"] != "middle.pdf" {
		t.Fatalf("accountId filter -> %d %+v", total, files)
	}
	// Counters: r1 is 2h old (both windows), r2 is 3 days old (week only), r3 and the
	// deleted r4 are outside both windows.
	_, _, _, last24, last7 := read("")
	if last24 != 1 || last7 != 2 {
		t.Fatalf("counters = %d/%d, want 1/2", last24, last7)
	}
}
