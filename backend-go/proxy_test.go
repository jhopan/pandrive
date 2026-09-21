package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Proxy settings: provider validation, Caddyfile generation to a writable config dir, systemd skip
// when not root.
func TestProxySettingsAndCaddyfile(t *testing.T) {
	app := newTestApp(t)
	token, _ := registerAndLogin(t, app, "proxy@example.test")
	H := map[string]string{"Authorization": "Bearer " + token}

	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range H {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		return w
	}

	if w := do("PUT", "/api/settings/proxy", `{"provider":"nginx","domain":"x.com"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad provider = %d, want 400", w.Code)
	}
	if w := do("PUT", "/api/settings/proxy", `{"provider":"caddy","domain":"notadomain"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad domain = %d, want 400", w.Code)
	}
	if w := do("PUT", "/api/settings/proxy", `{"provider":"caddy","domain":"drive.example.com"}`); w.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", w.Code, w.Body.String())
	}
	if got := app.settingValue("proxy_provider"); got != "caddy" {
		t.Fatalf("provider stored = %q", got)
	}

	// Point the config dir at a temp dir for a deterministic write.
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp) // linux
	t.Setenv("AppData", tmp)         // windows (UserConfigDir)
	w := do("POST", "/api/settings/proxy/caddyfile", `{"domain":"drive.example.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("caddyfile = %d: %s", w.Code, w.Body.String())
	}
	written := filepath.Join(tmp, "pandrive", "Caddyfile")
	if _, err := os.Stat(written); err != nil {
		t.Fatalf("Caddyfile not written to %s: %v", written, err)
	}
	content, _ := os.ReadFile(written)
	if !strings.Contains(string(content), "drive.example.com") || !strings.Contains(string(content), "reverse_proxy 127.0.0.1:") {
		t.Fatalf("Caddyfile content wrong: %s", content)
	}
	if !strings.Contains(w.Body.String(), `"systemdReloaded":false`) {
		t.Fatalf("non-root must not claim systemd reload: %s", w.Body.String())
	}

	// GET reflects the stored values.
	w = do("GET", "/api/settings/proxy", "")
	if !strings.Contains(w.Body.String(), `"provider":"caddy"`) || !strings.Contains(w.Body.String(), `"written":true`) {
		t.Fatalf("proxy GET = %s", w.Body.String())
	}
}
