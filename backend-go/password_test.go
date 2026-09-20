package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Changing a password must prove the current one, invalidate other sessions, and refuse the
// password-through-/auth/me path that would let a stolen token rotate it.
func TestChangePasswordFlow(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "pw@example.test")

	post := func(path, body string, bearer string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		return w
	}
	login := func(password string) int {
		t.Helper()
		return post("/auth/login", `{"email":"pw@example.test","password":"`+password+`"}`, "").Code
	}

	if got := login("password-123"); got != http.StatusOK {
		t.Fatalf("baseline login = %d, want 200", got)
	}

	// Wrong current password, weak new password, unchanged password, missing current password.
	if w := post("/auth/change-password", `{"currentPassword":"wrong","newPassword":"brandnew123"}`, token); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current = %d, want 401", w.Code)
	}
	if w := post("/auth/change-password", `{"currentPassword":"password-123","newPassword":"short"}`, token); w.Code != http.StatusBadRequest {
		t.Fatalf("weak new = %d, want 400", w.Code)
	}
	if w := post("/auth/change-password", `{"currentPassword":"password-123","newPassword":"password-123"}`, token); w.Code != http.StatusBadRequest {
		t.Fatalf("same password = %d, want 400", w.Code)
	}
	if w := post("/auth/change-password", `{"newPassword":"brandnew123"}`, token); w.Code != http.StatusBadRequest {
		t.Fatalf("missing current = %d, want 400", w.Code)
	}
	if got := login("password-123"); got != http.StatusOK {
		t.Fatalf("failed attempts must not change the password (login = %d)", got)
	}

	// Session to be invalidated: one issued before the change.
	before := post("/auth/login", `{"email":"pw@example.test","password":"password-123"}`, "")
	var session struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	_ = json.Unmarshal(before.Body.Bytes(), &session)

	w := post("/auth/change-password", `{"currentPassword":"password-123","newPassword":"brandnew123"}`, token)
	if w.Code != http.StatusOK {
		t.Fatalf("change = %d: %s", w.Code, w.Body.String())
	}
	var rotated struct {
		AccessToken string `json:"accessToken"`
		User        struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.AccessToken == "" || rotated.User.Email != user.Email {
		t.Fatalf("response must contain a fresh session: %+v", rotated)
	}

	if got := login("password-123"); got != http.StatusUnauthorized {
		t.Fatalf("old password still works (login = %d, want 401)", got)
	}
	if got := login("brandnew123"); got != http.StatusOK {
		t.Fatalf("new password rejected (login = %d, want 200)", got)
	}
	// Every previously issued refresh token is revoked.
	refresh := post("/auth/refresh", `{"refreshToken":"`+session.RefreshToken+`"}`, "")
	if refresh.Code != http.StatusUnauthorized {
		t.Fatalf("old refresh token = %d, want 401 (sessions must be revoked)", refresh.Code)
	}

	// /auth/me must not accept a password (no current-password proof there).
	req := httptest.NewRequest(http.MethodPut, "/auth/me", strings.NewReader(`{"name":"N","email":"pw@example.test","password":"another123"}`))
	req.Header.Set("Authorization", "Bearer "+rotated.AccessToken)
	w2 := httptest.NewRecorder()
	app.Router().ServeHTTP(w2, req)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("password via /auth/me = %d, want 400", w2.Code)
	}
	// Name/email edits still work.
	req2 := httptest.NewRequest(http.MethodPut, "/auth/me", strings.NewReader(`{"name":"Renamed","email":"pw2@example.test"}`))
	req2.Header.Set("Authorization", "Bearer "+rotated.AccessToken)
	w3 := httptest.NewRecorder()
	app.Router().ServeHTTP(w3, req2)
	if w3.Code != http.StatusOK {
		t.Fatalf("profile update = %d: %s", w3.Code, w3.Body.String())
	}
}
