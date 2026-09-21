package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// The branded /s/<id> page: public, shows name+size, links to Google, honours revocation and expiry.
// view-url must return the Drive webViewLink through the faked Google endpoints.
func TestSharePagePublicExpiryAndRevoke(t *testing.T) {
	app := newTestApp(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/files/pfile-1") && r.URL.Query().Get("fields") == "webViewLink" {
			w.Write([]byte(`{"webViewLink":"https://drive.google.com/file/d/pfile-1/view"}`))
			return
		}
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"access","token_type":"Bearer","expires_in":3600}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()
	app.GoogleDriveAPIURL = api.URL + "/drive/v3"
	app.GoogleEndpoint = oauth2.Endpoint{AuthURL: api.URL + "/auth", TokenURL: api.URL + "/token"}

	token, user := registerAndLogin(t, app, "page@example.test")

	acctID := randomID()
	if _, err := app.DB.Exec(`INSERT INTO provider_configs (id,user_id,provider,client_id_encrypted,client_secret_encrypted,redirect_uri) VALUES (?,?,?,?,?,?)`,
		"config", user.ID, "google_drive", app.encrypt("client"), app.encrypt("secret"), "http://localhost:4000/callback"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes,provider_config_id,status) VALUES (?,?,?,?,?,?,?,?,?,?,'connected')`,
		acctID, user.ID, "google_drive", "google", "page@example.test", app.encrypt("access"), app.encrypt("refresh"), "2099-01-01T00:00:00Z", "[]", "config"); err != nil {
		t.Fatal(err)
	}
	fileID := randomID()
	if _, err := app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes,status) VALUES (?,?,?,?,?,?,?,?,?)`,
		fileID, user.ID, acctID, "google_drive", "pfile-1", "big video.mp4", "video/mp4", 300*1024*1024, "active"); err != nil {
		t.Fatal(err)
	}
	shareID := randomID()
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := app.DB.Exec(`INSERT INTO share_links (id,user_id,file_id,connected_account_id,provider_file_id,permission_id,url,expires_at,auto_revoke) VALUES (?,?,?,?,?,?,?,?,1)`,
		shareID, user.ID, fileID, acctID, "pfile-1", "perm-1", "https://page.test/s/"+shareID, future); err != nil {
		t.Fatal(err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		return w
	}

	// Public: no auth, branded HTML, noindex, size label, Wi-Fi warning for >= 200 MB.
	w := get("/s/" + shareID)
	if w.Code != http.StatusOK {
		t.Fatalf("share page = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "big video.mp4") || !strings.Contains(body, "PanDrive") {
		t.Fatal("page must show the file name and PanDrive branding")
	}
	if !strings.Contains(body, "300.0 MB") || !strings.Contains(body, "Wi-Fi") {
		t.Fatal("page must show the size label and the heavy-file warning")
	}
	if w.Header().Get("X-Robots-Tag") != "noindex" {
		t.Fatal("share page must be noindex")
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %s", ct)
	}

	// view-url (authenticated) returns the Drive webViewLink.
	req := httptest.NewRequest(http.MethodGet, "/api/files/"+fileID+"/view-url", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w2 := httptest.NewRecorder()
	app.Router().ServeHTTP(w2, req)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), "drive.google.com/file/d/pfile-1/view") {
		t.Fatalf("view-url = %d %s", w2.Code, w2.Body.String())
	}

	// Revoked link -> 404.
	if _, err := app.DB.Exec(`UPDATE share_links SET revoked_at=CURRENT_TIMESTAMP WHERE id=?`, shareID); err != nil {
		t.Fatal(err)
	}
	if w := get("/s/" + shareID); w.Code != http.StatusNotFound {
		t.Fatalf("revoked share = %d, want 404", w.Code)
	}

	// Expired link -> 410.
	expired := randomID()
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := app.DB.Exec(`INSERT INTO share_links (id,user_id,file_id,connected_account_id,provider_file_id,permission_id,url,expires_at,auto_revoke) VALUES (?,?,?,?,?,?,?,?,1)`,
		expired, user.ID, fileID, acctID, "pfile-1", "perm-2", "https://page.test/s/"+expired, past); err != nil {
		t.Fatal(err)
	}
	if w := get("/s/" + expired); w.Code != http.StatusGone {
		t.Fatalf("expired share = %d, want 410", w.Code)
	}
}
