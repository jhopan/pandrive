package main

import (
	"time"
	"fmt"
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2/google"
	_ "modernc.org/sqlite"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	// Unique in-memory DB per test: the shared default made tests interfere
	// (a background goroutine from one test could close the handle another test uses).
	dsn := fmt.Sprintf("file:test_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	app := &App{DB: db, Config: Config{JWTSecret: "test-secret-long-enough-for-jwt", TokenKey: "12345678901234567890123456789012", FrontendURL: "http://localhost:5173"}, HTTPClient: http.DefaultClient, GoogleEndpoint: google.Endpoint, GoogleUserInfoURL: "https://www.googleapis.com/oauth2/v2/userinfo", GoogleDriveAPIURL: "https://www.googleapis.com/drive/v3", GoogleUploadAPIURL: "https://www.googleapis.com/upload/drive/v3/files", loginFails: map[string]*loginFail{}}
	if err := app.migrate(); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestHealth(t *testing.T) {
	r := newTestApp(t).Router()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestRegisterLoginAndProtectedRoute(t *testing.T) {
	app := newTestApp(t)
	r := app.Router()
	body := []byte(`{"name":"Test","email":"test@example.com","password":"password-123"}`)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/auth/register", bytes.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader([]byte(`{"email":"test@example.com","password":"password-123"}`))))
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", w.Code, w.Body.String())
	}
	var login struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	if login.AccessToken == "" || login.RefreshToken == "" {
		t.Fatal("missing session tokens")
	}

	request := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	request.Header.Set("Authorization", "Bearer "+login.AccessToken)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("me = %d: %s", w.Code, w.Body.String())
	}
}

func TestProtectedRouteRejectsMissingToken(t *testing.T) {
	r := newTestApp(t).Router()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/me", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", w.Code)
	}
}
