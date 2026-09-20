package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The sliding-window meter must only count the last 100 seconds and expose a 100-slot history.
func TestRateMeterWindowsAndHistory(t *testing.T) {
	m := &rateMeter{}
	m.add(5)
	total, history := m.snapshot()
	if total != 5 || len(history) != 100 {
		t.Fatalf("total=%d historyLen=%d, want 5/100", total, len(history))
	}
	if history[99] != 5 {
		t.Fatalf("newest bucket = %d, want 5 (requests land in the last slot)", history[99])
	}
	if m.peakTotal() != 5 {
		t.Fatalf("peak = %d, want 5", m.peakTotal())
	}

	// A bucket older than the window must drop out of the total.
	m.mu.Lock()
	m.buckets[50] = 1000
	m.stamp[50] = 1 // epoch second 1 is far outside any 100s window
	m.mu.Unlock()
	total, _ = m.snapshot()
	if total != 5 {
		t.Fatalf("stale bucket leaked into total: %d", total)
	}
	// A bucket outside the window must not inflate the peak either.
	if m.peakTotal() != 5 {
		t.Fatalf("peak = %d, want 5 (stale bucket must not count)", m.peakTotal())
	}
}

// Every request through the counting transport must be metered.
func TestCountingTransportMetersRequests(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	meter := &rateMeter{}
	client := &http.Client{Transport: &countingTransport{base: http.DefaultTransport, meter: meter}}
	for i := 0; i < 3; i++ {
		resp, err := client.Get(api.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if total, _ := meter.snapshot(); total != 3 {
		t.Fatalf("metered %d requests, want 3", total)
	}
}

// The rate-limits endpoint reports the window, configs and honest notes.
func TestRateLimitsEndpoint(t *testing.T) {
	app := newTestApp(t)
	token, user := registerAndLogin(t, app, "ratelimit@example.test")
	_, _ = app.DB.Exec(`INSERT INTO provider_configs (id,user_id,provider,client_id_encrypted,client_secret_encrypted,redirect_uri,label) VALUES (?,?,?,?,?,?,?)`, "cfg", user.ID, "google_drive", app.encrypt("cid"), app.encrypt("cs"), "http://localhost/cb", "Primary")
	_, _ = app.DB.Exec(`INSERT INTO provider_config_quota (id,provider_config_id,request_count,window_start) VALUES (?,?,?,?)`, "q1", "cfg", 42, time.Now().UTC().Format(time.RFC3339Nano))
	_, _ = app.DB.Exec(`INSERT INTO connected_accounts (id,user_id,provider,provider_account_id,email,scopes,provider_config_id) VALUES (?,?,?,?,?,?,?)`, "acc", user.ID, "google_drive", "p", "a@example.test", "[]", "cfg")
	app.RateMeter.add(7)

	req := httptest.NewRequest(http.MethodGet, "/system/rate-limits", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("rate-limits = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		WindowSeconds    int   `json:"windowSeconds"`
		Limit            int   `json:"limit"`
		Threshold        int   `json:"threshold"`
		RequestsLast100s int   `json:"requestsLast100s"`
		History          []int `json:"history"`
		Configs          []struct {
			Label          string `json:"label"`
			FlowStarts     int    `json:"flowStarts"`
			WindowResetsIn int64  `json:"windowResetsInSeconds"`
			Accounts       int    `json:"accounts"`
		} `json:"configs"`
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.WindowSeconds != 100 || out.Limit != 10000 || out.Threshold != 8000 {
		t.Fatalf("window config = %+v", out)
	}
	if out.RequestsLast100s != 7 || len(out.History) != 100 {
		t.Fatalf("meter not reported: %d / %d", out.RequestsLast100s, len(out.History))
	}
	if len(out.Configs) != 1 || out.Configs[0].FlowStarts != 42 || out.Configs[0].Accounts != 1 {
		t.Fatalf("configs = %+v", out.Configs)
	}
	// A window started "now" is still open, so the countdown must be positive.
	if out.Configs[0].WindowResetsIn <= 0 || out.Configs[0].WindowResetsIn > 100 {
		t.Fatalf("windowResetsInSeconds = %d, want 1..100", out.Configs[0].WindowResetsIn)
	}
	if len(out.Notes) == 0 {
		t.Fatal("endpoint must explain what the counters mean")
	}
}

// The SPA shell is served with a CSP that hashes its inline bootstrap script.
func TestContentSecurityPolicyAllowsInlinedBootstrap(t *testing.T) {
	app := newTestApp(t)
	policy := app.contentSecurityPolicy()
	if !strings.Contains(policy, "script-src 'self' 'sha256-") {
		t.Fatalf("policy missing inline script hash: %s", policy)
	}
	if !strings.Contains(policy, "object-src 'none'") || !strings.Contains(policy, "frame-ancestors 'none'") {
		t.Fatalf("policy missing hardening directives: %s", policy)
	}
	// Origins the UI actually loads. A too-strict policy broke every remote image (folder icons
	// rendered as broken placeholders with naturalWidth 0), so these are asserted explicitly.
	for _, host := range []string{
		"https://fonts.gstatic.com", "https://fonts.googleapis.com",
		"https://api.iconify.design", "https://api.dicebear.com", "https://i.pravatar.cc", "https://www.gravatar.com",
		"https://cdnjs.cloudflare.com", "https://view.officeapps.live.com", "https://drive.google.com",
	} {
		if !strings.Contains(policy, host) {
			t.Fatalf("policy must allow %s (the UI loads it): %s", host, policy)
		}
	}
	// The hash must match the shipped shell, otherwise the theme script is blocked.
	blocks := inlineScriptBlocks([]byte("<html><script>a</script><script >b</script></body>"))
	if len(blocks) != 1 || string(blocks[0]) != "a" {
		t.Fatalf("inlineScriptBlocks = %q, want only attribute-less scripts", blocks)
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if got := w.Header().Get("Content-Security-Policy"); got == "" {
		t.Fatal("CSP header missing on the response")
	}
	// HSTS only over TLS.
	if w.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS must not be advertised for plain HTTP")
	}
	req2 := httptest.NewRequest(http.MethodGet, "/health", nil)
	req2.Header.Set("X-Forwarded-Proto", "https")
	w2 := httptest.NewRecorder()
	app.Router().ServeHTTP(w2, req2)
	if w2.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("HSTS missing when the request arrived over https")
	}
}
