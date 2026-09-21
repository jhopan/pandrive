package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConnectedAccountsListsOnlyCurrentUser(t *testing.T) {
	app := newTestApp(t)
	tokenA, userA := registerAndLogin(t, app, "accounts-a@example.test")
	_, userB := registerAndLogin(t, app, "accounts-b@example.test")
	_, err := app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "account-a", userA.ID, "google_drive", "google-a", "drive-a@example.test", "[]")
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "account-b", userB.ID, "google_drive", "google-b", "drive-b@example.test", "[]")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/connected-accounts", nil)
	req.Header.Set("Authorization", "Bearer "+tokenA)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Accounts []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 1 || body.Accounts[0].ID != "account-a" {
		t.Fatalf("accounts = %#v", body.Accounts)
	}
}

func TestStorageSummaryTotalsAccounts(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "summary@example.test")
	_, err := app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes) VALUES (?,?,?,?,?,?)`, "account", user.ID, "google_drive", "google", "drive@example.test", "[]")
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.DB.Exec(`INSERT INTO storage_accounts (id,connected_account_id,total_bytes,used_bytes,available_bytes) VALUES (?,?,?,?,?)`, "storage", "account", 1000, 250, 750)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/storage/summary", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		TotalBytes     string `json:"totalBytes"`
		UsedBytes      string `json:"usedBytes"`
		AvailableBytes string `json:"availableBytes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.TotalBytes != "1000" || body.UsedBytes != "250" || body.AvailableBytes != "750" {
		t.Fatalf("summary = %#v", body)
	}
}
