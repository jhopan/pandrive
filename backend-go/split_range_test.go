package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Split download must honor Range: single-part window, cross-boundary window, and suffix.
func TestSplitDownloadRangeWindows(t *testing.T) {
	// 3 parts of 4 bytes: "AAAA" "BBBB" "CCCC" -> logical 12 bytes.
	app, token := newSplitRangeEnv(t, []string{"AAAA", "BBBB", "CCCC"})
	r := app.Router()

	cases := []struct {
		name   string
		rng    string
		status int
		want   string
		cr     string
	}{
		{"full", "", 200, "AAAABBBBCCCC", ""},
		{"middle-only", "bytes=4-7", 206, "BBBB", "bytes 4-7/12"},
		{"cross-boundary", "bytes=2-9", 206, "AABBBBCC", "bytes 2-9/12"},
		{"tail-suffix", "bytes=-3", 206, "CCC", "bytes 9-11/12"},
		{"head", "bytes=0-1", 206, "AA", "bytes 0-1/12"},
		{"resume-from-10", "bytes=10-", 206, "CC", "bytes 10-11/12"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/files/part-file-1/download", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if tc.rng != "" {
			req.Header.Set("Range", tc.rng)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("%s: status = %d body %s", tc.name, w.Code, w.Body.String())
		}
		if w.Body.String() != tc.want {
			t.Fatalf("%s: body = %q (%d bytes) want %q | headers CR=%q CL=%q", tc.name, w.Body.String(), w.Body.Len(), tc.want, w.Header().Get("Content-Range"), w.Header().Get("Content-Length"))
		}
		if tc.cr != "" && w.Header().Get("Content-Range") != tc.cr {
			t.Fatalf("%s: content-range = %q want %q", tc.name, w.Header().Get("Content-Range"), tc.cr)
		}
		if tc.rng == "" && w.Header().Get("Accept-Ranges") != "bytes" {
			t.Fatalf("%s: missing Accept-Ranges", tc.name)
		}
	}
}

// parseByteRange unit checks (malformed, out of range).
func TestParseByteRange(t *testing.T) {
	if _, _, ok := parseByteRange("bytes=abc", 100); ok {
		t.Fatal("malformed accepted")
	}
	if _, _, ok := parseByteRange("bytes=200-300", 100); ok {
		t.Fatal("out-of-range accepted")
	}
	s, e, ok := parseByteRange("bytes=50-", 100)
	if !ok || s != 50 || e != 99 {
		t.Fatalf("open-ended = %d-%d ok=%v", s, e, ok)
	}
}

func newSplitRangeEnv(t *testing.T, partBytes []string) (*App, string) {
	t.Helper()
	app := newTestApp(t)
	// Media stub keyed by provider id; Range support via simple slicing.
	mux := http.NewServeMux()
	for i, data := range partBytes {
		id := fmt.Sprintf("drv-p%d", i+1)
		part := data // capture per-iteration copy
		mux.HandleFunc("/drive/v3/files/"+id, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("alt") != "media" {
				http.NotFound(w, r)
				return
			}
			rng := r.Header.Get("Range")
			if rng == "" {
				w.Write([]byte(part))
				return
			}
			var st, en int64
			if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &st, &en); err == nil {
				w.WriteHeader(http.StatusPartialContent)
				w.Write([]byte(part)[st : en+1])
			}
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	app.GoogleDriveAPIURL = srv.URL + "/drive/v3"

	token, user := registerAndLogin(t, app, "range@example.test")
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,access_token_encrypted,refresh_token_encrypted,token_expires_at,scopes) VALUES (?,?,?,?,?,?,?,?,?)`, "acc", user.ID, "google_drive", "p", "r@example.test", app.encrypt("tok"), app.encrypt("r"), "2099-01-01T00:00:00Z", "[]")
	_, _ = app.DB.Exec(`INSERT INTO split_files (id,user_id,name,mime_type,size_bytes,part_count,status) VALUES ('sp',?,?,?,?,?,'complete')`, user.ID, "movie.mkv", "video/x-matroska", 4*len(partBytes), len(partBytes))
	for i, data := range partBytes {
		fid := fmt.Sprintf("part-file-%d", i+1)
		if _, err := app.DB.Exec(`INSERT INTO files (id,user_id,connected_account_id,provider,provider_file_id,name,mime_type,size_bytes) VALUES (?,?,?,?,?,?,?,?)`, fid, user.ID, "acc", "google_drive", fmt.Sprintf("drv-p%d", i+1), fmt.Sprintf("pd-split-x.part%03d", i+1), "application/octet-stream", len(data)); err != nil {
			t.Fatal(err)
		}
		if _, err := app.DB.Exec(`INSERT INTO split_parts (id,split_id,part_index,file_id,connected_account_id,size_bytes) VALUES (?,?,?,?,?,?)`, fmt.Sprintf("pp%d", i+1), "sp", i+1, fid, "acc", len(data)); err != nil {
			t.Fatal(err)
		}
	}
	return app, token
}

var _ = json.Marshal
var _ = strings.TrimSpace
