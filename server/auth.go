package server

import (
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	jwtKeyFilename = "jwt.key"
	authPassKey    = "auth.password_hash"
	jwtTTL         = 7 * 24 * time.Hour
	keyChars       = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	// 下限与 setup 页的前端校验一致——校验只放在前端等于没放，直接打 API 就能
	// 绕过。上限是 bcrypt 的硬限制：超过 72 字节 GenerateFromPassword 会返回
	// ErrPasswordTooLong，提前挡掉好过让用户收到一句含义不明的「密码加密失败」。
	minPasswordRunes = 8
	maxPasswordBytes = 72

	// authBodyLimit caps request bodies on the unauthenticated /api/auth/* routes.
	authBodyLimit = 8 << 10
)

// errDataSourceUnavailable 是密码相关读操作失败时统一的回复。这些 handler 绝不能
// 把"读不到"当成"没有设置"：authInit 曾因此在数据库报错时放行，让未认证请求覆盖
// 掉已有的管理员密码。
const errDataSourceUnavailable = "数据源暂时不可用，请稍后重试"

// validatePassword 返回空串表示通过，否则返回可直接展示给用户的中文原因。
func validatePassword(pw string) string {
	if utf8.RuneCountInString(pw) < minPasswordRunes {
		return fmt.Sprintf("密码长度至少 %d 位", minPasswordRunes)
	}
	if len(pw) > maxPasswordBytes {
		return fmt.Sprintf("密码长度不能超过 %d 字节", maxPasswordBytes)
	}
	return ""
}

// loadOrCreateJWTKey reads the 32-byte signing key from keyDir/jwt.key. keyDir is
// the project base dir (next to the executable), NOT the browsable workspace root
// (dataDir) — the signing key must never be listable/downloadable via the file
// manager. Legacy installs kept it at dataDir/jwt.key; if present there and not yet
// at the new location, it is migrated (key preserved, so sessions stay valid) and
// the old file removed so it disappears from the workspace. On first run a random
// key is generated and persisted.
func loadOrCreateJWTKey(keyDir, dataDir string) ([]byte, error) {
	path := filepath.Join(keyDir, jwtKeyFilename)
	// one-time migration out of the old in-workspace location.
	if legacy := filepath.Join(dataDir, jwtKeyFilename); legacy != path {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if data, rerr := os.ReadFile(legacy); rerr == nil {
				if werr := os.WriteFile(path, data, 0o600); werr == nil {
					_ = os.Remove(legacy)
					log.Printf("[auth] JWT key 已从 %s 迁移到 %s（移出可浏览工作区）", legacy, path)
				}
			}
		}
	}
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) >= 32 {
		return []byte(strings.TrimSpace(string(data))), nil
	}
	buf := make([]byte, 32)
	for i := range buf {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(keyChars))))
		if err != nil {
			return nil, fmt.Errorf("generate jwt key: %w", err)
		}
		buf[i] = keyChars[n.Int64()]
	}
	if err := os.WriteFile(path, buf, 0600); err != nil {
		return nil, fmt.Errorf("write jwt key: %w", err)
	}
	log.Printf("[auth] 新 JWT key 已写入 %s", path)
	return buf, nil
}

// signJWT issues a 7-day HS256 token for user ARTEX.
func signJWT(key []byte) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   "ARTEX",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(jwtTTL)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}).SignedString(key)
}

// parseJWT returns the claims of a valid, non-expired HS256 token signed with key.
func parseJWT(tokenStr string, key []byte) (*jwt.RegisteredClaims, bool) {
	claims := &jwt.RegisteredClaims{}
	t, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return key, nil
	})
	if err != nil || !t.Valid {
		return nil, false
	}
	return claims, true
}

// A password change must end every session issued before it: tokens are
// stateless and live 7 days, so otherwise a leaked one kept working after the
// admin changed or reset the password. The cutoff is the password row's
// updated_at, which every write path stamps — the in-app change, first-run init
// and reset-password.sh alike — so no path needs extra bookkeeping.
const (
	// pwEpochTTL bounds how long an out-of-process reset (reset-password.sh) can
	// go unnoticed. In-process changes invalidate the cache immediately.
	pwEpochTTL = 5 * time.Second
	// pwEpochSkew absorbs clock skew between the database (updated_at) and this
	// process (iat) plus iat's one-second truncation, so a token issued right
	// after a change is never rejected.
	pwEpochSkew = 5 * time.Second
)

type pwEpochCache struct {
	mu     sync.Mutex
	at     time.Time // last password write; zero = no password yet
	loaded time.Time
	valid  bool
}

type tokenCheck int

const (
	tokenOK tokenCheck = iota
	tokenInvalid
	tokenUnverifiable // the password epoch could not be read; fail closed
)

// passwordChangedAt returns when the admin password was last written (cached).
// ok=false only when it has never been loaded and the database cannot answer.
func (s *Server) passwordChangedAt() (at time.Time, ok bool) {
	if s.m == nil || s.m.pg == nil {
		return time.Time{}, true // no data source: nothing to compare against
	}
	c := &s.pwEpoch
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && time.Since(c.loaded) < pwEpochTTL {
		return c.at, true
	}
	at, _, err := s.m.pg.SettingUpdatedAt(authPassKey)
	if err != nil {
		if c.valid {
			return c.at, true // transient error: keep enforcing the last known cutoff
		}
		return time.Time{}, false
	}
	c.at, c.loaded, c.valid = at, time.Now(), true
	return at, true
}

// forgetPasswordEpoch drops the cache after an in-process password write.
func (s *Server) forgetPasswordEpoch() {
	s.pwEpoch.mu.Lock()
	s.pwEpoch.valid = false
	s.pwEpoch.mu.Unlock()
}

// checkToken validates signature and expiry, then rejects tokens issued before
// the current password was set.
func (s *Server) checkToken(tokenStr string) tokenCheck {
	claims, ok := parseJWT(tokenStr, s.jwtKey)
	if !ok {
		return tokenInvalid
	}
	changedAt, ok := s.passwordChangedAt()
	if !ok {
		return tokenUnverifiable
	}
	if changedAt.IsZero() {
		return tokenOK
	}
	if claims.IssuedAt == nil || claims.IssuedAt.Time.Before(changedAt.Add(-pwEpochSkew)) {
		return tokenInvalid
	}
	return tokenOK
}

// extractToken reads the JWT from the Authorization: Bearer header, or the
// ?token= query param (EventSource cannot set headers).
//
// The artex_token cookie is deliberately NOT a credential. The UI writes it from
// JavaScript (so it can't be HttpOnly) only for the Next route guard, and sends
// the Authorization header on every API call. Accepting it here let any page on
// the same host — a different port counts as same-site, e.g. an HTTP server an
// agent started — make the browser attach it to a no-preflight POST and drive the
// API as the admin (cross-site request forgery).
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

// requireAuth wraps h with JWT validation.
// /api/auth/* and /api/health are exempt.
func (s *Server) requireAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/auth/") || p == "/api/health" {
			// These run before any authentication, so an unauthenticated caller
			// controls the body size; a multi-GB "password" was read into memory.
			// The largest legitimate body (change-password) is well under 1 KiB.
			r.Body = http.MaxBytesReader(w, r.Body, authBodyLimit)
			h.ServeHTTP(w, r)
			return
		}
		tok := extractToken(r)
		if tok == "" {
			writeErr(w, 401, "未授权")
			return
		}
		switch s.checkToken(tok) {
		case tokenOK:
		case tokenUnverifiable:
			writeErr(w, 503, errDataSourceUnavailable)
			return
		default:
			writeErr(w, 401, "token 无效或已过期")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// GET /api/auth/status — reports whether the admin password has been initialised.
// 读失败必须回 503 而不是 initialized:false：前端在 initialized:false 时会把用户
// 送到 /setup 去设置密码（login/page.tsx），把数据库故障包装成 200 等于把用户往
// 覆盖已有密码的路上推。
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	hash, _, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if hash == "" {
		s.setup.announce() // idempotent; covers a database that was down at startup
	}
	writeJSON(w, 200, map[string]any{"initialized": hash != ""})
}

// POST /api/auth/init — sets the password for the first time; rejected if already
// set, and without the one-time setup token (see setupGate).
func (s *Server) authInit(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	existing, _, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if existing != "" {
		writeErr(w, 403, "密码已设置")
		return
	}
	var req struct {
		Password   string `json:"password"`
		SetupToken string `json:"setup_token"`
	}
	if err := decode(r, &req); err != nil || req.Password == "" {
		writeErr(w, 400, "密码不能为空")
		return
	}
	// Checked before bcrypt so an unauthenticated caller can't make us burn CPU.
	if !s.setup.matches(req.SetupToken) {
		writeErr(w, 403, "初始化令牌不正确：请查看服务启动日志，或程序目录下的 setup-token 文件")
		return
	}
	if msg := validatePassword(req.Password); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "密码加密失败")
		return
	}
	// 用 INSERT ... ON CONFLICT DO NOTHING 而不是 upsert：上面那次 GetSetting 只是
	// 快速失败路径，真正"仅首次可设"的保证落在主键约束上。bcrypt 要跑几十毫秒，
	// 这期间别的请求完全可能先把密码设好，而读检查本身也可能因故障而失效。
	inserted, err := pg.InsertSettingIfAbsent(authPassKey, string(hash))
	if err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if !inserted {
		writeErr(w, 403, "密码已设置")
		return
	}
	s.setup.retire()
	s.forgetPasswordEpoch()
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		writeErr(w, 500, "token 生成失败")
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok})
}

// POST /api/auth/change-password — changes the admin password. Requires a valid
// token (this route is under /api/auth/* which requireAuth exempts, so the token
// is validated here) AND the current password.
func (s *Server) authChangePassword(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	switch s.checkToken(extractToken(r)) {
	case tokenOK:
	case tokenUnverifiable:
		writeErr(w, 503, errDataSourceUnavailable)
		return
	default:
		writeErr(w, 401, "未授权")
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if req.NewPassword == "" {
		writeErr(w, 400, "新密码不能为空")
		return
	}
	if msg := validatePassword(req.NewPassword); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	hash, ok, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if !ok || hash == "" {
		writeErr(w, 403, "密码未初始化，请先设置密码")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.OldPassword)); err != nil {
		writeErr(w, 401, "当前密码错误")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "密码加密失败")
		return
	}
	if err := pg.SetSetting(authPassKey, string(newHash)); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	// Every token issued before this write is now rejected (checkToken), the
	// caller's included — hand it a fresh one so this session carries on.
	s.forgetPasswordEpoch()
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "token": tok})
}

// POST /api/auth/login — validates username/password and returns a JWT.
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if req.Username != "ARTEX" {
		writeErr(w, 401, "用户名或密码错误")
		return
	}
	hash, ok, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if !ok || hash == "" {
		writeErr(w, 403, "密码未初始化，请先设置密码")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		writeErr(w, 401, "用户名或密码错误")
		return
	}
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		writeErr(w, 500, "token 生成失败")
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok})
}
