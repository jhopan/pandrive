package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The audit trail must record which actions happened, filter by action, and expose an IP.
func TestActivityLogRecordsAndFilters(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "audit@example.test")

	// A successful login is recorded by the login handler itself.
	login := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"email":"audit@example.test","password":"password-123"}`))
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, login)
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", w.Code, w.Body.String())
	}

	// A failed login against a known account is attributed to that account.
	bad := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"email":"audit@example.test","password":"wrong"}`))
	w = httptest.NewRecorder()
	app.Router().ServeHTTP(w, bad)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad login = %d", w.Code)
	}

	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "acc", user.ID, "google_drive", "p", "audit@example.test", "[]")
	_, _ = app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, "f1", user.ID, "acc", "google_drive", "pid", "notes.txt", "text/plain", 42)

	del := httptest.NewRequest(http.MethodDelete, "/files/f1", nil)
	del.Header.Set("Authorization", "Bearer "+token)
	del.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	w = httptest.NewRecorder()
	app.Router().ServeHTTP(w, del)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}

	read := func(query string) (int, []map[string]any, []string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/activity"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("activity%s = %d: %s", query, w.Code, w.Body.String())
		}
		var out struct {
			Entries []map[string]any `json:"entries"`
			Total   int              `json:"total"`
			Actions []string         `json:"actions"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Total, out.Entries, out.Actions
	}

	total, entries, actions := read("")
	if total < 3 {
		t.Fatalf("total = %d, want >= 3 (login, login_failed, file_delete)", total)
	}
	if len(entries) == 0 || entries[0]["action"] != "file_delete" {
		t.Fatalf("newest entry = %+v, want file_delete first", entries[0])
	}
	// The delete must carry the client IP from X-Forwarded-For, not the proxy address.
	if got := entries[0]["ip"]; got != "203.0.113.9" {
		t.Fatalf("ip = %v, want 203.0.113.9", got)
	}
	if entries[0]["targetName"] != "notes.txt" || entries[0]["sizeBytes"] != "42" {
		t.Fatalf("entry payload = %+v", entries[0])
	}

	if n, _, _ := read("?action=login_failed"); n != 1 {
		t.Fatalf("login_failed total = %d, want 1", n)
	}
	if n, _, _ := read("?action=file_delete"); n != 1 {
		t.Fatalf("file_delete total = %d, want 1", n)
	}
	if n, _, _ := read("?accountId=acc"); n != 1 {
		t.Fatalf("account filter total = %d, want 1", n)
	}
	found := false
	for _, a := range actions {
		if a == "file_delete" {
			found = true
		}
	}
	if !found {
		t.Fatalf("actions = %v, want file_delete present", actions)
	}
}
