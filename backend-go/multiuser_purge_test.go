package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Multi-user: roles gate admin endpoints; disabled users cannot log in; registration honours the
// open_registration switch. Trash auto-purge: opt-in days, deletes old trash rows.
func TestMultiUserAdminAndTrashPurge(t *testing.T) {
	app := newTestApp(t)
	token, admin := registerAndLogin(t, app, "admin@example.test")
	H := map[string]string{"Authorization": "Bearer " + token}

	// bootstrap admin created by registerAndLogin is role 'user' by default in tests; promote it.
	if _, err := app.DB.Exec(`UPDATE users SET role='admin' WHERE id=?`, admin.ID); err != nil {
		t.Fatal(err)
	}

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

	// Enable open registration, then create a second user through /auth/register.
	if w := do("PUT", "/api/settings/notifications", `{"topic":"x"}`); w.Code != http.StatusOK {
		t.Fatalf("seed settings = %d", w.Code)
	}
	if err := app.setSetting("open_registration", "1"); err != nil {
		t.Fatal(err)
	}
	reg := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"email":"second@example.test","password":"secondpass1","name":"Second"}`))
	w2 := httptest.NewRecorder()
	app.Router().ServeHTTP(w2, reg)
	if w2.Code != http.StatusCreated && w2.Code != http.StatusOK {
		t.Fatalf("register = %d: %s", w2.Code, w2.Body.String())
	}
	var secondID string
	if err := app.DB.QueryRow(`SELECT id FROM users WHERE email='second@example.test'`).Scan(&secondID); err != nil {
		t.Fatal(err)
	}

	// Second user (role user) is NOT allowed to list admin users.
	var session struct {
		AccessToken string `json:"accessToken"`
	}
	regLogin := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"email":"second@example.test","password":"secondpass1"}`))
	wl := httptest.NewRecorder()
	app.Router().ServeHTTP(wl, regLogin)
	if wl.Code != http.StatusOK {
		t.Fatalf("second login = %d", wl.Code)
	}
	_ = json.Unmarshal(wl.Body.Bytes(), &session)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	req.Header.Set("Authorization", "Bearer "+session.AccessToken)
	wf := httptest.NewRecorder()
	app.Router().ServeHTTP(wf, req)
	if wf.Code != http.StatusForbidden {
		t.Fatalf("non-admin list users = %d, want 403", wf.Code)
	}

	// Admin sees both users.
	w := do("GET", "/api/admin/users", "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin list users = %d: %s", w.Code, w.Body.String())
	}
	var list struct {
		Users []struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"users"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Users) < 2 {
		t.Fatalf("users listed = %d", len(list.Users))
	}

	// Disable the second user -> its login is refused, sessions revoked.
	if w := do("PATCH", "/api/admin/users/"+secondID, `{"disabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("disable user = %d: %s", w.Code, w.Body.String())
	}
	wl2 := httptest.NewRecorder()
	app.Router().ServeHTTP(wl2, httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"email":"second@example.test","password":"secondpass1"}`)))
	if wl2.Code != http.StatusForbidden {
		t.Fatalf("disabled login = %d, want 403", wl2.Code)
	}

	// Self-lockout guard.
	if w := do("PATCH", "/api/admin/users/"+admin.ID, `{"disabled":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("self-disable = %d, want 400", w.Code)
	}

	// Trash auto-purge: seed a trashed file older than the window and purge.
	acctID := randomID()
	if _, err := app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,status) VALUES (?,?,?,?,?,'connected')`, acctID, admin.ID, "google_drive", "ga-1", "admin@example.test"); err != nil {
		t.Fatal(err)
	}
	oldDeleted := time.Now().UTC().AddDate(0, 0, -40).Format(time.RFC3339Nano)
	fid := randomID()
	if _, err := app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes,status,deleted_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		fid, admin.ID, acctID, "google_drive", "pfile-old", "old-trash.bin", "application/octet-stream", 10, "deleted", oldDeleted); err != nil {
		t.Fatal(err)
	}
	if err := app.setSetting("trash_autopurge_days", "30"); err != nil {
		t.Fatal(err)
	}
	purged, err := app.purgeOldTrash()
	if err != nil {
		t.Fatal(err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	var gone int
	_ = app.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE id=?`, fid).Scan(&gone)
	if gone != 0 {
		t.Fatal("old trash row must be removed")
	}

	// A fresh trashed file inside the window stays.
	recent := randomID()
	if _, err := app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes,status,deleted_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		recent, admin.ID, acctID, "google_drive", "pfile-new", "recent-trash.bin", "application/octet-stream", 10, "deleted", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	purged, err = app.purgeOldTrash()
	if err != nil {
		t.Fatal(err)
	}
	if purged != 0 {
		t.Fatalf("recent trash purged = %d, want 0", purged)
	}
	var kept int
	_ = app.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE id=?`, recent).Scan(&kept)
	if kept != 1 {
		t.Fatal("recent trash must be kept")
	}

	// Disable purge -> no-op.
	if err := app.setSetting("trash_autopurge_days", ""); err != nil {
		t.Fatal(err)
	}
	purged, _ = app.purgeOldTrash()
	if purged != 0 {
		t.Fatalf("disabled purge = %d, want 0", purged)
	}
}
