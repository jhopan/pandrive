package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Non-split download must redirect to Google's link (zero server bandwidth) and keep
// ?proxy=1 as the byte-forwarding fallback. Split files must never take the redirect.
func TestDownloadRedirectsNonSplit(t *testing.T) {
	app := newTestApp(t)
	var metaCalls int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "webContentLink") {
			atomic.AddInt32(&metaCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]string{"webContentLink": "https://drive.google.com/uc?export=download&id=drv-1"})
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()
	app.GoogleDriveAPIURL = api.URL + "/drive/v3"

	token, user := registerAndLogin(t, app, "dl@example.test")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes) VALUES (?,?,?,?,?,?,?,?,?)`, "acc", user.ID, "google_drive", "p", "a@example.test", app.encrypt("tok"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]")
	_, _ = app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, "file-1", user.ID, "acc", "google_drive", "drv-1", "video.mp4", "video/mp4", 123)

	r := app.Router()
	req := httptest.NewRequest(http.MethodGet, "/files/file-1/download", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("download = %d: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://drive.google.com/uc?export=download") {
		t.Fatalf("redirect target = %q", loc)
	}
	if n := atomic.LoadInt32(&metaCalls); n != 1 {
		t.Fatalf("meta calls = %d", n)
	}

	// proxy=1 bypasses the redirect (used when Google omits the link).
	req2 := httptest.NewRequest(http.MethodGet, "/files/file-1/download?proxy=1", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	// The stub has no alt=media route; what matters is that NO redirect was issued.
	if w2.Code == http.StatusFound {
		t.Fatal("proxy=1 must not redirect")
	}
}

// A completed split part must go through the merge stream, never the redirect.
func TestDownloadSplitNeverRedirects(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "splitdl@example.test")
	if _, err := app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes) VALUES (?,?,?,?,?,?,?,?,?)`, "acc", user.ID, "google_drive", "p", "s@example.test", app.encrypt("tok"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]"); err != nil {
		t.Fatal("accounts insert: ", err)
	}
	if _, err := app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, "part-file", user.ID, "acc", "google_drive", "drv-p1", "pd-split-x.part001", "application/octet-stream", 5); err != nil {
		t.Fatal("files insert: ", err)
	}
	if _, err := app.DB.Exec(`INSERT INTO split_files (id,user_id,name,mime_type,size_bytes,part_count,status) VALUES ('sp1',?,?,?,?,?, 'complete')`, user.ID, "movie.mkv", "video/x-matroska", 5, 1); err != nil {
		t.Fatal("split_files insert: ", err)
	}
	if _, err := app.DB.Exec(`INSERT INTO split_parts (id,split_id,part_index,file_id,connected_account_id,size_bytes) VALUES ('pp1','sp1',1,'part-file','acc',5)`); err != nil {
		t.Fatal("split_parts insert: ", err)
	}

	app.GoogleDriveAPIURL = newMediaStub(t, []byte("AAAAA"))

	req := httptest.NewRequest(http.MethodGet, "/files/part-file/download", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("split download = %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Location") != "" {
		t.Fatal("split download must not redirect")
	}
	if w.Body.String() != "AAAAA" {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func newMediaStub(t *testing.T, data []byte) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") == "media" {
			w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/drive/v3"
}
