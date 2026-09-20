package main

import (
	"bytes"
	"encoding/json"
	"golang.org/x/crypto/bcrypt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func registerAndLogin(t *testing.T, app *App, email string) (string, authUser) {
	t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte("password-123"), 10)
	user := authUser{ID: randomID(), Name: "OAuth Test", Email: email}
	app.DB.Exec(`INSERT INTO users (id,name,email,password_hash) VALUES (?,?,?,?)`, user.ID, user.Name, user.Email, hash)

	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewBufferString(`{"email":"`+email+`","password":"password-123"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", w.Code, w.Body.String())
	}
	var session struct {
		AccessToken string   `json:"accessToken"`
		User        authUser `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	return session.AccessToken, session.User
}

func TestGoogleConfigAndConnectURL(t *testing.T) {
	app := newTestApp(t)
	token, _ := registerAndLogin(t, app, "oauth@example.test")
	r := app.Router()

	config := httptest.NewRequest(http.MethodPost, "/system/google-config", bytes.NewBufferString(`{"clientId":"client-id","clientSecret":"client-secret","redirectUri":"http://localhost:4000/connected-accounts/google/callback"}`))
	config.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, config)
	if w.Code != http.StatusCreated {
		t.Fatalf("config = %d: %s", w.Code, w.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/connected-accounts/google/connect-url", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("url = %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.URL, "accounts.google.com/o/oauth2") || !strings.Contains(response.URL, "state=") {
		t.Fatalf("unexpected oauth url: %s", response.URL)
	}
}

func TestGoogleConnectRequiresConfig(t *testing.T) {
	app := newTestApp(t)
	token, _ := registerAndLogin(t, app, "missing-config@example.test")
	request := httptest.NewRequest(http.MethodGet, "/connected-accounts/google/connect-url", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, request)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
}
