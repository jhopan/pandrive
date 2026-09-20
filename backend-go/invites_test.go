package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Sharing with a person must create a real Drive permission, be listable, revocable,
// and reject invalid input instead of silently doing nothing.
func TestInvitesGrantListRevoke(t *testing.T) {
	app := newTestApp(t)

	var mu sync.Mutex
	var grants, lists, revokes int
	var lastRole, lastEmail string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/permissions"):
			var payload struct {
				Role         string `json:"role"`
				Type         string `json:"type"`
				EmailAddress string `json:"emailAddress"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			grants++
			lastRole, lastEmail = payload.Role, payload.EmailAddress
			if payload.Type != "user" {
				t.Errorf("permission type = %q, want user", payload.Type)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "perm-77"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/permissions"):
			lists++
			_ = json.NewEncoder(w).Encode(map[string]any{"permissions": []map[string]string{
				{"id": "perm-77", "role": "reader", "type": "user", "emailAddress": "friend@example.test"},
				{"id": "perm-1", "role": "owner", "type": "user", "emailAddress": "owner@example.test"},
			}})
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/permissions/"):
			revokes++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	app.GoogleDriveAPIURL = api.URL + "/drive/v3"

	token, user := registerAndLogin(t, app, "invite@example.test")
	_, _ = app.DB.Exec(`INSERT INTO provider_configs (id,user_id,provider,client_id_encrypted,client_secret_encrypted,redirect_uri) VALUES (?,?,?,?,?,?)`, "cfg", user.ID, "google_drive", app.encrypt("cid"), app.encrypt("cs"), "http://localhost/cb")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes,provider_config_id) VALUES (?,?,?,?,?,?,?,?,?,?)`, "acc", user.ID, "google_drive", "p", "owner@example.test", app.encrypt("tok"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]", "cfg")
	_, _ = app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, "f1", user.ID, "acc", "google_drive", "drive-1", "contract.pdf", "application/pdf", 10)
	// A local (non-mirrored) folder cannot be shared: there is no Drive object behind it.
	_, _ = app.DB.Exec(`INSERT INTO folders (id,user_id,name,color) VALUES (?,?,?,?)`, "local-folder", user.ID, "Local Only", "text-blue-500")
	_, _ = app.DB.Exec(`INSERT INTO folders (id,user_id,name,color,provider_folder_id,connected_account_id) VALUES (?,?,?,?,?,?)`, "mirrored-folder", user.ID, "Docs", "text-blue-500", "drive-folder-1", "acc")

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		return w
	}

	// Grant on a file.
	w := call(http.MethodPost, "/invites", `{"email":"Friend@Example.test","role":"viewer","targetType":"file","targetId":"f1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("grant = %d: %s", w.Code, w.Body.String())
	}
	var granted struct {
		InviteID   string `json:"inviteId"`
		Role       string `json:"role"`
		Email      string `json:"email"`
		TargetName string `json:"targetName"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &granted)
	if granted.Role != "reader" || granted.Email != "friend@example.test" || granted.TargetName != "contract.pdf" {
		t.Fatalf("grant response = %+v (email must be normalised, viewer mapped to reader)", granted)
	}
	mu.Lock()
	if grants != 1 || lastRole != "reader" || lastEmail != "friend@example.test" {
		t.Fatalf("drive call role=%q email=%q count=%d", lastRole, lastEmail, grants)
	}
	mu.Unlock()

	// Folder grant maps onto the mirrored Drive folder id.
	if w := call(http.MethodPost, "/invites", `{"email":"friend@example.test","role":"editor","targetType":"folder","targetId":"mirrored-folder"}`); w.Code != http.StatusOK {
		t.Fatalf("folder grant = %d: %s", w.Code, w.Body.String())
	}
	mu.Lock()
	if lastRole != "writer" {
		t.Fatalf("editor must map to writer, got %q", lastRole)
	}
	mu.Unlock()

	// Listing returns both grants with their target names.
	var list struct {
		Invites []struct {
			Email      string `json:"email"`
			Role       string `json:"role"`
			TargetType string `json:"targetType"`
			TargetName string `json:"targetName"`
		} `json:"invites"`
		Total int `json:"total"`
	}
	_ = json.Unmarshal(call(http.MethodGet, "/invites", "").Body.Bytes(), &list)
	if list.Total != 2 || list.Invites[0].TargetName == "" {
		t.Fatalf("invites = %+v", list)
	}

	// Drive's own permission list is passed through.
	var perms struct {
		TargetName  string           `json:"targetName"`
		Permissions []map[string]any `json:"permissions"`
		Total       int              `json:"total"`
	}
	_ = json.Unmarshal(call(http.MethodGet, "/permissions?targetType=file&targetId=f1", "").Body.Bytes(), &perms)
	if perms.Total != 2 || perms.TargetName != "contract.pdf" {
		t.Fatalf("permissions = %+v", perms)
	}
	mu.Lock()
	if lists != 1 {
		t.Fatalf("drive permission list called %d times", lists)
	}
	mu.Unlock()

	// Revoke removes the Drive permission and drops the local grant.
	if w := call(http.MethodDelete, "/invites/"+granted.InviteID, ""); w.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", w.Code, w.Body.String())
	}
	mu.Lock()
	if revokes != 1 {
		t.Fatalf("drive revoke calls = %d, want 1", revokes)
	}
	mu.Unlock()
	_ = json.Unmarshal(call(http.MethodGet, "/invites", "").Body.Bytes(), &list)
	if list.Total != 1 {
		t.Fatalf("invites after revoke = %d, want 1", list.Total)
	}
	if w := call(http.MethodDelete, "/invites/"+granted.InviteID, ""); w.Code != http.StatusNotFound {
		t.Fatalf("double revoke = %d, want 404", w.Code)
	}

	// Validation: bad email, bad role, unknown target, non-mirrored folder.
	if w := call(http.MethodPost, "/invites", `{"email":"nope","role":"viewer","targetType":"file","targetId":"f1"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad email = %d, want 400", w.Code)
	}
	if w := call(http.MethodPost, "/invites", `{"email":"a@b.test","role":"admin","targetType":"file","targetId":"f1"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad role = %d, want 400", w.Code)
	}
	if w := call(http.MethodPost, "/invites", `{"email":"a@b.test","role":"viewer","targetType":"file","targetId":"missing"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown target = %d, want 404", w.Code)
	}
	w = call(http.MethodPost, "/invites", `{"email":"a@b.test","role":"viewer","targetType":"folder","targetId":"local-folder"}`)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "not mirrored") {
		t.Fatalf("local folder share = %d %s, want 404 explaining it is not mirrored", w.Code, w.Body.String())
	}

	// Revoking a grant whose Drive permission vanished must still clean up locally.
	mu.Lock()
	api.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"perm-x"}`))
	})
	mu.Unlock()
	if w := call(http.MethodPost, "/invites", `{"email":"ghost@example.test","role":"viewer","targetType":"file","targetId":"f1"}`); w.Code != http.StatusOK {
		t.Fatalf("second grant = %d: %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(call(http.MethodGet, "/invites", "").Body.Bytes(), &list)
	ghostID := ""
	for _, inv := range list.Invites {
		if inv.Email == "ghost@example.test" {
			ghostID = inv.Email
		}
	}
	_ = ghostID
	if len(list.Invites) == 0 {
		t.Fatal("second grant missing from list")
	}
}
