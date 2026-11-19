package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// GetSetting 对"键不存在"和"读取出错"的返回值只差一个 error：两种情况 value 都是
// 空串。密码相关的 handler 一旦把 error 当成"还没设置密码"，就会在数据库抖动期间
// 敞开初始化入口——authInit 会放行一个未认证请求去覆盖已有的管理员密码，
// authStatus 则会把前端直接送到 /setup 去照着做这件事。
//
// 这两个测试把 handler 的连接池关掉来制造读取失败，断言两处都 fail closed。
func TestAuthStatusFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	// 关掉池子，让后续 GetSetting 返回 error 而不是 sql.ErrNoRows。
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	s.authStatus(w, httptest.NewRequest("GET", "/api/auth/status", nil))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (读失败被当成未初始化会把用户送去 /setup 覆盖密码); body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Initialized *bool `json:"initialized"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err == nil && payload.Initialized != nil {
		t.Fatalf("读失败时不应回答 initialized，得到 %v", *payload.Initialized)
	}
}

func TestAuthInitFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"password":"correct horse battery"}`)
	s.authInit(w, httptest.NewRequest("POST", "/api/auth/init", body))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (读失败时放行会让未认证请求覆盖已有密码); body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "token") {
		t.Fatalf("读失败时不应签发 token: %s", w.Body.String())
	}
}

func TestValidatePassword(t *testing.T) {
	for _, tc := range []struct {
		name, pw string
		wantErr  bool
	}{
		{"空", "", true},
		{"七位", "1234567", true},
		{"八位", "12345678", false},
		{"八个汉字按字符数而非字节数计", "密码密码密码密码", false},
		{"三个汉字够 9 字节但只有 3 个字符", "密码强", true},
		{"72 字节", strings.Repeat("a", 72), false},
		{"73 字节超出 bcrypt 上限", strings.Repeat("a", 73), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validatePassword(tc.pw); (got != "") != tc.wantErr {
				t.Fatalf("validatePassword(%q)=%q, wantErr=%v", tc.pw, got, tc.wantErr)
			}
		})
	}
}

// The artex_token cookie is written by JavaScript for the Next route guard only.
// Accepting it as an API credential let any same-site page (another port on the
// same host counts) forge admin requests, so only the header and ?token= count.
func TestRequireAuthIgnoresCookieCredential(t *testing.T) {
	s := &Server{jwtKey: []byte("cookie-csrf-test-signing-key-0123456789")}
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	h := s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	call := func(r *http.Request) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	cookieOnly := httptest.NewRequest("POST", "/api/mcp", nil)
	cookieOnly.AddCookie(&http.Cookie{Name: "artex_token", Value: tok})
	if code := call(cookieOnly); code != 401 {
		t.Fatalf("cookie-only request status=%d, want 401", code)
	}

	bearer := httptest.NewRequest("GET", "/api/tasks", nil)
	bearer.Header.Set("Authorization", "Bearer "+tok)
	if code := call(bearer); code != 200 {
		t.Fatalf("bearer request status=%d, want 200", code)
	}
	if code := call(httptest.NewRequest("GET", "/api/logs/stream?token="+tok, nil)); code != 200 {
		t.Fatalf("?token= (EventSource) request status=%d, want 200", code)
	}
}

// A password change (in-app, init, or reset-password.sh — all stamp updated_at)
// must end every session issued before it; tokens are stateless and live 7 days.
func TestPasswordChangeRevokesEarlierTokens(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() }) // registered first: the settings restore needs the pool
	s := &Server{m: m, jwtKey: []byte("revocation-test-signing-key-0123456789")}
	preserveRuntimeSettings(t, s, authPassKey)

	hash, err := bcrypt.GenerateFromPassword([]byte("old password 123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetSetting(authPassKey, string(hash)); err != nil {
		t.Fatal(err)
	}
	s.forgetPasswordEpoch()

	stale, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   "ARTEX",
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.checkToken(stale); got != tokenInvalid {
		t.Fatalf("token issued before the password was set: check=%v, want invalid", got)
	}
	fresh, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.checkToken(fresh); got != tokenOK {
		t.Fatalf("token issued after the password was set: check=%v, want ok", got)
	}

	// The change endpoint revokes, and hands the caller a working replacement.
	body := strings.NewReader(`{"old_password":"old password 123","new_password":"new password 456"}`)
	req := httptest.NewRequest("POST", "/api/auth/change-password", body)
	req.Header.Set("Authorization", "Bearer "+fresh)
	w := httptest.NewRecorder()
	s.authChangePassword(w, req)
	var resp struct {
		Token string `json:"token"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resp) != nil || resp.Token == "" {
		t.Fatalf("change-password status=%d body=%s", w.Code, w.Body)
	}
	if got := s.checkToken(resp.Token); got != tokenOK {
		t.Fatalf("replacement token rejected: %v", got)
	}
	if got := s.checkToken(stale); got != tokenInvalid {
		t.Fatalf("stale token accepted after change: %v", got)
	}
}

// Never having read the cutoff and being unable to read it now must fail closed.
func TestTokenCheckFailsClosedWhenEpochUnreadable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: m, jwtKey: []byte("revocation-test-signing-key-0123456789")}
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	h := s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest("GET", "/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 503 {
		t.Fatalf("status=%d, want 503 when the password cutoff cannot be read", w.Code)
	}
}

// /api/auth/* runs before authentication: the caller must not choose how much
// the server reads into memory.
func TestAuthRoutesCapRequestBody(t *testing.T) {
	s := &Server{jwtKey: []byte("body-cap-test-signing-key-0123456789")}
	var readErr error
	h := s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.Copy(io.Discard, r.Body)
	}))
	huge := strings.NewReader(`{"password":"` + strings.Repeat("a", authBodyLimit*4) + `"}`)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/auth/login", huge))
	var tooLarge *http.MaxBytesError
	if !errors.As(readErr, &tooLarge) {
		t.Fatalf("oversized unauthenticated body read without a cap: err=%v", readErr)
	}
}
