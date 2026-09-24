package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Rebalancer: analyze builds a largest-first dry-run plan bounded by destination free
// space; execute performs server-side transfers per file and reports per-file results.
func TestRebalanceAnalyzePlansLargestFirst(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "rebal@example.test")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes) VALUES (?,?,?,?,?,?,?,?,?)`, "src", user.ID, "google_drive", "p", "src@x.test", app.encrypt("t"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes) VALUES (?,?,?,?,?,?,?,?,?)`, "dst", user.ID, "google_drive", "q", "dst@x.test", app.encrypt("t"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]")
	_, _ = app.DB.Exec(`INSERT INTO storage_accounts (id,connected_account_id,total_bytes,used_bytes,available_bytes) VALUES ('ss','src',100000,90000,10000)`)
	_, _ = app.DB.Exec(`INSERT INTO storage_accounts (id,connected_account_id,total_bytes,used_bytes,available_bytes) VALUES ('sd','dst',50000,0,50000)`)
	for i, size := range []int64{30000, 25000, 20000, 10000} {
		_, _ = app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, fmt.Sprintf("f%d", i+1), user.ID, "src", "google_drive", fmt.Sprintf("drv-%d", i+1), fmt.Sprintf("file%d.bin", i+1), "application/octet-stream", size)
	}

	r := app.Router()
	req := httptest.NewRequest(http.MethodPost, "/rebalance/analyze", strings.NewReader(`{"sourceAccountId":"src","targetPercent":80}`))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("analyze = %d: %s", w.Code, w.Body.String())
	}
	var plan struct {
		Moves []struct {
			FileID    string `json:"fileId"`
			SizeBytes string `json:"sizeBytes"`
		} `json:"moves"`
		MovedBytes string `json:"movedBytes"`
		ResultUsed string `json:"resultUsedBytes"`
		Target     string `json:"targetAccountId"`
		TargetFree string `json:"targetFreeBytes"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Target != "dst" {
		t.Fatalf("auto target = %q", plan.Target)
	}
	if len(plan.Moves) != 1 || plan.Moves[0].FileID != "f1" {
		t.Fatalf("moves = %+v", plan.Moves)
	}
	if plan.ResultUsed != "60000" {
		t.Fatalf("result used = %q", plan.ResultUsed)
	}
}

func TestRebalanceAnalyzeRefusesWhenShort(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "rebal2@example.test")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes) VALUES (?,?,?,?,?,?,?,?,?)`, "src", user.ID, "google_drive", "p", "s@x.test", app.encrypt("t"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes) VALUES (?,?,?,?,?,?,?,?,?)`, "dst", user.ID, "google_drive", "q", "d@x.test", app.encrypt("t"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]")
	_, _ = app.DB.Exec(`INSERT INTO storage_accounts (id,connected_account_id,total_bytes,used_bytes,available_bytes) VALUES ('ss','src',100000,90000,10000)`)
	_, _ = app.DB.Exec(`INSERT INTO storage_accounts (id,connected_account_id,total_bytes,used_bytes,available_bytes) VALUES ('sd','dst',1000,0,1000)`)
	_, _ = app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES ('f1',?,?,?,?,?,?,?)`, user.ID, "src", "google_drive", "drv-1", "big.bin", "application/octet-stream", 500)

	req := httptest.NewRequest(http.MethodPost, "/rebalance/analyze", strings.NewReader(`{"sourceAccountId":"src","targetPercent":10}`))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("analyze = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "can only move") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestRebalanceExecuteTransfersServerSide(t *testing.T) {
	app := newTestApp(t)
	var mu sync.Mutex
	var shares, copies int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/permissions"):
			shares++
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "perm"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/copy"):
			copies++
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "copied"})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	app.GoogleDriveAPIURL = api.URL + "/drive/v3"

	token, user := registerAndLogin(t, app, "rebal3@example.test")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes) VALUES (?,?,?,?,?,?,?,?,?)`, "src", user.ID, "google_drive", "p", "s3@x.test", app.encrypt("tok"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes) VALUES (?,?,?,?,?,?,?,?,?)`, "dst", user.ID, "google_drive", "q", "d3@x.test", app.encrypt("t2"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]")
	_, _ = app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES ('f1',?,?,?,?,?,?,?)`, user.ID, "src", "google_drive", "drv-1", "a.bin", "application/octet-stream", 100)
	_, _ = app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES ('f2',?,?,?,?,?,?,?)`, user.ID, "src", "google_drive", "drv-2", "b.bin", "application/octet-stream", 200)

	req := httptest.NewRequest(http.MethodPost, "/rebalance/execute", strings.NewReader(`{"sourceAccountId":"src","targetAccountId":"dst","fileIds":["f1","f2"]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("execute = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Moved   int `json:"moved"`
		Failed  int `json:"failed"`
		Results []struct {
			FileID string `json:"fileId"`
			OK     bool   `json:"ok"`
		} `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Moved != 2 || out.Failed != 0 {
		t.Fatalf("moved=%d failed=%d body=%s", out.Moved, out.Failed, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if shares != 2 || copies != 2 {
		t.Fatalf("shares=%d copies=%d", shares, copies)
	}
	var dstCount, srcActive int
	_ = app.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE connected_account_id='dst'`).Scan(&dstCount)
	_ = app.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE connected_account_id='src' AND status='active'`).Scan(&srcActive)
	if dstCount != 2 || srcActive != 0 {
		t.Fatalf("dst=%d srcActive=%d", dstCount, srcActive)
	}
}
