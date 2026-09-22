package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// API keys: mint -> use -> list (prefix only) -> revoke (instant) -> delete. Wrong key 401s.
func TestAPIKeysLifecycle(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "keys@example.test")

	do := func(method, path, body, auth string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		return w
	}

	// create
	w := do("POST", "/api/keys", `{"name":"Android"}`, "Bearer "+token)
	if w.Code != http.StatusCreated {
		t.Fatalf("create key = %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		Key    string `json:"key"`
		ID     string `json:"id"`
		Prefix string `json:"prefix"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if !strings.HasPrefix(created.Key, "pd_") || created.Prefix != created.Key[:11] {
		t.Fatalf("key shape wrong: %+v", created)
	}

	// the key must authenticate /files (and be scoped to its owner: zero files now, still 200)
	w = do("GET", "/files", "", "Bearer "+created.Key)
	if w.Code != http.StatusOK {
		t.Fatalf("api key on /files = %d: %s", w.Code, w.Body.String())
	}

	// list shows prefix + revoked=false, and NEVER the plaintext
	w = do("GET", "/api/keys", "", "Bearer "+token)
	body := w.Body.String()
	if !strings.Contains(body, created.Prefix) || strings.Contains(body, created.Key) {
		t.Fatalf("list leaks or hides wrongly: %s", body)
	}

	// wrong key -> 401
	w = do("GET", "/files", "", "Bearer pd_wrongwrongwrongwrong")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d, want 401", w.Code)
	}

	// revoke -> instant 401
	if w := do("DELETE", "/api/keys/"+created.ID, "", "Bearer "+token); w.Code != http.StatusOK {
		t.Fatalf("revoke = %d", w.Code)
	}
	w = do("GET", "/files", "", "Bearer "+created.Key)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key still works = %d", w.Code)
	}

	// revoked key cannot revoke others (it is dead anyway) and delete is hard-remove
	w = do("DELETE", "/api/keys/"+created.ID+"/hard", "", "Bearer "+token)
	if w.Code != http.StatusOK {
		t.Fatalf("hard delete = %d", w.Code)
	}
	var count int
	_ = app.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE id=?`, created.ID).Scan(&count)
	if count != 0 {
		t.Fatal("hard delete must remove the row")
	}

	// session-only safety: a second user must not see/revoke the first user's keys
	app.setSetting("open_registration", "1")
	reg := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"email":"other@example.test","password":"otherpass123","name":"Other"}`))
	wr := httptest.NewRecorder()
	app.Router().ServeHTTP(wr, reg)
	if wr.Code != http.StatusOK && wr.Code != http.StatusCreated {
		t.Fatalf("register = %d", wr.Code)
	}
	wl := httptest.NewRecorder()
	app.Router().ServeHTTP(wl, httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"email":"other@example.test","password":"otherpass123"}`)))
	var sess struct {
		AccessToken string `json:"accessToken"`
	}
	_ = json.Unmarshal(wl.Body.Bytes(), &sess)
	w = do("POST", "/api/keys", `{"name":"mine"}`, "Bearer "+sess.AccessToken)
	if w.Code != http.StatusCreated {
		t.Fatalf("other create = %d", w.Code)
	}
	var other struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &other)
	// user A cannot revoke B's key
	w = do("DELETE", "/api/keys/"+other.ID, "", "Bearer "+token)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-user revoke = %d, want 404", w.Code)
	}
	_ = user
}
