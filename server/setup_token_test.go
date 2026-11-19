package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSetupGateMatchesRetiresAndAnnounces(t *testing.T) {
	dir := t.TempDir()
	g, err := newSetupGate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.token) != setupTokenLen || strings.Count(g.display, "-") != setupTokenLen/4-1 {
		t.Fatalf("unexpected token shape: token=%q display=%q", g.token, g.display)
	}
	other, err := newSetupGate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other.token == g.token {
		t.Fatal("two gates produced the same token")
	}

	for _, in := range []string{g.display, g.token, strings.ToUpper(g.display), "  " + strings.ReplaceAll(g.display, "-", " ") + "\n"} {
		if !g.matches(in) {
			t.Errorf("valid token form %q was rejected", in)
		}
	}
	for _, in := range []string{"", g.display[:len(g.display)-1], other.display} {
		if g.matches(in) {
			t.Errorf("invalid token %q was accepted", in)
		}
	}

	g.announce()
	g.announce() // idempotent
	data, err := os.ReadFile(filepath.Join(dir, setupTokenFilename))
	if err != nil || strings.TrimSpace(string(data)) != g.display {
		t.Fatalf("announce did not write the token file: %q %v", data, err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Join(dir, setupTokenFilename)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("token file mode=%v err=%v, want 0600", fi.Mode().Perm(), err)
		}
	}

	display := g.display
	g.retire()
	if g.matches(display) {
		t.Fatal("retired token still accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, setupTokenFilename)); !os.IsNotExist(err) {
		t.Fatalf("retire left the token file behind: %v", err)
	}
	g.announce() // nothing left to announce
	if _, err := os.Stat(filepath.Join(dir, setupTokenFilename)); !os.IsNotExist(err) {
		t.Fatal("announce after retire recreated the token file")
	}

	var nilGate *setupGate
	if nilGate.matches(display) {
		t.Fatal("a missing gate must reject, never admit")
	}
	nilGate.announce()
	nilGate.retire()
}

// A web page can POST text/plain to /api/auth/* without a preflight; with "*" it
// could also read the JWT in the response. Those routes must not be
// cross-origin readable, while the SSE/API routes keep "*" for dev mode.
func TestCORSNeverExposesTokenIssuingRoutes(t *testing.T) {
	h := cors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	for _, tc := range []struct {
		method, path string
		wantACAO     bool
	}{
		{"POST", "/api/auth/login", false},
		{"POST", "/api/auth/init", false},
		{"OPTIONS", "/api/auth/init", false},
		{"GET", "/api/auth/status", false},
		{"GET", "/api/logs/stream", true},
		{"OPTIONS", "/api/tasks", true},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if got := w.Header().Get("Access-Control-Allow-Origin") != ""; got != tc.wantACAO {
			t.Errorf("%s %s: Access-Control-Allow-Origin present=%v, want %v", tc.method, tc.path, got, tc.wantACAO)
		}
	}
}

func TestAuthInitRequiresSetupToken(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Registered first so it runs last: the settings restore below needs the pool
	// open. A plain defer would close it before t.Cleanup runs, and the original
	// password hash would silently fail to come back.
	t.Cleanup(func() { m.Close() })
	dir := t.TempDir()
	gate, err := newSetupGate(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{m: m, jwtKey: []byte("setup-token-test-signing-key-0123456789"), setup: gate}
	preserveRuntimeSettings(t, s, authPassKey)
	if _, err := m.pg.Exec(`DELETE FROM settings WHERE key=$1`, authPassKey); err != nil {
		t.Fatal(err)
	}

	// Status on an uninitialized instance announces the token (idempotently).
	w := httptest.NewRecorder()
	s.authStatus(w, httptest.NewRequest("GET", "/api/auth/status", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, setupTokenFilename)); err != nil {
		t.Fatalf("status did not announce the setup token: %v", err)
	}

	doInit := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.authInit(w, httptest.NewRequest("POST", "/api/auth/init", strings.NewReader(body)))
		return w
	}
	for name, body := range map[string]string{
		"missing token": `{"password":"correct horse battery"}`,
		"wrong token":   `{"password":"correct horse battery","setup_token":"aaaa-bbbb-cccc-dddd"}`,
	} {
		w := doInit(body)
		if w.Code != 403 || strings.Contains(w.Body.String(), `"token"`) {
			t.Fatalf("%s: status=%d body=%s, want 403 without a token", name, w.Code, w.Body)
		}
		if hash, _, _ := m.pg.GetSetting(authPassKey); hash != "" {
			t.Fatalf("%s: password was set without a valid setup token", name)
		}
	}

	display := gate.display
	b, _ := json.Marshal(map[string]string{"password": "correct horse battery", "setup_token": display})
	w = doInit(string(b))
	var resp struct {
		Token string `json:"token"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resp) != nil || resp.Token == "" {
		t.Fatalf("valid setup token rejected: status=%d body=%s", w.Code, w.Body)
	}
	if gate.matches(display) {
		t.Fatal("setup token still valid after a successful init")
	}
	if _, err := os.Stat(filepath.Join(dir, setupTokenFilename)); !os.IsNotExist(err) {
		t.Fatalf("setup token file survived a successful init: %v", err)
	}
}
