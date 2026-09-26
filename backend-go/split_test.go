package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// setupSplitEnv wires two connected accounts against a stub Drive that serves
// resumable uploads and media reads, and registers the storage rows.
func setupSplitEnv(t *testing.T) (*App, string, *splitStub) {
	t.Helper()
	app := newTestApp(t)
	stub := newSplitStub(t)
	app.GoogleDriveAPIURL = stub.base + "/drive/v3"
	app.GoogleUploadAPIURL = stub.base + "/upload/drive/v3/files"

	token, user := registerAndLogin(t, app, "split@example.test")
	_, _ = app.DB.Exec(`INSERT INTO provider_configs (id,user_id,provider,client_id_encrypted,client_secret_encrypted,redirect_uri) VALUES (?,?,?,?,?,?)`, "cfg", user.ID, "google_drive", app.encrypt("cid"), app.encrypt("secret"), "http://localhost:4000/cb")
	for _, row := range []struct{ id, email, tok string }{
		{"accA", "a@example.test", "tok-a"},
		{"accB", "b@example.test", "tok-b"},
	} {
		_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes,provider_config_id) VALUES (?,?,?,?,?,?,?,?,?,?)`, row.id, user.ID, "google_drive", row.id, row.email, app.encrypt(row.tok), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]", "cfg")
	}
	for _, id := range []string{"accA", "accB"} {
		_, _ = app.DB.Exec(`INSERT INTO storage_accounts (id,connected_account_id,total_bytes,used_bytes,available_bytes) VALUES (?,?,?,?,?)`, "s-"+id, id, 0, 0, 0)
	}
	return app, token, stub
}

// splitStub is a minimal Drive fake: resumable init (Location header), chunk PUT
// (bytes accepted), file metadata, alt=media (bytes out).
type splitStub struct {
	mu      sync.Mutex
	base    string
	buckets map[string][]byte // provider file id -> bytes
	nextID  int
	sess    map[string]string // session path -> bucket id
}

func newSplitStub(t *testing.T) *splitStub {
	s := &splitStub{buckets: map[string][]byte{}, sess: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/upload/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		s.nextID++
		id := fmt.Sprintf("part-file-%d", s.nextID)
		uri := s.base + "/upload/session/" + id
		s.sess["/upload/session/"+id] = id
		w.Header().Set("Location", uri)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload/session/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id := s.sess[r.URL.Path]
		if r.Method == http.MethodPut {
			b, _ := io.ReadAll(r.Body)
			s.buckets[id] = append(s.buckets[id], b...)
			cr := r.Header.Get("Content-Range")
			// Final chunk when the range's total equals what we now hold.
			if i := strings.LastIndex(cr, "/"); i >= 0 {
				var total int64
				if _, err := fmt.Sscan(cr[i+1:], &total); err == nil && total == int64(len(s.buckets[id])) {
					_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": "part", "mimeType": "application/octet-stream", "size": fmt.Sprint(len(s.buckets[id]))})
					return
				}
			}
			w.Header().Set("Range", "bytes=0-"+fmt.Sprint(len(s.buckets[id])-1))
			w.WriteHeader(http.StatusPermanentRedirect)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/drive/v3/files/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		if r.URL.Query().Get("alt") == "media" {
			w.Write(s.buckets[id])
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": "part", "mimeType": "application/octet-stream", "size": fmt.Sprint(len(s.buckets[id]))})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s.base = srv.URL
	return s
}

// planSplit must divide proportional to free space, and refuse when the total is short.
func TestPlanSplitProportionalAndRefusal(t *testing.T) {
	app, _, _ := setupSplitEnv(t)
	var userID string
	if err := app.DB.QueryRow(`SELECT id FROM users WHERE email='split@example.test'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	// Unknown user: nothing to plan.
	if plans, _ := app.planSplit("nobody", 16<<30); plans != nil {
		t.Fatalf("unknown user should plan nothing, got %v", plans)
	}

	oldBuf, oldMin := splitBufferBytes, splitMinSize
	splitBufferBytes = 0
	splitMinSize = 0
	defer func() { splitBufferBytes = oldBuf; splitMinSize = oldMin }()

	// 16.5 GB file, free 15+1 (=16) -> refuse (total short by 0.5 GB).
	_, _ = app.DB.Exec(`UPDATE storage_accounts SET available_bytes=? WHERE connected_account_id='accA'`, int64(15)<<30)
	_, _ = app.DB.Exec(`UPDATE storage_accounts SET available_bytes=? WHERE connected_account_id='accB'`, int64(1)<<30)
	if plans, _ := app.planSplit(userID, (33<<30)/2); plans != nil {
		t.Fatalf("expected refusal when total is short, got %v", plans)
	}

	// 20 MB free + 8 MB free, 24 MB file -> feasible, proportional (A gets 16, B gets 8).
	_, _ = app.DB.Exec(`UPDATE storage_accounts SET available_bytes=? WHERE connected_account_id='accA'`, int64(20)<<20)
	_, _ = app.DB.Exec(`UPDATE storage_accounts SET available_bytes=? WHERE connected_account_id='accB'`, int64(8)<<20)
	plans, err := app.planSplit(userID, 24<<20)
	if err != nil {
		t.Fatal(err)
	}
	if plans == nil || len(plans) != 2 {
		t.Fatalf("expected 2-part plan, got %v", plans)
	}
	var total int64
	for _, p := range plans {
		total += p.Size
	}
	if total != 24<<20 {
		t.Fatalf("parts sum %d != size %d", total, 24<<20)
	}
	if plans[0].Size != 20<<20 || plans[1].Size != 4<<20 {
		t.Fatalf("greedy split wrong: %+v", plans)
	}
}

// Full round trip: split-init -> upload each part via the chunk endpoint -> merged download.
func TestSplitUploadAndMergedDownload(t *testing.T) {
	oldBuf := splitBufferBytes
	splitMin := splitMinSize
	splitBufferBytes = 0
	splitMinSize = 0
	defer func() { splitBufferBytes = oldBuf; splitMinSize = splitMin }()

	app, token, stub := setupSplitEnv(t)
	var userID string
	_ = app.DB.QueryRow(`SELECT id FROM users WHERE email='split@example.test'`).Scan(&userID)
	_, _ = app.DB.Exec(`UPDATE storage_accounts SET available_bytes=? WHERE connected_account_id='accA'`, int64(20)<<20)
	_, _ = app.DB.Exec(`UPDATE storage_accounts SET available_bytes=? WHERE connected_account_id='accB'`, int64(8)<<20)

	r := app.Router()
	init := httptest.NewRequest(http.MethodPost, "/uploads/split-init", strings.NewReader(`{"fileName":"movie.mkv","mimeType":"video/x-matroska","sizeBytes":"25165824"}`))
	init.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, init)
	if w.Code != http.StatusOK {
		t.Fatalf("split-init = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		SplitID string `json:"splitId"`
		Parts   []struct {
			Index     string `json:"index"`
			Size      string `json:"sizeBytes"`
			SessionID string `json:"sessionId"`
			AccountID string `json:"accountId"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Parts) != 2 || resp.SplitID == "" {
		t.Fatalf("expected 2 parts, got %+v", resp)
	}

	// Payload sizes follow the plan: A=16 MiB "A"s, B=8 MiB "B"s (proportional order).
	payloadA := strings.Repeat("A", 20<<20)
	payloadB := strings.Repeat("B", 4<<20)
	parts := map[string]string{}
	for _, p := range resp.Parts {
		if p.AccountID == "accA" {
			parts[p.SessionID] = payloadA
		} else {
			parts[p.SessionID] = payloadB
		}
	}
	for _, p := range resp.Parts {
		body := parts[p.SessionID]
		req := httptest.NewRequest(http.MethodPut, "/uploads/resumable/chunk/"+p.SessionID, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
		wr := httptest.NewRecorder()
		r.ServeHTTP(wr, req)
		if wr.Code != http.StatusOK {
			t.Fatalf("chunk part %s = %d: %s", p.Index, wr.Code, wr.Body.String())
		}
	}

	var status string
	if err := app.DB.QueryRow(`SELECT status FROM split_files WHERE id=?`, resp.SplitID).Scan(&status); err != nil {
		t.Fatalf("split row missing: %v", err)
	}
	if status != "complete" {
		t.Fatalf("split status = %q", status)
	}

	var anyPartFile string
	if err := app.DB.QueryRow(`SELECT file_id FROM split_parts WHERE split_id=? ORDER BY part_index LIMIT 1`, resp.SplitID).Scan(&anyPartFile); err != nil {
		t.Fatal(err)
	}
	dl := httptest.NewRequest(http.MethodGet, "/files/"+anyPartFile+"/download", nil)
	dl.Header.Set("Authorization", "Bearer "+token)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, dl)
	if w2.Code != http.StatusOK {
		t.Fatalf("download = %d: %s", w2.Code, w2.Body.String())
	}
	got := w2.Body.String()
	if got != payloadA+payloadB {
		t.Fatalf("merged download mismatch: got %d bytes, want %d", len(got), len(payloadA)+len(payloadB))
	}
	if got[:1<<20] != strings.Repeat("A", 1<<20) || !strings.HasSuffix(got, strings.Repeat("B", 1<<20)) {
		t.Fatal("part order wrong in merged stream")
	}
	if w2.Header().Get("X-Split-Parts") != "2" {
		t.Fatalf("X-Split-Parts = %q", w2.Header().Get("X-Split-Parts"))
	}
	if len(stub.buckets) != 2 {
		t.Fatalf("stub buckets = %d, want 2", len(stub.buckets))
	}
}

// Scenario: 30 GB file with A=1, B=5, C=10, D=15, E=10 GB free. Greedy fills the
// biggest accounts first and leaves the small ones untouched.
func TestPlanSplitThirtyGBFiveAccounts(t *testing.T) {
	app, _, _ := setupSplitEnv(t)
	var userID string
	if err := app.DB.QueryRow(`SELECT id FROM users WHERE email='split@example.test'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	oldBuf, oldMin := splitBufferBytes, splitMinSize
	splitBufferBytes = 0
	splitMinSize = 0
	defer func() { splitBufferBytes = oldBuf; splitMinSize = oldMin }()

	// Free space per the user's scenario (GiB).
	sizes := map[string]int64{
		"accA": 1 << 30,
		"accB": 5 << 30,
		"accC": 10 << 30,
		"accD": 15 << 30,
		"accE": 10 << 30,
	}
	for id, free := range sizes {
		if _, err := app.DB.Exec(`INSERT OR IGNORE INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes,provider_config_id) VALUES (?,?,?,?,?,?,?,?,?,?)`, id, userID, "google_drive", id, id+"@x.test", app.encrypt("t"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]", "cfg"); err != nil {
			t.Fatal(err)
		}
		if _, err := app.DB.Exec(`INSERT INTO storage_accounts (id,connected_account_id,total_bytes,used_bytes,available_bytes) VALUES (?,?,-1,0,?) ON CONFLICT(connected_account_id) DO UPDATE SET available_bytes=excluded.available_bytes`, "st-"+id, id, free); err != nil {
			t.Fatal(err)
		}
	}

	// 30 GB fits: D then C then E, B and A stay untouched.
	plans, err := app.planSplit(userID, 30<<30)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 {
		t.Fatalf("expected 3 parts (D,C,E), got %d: %+v", len(plans), plans)
	}
	want := []struct {
		id   string
		size int64
	}{
		{"accD", 15 << 30},
		{"accC", 10 << 30},
		{"accE", 5 << 30},
	}
	for i, w := range want {
		if plans[i].AccountID != w.id || plans[i].Size != w.size {
			t.Fatalf("part %d = %s/%d, want %s/%d", i, plans[i].AccountID, plans[i].Size, w.id, w.size)
		}
	}
}

// Same accounts, 31 GB file: E takes the remaining 6 GB; B and A stay untouched.
func TestPlanSplitThirtyOneGBFillsNextLargest(t *testing.T) {
	app, _, _ := setupSplitEnv(t)
	var userID string
	if err := app.DB.QueryRow(`SELECT id FROM users WHERE email='split@example.test'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	oldBuf, oldMin := splitBufferBytes, splitMinSize
	splitBufferBytes = 0
	splitMinSize = 0
	defer func() { splitBufferBytes = oldBuf; splitMinSize = oldMin }()

	sizes := map[string]int64{
		"accA": 1 << 30,
		"accB": 5 << 30,
		"accC": 10 << 30,
		"accD": 15 << 30,
		"accE": 10 << 30,
	}
	for id, free := range sizes {
		if _, err := app.DB.Exec(`INSERT OR IGNORE INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes,provider_config_id) VALUES (?,?,?,?,?,?,?,?,?,?)`, id, userID, "google_drive", id, id+"@x.test", app.encrypt("t"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]", "cfg"); err != nil {
			t.Fatal(err)
		}
		if _, err := app.DB.Exec(`INSERT INTO storage_accounts (id,connected_account_id,total_bytes,used_bytes,available_bytes) VALUES (?,?,-1,0,?) ON CONFLICT(connected_account_id) DO UPDATE SET available_bytes=excluded.available_bytes`, "st-"+id, id, free); err != nil {
			t.Fatal(err)
		}
	}

	plans, err := app.planSplit(userID, 31<<30)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 {
		t.Fatalf("expected 3 parts (D,C,E), got %d: %+v", len(plans), plans)
	}
	want := []struct {
		id   string
		size int64
	}{
		{"accD", 15 << 30},
		{"accC", 10 << 30},
		{"accE", 6 << 30},
	}
	for i, w := range want {
		if plans[i].AccountID != w.id || plans[i].Size != w.size {
			t.Fatalf("part %d = %s/%d, want %s/%d", i, plans[i].AccountID, plans[i].Size, w.id, w.size)
		}
	}
}

// With the real 128 MB buffer on, the exact-fit case refuses: 2+3+5 GB minus
// 3 buffers = 9.625 GB usable < 10 GB.
func TestPlanSplitExactFitRefusedByBuffer(t *testing.T) {
	app, _, _ := setupSplitEnv(t)
	var userID string
	if err := app.DB.QueryRow(`SELECT id FROM users WHERE email='split@example.test'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	oldBuf, oldMin := splitBufferBytes, splitMinSize
	splitBufferBytes = 128 << 20
	splitMinSize = 0
	defer func() { splitBufferBytes = oldBuf; splitMinSize = oldMin }()

	sizes := map[string]int64{
		"accA": 2 << 30,
		"accB": 3 << 30,
		"accC": 5 << 30,
	}
	for id, free := range sizes {
		if _, err := app.DB.Exec(`INSERT OR IGNORE INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes,provider_config_id) VALUES (?,?,?,?,?,?,?,?,?,?)`, id, userID, "google_drive", id, id+"@x.test", app.encrypt("t"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]", "cfg"); err != nil {
			t.Fatal(err)
		}
		if _, err := app.DB.Exec(`INSERT INTO storage_accounts (id,connected_account_id,total_bytes,used_bytes,available_bytes) VALUES (?,?,-1,0,?) ON CONFLICT(connected_account_id) DO UPDATE SET available_bytes=excluded.available_bytes`, "st-"+id, id, free); err != nil {
			t.Fatal(err)
		}
	}

	if plans, _ := app.planSplit(userID, 10<<30); plans != nil {
		t.Fatalf("exact fit must refuse (buffer), got %+v", plans)
	}
	// 9.6 GiB fits inside the 9.625 GiB usable window.
	wantSize := int64(9830400) << 10 // 9.6 GiB in KiB-shifted integer math
	plans, err := app.planSplit(userID, wantSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) < 2 {
		t.Fatalf("9.6 GiB should split across >=2 parts, got %+v", plans)
	}
	var total int64
	for _, pl := range plans {
		total += pl.Size
	}
	if total != wantSize {
		t.Fatalf("parts sum %d != %d", total, wantSize)
	}
}
